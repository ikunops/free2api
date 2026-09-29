// Package auth 解析 WorkBuddy auth 文件（嵌套形/扁平形双形态），
// 提供 refresh 后的原子写回。
package auth

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"free2api/internal/logfmt"
)

// Auth 是归一化后的账号凭证（来源可以是插件 OAuth 嵌套形或手写扁平形）。
type Auth struct {
	// mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token。
	mu sync.Mutex

	AccessToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒
	Domain       string
	// realm 账号域（"cn" / "global"），落盘于 auth.realm（嵌套形）或顶层 realm（扁平形）。
	// 空 = 缺省：Realm() 按 domain 后缀回落，最终恒非空。
	//
	// 命名注记：Go 不允许字段与方法同名，持久化字段用未导出 realm，计算访问器用
	// 导出的 Realm()（跨包调用全部走方法）。Parse/SaveAtomic/login 在包内读写字段。
	realm        string
	UID          string
	EnterpriseID string
	Nickname     string
	FilePath     string // 来源文件；refresh 后原子写回此处

	// DeviceToken 设备风控 Token（X-Device-Token 头），来源 auth 文件的 device_token 键。
	// 缺省为空 = 不注入该头（容器内无桌面端 Turing SDK 的常见部署）。
	// 手写扁平形 auth 文件可直接写 "device_token": "..."；插件 OAuth 嵌套形
	// 顶层 device_token 也会被解析（与桌面端共用状态文件的部署方式）。
	DeviceToken string

	// producer 账号归属的 AI 客户端（workbuddy / zcode / qoder …），来源 auth 文件的
	// 顶层 producer 键。空 = workbuddy（历史凭证零回归，出站分派按老路径走）。
	//
	// 为什么凭证自己带一份、而不是只用来源台账（auths/.origins.json）：
	//   - 台账是给管理页分组/筛选看的，是可选的展示层；出站请求**每次**都要判
	//     「打哪套上游」，把分派建立在展示层上会让「删掉台账 → 号全变 workbuddy」。
	//   - 单文件整体迁移语义：把一个 workbuddy-zcode-xxx.json 拷到另一台机器，
	//     不带台账也依然知道自己该打哪套上游。
	// 命名同 realm：Go 不允许字段与方法同名，故未导出字段 producer + 导出访问器
	// Producer()（跨包一律走方法）。
	producer string

	// upstreamBase 该账号要打的上游 base URL（不含路径尾巴）。空 = 按 producer 取
	// 内置缺省（upstream 侧决定）。来源 auth 文件的顶层 upstream_base 键。
	// 这是「用户自己选择 url 是什么」的落点：导入时写死，之后仍可单文件改。
	upstreamBase string

	// zcodeJWT / zcodeDeviceMID zcode 号的**观测凭据**：查套餐到期与剩余额度用。
	// 来源 auth 文件顶层 zcode_jwt / zcode_device_mid 键。
	//
	// 与 accessToken 的分工：accessToken 是反代用的上游 Bearer（id.secret），
	// 出站请求只用它；这两样只在「按需查余额 / 签到」时用（打 zcode.z.ai 的
	// plan/billing 链路 + 必需的 X-Device-Mid 头）。
	//
	// 为什么要单独落盘：这两样在 zcode 生态里原本只躺在 zcode-switch 账本或 ZCode
	// 应用私有目录里（后者按「机器 + 用户名」加密，换机器解不开）。凭证文件自带它们，
	// 才能做到「一个文件拷到另一台机器，额度照样读得到」。
	// 命名同 realm/producer：未导出字段 + 导出访问器。
	zcodeJWT       string
	zcodeDeviceMID string
}

// producerZCode 静态密钥型生产者的标识（与 source.ProducerZCode / upstream.ProducerZCode
// 逐字同值）。刻意不在此包引入上层包：upstream 已
// 反向依赖 auth，从 audit 侧 import upstream 会成环。三处同值由测试锚定
// （TestProducerZCodeLiteralMatchesUpstream / source 各自一条）。
const producerZCode = "zcode"

// Lock 供同进程内其他包（upstream.RefreshToken）在改写 Auth 字段期间加锁。
func (a *Auth) Lock() { a.mu.Lock() }

// Unlock 释放 a.Lock 获取的锁。
func (a *Auth) Unlock() { a.mu.Unlock() }

// AccessTokenValue 加锁读取 AccessToken（出站请求头一律经此取值，勿直读字段）。
//
// 为什么必须加锁：RefreshToken 在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt
// （client.go「第 2 段（锁内）：校验快照一致后写回」），而所有出站请求头构造
// （ChatHeaders / BillingHeaders / fetchEnterpriseModels / fetchV3Models /
// global_models）与调度器的 token 检查都在锁外直读这些字段。生产上两侧真会并发：
// Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号刷新（与是否有在途请求无关），
// 而 handler 正基于**同一个** *auth.Auth 指针构造请求头（Pool.AuthByUID/List 返回的
// 就是池内同一个对象）。无同步直读构成数据竞争，go test -race 实证：
//
//	WARNING: DATA RACE
//	Write at ... by goroutine:
//	  (*Client).RefreshToken()  internal/upstream/client.go:929
//	Previous read at ... by goroutine:
//	  (*Client).ChatHeaders()   internal/upstream/headers.go:224
//
// （回归测试 upstream.TestChatHeadersRacesRefreshToken）。
func (a *Auth) AccessTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.AccessToken
}

// Producer 返回账号归属的 AI 客户端（workbuddy / zcode / qoder …）。
// 空字符串 = 未标注：历史上没有 producer 键的凭证一律按 workbuddy 处置（零回归）。
// 走加锁读：SaveAtomic 在锁内序列化 producer，与读侧同锁（同 AccessTokenValue 理由）。
func (a *Auth) Producer() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.producer
}

// SetProducer 写回 producer（导入路径用）。加锁写，避免与 SaveAtomic 并发读串。
func (a *Auth) SetProducer(p string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.producer = p
}

// UpstreamBase 返回该账号显式指定的上游 base URL（空 = 按 producer 取内置缺省）。
func (a *Auth) UpstreamBase() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.upstreamBase
}

// SetUpstreamBase 写回 upstream_base（导入路径用）。
func (a *Auth) SetUpstreamBase(v string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.upstreamBase = v
}

// ZCodeJWT 返回 zcode 号的观测凭据 zcodejwttoken（空 = 没导入到，读额度要另找来源）。
// 走加锁读：与 SaveAtomic 的序列化同锁（同 AccessTokenValue 的理由）。
func (a *Auth) ZCodeJWT() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.zcodeJWT
}

// SetZCodeJWT 写回 zcode_jwt（导入路径用）。
func (a *Auth) SetZCodeJWT(v string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.zcodeJWT = v
}

// ZCodeDeviceMID 返回 zcode 号的 X-Device-Mid（空 = 上游对空头也认，只是口径可能变）。
func (a *Auth) ZCodeDeviceMID() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.zcodeDeviceMID
}

// SetZCodeDeviceMID 写回 zcode_device_mid（导入路径用）。
func (a *Auth) SetZCodeDeviceMID(v string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.zcodeDeviceMID = v
}

// DomainValue 加锁读取 Domain（同 AccessTokenValue：RefreshToken 在锁内改写它）。
func (a *Auth) DomainValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.Domain
}

// RefreshTokenValue 加锁读取 RefreshToken（同 AccessTokenValue：RefreshToken 在锁内
// 改写它）。调度器的「有无凭证」前置守卫（checkin/keepalive/travel 的
// `a.RefreshToken == ""`）必须经此取值，勿直读字段。
//
// 与 #125 修的 AccessToken/Domain 属同一类：守卫是纯读、刷新是纯写，二者无同步即
// 构成数据竞争（RefreshToken 写回在 client.go「第 2 段（锁内）」的 `if tok.RefreshToken
// != ""` 分支）。go test -race 实证（回归测试 scheduler.TestKeepaliveGuardRacesRefreshToken）：
//
//	WARNING: DATA RACE
//	Read at ... by goroutine:
//	  (*Scheduler).RunKeepaliveNow()  internal/scheduler/scheduler.go:654
//	Previous write at ... by goroutine:
//	  (*Client).RefreshToken()        internal/upstream/client.go:931
func (a *Auth) RefreshTokenValue() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.RefreshToken
}

// ExpiresAtValue 加锁读取 ExpiresAt（Unix 秒）。与 AccessTokenValue/DomainValue 同因：
// RefreshToken 在锁内改写 ExpiresAt，管理页读「这号还能撑到什么时候」必须走同步取值，
// 直读字段与刷新并发构成数据竞争。
func (a *Auth) ExpiresAtValue() int64 {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ExpiresAt
}

// globalEnabled 全局开关：global realm 是否路由（D5 双保险）。
// 默认开启（与 config global.enabled 缺省 true 一致）：Realm() 正常按显式 realm/
// domain 判定 global/cn。显式 SetGlobalEnabled(false)（config "enabled": false）关闭
// → 逃生门：纯 CN 部署，即便 auth 文件写了 realm=global 或 domain 为 .workbuddy.ai
// 也恒判 cn——「关了才锁死」的单一闸口集中收敛在 Realm()/IsGlobal() 里。
var globalEnabled atomic.Bool

func init() { globalEnabled.Store(true) }

// SetGlobalEnabled 注入 global realm 路由开关（false = 锁死纯 CN，逃生门）。
func SetGlobalEnabled(enabled bool) { globalEnabled.Store(enabled) }

// Realm 返回账号的归一化域：显式 Realm=="global" 或 domain 后缀 .workbuddy.ai → "global"，
// 否则 "cn"。显式 global 优先于 domain 回落（D1）。
// 全局开关 SetGlobalEnabled(false) 时恒 "cn"（逃生门：纯 CN 锁定，不影响默认行为）。
// 空 realm + 空 domain → "cn"（老 CN 凭证零回归）。
func (a *Auth) Realm() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realmLocked()
}

// realmLocked Realm 的无锁内部实现：仅限**已持 a.mu** 的调用方使用（sync.Mutex 不可重入，
// 锁内再调 Realm() 会自锁）。realm 由 BackfillRealm 改写、Domain 由 RefreshToken 在锁内
// 改写，故读取必须与写方同锁（理由见 AccessTokenValue 注释）。
func (a *Auth) realmLocked() string {
	if !globalEnabled.Load() {
		return "cn"
	}
	if strings.TrimSpace(a.realm) == "global" || isGlobalDomain(a.Domain) {
		return "global"
	}
	return "cn"
}

// ResolveRealm 归一化 realm（cn/global）：显式非空优先，否则按原始 domain 推断
// （isGlobalDomain）。不受逃生门影响（逃生门是路由锁，不应影响标识判定）；
// domain 也为空 → "cn"（老 CN 凭证零回归）。
func ResolveRealm(explicit, domain string) string {
	if r := strings.TrimSpace(explicit); r != "" {
		return r
	}
	if isGlobalDomain(domain) {
		return "global"
	}
	return "cn"
}

// BackfillRealm 为缺省 realm 标识的账号持久化补标识：a.realm 为空时按「原始 domain 推断」
// 写回（cn/global），返回 (是否有变更, 归一化后的 realm)。已有标识不动（幂等）。
//
// 注意用 isGlobalDomain(a.Domain) 直接推断，而非 Realm()——Realm() 在逃生门
// （SetGlobalEnabled(false)）下恒降级 cn，把 global 账号写死成 cn 会永久污染凭证
// （逃生门是纯 CN 部署的临时锁，不应改写落盘数据）。domain 也为空时写 "cn"（老 CN 凭证）。
func (a *Auth) BackfillRealm() (bool, string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.realm) != "" {
		return false, a.realm
	}
	r := ResolveRealm("", a.Domain)
	a.realm = r
	return true, r
}

// RealmStored 直读持久化的 realm 标识（可能为空 = 未 backfill 的旧文件，Realm() 会 fallback）。
func (a *Auth) RealmStored() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.realm
}

// IsGlobal 报告账号是否属于 global realm（= Realm() == "global"）。
func (a *Auth) IsGlobal() bool { return a.Realm() == "global" }

// isGlobalDomain 判定 domain 是否指向 www.workbuddy.ai 家族。
// 同时接受裸域 workbuddy.ai 与任意子域（HasSuffix("www.workbuddy.ai") 或裸域本身）。
func isGlobalDomain(d string) bool {
	d = strings.ToLower(strings.TrimSpace(d))
	return d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai")
}

// NeedsRefresh 报告 token 是否将在 within 内过期（或已过期/无 expiry）。
//
// 静态密钥型生产者（producerZCode：智谱 API Key，凭据本体就是长期密钥，
// 上游根本没有刷新端点）恒返回 false。这类凭据的 ExpiresAt 恒为 0，
// 语义是「无过期时间」而不是「已过期」：若沿用 OAuth 的「零值 = 需刷新」口径，
// 每个请求都会走「刷新→失败→换号」，整个号池被无谓轮空
// （实测表现：/status 全绿、 /v1/models 正常，但 /v1/chat/completions 恒 503
// no_healthy_account）。workbuddy 的 OAuth token 语义不变（零值
// 仍 = 需刷新）。
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.producer == producerZCode {
		return false
	}
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}

// Parse 兼容两种磁盘形态：
//
//	嵌套形 {"auth":{...},"account":{...}}  （插件 OAuth 输出）
//	扁平形 {"accessToken":...,"uid":...}   （手写/旧版）
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var a Auth
	if _, nested := probe["auth"]; nested {
		var n struct {
			Auth struct {
				AccessToken  string `json:"accessToken"`
				RefreshToken string `json:"refreshToken"`
				ExpiresAt    int64  `json:"expiresAt"`
				Domain       string `json:"domain"`
				Realm        string `json:"realm"`
			} `json:"auth"`
			Account struct {
				UID          string `json:"uid"`
				EnterpriseID string `json:"enterpriseId"`
				Nickname     string `json:"nickname"`
			} `json:"account"`
			// DeviceToken 顶层 device_token（嵌套形与扁平形共用）。
			// 放在 auth 段之外，手写时无需嵌进 auth 对象，降低配置门槛。
			DeviceToken string `json:"device_token"`
			// Producer 顶层 producer（嵌套形与扁平形共用）。
			Producer string `json:"producer"`
			// UpstreamBase 顶层 upstream_base（嵌套形与扁平形共用）。
			UpstreamBase string `json:"upstream_base"`
			// ZCodeJWT / ZCodeDeviceMID 顶层 zcode_jwt / zcode_device_mid（两形共用）。
			// 只有 zcode 号的凭证文件会带，空 = 没有（workbuddy 号零影响）。
			ZCodeJWT       string `json:"zcode_jwt"`
			ZCodeDeviceMID string `json:"zcode_device_mid"`
		}
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:    n.Auth.AccessToken,
			RefreshToken:   n.Auth.RefreshToken,
			ExpiresAt:      n.Auth.ExpiresAt,
			Domain:         n.Auth.Domain,
			realm:          n.Auth.Realm,
			UID:            n.Account.UID,
			EnterpriseID:   n.Account.EnterpriseID,
			Nickname:       n.Account.Nickname,
			DeviceToken:    n.DeviceToken,
			producer:       n.Producer,
			upstreamBase:   n.UpstreamBase,
			zcodeJWT:       n.ZCodeJWT,
			zcodeDeviceMID: n.ZCodeDeviceMID,
		}
	} else {
		var f struct {
			AccessToken    string `json:"accessToken"`
			RefreshToken   string `json:"refreshToken"`
			ExpiresAt      int64  `json:"expiresAt"`
			Domain         string `json:"domain"`
			Realm          string `json:"realm"`
			UID            string `json:"uid"`
			EnterpriseID   string `json:"enterpriseId"`
			Nickname       string `json:"nickname"`
			DeviceToken    string `json:"device_token"`
			Producer       string `json:"producer"`
			UpstreamBase   string `json:"upstream_base"`
			ZCodeJWT       string `json:"zcode_jwt"`
			ZCodeDeviceMID string `json:"zcode_device_mid"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("storage_parse_error: %w", err)
		}
		a = Auth{
			AccessToken:    f.AccessToken,
			RefreshToken:   f.RefreshToken,
			ExpiresAt:      f.ExpiresAt,
			Domain:         f.Domain,
			realm:          f.Realm,
			UID:            f.UID,
			EnterpriseID:   f.EnterpriseID,
			Nickname:       f.Nickname,
			DeviceToken:    f.DeviceToken,
			producer:       f.Producer,
			upstreamBase:   f.UpstreamBase,
			zcodeJWT:       f.ZCodeJWT,
			zcodeDeviceMID: f.ZCodeDeviceMID,
		}
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	return &a, nil
}

// SaveAtomic 以嵌套形原子写回 FilePath（tmp + rename），保持嵌套形（插件可读）格式。
// 全程持 a.mu：防止与 RefreshToken 修改 token 字段并发，杜绝写回半更新。
// 防御：accessToken 为空时拒绝写回，避免误用空凭证覆盖有效文件。
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("save refused: empty accessToken (uid=%s)", a.UID)
	}
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	doc := map[string]any{
		"auth": map[string]any{
			"accessToken":  a.AccessToken,
			"refreshToken": a.RefreshToken,
			"expiresAt":    a.ExpiresAt,
			"domain":       a.Domain,
			"realm":        a.realm,
		},
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	// DeviceToken 非空才写回顶层 device_token：避免在无该字段的旧文件里引入空键
	// （保持与插件 OAuth 输出形状一致，插件读取忽略未知键）。
	if a.DeviceToken != "" {
		doc["device_token"] = a.DeviceToken
	}
	// producer 同理：非空才写。空 = workbuddy，不引入空键（旧文件零改动）。
	if a.producer != "" {
		doc["producer"] = a.producer
	}
	if a.upstreamBase != "" {
		doc["upstream_base"] = a.upstreamBase
	}
	// zcode 观测凭据同理：非空才写（workbuddy 号不引入空键）。
	if a.zcodeJWT != "" {
		doc["zcode_jwt"] = a.zcodeJWT
	}
	if a.zcodeDeviceMID != "" {
		doc["zcode_device_mid"] = a.zcodeDeviceMID
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := a.FilePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.FilePath)
}

// AuthFileGlob auth 文件的统一 glob 模式（宽侧：workbuddy*.json）。
// 网关 LoadDir 与 cmd 运维工具（signin/credit/trial）共用此单一来源——
// 此前 cmd 侧私用 workbuddy-*.json 窄模式，不带连字符的文件（如
// workbuddy_new.json）被网关加载却被运维工具跳过，排障口径对不上
// （审查发现 10）。
const AuthFileGlob = "workbuddy*.json"

// LoadAuthFiles 返回 dir 下按 AuthFileGlob 匹配的 auth 文件清单（已排序）。
// 供 cmd 运维工具复用：只列文件、不解析不迁移（LoadDir 才做 backfill 等副作用），
// 保持 signin/credit/trial 原有的「逐文件 Parse、损坏即跳过/报行错」流程不变。
func LoadAuthFiles(dir string) ([]string, error) {
	files, err := filepath.Glob(filepath.Join(dir, AuthFileGlob))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

// LoadDir 扫描并解析 dir 下 workbuddy*.json；解析失败的文件静默跳过（启动日志由调用方统计）。
// 顺带做 realm 标识存量迁移：对空 realm 的 auth 自动 backfill（原始 domain 推断）并 SaveAtomic
// 落盘，一次性把旧文件补上 realm 键。单个文件写失败不阻断启动（log WARN 继续），
// 避免历史 auth 目录个别文件不可写时整个服务起不来。
func LoadDir(dir string) ([]*Auth, error) {
	files, err := LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	// seenUID 重复 UID 检测：同 UID 出现在多个文件时（双 realm 同名 UID 概率近零）
	// 打 WARN 告警含两文件路径，由「后载入者胜出」保持现状行为（不改变加载结果）。
	seenUID := make(map[string]string, len(files))
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		if prev, ok := seenUID[a.UID]; ok {
			log.Printf("WARN: uid %s duplicated across %s and %s — 后者覆盖（不同 realm 同名 UID？）",
				logfmt.Label(a.UID, a.Nickname), prev, f)
		}
		seenUID[a.UID] = f
		if a.RealmStored() == "" {
			if changed, r := a.BackfillRealm(); changed {
				if err := a.SaveAtomic(); err != nil {
					log.Printf("WARN: auth %s realm backfill save: %v", logfmt.Label(a.UID, a.Nickname), err)
				} else if r == "global" {
					log.Printf("auth %s 存量迁移: 补 realm=global（domain=%s）", logfmt.Label(a.UID, a.Nickname), a.Domain)
				}
			}
		}
		out = append(out, a)
	}
	return out, nil
}

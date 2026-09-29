// Package source 负责「获取源」：把本机各类 AI 工具的登录态归一化成
// free2api 可用的 auth 文件（扁平形），写进 auths 目录。
//
// 存在的理由：原版 free2api 只认 auths 目录里的文件，账号要从别的工具
// （wb-switch 账本、手工粘贴的 JSON、其它机器导出的快照）搬进来，得靠脚本
// 拼装。本包把这一步做成网关自带能力，配合 /admin/sources* 端点即可在
// 管理页里「一键取源」，不必再大包小包迁移。
//
// 三种探查方式（对应产品里的三个入口）：
//
//  1. 用现有 switch 账本  → wb-switch 已经把桌面端登录态聚合在
//     ~/.wb-switch/accounts.json，读它最省事（本包主路径）。
//  2. 导入（粘贴 / 快照）  → 任意 JSON：wb-switch 条目、扁平形 auth、
//     甚至是插件 OAuth 嵌套形，都能吃进来（ImportJSON）。
//  3. 读网关自己已加载的   → auths 目录现状（ListAuths），用于回显「已在池」。
//
// 单位陷阱（务必）：wb-switch 的 expiresAt 是**毫秒**，而 internal/auth 的
// Auth.ExpiresAt 是 **Unix 秒**。normalizeExpiry 做一次性归一（>=1e12 视为毫秒）。
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"free2api/internal/auth"
)

// msThreshold Unix 秒绝不可能达到的阈值：>= 此值即判定为毫秒时间戳。
// 1e12 秒 ≈ 公元 33658 年，任何真实 expiresAt 秒值都远小于它。
const msThreshold = 1_000_000_000_000

// Flat 是 free2api 认得的「扁平形」auth 文件形态（internal/auth.Parse
// 可直接解析）。写盘一律用这个形态：键名自解释、跨机器拷贝即用、不依赖插件。
type Flat struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"` // Unix 秒
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId,omitempty"`
	Nickname     string `json:"nickname,omitempty"`
	DeviceToken  string `json:"device_token,omitempty"`
	// Producer 账号归属的 AI 客户端（workbuddy / zcode / qoder …）。写进凭证文件本身
	// （internal/auth.Auth 的顶层 producer 键），出站请求据此选上游；空 = workbuddy。
	Producer string `json:"producer,omitempty"`
	// UpstreamBase 该账号要打的上游 base URL（不含路径）。留空 = 按 Producer 取缺省：
	// workbuddy → 网关内置 CN/global 域；zcode → https://open.bigmodel.cn/api/paas/v4。
	// 显式写出即可指向自建/镜像上游（用户「自己选择 url 是什么」的落点）。
	UpstreamBase string `json:"upstream_base,omitempty"`
	// ZCodeJWT / ZCodeDeviceMID zcode 号的**观测凭据**（查套餐到期与剩余额度用），
	// 与上面反代用的 accessToken 是两套东西：zcodejwttoken 打的是 zcode.z.ai 的
	// plan/billing 链路，device mid 是必需的 X-Device-Mid 头。
	//
	// 为什么必须落盘：这两样在 zcode 生态里原本只躺在 zcode-switch 账本或 ZCode
	// 应用私有目录里（且后者按「机器 + 用户名」加密，换机器解不开）。号池文件不带
	// 它们的话，换机器 / 删账本就再也读不到额度——「大包小包地迁移」正是要避免的。
	ZCodeJWT       string `json:"zcode_jwt,omitempty"`
	ZCodeDeviceMID string `json:"zcode_device_mid,omitempty"`
}

// Kind 一类「源」（生产者）。Available=false 表示本机没有该工具的数据，
// 前端据此把入口置灰并显示 Note 里的原因。
type Kind struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Path      string `json:"path"`
	Available bool   `json:"available"`
	Count     int    `json:"count"`
	// Usable 「可取用」计数，口径与 Count 不同：Count 是源里的**存档/账号**总数
	// （能看到几个号），Usable 是其中真正抽到上游凭据、能进号池反代的数量。
	// 目前只有 zcode-switch 会两者不等（见 zcode.go 文件头两条硬线）；别的源留空，
	// 前端据此只在需要时讲清「N 个存档 · M 个可取用」，不各写一份分支。
	Usable  int      `json:"usable,omitempty"`
	Methods []string `json:"methods"` // "switch" / "import" / "current"
	// Method 该源属于三种「来源」中的哪一种：app / switch / file。
	// auths 是导入的**结果**（号池本身）而不是来源，Method 留空。
	Method string `json:"method,omitempty"`
	Note   string `json:"note,omitempty"`
}

// Candidate 一条可导入的账号。Imported 表示目标 auths 目录里已存在（按 uid）。
type Candidate struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Detail     string `json:"detail,omitempty"`
	Realm      string `json:"realm"`
	ExpiresAt  int64  `json:"expires_at"`
	Expired    bool   `json:"expired"`
	HasRefresh bool   `json:"has_refresh"`
	Imported   bool   `json:"imported"`
	ImportedAs string `json:"imported_as,omitempty"`

	// 以下为 zcode 这类「凭据形态不同的源」补充的字段（workbuddy 侧留空，零回归）。
	// Producer 该候选归属的 AI 客户端；Models 账号自带的模型 id 清单（**不是** agent
	// 侧配置里的自定义模型）；Upstream 要打的上游 base；APIKeyHint 凭据指纹（前 6 后 4，
	// 明文永不回显——页面上要能对账「是不是同一把 key」但不能泄露）。
	Producer   string   `json:"producer,omitempty"`
	Models     []string `json:"models,omitempty"`
	Upstream   string   `json:"upstream,omitempty"`
	APIKeyHint string   `json:"api_key_hint,omitempty"`
	Note       string   `json:"note,omitempty"`
	// Usable 该候选今天能不能真的反代（凭据已抽到、未被加密/未被 captcha 闸拦）。
	// 前端据此把「只能看的号」与「能接流量的号」分开，不靠 HasRefresh 猜语义。
	Usable bool `json:"usable"`
}

// ImportResult 导入结果汇总。Skipped 是「目标已存在同名 uid」被跳过的，
// Errors 是单条失败原因（不阻断其余条目）。
type ImportResult struct {
	Kind     string   `json:"kind"`
	AuthDir  string   `json:"auth_dir"`
	Imported []string `json:"imported"`
	Skipped  []string `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

// ---------- 默认路径 ----------

// HomeDir 返回当前用户主目录（取不到时回落 "."，避免 panic）。
func HomeDir() string {
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return h
	}
	return "."
}

// DefaultWBSwitchPath WorkBuddy Switch 账本路径。
func DefaultWBSwitchPath() string {
	return filepath.Join(HomeDir(), ".wb-switch", "accounts.json")
}

// DefaultZCodeSwitchDir zcode-switch 账号快照目录（另一条链路，本包只发现不导入）。
func DefaultZCodeSwitchDir() string {
	return filepath.Join(HomeDir(), ".zcode-switch", "accounts")
}

// ---------- 内部解析结构 ----------

// portable 是「什么形状都先吃进来」的宽口结构：同时容纳
//   - wb-switch 账本条目（snake_case + variant + camelCase expiresAt[毫秒]）
//   - 扁平形 auth 文件（camelCase + realm）
//   - 插件 OAuth 嵌套形（{auth:{...},"account":{...}}）
//
// 冲突字段（expiresAt）只留一个，靠量级区分毫秒/秒（见 normalizeExpiry）。
type portable struct {
	// snake（wb-switch / 部分导出器）
	AccessTokenSnake  string `json:"access_token"`
	RefreshTokenSnake string `json:"refresh_token"`
	Variant           string `json:"variant"`
	// camel（扁平形 auth 文件）
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// 公共
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
	DeviceToken  string `json:"device_token"`
	Producer     string `json:"producer"`
	UpstreamBase string `json:"upstream_base"`
	// 嵌套形（插件 OAuth）
	Auth    *nestedAuth    `json:"auth"`
	Account *nestedAccount `json:"account"`
}

type nestedAuth struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	Realm        string `json:"realm"`
}

type nestedAccount struct {
	UID          string `json:"uid"`
	EnterpriseID string `json:"enterpriseId"`
	Nickname     string `json:"nickname"`
}

// normalizeExpiry 把毫秒时间戳归一成 Unix 秒。已是秒的原样返回。
func normalizeExpiry(v int64) int64 {
	if v >= msThreshold {
		return v / 1000
	}
	return v
}

// fromPortable 把宽口结构收敛成 Flat（不做写盘）。accessToken 为空时报错。
func fromPortable(p portable) (Flat, error) {
	f := Flat{
		ExpiresAt:    normalizeExpiry(p.ExpiresAt),
		Domain:       p.Domain,
		Realm:        p.Realm,
		UID:          p.UID,
		EnterpriseID: p.EnterpriseID,
		Nickname:     p.Nickname,
		DeviceToken:  p.DeviceToken,
		Producer:     p.Producer,
		UpstreamBase: p.UpstreamBase,
	}
	if p.Auth != nil { // 嵌套形优先（插件 OAuth 输出的权威字段）
		if p.Auth.AccessToken != "" {
			f.AccessToken = p.Auth.AccessToken
			f.RefreshToken = p.Auth.RefreshToken
			f.ExpiresAt = normalizeExpiry(p.Auth.ExpiresAt)
			if p.Auth.Domain != "" {
				f.Domain = p.Auth.Domain
			}
			if p.Auth.Realm != "" {
				f.Realm = p.Auth.Realm
			}
		}
		if p.Account != nil {
			if p.Account.UID != "" {
				f.UID = p.Account.UID
			}
			if p.Account.EnterpriseID != "" {
				f.EnterpriseID = p.Account.EnterpriseID
			}
			if p.Account.Nickname != "" {
				f.Nickname = p.Account.Nickname
			}
		}
	}
	if f.AccessToken == "" {
		f.AccessToken = firstNonEmpty(p.AccessToken, p.AccessTokenSnake)
	}
	if f.RefreshToken == "" {
		f.RefreshToken = firstNonEmpty(p.RefreshToken, p.RefreshTokenSnake)
	}
	if f.Realm == "" {
		f.Realm = p.Variant // wb-switch 用 variant 表达 cn/global
	}
	if strings.TrimSpace(f.AccessToken) == "" {
		return Flat{}, fmt.Errorf("missing access token (accessToken / access_token / auth.accessToken 全空)")
	}
	// realm 归一：先把各工具的叫法收成 cn/global（ai→global），再走
	// auth.ResolveRealm（显式优先、否则按 domain 推断）。
	f.Realm = auth.ResolveRealm(normalizeRealmAlias(f.Realm), f.Domain)
	if f.UID == "" {
		f.UID = derivedUID(f.AccessToken)
	}
	return f, nil
}

// realmAliases 各工具对「域」的叫法不一：wb-switch 用 variant=cn/ai，
// 网关词表只有 cn/global。这里做一次别名收敛，别名之外的未知值一律当"未指定"，
// 交给 auth.ResolveRealm 按 domain 推断——绝不把 "ai" 这种原生词直接落盘，
// 否则 auth.Parse 会把它当成显式 realm 记下来（domain 为空时就会错路由）。
var realmAliases = map[string]string{
	"cn":       "cn",
	"global":   "global",
	"ai":       "global", // wb-switch 的海外域叫法
	"intl":     "global",
	"overseas": "global",
	"海外":       "global",
	"国内":       "cn",
}

// normalizeRealmAlias 收敛别名；未知值返回空串（= 未指定，按 domain 推断）。
func normalizeRealmAlias(v string) string {
	k := strings.ToLower(strings.TrimSpace(v))
	if k == "" {
		return ""
	}
	if canon, ok := realmAliases[k]; ok {
		return canon
	}
	return ""
}

// derivedUID 在源没给 uid 时，用 access token 的哈希前缀造一个稳定 id，
// 保证同一账号重复导入落到同一文件名（幂等）。
func derivedUID(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return "anon-" + hex.EncodeToString(sum[:6])
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------- 发现 ----------

var uidUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// sanitizeUID 把 uid 压成安全文件名片（防目录穿越 / 非法字符）。
func sanitizeUID(uid string) string {
	s := uidUnsafe.ReplaceAllString(strings.TrimSpace(uid), "_")
	return strings.Trim(s, ".")
}

// AuthFileName 目标文件名（必须匹配 auth.AuthFileGlob = workbuddy*.json）。
func AuthFileName(uid string) string {
	return "workbuddy-" + sanitizeUID(uid) + ".json"
}

// Detect 扫描本机所有可用的源，返回给前端渲染来源卡。
func Detect(wbSwitchPath, zcodeDir, authDir string) []Kind {
	if wbSwitchPath == "" {
		wbSwitchPath = DefaultWBSwitchPath()
	}
	if zcodeDir == "" {
		zcodeDir = DefaultZCodeSwitchDir()
	}
	out := []Kind{
		detectApp(),
		detectWBSwitch(wbSwitchPath),
		{
			ID:      "paste",
			Name:    "导入（粘贴 / 快照）",
			Path:    "",
			Methods: []string{"import"},
			Method:  MethodFile,
			Note:    "把任意账号 JSON 贴进来：wb-switch 条目、扁平形 auth、插件 OAuth 嵌套形都能识别",
		},
		detectZCodeSwitch(zcodeDir),
		detectOpenCode(),
		detectAuths(authDir),
	}
	return out
}

// detectOpenCode OpenCode（CLI/TUI）自家账号的取源结论。它只有「本机 auth.json」这一条路
// （OpenCode 没有 switch 类账本工具），所以 Method 记 app：账号来自工具本体自己的登录态。
func detectOpenCode() Kind {
	path := DefaultOpenCodeAuthPath()
	k := Kind{
		ID:      "opencode",
		Name:    "OpenCode CLI/TUI（OpenCode Zen / Go）",
		Path:    path,
		Methods: []string{"app"},
		Method:  MethodApp,
	}
	accounts, err := ListOpenCodeAccounts(path)
	switch {
	case err != nil:
		k.Note = "没读到 " + path + "：OpenCode 没装 / 没登录过。装好后在 OpenCode 里登录一次即可"
		return k
	case len(accounts) == 0:
		k.Note = "auth.json 存在，但没有 OpenCode 自家的凭据（只找到第三方 provider 的 key）；" +
			"在 OpenCode 里登录 OpenCode Zen / OpenCode Go 后再来"
		return k
	}
	k.Available = true
	k.Count = len(accounts)
	k.Usable = len(accounts)
	var zen, goN int
	for _, a := range accounts {
		if a.Provider == OpenCodeGoProvider {
			goN++
		} else {
			zen++
		}
	}
	k.Note = "读到 " + itoa(len(accounts)) + " 条自家凭据（OpenCode Zen " + itoa(zen) + " · OpenCode Go " + itoa(goN) + "）；" +
		"Zen 付费模型可直连反代；免费层有服务端闸（只能从 OpenCode 本体发起，直连 403 FreeTierError），本网关不反代免费层"
	return k
}

// detectApp 汇总「应用内登录态」探查结论：本机装了哪些工具、登录态存在哪、
// 能不能直接读。可用性 = 至少探测到一个工具的数据目录。
func detectApp() Kind {
	k := Kind{
		ID:      "app",
		Name:    "应用内登录态（本机工具自己存的那一个登录）",
		Path:    HomeDir(),
		Methods: []string{"app"},
		Method:  MethodApp,
	}
	readable, found := 0, 0
	for _, t := range Probe() {
		if !t.Exists {
			continue
		}
		found++
		if t.Readable {
			readable += t.Accounts
		}
	}
	k.Available = found > 0
	k.Count = readable
	switch {
	case found == 0:
		k.Note = "本机没探查到已知的 AI 工具数据目录"
	case readable == 0:
		k.Note = "探查到 " + itoa(found) + " 个工具目录，但登录态都是加密/私有格式（见下方逐个结论）；" +
			"要批量取号请走 switch 账本，或从别的机器导出文件导入"
	default:
		k.Note = "可直接读取的账号 " + itoa(readable) + " 个（其余工具目录为加密/私有格式）"
	}
	return k
}

func detectWBSwitch(path string) Kind {
	k := Kind{
		ID:      "wb-switch",
		Name:    "WorkBuddy 账本（wb-switch）",
		Path:    path,
		Methods: []string{"switch", "current"},
		Method:  MethodSwitch,
	}
	cands, err := ListWBSwitch(path)
	switch {
	case err != nil:
		k.Note = "读取失败：" + err.Error()
	case len(cands) == 0:
		k.Note = "账本为空：先用 wb-switch 登录至少一个账号"
	default:
		k.Available = true
		k.Count = len(cands)
	}
	return k
}

func detectZCodeSwitch(dir string) Kind {
	k := Kind{
		ID:      "zcode-switch",
		Name:    "zcode 账本（zcode-switch）",
		Path:    dir,
		Methods: []string{"switch"},
		Method:  MethodSwitch,
	}
	cands, err := ListZCodeSwitch(dir)
	if err != nil {
		k.Note = "未找到 zcode-switch 账号目录（zcode 是独立链路，需要单独的 zcode 适配器）"
		return k
	}
	plain := 0
	for _, c := range cands {
		if c.HasRefresh {
			plain++
		}
	}
	k.Count = len(cands)
	k.Available = len(cands) > 0
	// Usable 与 Count 是两个口径：zswitch 的「存档数」包含只有 z-code 计划 JWT、
	// 没有智谱上游 key 的号（见 zcode.go 文件头硬线 2）。这些号能显示、能读额度，
	// 但进不了号池，所以「获取源」卡片必须同时给出两个数，别让用户拿 10 去对号池的 7。
	if accounts, aerr := ListZCodeAccounts(dir); aerr == nil {
		for _, acc := range accounts {
			if _, ok := acc.usableKey(); ok {
				k.Usable++
			}
		}
	}
	switch {
	case len(cands) == 0:
		k.Note = "没读到账号快照"
	case plain == 0:
		k.Note = "读到 " + itoa(len(cands)) + " 个存档，但凭据全被加密（enc:v1）——要导出得在装 zcode 的那台机器上取"
	case k.Usable < k.Count:
		k.Note = "读到 " + itoa(len(cands)) + " 个存档，其中 " + itoa(k.Usable) + " 个可取用；其余只有 z-code 计划 JWT（zcode.z.ai 链路被 captcha 拦）或加密凭据，进不了号池"
	default:
		k.Note = "读到 " + itoa(len(cands)) + " 个账号：凭据明文可读 " + itoa(plain) + " 个、加密 " + itoa(len(cands)-plain) + " 个"
	}
	return k
}

// zcodeAccount zcode-switch 单账号快照里本包关心的字段。
// 只按**字段名**取用，不解析凭证内容——zcode 的 token 属于智谱（Z.ai / BigModel）
// 链路，与 WorkBuddy 的 auth 文件不同构，不能混进同一个号池。
type zcodeAccount struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	UpdatedAt   string                     `json:"updated_at"`
	Credentials map[string]json.RawMessage `json:"credentials"`
}

// zcodeCredPrefix 加密字段的统一前缀（zcode 用 Electron safeStorage 之类封的）。
const zcodeCredPrefix = "enc:v1:"

// zcodePlainCred 判断一个凭证字段是否为明文（非 enc:v1: 前缀）。
// JSON 值可能是带引号的字符串，也可能是个对象；对象一律当「读不出」（不猜）。
func zcodePlainCred(v json.RawMessage) bool {
	s := strings.TrimSpace(string(v))
	if s == "" || strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		return false
	}
	return !strings.HasPrefix(strings.Trim(s, "\""), zcodeCredPrefix)
}

// zcodeProvider 从 credentials 的 oauth:active_provider 读出归属方（zai / bigmodel）。
// 取不到或该键本身被加密 → 返回 ""（不猜）。
func zcodeProvider(c map[string]json.RawMessage) string {
	raw, ok := c["oauth:active_provider"]
	if !ok || !zcodePlainCred(raw) {
		return ""
	}
	return strings.Trim(strings.TrimSpace(string(raw)), "\"")
}

// ListZCodeSwitch 读 zcode-switch 账号目录，返回**只读**候选（当前不可导入）。
//
// 为什么只读不导入：zcode 的账号是智谱系（Z.ai 国际版 / BigModel 国内版）的凭证，
// 与 WorkBuddy 的 token 不同构、端点也不同，落进同一个号池会被当成 WorkBuddy 号打上游
// 风控。这一路当前的价值是「看清本机有哪些号、归属哪个 provider、有几个能明文导出」；
// 真正接反代要走独立的 zcode provider 适配器。
//
// HasRefresh 在此复用为「凭据是否明文可读」：明文 = 能导出，加密 = 必须回原机器取。
func ListZCodeSwitch(dir string) ([]Candidate, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			continue
		}
		var a zcodeAccount
		if jerr := json.Unmarshal(raw, &a); jerr != nil {
			continue
		}
		plain, enc := 0, 0
		for k, v := range a.Credentials {
			if k == "oauth:active_provider" {
				continue
			}
			if zcodePlainCred(v) {
				plain++
			} else {
				enc++
			}
		}
		// provider 归属：优先取 active_provider，再从凭证键名兜底（oauth:zai: / oauth:bigmodel:）。
		providers := make([]string, 0, 2)
		if p := zcodeProvider(a.Credentials); p != "" {
			providers = append(providers, p)
		}
		// 凭证键形如 oauth:<provider>:<field>（oauth:zai:access_token /
		// oauth:bigmodel:user_info）。provider 名取中间段；键名本身也是「这号归属谁」
		// 的可靠证据（比只看 active_provider 更全：一号可同时持有两个 provider）。
		for k := range a.Credentials {
			if !strings.HasPrefix(k, "oauth:") {
				continue
			}
			parts := strings.Split(k, ":")
			if len(parts) < 3 {
				continue
			}
			name := parts[1]
			if name == "" || name == "active_provider" || containsStr(providers, name) {
				continue
			}
			providers = append(providers, name)
		}
		sort.Strings(providers)
		id := a.ID
		if id == "" {
			id = strings.TrimSuffix(e.Name(), filepath.Ext(e.Name()))
		}
		label := strings.TrimSpace(a.Name)
		if label == "" {
			label = id
		}
		detail := "provider=" + strings.Join(providers, "/")
		if len(providers) == 0 {
			detail = "provider=未知"
		}
		detail += " · 明文字段 " + itoa(plain) + " / 加密字段 " + itoa(enc)
		out = append(out, Candidate{
			ID:         id,
			Label:      label,
			Detail:     detail,
			Realm:      "zcode",
			HasRefresh: plain > 0,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// containsStr 小工具：字符串切片包含判定（providers 去重用）。
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func detectAuths(dir string) Kind {
	k := Kind{
		ID:      "auths",
		Name:    "网关已加载（auths 目录）",
		Path:    dir,
		Methods: []string{"current"},
	}
	cands, err := ListAuths(dir)
	if err != nil {
		k.Note = "读取失败：" + err.Error()
		return k
	}
	k.Available = true
	k.Count = len(cands)
	k.Note = "网关当前已在用的账号；这里只读回显，新增请走上方的账本 / 导入"
	return k
}

// ---------- 列出候选 ----------

// ListWBSwitch 读 wb-switch 账本，返回可导入候选（已标注是否已在 auths 目录）。
func ListWBSwitch(path string) ([]Candidate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCandidates(raw, "")
}

// ListWBSwitchMarked 同 ListWBSwitch，但顺带按 uid 标注「已在目标 auths 目录」，
// 供管理页把已导入的候选置灰。
func ListWBSwitchMarked(path, authDir string) ([]Candidate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parseCandidates(raw, authDir)
}

// ListAuths 读 auths 目录现状（回显用）。
func ListAuths(dir string) ([]Candidate, error) {
	files, err := auth.LoadAuthFiles(dir)
	if err != nil {
		return nil, err
	}
	out := make([]Candidate, 0, len(files))
	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		p, perr := parseOne(raw)
		if perr != nil {
			continue
		}
		out = append(out, Candidate{
			ID:         p.UID,
			Label:      labelOf(p),
			Detail:     p.Domain,
			Realm:      p.Realm,
			ExpiresAt:  p.ExpiresAt,
			Expired:    isExpired(p.ExpiresAt),
			HasRefresh: p.RefreshToken != "",
			Imported:   true,
			ImportedAs: filepath.Base(f),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// parseCandidates 解析「数组或单对象」的 JSON，全部收敛成 Candidate。
// importedDir 非空时顺带标注 Imported（按 uid 比对文件名）。
func parseCandidates(raw []byte, importedDir string) ([]Candidate, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("empty json")
	}
	var items []json.RawMessage
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, fmt.Errorf("parse array: %w", err)
		}
	} else {
		items = []json.RawMessage{json.RawMessage(raw)}
	}
	existing := map[string]string{}
	if importedDir != "" {
		existing = importedIndex(importedDir)
	}
	out := make([]Candidate, 0, len(items))
	var firstErr error
	for _, it := range items {
		p, err := parseOne(it)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		c := Candidate{
			ID:         p.UID,
			Label:      labelOf(p),
			Detail:     p.Domain,
			Realm:      p.Realm,
			ExpiresAt:  p.ExpiresAt,
			Expired:    isExpired(p.ExpiresAt),
			HasRefresh: p.RefreshToken != "",
		}
		if fn, ok := existing[p.UID]; ok {
			c.Imported = true
			c.ImportedAs = fn
		}
		out = append(out, c)
	}
	if len(out) == 0 && firstErr != nil {
		return nil, firstErr
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// parseOne 解析单条记录为 Flat。
func parseOne(raw []byte) (Flat, error) {
	var p portable
	if err := json.Unmarshal(raw, &p); err != nil {
		return Flat{}, fmt.Errorf("parse: %w", err)
	}
	return fromPortable(p)
}

func labelOf(p Flat) string {
	if strings.TrimSpace(p.Nickname) != "" {
		return p.Nickname
	}
	if len(p.UID) > 8 {
		return p.UID[:8]
	}
	if p.UID != "" {
		return p.UID
	}
	return "(无名账号)"
}

func isExpired(expUnix int64) bool {
	return expUnix > 0 && expUnix <= time.Now().Unix()
}

// importedIndex 扫 auths 目录，返回 uid → 文件名。
func importedIndex(dir string) map[string]string {
	files, err := auth.LoadAuthFiles(dir)
	if err != nil {
		return nil
	}
	out := make(map[string]string, len(files))
	for _, f := range files {
		raw, rerr := os.ReadFile(f)
		if rerr != nil {
			continue
		}
		p, perr := parseOne(raw)
		if perr != nil {
			continue
		}
		out[p.UID] = filepath.Base(f)
	}
	return out
}

// ---------- 导入 ----------

// ImportOptions 导入时附带的来源归属（决定账号在管理页里归到哪个来源）。
// Method 取 app / switch / file（见 origins.go）；留空按「导入文件」算——
// 没说明来源的场合，用户多半是在贴别的机器导出的东西。
type ImportOptions struct {
	Method   string
	Producer string
	Detail   string // 来源细节：账本路径 / 文件名，落进台账便于事后对账
	// UpstreamBase 显式指定该批账号要打的上游 base（空 = 按 Producer 取缺省）。
	// 落盘在凭证文件的 upstream_base 键，导入后仍可单独改。
	UpstreamBase string
}

func (o ImportOptions) normalized() ImportOptions {
	if o.Method == "" {
		o.Method = MethodFile
	}
	if o.Producer == "" {
		o.Producer = ProducerWorkbuddy
	}
	return o
}

// ImportWBSwitch 从 wb-switch 账本导入。ids 为空 = 全量；否则只导 id 命中的。
// 来源记为「switch 账本」；要显式指定来源（例如另一台机器导出的账本快照）用 ImportWBSwitchAs。
func ImportWBSwitch(path, authDir string, ids []string) (ImportResult, error) {
	return ImportWBSwitchAs(path, authDir, ids, ImportOptions{Method: MethodSwitch, Detail: path})
}

// ImportWBSwitchAs 同 ImportWBSwitch，但显式指定来源归属。
func ImportWBSwitchAs(path, authDir string, ids []string, opt ImportOptions) (ImportResult, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ImportResult{}, err
	}
	opt = opt.normalized()
	if opt.Detail == "" {
		opt.Detail = path
	}
	return importBlob("wb-switch", authDir, raw, ids, opt)
}

// ImportJSON 导入任意 JSON（粘贴 / 快照）。形状兼容 wb-switch 条目、
// 扁平形 auth、插件 OAuth 嵌套形；可为单对象或数组。来源记为「导入文件」。
func ImportJSON(authDir string, raw []byte, ids []string) (ImportResult, error) {
	return ImportJSONAs(authDir, raw, ids, ImportOptions{Method: MethodFile})
}

// ImportJSONAs 同 ImportJSON，但显式指定来源归属。
func ImportJSONAs(authDir string, raw []byte, ids []string, opt ImportOptions) (ImportResult, error) {
	return importBlob("import", authDir, raw, ids, opt.normalized())
}

func importBlob(kind, authDir string, raw []byte, ids []string, opt ImportOptions) (ImportResult, error) {
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return ImportResult{}, err
	}
	trimmed := strings.TrimSpace(string(raw))
	var items []json.RawMessage
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal(raw, &items); err != nil {
			return ImportResult{}, fmt.Errorf("parse array: %w", err)
		}
	} else {
		items = []json.RawMessage{json.RawMessage(raw)}
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	res := ImportResult{Kind: kind, AuthDir: authDir}
	labels := map[string]string{}
	for _, it := range items {
		p, err := parseOne(it)
		if err != nil {
			res.Errors = append(res.Errors, err.Error())
			continue
		}
		if len(want) > 0 && !want[p.UID] {
			continue
		}
		// 生产者归属写进凭证本身：出站分派（upstream）读的是凭证，不是台账。
		// 导入器给的 opt.Producer 优先于原始 JSON 里可能带的 producer（导入时的
		// 选择就是权威），没给则保留原值，最后兜底 workbuddy。
		if opt.Producer != "" {
			p.Producer = opt.Producer
		}
		if p.Producer == "" {
			p.Producer = ProducerWorkbuddy
		}
		if opt.UpstreamBase != "" {
			p.UpstreamBase = opt.UpstreamBase
		}
		dest := filepath.Join(authDir, AuthFileName(p.UID))
		if _, serr := os.Stat(dest); serr == nil {
			res.Skipped = append(res.Skipped, p.UID)
			continue
		}
		if werr := writeFlatAtomic(dest, p); werr != nil {
			res.Errors = append(res.Errors, p.UID+": "+werr.Error())
			continue
		}
		res.Imported = append(res.Imported, p.UID)
		labels[p.UID] = labelOf(p)
	}
	sort.Strings(res.Imported)
	sort.Strings(res.Skipped)
	// 记来源归属：凭证已经落盘，台账写失败只是少个标注，不该让导入报错。
	if len(res.Imported) > 0 {
		mark := make(map[string]Origin, len(res.Imported))
		for _, uid := range res.Imported {
			mark[uid] = Origin{Method: opt.Method, Producer: opt.Producer, Label: labels[uid], Detail: opt.Detail}
		}
		_ = LoadRegistry(authDir).Mark(mark)
	}
	return res, nil
}

// writeFlatAtomic tmp + rename 原子写盘，权限 0600（含明文 token）。
func writeFlatAtomic(path string, f Flat) error {
	buf, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

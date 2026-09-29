// zcode.go 智谱（BigModel / Z.ai）上游适配器：把「zcode 账号自带的 apiKey」接到
// 标准 OpenAI 兼容端点上。
//
// 与 workbuddy 的分工：workbuddy 账号打 {chatBase}/v2/chat/completions，要带一整套
// 桌面端头族（X-Enterprise-Id / X-Domain / 设备风控 token / 会话链路头 …）；zcode
// 账号打 https://open.bigmodel.cn/api/paas/v4/chat/completions——标准 OpenAI 兼容端点，
// 认证只有一枚 Bearer。所以本适配器**刻意做薄**：
//
//   - 只换 base URL 与头组（Authorization + Content-Type + Accept），不注入任何
//     workbuddy 专有头——那些头对智谱是垃圾，注进去反而可能被风控当异常流量。
//   - 请求体原样透传，不过 prepareBody（不做 fingerprint 中和、不注入 prompt_cache_key、
//     不做 effort 档位降级）：这些是 workbuddy 上游的特定需要，智谱端点认的是标准字段，
//     动它只会引入「未知字段被拒」的风险。
//   - realm 不参与分派：zcode 账号的 realm 恒 cn（导入时写死），双域那套不适用。
//
// 实测依据（2026-09，本机 10 个号轮测）：
//
//	POST /api/paas/v4/chat/completions  Bearer <49位 id.secret>  → 200 真回复
//	GET  /api/paas/v4/models            Bearer <同一把 key>      → 200 模型清单
//	POST …/chat/completions  Bearer <裸 32 位 hex>               → 401 令牌已过期
//	GET  …/models            无 Authorization                    → 1001 未收到 Authorization
//	POST …/chat/completions  合法体但打 zcode.z.ai 计划端点        → 3007 captcha verify failed
//
// 最后一条是**红线**：zcode.z.ai 那条计划链路被阿里云风险识别（captcha）挡在业务逻辑
// 之前，认证已过但拿不到业务结果。本适配器只做 open.bigmodel.cn / api.z.ai 这条，
// 不碰计划端点（见 internal/source/zcode.go 文件头硬线 2）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
)

const (
	// defaultZCodeChatBase 智谱 BigModel（国内）的 OpenAI 兼容前缀。
	defaultZCodeChatBase = "https://open.bigmodel.cn/api/paas/v4"
	// zcodeChatPath OpenAI 兼容的对话补全路径（拼在 base 之后）。
	zcodeChatPath = "/chat/completions"
	// zcodeModelsPath OpenAI 兼容的模型清单路径。
	zcodeModelsPath = "/models"

	// zcodeBalanceURL zcode 计划链路的额度查询地址（**只读**，不在 captcha 闸后；
	// 领取余额的 POST /billing/claim 才需要 captcha，本适配器不碰）。
	zcodeBalanceURL = "https://zcode.z.ai/api/v1/zcode-plan/billing/balance"
	// zcodeAppVersion 计划端点要求的版本号（缺它上游会按老客户端处理/拒答）。
	zcodeAppVersion = "3.11.2"
)

// zcodeOn 报告账号是否走 zcode（智谱）上游。判据是凭据自带的 producer 键
// （见 auth.Auth.Producer）：空/producer=workbuddy 一律走原路径，零回归。
func (c *Client) zcodeOn(a *auth.Auth) bool { return a != nil && a.Producer() == ProducerZCode }

// ProducerZCode zcode 生产者标识。与 internal/source.ProducerZCode 同值——
// 这里刻意不 import internal/source：source 是「取源」层（文件 I/O），
// upstream 是「出站」层，让出站层反向依赖取源层会把两个方向的依赖搅在一起。
// 两边同值由 internal/source 的常量测试锚定（见 source/zcode_test.go）。
const ProducerZCode = "zcode"

// zcodeBase 生效的 zcode chat base：凭证自带 upstream_base > Client 覆盖 > 内置缺省。
// 凭证自带那档是「用户自己选 url」的落点（导入时写进 auth 文件的 upstream_base 键）。
func (c *Client) zcodeBase(a *auth.Auth) string {
	if a != nil {
		if b := strings.TrimSpace(a.UpstreamBase()); b != "" {
			return strings.TrimRight(b, "/")
		}
	}
	if b := strings.TrimSpace(c.ChatBaseZCode); b != "" {
		return strings.TrimRight(b, "/")
	}
	return defaultZCodeChatBase
}

// zcodeHeaders 智谱端点要的头。只有三样：鉴权、内容类型、接受类型。
// 其余头（Accept-Language / User-Agent 等）交给 Go 的默认值——实测不带任何
// 伪装头也能 200，多注入反而可能被风险识别盯上。
func (c *Client) zcodeHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
}

// classifyZCode 在通用 Classify 之上补两条智谱特有的口径：
//
//   - 鉴权失败（401 / 1001 未收到 Authorization / 令牌已过期）→ ErrSessionDead。
//     通用 Classify 会把 401 兜底成 ErrClient（「只换号不罚」），而 zcode 的 401
//     是**确定性**的坏凭据（实测：裸 32 位 hex 恒 401），必须走「连续 N 次 → 禁用」
//     这条终态路径，否则它会一直留在池里被反复选中、每次都白打一趟。
//   - 余额不足（1113 / 余额不足 / insufficient balance）→ ErrHardCredit 长冷却，
//     而不是 429 的软限流（软限流会 60s 后回来接着撞，余额没了不会自己回来）。
func classifyZCode(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized ||
		hasBusinessCode(body, "1001") ||
		strings.Contains(lower, "令牌已过期") ||
		strings.Contains(lower, "未收到 authorization") ||
		strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "authentication") {
		return ErrSessionDead
	}
	if hasBusinessCode(body, "1113") ||
		strings.Contains(body, "余额不足") ||
		strings.Contains(lower, "insufficient balance") ||
		strings.Contains(lower, "insufficient quota") {
		return ErrHardCredit
	}
	return Classify(status, body)
}

// zcodeForceStream 把出站体的 stream 字段强制为 true，**其余字段一律不动**。
//
// 为什么需要这一处最小改写：网关对客户端只有一种上游通道——流式（非流式客户端
// 由 handler 侧 Aggregate 聚合 SSE）。而 zcode 适配器秉持「body 原样透传」，客户端
// 写 stream:false 时智谱回的是非流式 JSON，Aggregate 解析不出 data 帧 → 502
// upstream_parse（本机实测：stream:false 502、stream:true 200 且末帧自带 usage）。
//
// 边界刻意收窄到「只改 stream 一个键」：
//   - 已经是 true → 原字节返回（保持原样透传的最强形态，零重排）；
//   - body 不可解析 → 原样返回（让上游自然报错，不在这里二次错误化）；
//   - 其余字段（含 content/choices 之外的扩展、stream_options 是否注入）全部保持
//     客户端原样——不过 prepareBody，不做指纹中和/不注 prompt_cache_key/不做
//     effort 降级，智谱端点是标准 OpenAI 兼容接口，动多了只会引入被拒风险。
func zcodeForceStream(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if v, ok := obj["stream"].(bool); ok && v {
		return body
	}
	obj["stream"] = true
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// zcodeChatStreamContext zcode 的 chat 出站：单路径、原样 body、三件头。
// 结构对齐 ChatStreamContext（同一套错误信封与 idle 掐流），只是把路径/头/分类换成
// 智谱口径。返回签名与 ChatStreamContext 完全一致，handler 侧无需分支。
func (c *Client) zcodeChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// 最小改写：网关一律走上游流式通道（非流式客户端由 handler 侧 Aggregate 聚合），
	// 客户端写 stream:false 时智谱会回非流式 JSON，Aggregate 找不到 SSE 帧即 502
	// upstream_parse。只补这一个字段，其余字段（含 workbuddy 专有改写）一律不动。
	body = zcodeForceStream(body)
	url := c.zcodeBase(a) + zcodeChatPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	c.zcodeHeaders(req, a)
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] zcode chat acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			log.Printf("ERR: [upstream] zcode chat acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := classifyZCode(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] zcode chat acct=%s: upstream %d %s body=%s",
			logfmt.Label(a.UID, a.Nickname), resp.StatusCode, kind, truncate(string(raw), 200))
		if kind == ErrNone {
			return nil, resp.StatusCode, raw, nil
		}
		ue := &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
		if d, ok := ParseRetryAfter(resp.Header); ok {
			ue.RetryAfter = d
		}
		return nil, resp.StatusCode, raw, ue
	}
	return monitorBody(resp.Body, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

// FetchZCodeModels 拉智谱上游的模型清单（实时权威）。上游没号/拉失败时返回错误，
// 由调用方决定是透传还是回落账号自带清单。
func (c *Client) FetchZCodeModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.zcodeBase(a)+zcodeModelsPath, nil)
	if err != nil {
		return nil, err
	}
	c.zcodeHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: classifyZCode(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	// 智谱 /models 形态：{"data":[{"id":"glm-4.6",...}]}（OpenAI 标准）。
	// 也兼容裸数组（个别镜像上游）。
	var list []zcodeModelEntry
	var env struct {
		Data []zcodeModelEntry `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		list = env.Data
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("zcode models: 无法解析响应: %w", err)
	}
	out := make([]ModelInfo, 0, len(list))
	for _, m := range list {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		mi := ModelInfo{ID: id, Name: firstNonEmptyStr(m.Name, id)}
		if m.ContextWindow > 0 {
			mi.ContextWindow = m.ContextWindow
		} else if m.MaxInputTokens > 0 {
			mi.ContextWindow = m.MaxInputTokens
		}
		if m.MaxOutputTokens > 0 {
			mi.MaxTokens = m.MaxOutputTokens
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("zcode models: 上游返回空清单")
	}
	return out, nil
}

// zcodeModelEntry 智谱 /models 单条（兼容 OpenAI 标准字段与智谱扩展字段）。
type zcodeModelEntry struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	ContextWindow   int64  `json:"context_window"`
	MaxInputTokens  int64  `json:"max_input_tokens"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	OwnedBy         string `json:"owned_by"`
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ---------- 探活 ----------

// ZCodeProbeStatus 一次探活的结论。
type ZCodeProbeStatus string

const (
	ZCodeProbeOK        ZCodeProbeStatus = "ok"        // 200：凭据可用
	ZCodeProbeNoBalance ZCodeProbeStatus = "no_balance" // 429/1113：凭据可用但没额度
	ZCodeProbeBadKey    ZCodeProbeStatus = "bad_key"   // 401：凭据过期/不完整
	ZCodeProbeUnreach   ZCodeProbeStatus = "unreachable"
	ZCodeProbeError     ZCodeProbeStatus = "error"
)

// ZCodeProbeResult 探活结果（给管理页分档显示）。
type ZCodeProbeResult struct {
	Status ZCodeProbeStatus `json:"status"`
	HTTP   int              `json:"http,omitempty"`
	Msg    string           `json:"msg,omitempty"`
	Models []string         `json:"models,omitempty"`
}

// ProbeZCodeKey 用 GET /models 探一把凭据（**不消耗 token**：模型清单是只读接口，
// 比打一次 chat 补全便宜且没有计费副作用）。base 传空时用内置缺省。
// client 传 nil 时用包内一次性 client；调用方可注入与网关同款的 transport。
func ProbeZCodeKey(ctx context.Context, client *http.Client, base, key string) ZCodeProbeResult {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultZCodeChatBase
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+zcodeModelsPath, nil)
	if err != nil {
		return ZCodeProbeResult{Status: ZCodeProbeError, Msg: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return ZCodeProbeResult{Status: ZCodeProbeUnreach, Msg: truncate(err.Error(), 160)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := string(raw)
	switch {
	case resp.StatusCode == http.StatusOK:
		var env struct {
			Data []zcodeModelEntry `json:"data"`
		}
		var ids []string
		if json.Unmarshal(raw, &env) == nil {
			for _, m := range env.Data {
				if m.ID != "" {
					ids = append(ids, m.ID)
				}
			}
		}
		return ZCodeProbeResult{Status: ZCodeProbeOK, HTTP: resp.StatusCode, Models: ids}
	case classifyZCode(resp.StatusCode, body) == ErrHardCredit:
		return ZCodeProbeResult{Status: ZCodeProbeNoBalance, HTTP: resp.StatusCode, Msg: truncate(body, 160)}
	case resp.StatusCode == http.StatusUnauthorized:
		return ZCodeProbeResult{Status: ZCodeProbeBadKey, HTTP: resp.StatusCode, Msg: truncate(body, 160)}
	default:
		return ZCodeProbeResult{Status: ZCodeProbeError, HTTP: resp.StatusCode, Msg: truncate(body, 160)}
	}
}

// ---------- 额度（只读） ----------

// ZCodeBalance 额度查询结果（zcode 计划链路的 billing/balance）。
type ZCodeBalance struct {
	Plans    []ZCodePlan    `json:"plans,omitempty"`
	Balances []ZCodeGrant   `json:"balances,omitempty"`
	Raw      string         `json:"raw,omitempty"` // 原样回显（结构变了也能看）
}

// zcodeFlexString 收下上游「有时是字符串、有时是数字（Unix 时间戳）」的字段：
// 原样存文本，展示层不用关心类型。实测 starts_at / ends_at 会直接给数字。
type zcodeFlexString string

func (f *zcodeFlexString) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "" || t == "null" {
		*f = ""
		return nil
	}
	if t[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*f = zcodeFlexString(v)
		return nil
	}
	*f = zcodeFlexString(t)
	return nil
}

// zcodeFlexInt 同上：数字、数字字符串都收。
type zcodeFlexInt int64

func (f *zcodeFlexInt) UnmarshalJSON(b []byte) error {
	t := strings.TrimSpace(string(b))
	if t == "" || t == "null" {
		*f = 0
		return nil
	}
	if t[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		v = strings.TrimSpace(v)
		if v == "" {
			*f = 0
			return nil
		}
		n, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return err
		}
		*f = zcodeFlexInt(int64(n))
		return nil
	}
	n, err := json.Number(t).Float64()
	if err != nil {
		return err
	}
	*f = zcodeFlexInt(int64(n))
	return nil
}

// ZCodePlanEntitlement 套餐里的一条权益。真正标出「能用哪些模型」的是
// capabilities（形如 `model:glm-5.3-flash`）——周末活动这种只送单个模型的号，
// 这里就只有一条。**别再按 grant/capability 那套单数拼写猜**：实测上游是
// entitlements（复数）+ grant_units，早期写错导致权益整段为空。
type ZCodePlanEntitlement struct {
	EntitlementID string       `json:"entitlement_id,omitempty"`
	ShowName      string       `json:"show_name,omitempty"`
	Meter         string       `json:"meter,omitempty"`
	UnitType      string       `json:"unit_type,omitempty"`
	Capabilities  []string     `json:"capabilities,omitempty"`
	GrantUnits    zcodeFlexInt `json:"grant_units,omitempty"`
	Period        string       `json:"period,omitempty"`
	Priority      int          `json:"priority,omitempty"`
}

// ZCodePlan 一条订阅/计划。
type ZCodePlan struct {
	Name         string                 `json:"name,omitempty"`
	ID           string                 `json:"id,omitempty"`
	PlanID       string                 `json:"plan_id,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Status       string                 `json:"status,omitempty"`
	Priority     int                    `json:"priority,omitempty"`
	StartsAt     zcodeFlexString        `json:"starts_at,omitempty"`
	EndsAt       zcodeFlexString        `json:"ends_at,omitempty"`
	Entitlements []ZCodePlanEntitlement `json:"entitlements,omitempty"`
}

// UnmarshalJSON 兼容历史上猜过的单数键 `entitlement`（真实上游是 `entitlements`）。
func (p *ZCodePlan) UnmarshalJSON(b []byte) error {
	type planAlias ZCodePlan
	var aux struct {
		*planAlias
		Legacy []ZCodePlanEntitlement `json:"entitlement"`
	}
	aux.planAlias = (*planAlias)(p)
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	if len(p.Entitlements) == 0 && len(aux.Legacy) > 0 {
		p.Entitlements = aux.Legacy
	}
	return nil
}

// ZCodeGrant 一条额度（赠送/套餐）明细。
type ZCodeGrant struct {
	ShowName       string        `json:"show_name,omitempty"`
	PlanID         string        `json:"plan_id,omitempty"`
	EntitlementID  string        `json:"entitlement_id,omitempty"`
	TotalUnits     zcodeFlexInt  `json:"total_units,omitempty"`
	UsedUnits      zcodeFlexInt  `json:"used_units,omitempty"`
	RemainingUnits zcodeFlexInt  `json:"remaining_units,omitempty"`
	AvailableUnits zcodeFlexInt  `json:"available_units,omitempty"`
	UnitType       string        `json:"unit_type,omitempty"`
	ExpiresAt      zcodeFlexInt  `json:"expires_at,omitempty"`
	Capabilities   []string      `json:"capabilities,omitempty"`
	Meter          string        `json:"meter,omitempty"`
	Priority       int           `json:"priority,omitempty"`
}

// EntitledModels 抽出「这个账号当前**真正能用**的模型」。
//
// 判据是 capabilities 里的 `model:<id>`：它由套餐/额度决定，而不是 /models 能列出什么。
// 实测差别很大——周末活动的号 /models 能列 11 个，但 capabilities 只有
// `model:glm-5.3-flash`，真正走额度覆盖的就这一个。balances 与 plans.entitlements
// 两处都可能有，取并集后排序去重。
func (b *ZCodeBalance) EntitledModels() []string {
	if b == nil {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(caps []string) {
		for _, c := range caps {
			c = strings.TrimSpace(c)
			if len(c) < 6 || !strings.EqualFold(c[:6], "model:") {
				continue
			}
			id := strings.ToLower(strings.TrimSpace(c[6:]))
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, g := range b.Balances {
		add(g.Capabilities)
	}
	for _, p := range b.Plans {
		for _, e := range p.Entitlements {
			add(e.Capabilities)
		}
	}
	sort.Strings(out)
	return out
}

// PlanSummary 给管理页用的一句话套餐描述（名字 · 说明 · 到期），没有计划时返回空串。
func (b *ZCodeBalance) PlanSummary() string {
	if b == nil || len(b.Plans) == 0 {
		return ""
	}
	p := b.Plans[0]
	// 优先用人话（description 常是「ZCode 周末活动」这种），没有再退回计划名。
	if d := strings.TrimSpace(p.Description); d != "" {
		return d
	}
	return strings.TrimSpace(p.Name)
}

// FetchZCodeBalance 读 zcode 计划链路的余额（只读 GET，不在 captcha 闸后）。
// jwt 是账号文件 credentials["zcodejwttoken"]，deviceMID 是顶层 virtual_device_mid。
// 两者任一为空 → 直接返回错误（不猜、不发半截请求）。
func (c *Client) FetchZCodeBalance(ctx context.Context, jwt, deviceMID string) (*ZCodeBalance, error) {
	jwt = strings.TrimSpace(jwt)
	if jwt == "" {
		return nil, fmt.Errorf("zcode balance: 缺 zcodejwttoken（账号快照未登录/凭据被加密）")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		zcodeBalanceURL+"?app_version="+zcodeAppVersion, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("User-Agent", "ZCode/"+zcodeAppVersion)
	req.Header.Set("X-ZCode-App-Version", zcodeAppVersion)
	req.Header.Set("X-Platform", "win32-x64")
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	if strings.TrimSpace(deviceMID) != "" {
		req.Header.Set("X-Device-Mid", strings.TrimSpace(deviceMID))
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("zcode balance: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	return parseZCodeBalance(raw)
}

// parseZCodeBalance 解析 billing/balance 的响应体。独立成函数便于回归测试：
// 上游字段类型不稳定（starts_at / ends_at 实测直接给数字，grant / expires_at
// 也有字符串形态），早期按 string 解会让整条解析失败、页面显示「读失败」。
func parseZCodeBalance(raw []byte) (*ZCodeBalance, error) {
	var env struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Plans    []ZCodePlan  `json:"plans"`
			Balances []ZCodeGrant `json:"balances"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("zcode balance: 解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("zcode balance: code=%d msg=%s", env.Code, env.Msg)
	}
	return &ZCodeBalance{Plans: env.Data.Plans, Balances: env.Data.Balances,
		Raw: truncate(string(raw), 4096)}, nil
}

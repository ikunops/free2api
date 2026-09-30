// opencode.go OpenCode（CLI/TUI）上游适配器：把 OpenCode Zen / OpenCode Go 账号自带的
// apiKey 接到 Zen 的 OpenAI 兼容端点上。
//
// 与 workbuddy / zcode 的分工：workbuddy 账号打 {chatBase}/v2/chat/completions（一整套
// 桌面端头族）；zcode 账号打 open.bigmodel.cn（一枚 Bearer 即可）；opencode 账号打
// https://opencode.ai/zen/v1/chat/completions——OpenAI 兼容端点，但**要一枚
// x-opencode-session 头**（值任意，见下），否则上游会把它当裸第三方客户端。
//
// 本适配器同样做薄：只换 base URL 与头组，请求体原样透传（唯一改写是 stream 强制
// true，与 zcode 同因：网关对客户端只有流式上游通道）。不做 fingerprint 中和、
// 不注 prompt_cache_key、不做 effort 降级——那些是 workbuddy 上游的特定需要。
//
// ---------- 免费层（zen 的 *-free / big-pickle）怎么做 ----------
//
// 2026-09-30 更正：旧版本这里写着「免费层直连必 403、只有 Bun/TLS 指纹能过」——
// **那是错的**。免费层闸门是**三个条件的与**，与 UA / TLS 指纹**都无关**（对照实验：
// 把 CLI 自己的 195KB 真实请求原样重放即 200，说明上游不看指纹；换成小请求逐字段
// 二分，才定位到下面三条）：
//
//	1. x-opencode-session 头必须是 **"ses_" 前缀 + 恰好 26 位小写十六进制** 的合法
//	   形态。缺头 / 长度不对 / 含非 [0-9a-f] 字符 / 大写字母 → 一律 403 FreeTierError。
//	   上游不校验 id 对应的"真实会话"——随机生成的合规 id（如 ses_0123456789abcdef0123456789）
//	   也 200。这是**本适配器最易踩的一闸**：opencodeSessionID 用 sha256 前 13 字节
//	   （= 26 个小写 hex）派生，恰好合规；若把长度改成 12/14 字节就会全池静默 403。
//	2. stream=true —— 免费层只走流式。同一条请求把 stream 改 false 立刻 403。
//	3. tools 里同时含名为 bash 与 read 的工具（opencode 本体内置的两个工具名）。
//	   只给 bash 或只给 read 仍 403。
//
// 实测（2026-09-30，本机，Go net/http 直连，Bearer public 即可、无需任何 key）：
//
//	ses=26hex stream=T tools=[bash,read]  model=longcat-2.5-preview-free → 200 SSE
//	ses=26hex stream=T tools=[bash,read]  model=mimo-v2.5-free            → 200 SSE
//	ses=26hex stream=T tools=[bash,read]  model=big-pickle                → 200
//	ses=26hex stream=T tools=[bash,read]  model=glm-5.3-flash（付费）    → 200
//	ses=24hex（长度不对）                 model=big-pickle                → 403
//	ses=26hex stream=F  tools=[bash,read] model=longcat-2.5-preview-free → 403
//	ses=26hex stream=T  tools=[bash]      model=longcat-2.5-preview-free → 403
//	ses=26hex stream=T  无 tools          model=longcat-2.5-preview-free → 403
//
// 本适配器三条都自动满足：session 由 opencodeSessionID 派生（26hex，见上），stream 由
// opencodeForceStream 强制 true（网关本来只走流式上游通道），工具由
// opencodeInjectFreeTools 补齐 bash+read（见下，实测**只需这两个**，edit/glob/grep 可选）。
// 所以免费层和付费层共用同一个直连通道，无需任何特殊处理。
//
// 例外：space-bunny-free 是**无条件**放行（session/stream/tools 全不管都 200），
// 无需闸门。它挂在 Zen 免费层目录里，走同一路径即可。
//
// 免费层无需账号（Bearer public），但网关按「账号/号池」建模，故免费层仍挂在已有
// opencode 账号上借它的通道出去（session 鉴权沿用该账号）；无账号时也可显式导入一个
// 仅用免费层的占位号。
package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
)

const (
	// defaultOpenCodeChatBase OpenCode Zen 的 OpenAI 兼容前缀（直连）。
	defaultOpenCodeChatBase = "https://opencode.ai/zen/v1"
	// opencodeChatPath OpenAI 兼容的对话补全路径（拼在 base 之后）。
	opencodeChatPath = "/chat/completions"
	// opencodeModelsPath OpenAI 兼容的模型清单路径（实测带 opencode UA 时 200）。
	opencodeModelsPath = "/models"

	// opencodeUserAgent 出站 UA。Cloudflare 会拦默认 Go UA（403 error 1010），
	// 带 opencode 形态的 UA 才能过。版本段对齐本机 opencode 实测值。
	opencodeUserAgent = "opencode/1.18.32 ai-sdk/provider-utils/4.0.23 runtime/bun/1.3.14"

	// opencodeFreeToolNames 免费层闸门要求出站 tools 里出现的工具名（opencode 本体内置
	// 五个工具，实测只认名字，描述/参数随便）。闸门必需的是 bash + read；另三个
	// （edit / glob / grep）参考实现 zhuweiyou/oc2api 与 jasonxu114514/opencode2api 都会
	// 补全，这里对齐以稳妥。注意工具还需配合 stream=true，见 opencodeForceStream 与
	// 文件头的两条闸门说明。
	opencodeFreeToolNames = "bash,edit,glob,grep,read"
	// opencodeFreeToolDescription 补的占位工具的描述（与 oc2api 同款文案）。
	opencodeFreeToolDescription = "Compatibility marker only. Do not call or select this function."
	// opencodePublicAuth 免费层不需要任何凭据，Authorization 用这个占位值即可（实测 200）。
	opencodePublicAuth = "public"
)

// opencodeFreeToolNameList 拆开 opencodeFreeToolNames，供逐名判缺。
var opencodeFreeToolNameList = strings.Split(opencodeFreeToolNames, ",")

// ProducerOpenCode OpenCode 生产者标识。与 internal/source.ProducerOpenCode 同值——
// 这里刻意不 import internal/source（理由同 zcode.go：出站层不反向依赖取源层）。
const ProducerOpenCode = "opencode"

// opencodeOn 报告账号是否走 OpenCode（Zen / Go）上游。判据是凭据自带的 producer 键
// （见 auth.Auth.Producer）：空/workbuddy/zcode 一律走原路径，零回归。
func (c *Client) opencodeOn(a *auth.Auth) bool {
	return a != nil && a.Producer() == ProducerOpenCode
}

// opencodeBase 生效的 OpenCode chat base：凭证自带 upstream_base > Client 覆盖 > 内置缺省。
// 凭证自带那档是「用户自己选 url」的落点（导入时写进 auth 文件的 upstream_base 键）；
// Go 端点的凭证导入时写的就是 https://opencode.ai/zen/go/v1，天然走这里。
func (c *Client) opencodeBase(a *auth.Auth) string {
	if a != nil {
		if b := strings.TrimSpace(a.UpstreamBase()); b != "" {
			return strings.TrimRight(b, "/")
		}
	}
	if b := strings.TrimSpace(c.ChatBaseOpenCode); b != "" {
		return strings.TrimRight(b, "/")
	}
	return defaultOpenCodeChatBase
}

// opencodeSessionID 派生一枚**稳定**的 x-opencode-session 值。
//
// 上游对免费层的判定**严格依赖这个头的字面形态**：必须 "ses_" 前缀 + 恰好 26 位
// 小写十六进制（见文件头闸门 1）。值本身不校验——随机合规 id 一样放行。
//
// 所以 sha256 取前 13 字节不是随手选的：13 字节 == 26 个小写 hex 字符，恰好合规。
// **这个长度是承重的**，改成 12/14 字节（24/28 hex）会让免费层全池 403 FreeTierError，
// 而且付费模型照常 200、只有免费层静默全灭，极难排查。改长度前先做上游实测。
// 其余两个好处：同账号每次请求同一个值（上游侧归因稳定、便于排障），且**不泄露
// 凭据**（只透出哈希片段）。
func opencodeSessionID(a *auth.Auth) string {
	seed := ""
	if a != nil {
		seed = a.UID
	}
	if a != nil {
		if at := a.AccessTokenValue(); at != "" {
			seed = at
		}
	}
	sum := sha256.Sum256([]byte(seed))
	return "ses_" + hex.EncodeToString(sum[:13])
}

// opencodeHeaders OpenCode Zen 端点要的头：鉴权 + 内容类型 + opencode 客户端指纹。
// x-opencode-session 是**必需**的（缺它上游按裸第三方客户端处理，付费模型也可能被
// 归到免费层闸后）。x-opencode-client/project 是随包观测到的官方形态，一并带上。
func (c *Client) opencodeHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", opencodeUserAgent)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-session", opencodeSessionID(a))
	if at := a.AccessTokenValue(); at != "" {
		req.Header.Set("Authorization", "Bearer "+at)
	}
}

// classifyOpenCode 在通用 Classify 之上补四条 OpenCode 特有的口径。
//
// 关键区分：**401 不等于账号坏了**。2026-10-01 实测（同一枚有效 key）：
//
//	model=glm-5.3-flash（该 key 能跑）        → 200
//	model=gpt-5.4-mini / gpt-6-astra          → 401 {"type":"AuthError","message":"Incorrect API key provided: zen"}
//	model=claude-sonnet-4-5（协议不支持）      → 400 ModelProtocolUnsupported
//	整枚无效 key 打任何模型                     → 401 {"type":"AuthError","message":"Invalid API key."}
//
// 也就是说 Zen 对「这个号没有该模型的上游凭证」也回 401 AuthError，文案里带的是
// **模型供应商**（"Incorrect API key provided: zen" 指 zen 自己那条上游），不是
// 「你的 opencode key 无效」。把这种 401 当成 session dead 会让号池在轮转撞上几个
// 这类模型后把**好号**禁用掉（本机实测：opencode 号被连续 3 次 12153 打进终态，
// 之后连它本来能跑的免费层都拉不到目录了）——所以按「模型级」处理。
//
// 判定口径：
//   - 401 且文案是「你的 key 无效」（Invalid API key / Invalid credential）→ ErrSessionDead
//     （真的坏凭据，走连续 N 次 → 禁用）；
//   - 401 但文案指向模型侧（Incorrect API key provided: <provider>、model/upstream 相关）
//     → ErrModelBlocked（换号无意义，标记池级死模型）；
//   - 免费层门禁（403 FreeTierError）→ ErrModelBlocked；
//   - 模型不可用 / 协议不支持（400 Model is unavailable / ModelProtocolUnsupported）
//     → ErrModelBlocked。
func classifyOpenCode(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized {
		// 「Incorrect API key provided: <供应商>」是上游替**模型供应商**报的账（本机实测：
		// 同一枚有效 key 打 glm-5.3-flash 是 200、打 gpt-5.4-mini 就是这个 401），
		// 属于模型级。只有明确说「本 key 无效」的才是账号级。
		// 注意别用 "model"/"provider" 这种宽泛词做判据：真正的坏凭据文案里也可能带它们。
		if strings.Contains(lower, "incorrect api key provided") {
			return ErrModelBlocked
		}
		return ErrSessionDead
	}
	if strings.Contains(lower, "invalid credential") ||
		strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "authentication") {
		return ErrSessionDead
	}
	if strings.Contains(lower, "freetiererror") ||
		strings.Contains(lower, "free tier can only be used from within opencode") ||
		strings.Contains(lower, "model is unavailable") ||
		strings.Contains(lower, "modelprotocolunsupported") {
		return ErrModelBlocked
	}
	return Classify(status, body)
}

// opencodeInjectFreeTools 在出站体里补齐免费层闸门要求的工具名（bash + read）。
//
// 免费层闸门（2026-09-30 实测，见文件头）只认 tools 数组里是否**同时存在**名为
// "bash" 与 "read" 的 function 工具：缺哪个补哪个，客户端已带的同名工具原样保留。
// 上游不校验描述/参数，占位工具即可。付费模型不设闸，但补这两个占位工具对它们同样
// 无害（实测付费模型带 tools 仍 200），所以本函数对全部 opencode 出站一律生效，
// 不区分模型——免费/付费共用一条通道，路由层无需做模型分级。
//
// 与 oc2api 的差异：oc2api 无条件追加全部 5 个占位工具；这里只补闸门真正要求的
// bash/read，且仅在客户端**没带同名工具**时补——客户端自带的工具一律不动。另外
// 照 oc2api：客户端**完全没带 tools** 时，把 tool_choice 置 none，免得模型选中
// 这两个补进来的占位工具。客户端带了 tools 时 tool_choice 原样透传。
func opencodeInjectFreeTools(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	var tools []any
	if v, ok := obj["tools"].([]any); ok {
		tools = v
	}
	hadUserTools := len(tools) > 0
	present := make(map[string]bool, len(tools))
	for _, t := range tools {
		tm, ok := t.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tm["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok {
			present[name] = true
		}
	}
	changed := false
	for _, name := range opencodeFreeToolNameList {
		if name == "" || present[name] {
			continue
		}
		tools = append(tools, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": opencodeFreeToolDescription,
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		})
		present[name] = true
		changed = true
	}
	if !changed {
		return body
	}
	obj["tools"] = tools
	if !hadUserTools {
		obj["tool_choice"] = "none"
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// opencodeForceStream 把出站体的 stream 字段强制为 true，其余字段一律不动
// （与 zcodeForceStream 同因同形：网关只走流式上游通道，客户端写 stream:false 时
// 上游回非流式 JSON，Aggregate 找不到 SSE 帧即 502 upstream_parse）。
func opencodeForceStream(body []byte) []byte {
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

// opencodeChatStreamContext OpenCode 的 chat 出站：单路径、原样 body、opencode 头组。
// 结构对齐 ChatStreamContext（同一套错误信封与 idle 掐流），只是把路径/头/分类换成
// OpenCode 口径。返回签名与 ChatStreamContext 完全一致，handler 侧无需分支。
func (c *Client) opencodeChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body = opencodeForceStream(body)
	body = opencodeInjectFreeTools(body)
	url := c.opencodeBase(a) + opencodeChatPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	c.opencodeHeaders(req, a)
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] opencode chat acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			log.Printf("ERR: [upstream] opencode chat acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := classifyOpenCode(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] opencode chat acct=%s: upstream %d %s body=%s",
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

// FetchOpenCodeModels 拉 OpenCode Zen 上游的模型清单（实时权威）。
// GET /zen/v1/models 实测**必须带 opencode UA**（默认 Go UA 被 Cloudflare 403 error 1010）。
// 免费层与付费层一并列出：免费层现在可通过出站「stream=true + 补 bash/read 占位工具」
// 直连（见文件头
// 2026-09-30 更正与 opencodeInjectFreeTools）。上游没号/拉失败时返回错误，由调用方
// 决定是否回落。
func (c *Client) FetchOpenCodeModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.opencodeBase(a)+opencodeModelsPath, nil)
	if err != nil {
		return nil, err
	}
	c.opencodeHeaders(req, a)
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
		return nil, &Error{Kind: classifyOpenCode(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	// Zen /models 形态：{"object":"list","data":[{"id":"...","object":"model","owned_by":"opencode"}]}。
	// 也兼容裸数组（个别镜像上游）。
	type ocEntry struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		OwnedBy string `json:"owned_by"`
	}
	var env struct {
		Data []ocEntry `json:"data"`
	}
	var list []ocEntry
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		list = env.Data
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("opencode models: 无法解析响应: %w", err)
	}
	out := make([]ModelInfo, 0, len(list))
	for _, m := range list {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		out = append(out, ModelInfo{ID: id, Name: firstNonEmptyStr(m.Name, id), Free: isOpenCodeFreeTierModel(id)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("opencode models: 上游返回空清单")
	}
	return out, nil
}

// isOpenCodeFreeTierModel 判定模型 id 是否属于 OpenCode 免费层。
//
// Zen 目录的免费层命名有规律：绝大多数带 "-free" 后缀；少数例外按名硬编码
// （big-pickle 是本机实测确认的免费层）。免费层现在**可直连**（出站满足
// stream=true + tools 含 bash/read 两条闸门即可，见文件头 2026-09-30 更正），
// 所以此函数不再是「过滤」用途，只作分类标签（例如管理页可据此标「免费」）。
func isOpenCodeFreeTierModel(id string) bool {
	l := strings.ToLower(strings.TrimSpace(id))
	if l == "" {
		return false
	}
	if strings.HasSuffix(l, "-free") {
		return true
	}
	switch l {
	case "big-pickle", "x-preview-f-free":
		return true
	}
	return false
}

// OpenCodeProbeStatus 一次探活的结论。
type OpenCodeProbeStatus string

const (
	OpenCodeProbeOK      OpenCodeProbeStatus = "ok"      // 200：凭据可用
	OpenCodeProbeBadKey  OpenCodeProbeStatus = "bad_key" // 401：凭据过期/不完整
	OpenCodeProbeUnreach OpenCodeProbeStatus = "unreachable"
	OpenCodeProbeError   OpenCodeProbeStatus = "error"
)

// OpenCodeProbeResult 探活结果（给管理页分档显示）。
type OpenCodeProbeResult struct {
	Status OpenCodeProbeStatus `json:"status"`
	HTTP   int                 `json:"http,omitempty"`
	Msg    string              `json:"msg,omitempty"`
	Models []string            `json:"models,omitempty"`
}

// ProbeOpenCodeKey 用 GET /models 探一把凭据（**不消耗 token**：模型清单是只读接口）。
// base 传空时用内置缺省；client 传 nil 时用包内一次性 client。
func ProbeOpenCodeKey(ctx context.Context, client *http.Client, base, key string) OpenCodeProbeResult {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultOpenCodeChatBase
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+opencodeModelsPath, nil)
	if err != nil {
		return OpenCodeProbeResult{Status: OpenCodeProbeError, Msg: err.Error()}
	}
	req.Header.Set("User-Agent", opencodeUserAgent)
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-project", "global")
	req.Header.Set("x-opencode-session", "ses_probe")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return OpenCodeProbeResult{Status: OpenCodeProbeUnreach, Msg: truncate(err.Error(), 160)}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	body := string(raw)
	switch {
	case resp.StatusCode == http.StatusOK:
		var env struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		var ids []string
		if json.Unmarshal(raw, &env) == nil {
			for _, m := range env.Data {
				if id := strings.TrimSpace(m.ID); id != "" {
					ids = append(ids, id)
				}
			}
		}
		return OpenCodeProbeResult{Status: OpenCodeProbeOK, HTTP: resp.StatusCode, Models: ids}
	case resp.StatusCode == http.StatusUnauthorized || classifyOpenCode(resp.StatusCode, body) == ErrSessionDead:
		return OpenCodeProbeResult{Status: OpenCodeProbeBadKey, HTTP: resp.StatusCode, Msg: truncate(body, 160)}
	default:
		return OpenCodeProbeResult{Status: OpenCodeProbeError, HTTP: resp.StatusCode, Msg: truncate(body, 160)}
	}
}

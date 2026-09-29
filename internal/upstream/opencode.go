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
// ---------- 免费层（zen 的 *-free / big-pickle）为什么不做 ----------
//
// 实测（2026-09，本机 zen key）：
//
//	POST zen/v1/chat/completions  model=glm-5.3-flash（付费）  → 200 真回复
//	POST zen/v1/chat/completions  model=big-pickle（免费层）    → 403 FreeTierError
//	POST zen/v1/chat/completions  model=x-preview-f-free        → 403 FreeTierError
//
// 403 文案是「OpenCode's free tier can only be used from within OpenCode」。门禁
// 不是看请求头/请求体（把 opencode 本体发出的请求逐字节重放，仍然 403），而是
// **传输层指纹**：只有 opencode 本体（Bun runtime / 其 TLS·HTTP2 指纹）发起的连接
// 才被放行。Go / curl / Python 的 HTTP 客户端一律过不去。
//
// 唯一能过闸的路径是经本机 `opencode serve` 中转（它内部用 Bun 出站）。但那条路
// 有**硬伤**：serve 的 /session/{id}/message 会真的在宿主机执行内置 agent 的工具
// （实测：bash 能读到真实 hostname、能写文件）。把这条路径接进网关，等于把「远程
// 命令执行」暴露给任何能访问网关的客户端——本适配器**刻意不做**。
//
// 所以：本适配器只做 zen **付费模型**直连（安全、实测 200）。免费层模型在目录里
// 被过滤掉（见 isOpenCodeFreeTierModel），且真被调用时 403 FreeTierError 会被
// 分类成 ErrModelBlocked（池级标记为死模型，下次不再列出/不再路由）。
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
)

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
// 上游只要这个头存在且形态像会话 id（"ses" 前缀）就放行，值本身不校验（实测
// ses_probe 也过）。这里用凭据的 sha256 前缀派生，好处有二：同账号每次请求同一个
// 值（上游侧归因稳定、便于排障），且**不泄露凭据**（只透出哈希片段）。
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
	return "ses_" + hex.EncodeToString(sum[:8])
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

// classifyOpenCode 在通用 Classify 之上补三条 OpenCode 特有的口径：
//
//   - 鉴权失败（401 / Invalid credential / invalid api key）→ ErrSessionDead。
//     通用 Classify 会把 401 兜底成 ErrClient（「只换号不罚」），而 OpenCode 的 401
//     是确定性坏凭据，必须走「连续 N 次 → 禁用」的终态路径。
//   - 免费层门禁（403 FreeTierError / "free tier can only be used from within OpenCode"）
//     → ErrModelBlocked：这是**模型级**限制（任何账号、任何 IP 直连都一样），标记为
//     池级死模型后从目录与路由剔除，避免反复白打（见文件头「免费层为什么不做」）。
//   - 模型不可用（400 "Model is unavailable" / ModelProtocolUnsupported）→ ErrModelBlocked
//     （同上：换号无意义）。
func classifyOpenCode(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized ||
		strings.Contains(lower, "invalid credential") ||
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
// 免费层模型在这里被过滤掉（见 isOpenCodeFreeTierModel）：它们直连必 403，列出去
// 等于承诺「能调」。上游没号/拉失败时返回错误，由调用方决定是否回落。
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
		if id == "" || isOpenCodeFreeTierModel(id) {
			continue
		}
		out = append(out, ModelInfo{ID: id, Name: firstNonEmptyStr(m.Name, id)})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("opencode models: 上游返回空清单（或全部是免费层模型）")
	}
	return out, nil
}

// isOpenCodeFreeTierModel 判定模型 id 是否属于 OpenCode 免费层。
//
// Zen 目录的免费层命名有规律：绝大多数带 "-free" 后缀；少数例外按名硬编码
// （big-pickle 是本机实测确认的免费层）。这些模型直连必 403 FreeTierError
// （见文件头），所以从目录里剔除。漏网之鱼由 classifyOpenCode 的 ErrModelBlocked
// 兜底（真被调用时标记为死模型，下次不再出现）。
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
				if id := strings.TrimSpace(m.ID); id != "" && !isOpenCodeFreeTierModel(id) {
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

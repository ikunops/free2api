// kilo.go Kilo Code（api.kilo.ai）上游适配器：把 Kilo 的**匿名免费通道**接成
// 标准 OpenAI 兼容端点。
//
// 定位：Kilo 与 workbuddy / zcode / opencode 不同——它没有"账号"。Kilo 的
// OpenRouter 兼容网关对匿名调用直接放行（上一枚 `Bearer anonymous` 或干脆不带
// Authorization，实测都 200），免费模型走 `:free` 后缀 + models 目录里
// pricing.prompt==0 && pricing.completion==0 判定。
//
// 实测依据（2026-09-30，本机 Go net/http 直连）：
//
//	GET  https://api.kilo.ai/api/gateway/models        → 200（397 个模型，免鉴权）
//	POST https://api.kilo.ai/api/openrouter/chat/completions  Bearer anonymous  → 200
//	POST 同上  不带 Authorization                      → 200（匿名同样放行）
//	POST 同 endpoint  stream:true                      → 200 SSE 正常
//	GET  /api/openrouter/v1/models                     → 405（注意：没有 /v1/models 这一层）
//
// 因此本适配器做薄：只换 base URL 与头组，请求体原样透传（唯一改写是 stream
// 强制 true，与 zcode / opencode 同因：网关统一走上游流式通道）。
//
// 免费模型清单不在代码里硬编码：FetchKiloModels 实时拉 /gateway/models，
// 按 pricing 为 0 过滤（也认 isFree 旗标与 `:free` 后缀）。上游目录变了，
// 号池里的模型名单跟着变，不用改代码。
package upstream

import (
	"bytes"
	"context"
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
	// defaultKiloChatBase Kilo 的 OpenAI 兼容对话前缀（直连）。
	defaultKiloChatBase = "https://api.kilo.ai/api/openrouter"
	// kiloChatPath OpenAI 兼容的对话补全路径（拼在 base 之后）。
	kiloChatPath = "/chat/completions"
	// defaultKiloModelsBase Kilo 的模型目录前缀。**注意与 chat base 不同**：
	// chat 在 /api/openrouter 下，目录在 /api/gateway 下，/api/openrouter/v1/models
	// 实测 405。两个常量分开写，免得日后有人"顺手统一"又踩回去。
	defaultKiloModelsBase = "https://api.kilo.ai/api/gateway"
	// kiloModelsPath 目录路径（拼在 models base 之后）。
	kiloModelsPath = "/models"
	// kiloUserAgent 出站 UA。实测带不带都能 200；固定一枚便于上游统计时认出网关流量。
	kiloUserAgent = "kilo/1.0"
	// kiloAnonymousKey Kilo 匿名通道的占位凭据。Kilo 网关对 `Bearer anonymous`
	// 与"不带 Authorization"一视同仁，这里显式带一枚是为了让日志里能看出走的是匿名道。
	kiloAnonymousKey = "anonymous"
)

// ProducerKilo Kilo 生产者标识。与 internal/source.ProducerKilo 同值——这里刻意不
// import internal/source（理由同 zcode.go / opencode.go：出站层不反向依赖取源层）。
const ProducerKilo = "kilo"

// kiloOn 报告账号是否走 Kilo 上游。判据是凭据自带的 producer 键
// （见 auth.Auth.Producer）：空/workbuddy/zcode/opencode 一律走原路径，零回归。
func (c *Client) kiloOn(a *auth.Auth) bool {
	return a != nil && a.Producer() == ProducerKilo
}

// kiloBase 生效的 Kilo chat base：凭证自带 upstream_base > Client 覆盖 > 内置缺省。
// 凭证自带那档是"用户自己选 url"的落点（导入时写进 auth 文件的 upstream_base 键）。
func (c *Client) kiloBase(a *auth.Auth) string {
	if a != nil {
		if b := strings.TrimSpace(a.UpstreamBase()); b != "" {
			return strings.TrimRight(b, "/")
		}
	}
	if b := strings.TrimSpace(c.ChatBaseKilo); b != "" {
		return strings.TrimRight(b, "/")
	}
	return defaultKiloChatBase
}

// kiloModelsURL 模型目录地址：既然 base 可被用户覆盖，目录 base 也跟着用同一个
// upstream_base（用户自建镜像时目录也走镜像）。缺省用 /api/gateway（见常量注释）。
func (c *Client) kiloModelsURL(a *auth.Auth) string {
	if a != nil {
		if b := strings.TrimSpace(a.UpstreamBase()); b != "" {
			return strings.TrimRight(b, "/") + kiloModelsPath
		}
	}
	if b := strings.TrimSpace(c.ChatBaseKilo); b != "" {
		return strings.TrimRight(b, "/") + kiloModelsPath
	}
	return defaultKiloModelsBase + kiloModelsPath
}

// kiloHeaders Kilo 端点要的头。鉴权用凭据自带的 key；空则用匿名占位值
// （Kilo 对匿名是放行的，这样"没导入任何 key 也能跑"）。
func (c *Client) kiloHeaders(req *http.Request, a *auth.Auth) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", kiloUserAgent)
	key := ""
	if a != nil {
		key = strings.TrimSpace(a.AccessTokenValue())
	}
	if key == "" {
		key = kiloAnonymousKey
	}
	req.Header.Set("Authorization", "Bearer "+key)
}

// classifyKilo 在通用 Classify 之上补一条 Kilo 特有的口径：
//
//   - 鉴权失败（401/403）→ ErrClient，**不罚号**。Kilo 是**全局单条匿名通道**，
//     池里通常只有一条 kilo 号；401/403 只可能是上游侧策略变动或临时拒答，把这条
//     号禁用掉 = 整家 Kilo 能力消失。故只轮转、不冷却不熔断，等下个请求再试。
//     通用 Classify 会把 401 兜底成 ErrClient（同结论）、403 兜成 ErrClient（一致）。
//   - 其余（429 软限流 / 5xx / 余额不足）沿用通用口径。
func classifyKilo(status int, body string) ErrKind {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return ErrClient
	}
	return Classify(status, body)
}

// kiloForceStream 把出站体的 stream 字段强制为 true，其余字段一律不动。
// 与 zcodeForceStream / opencodeForceStream 同因同形：网关只走流式上游通道，
// 客户端写 stream:false 时上游回非流式 JSON，Aggregate 找不到 SSE 帧即 502 upstream_parse。
func kiloForceStream(body []byte) []byte {
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

// kiloChatStreamContext Kilo 的 chat 出站：单路径、原样 body、kilo 头组。
// 结构对齐 ChatStreamContext（同一套错误信封与 idle 掐流），只是把路径/头/分类换成
// Kilo 口径。返回签名与 ChatStreamContext 完全一致，handler 侧无需分支。
func (c *Client) kiloChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	body = kiloForceStream(body)
	url := c.kiloBase(a) + kiloChatPath
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	c.kiloHeaders(req, a)
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] kilo chat acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			log.Printf("ERR: [upstream] kilo chat acct=%s: read body: %v", logfmt.Label(a.UID, a.Nickname), rerr)
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := classifyKilo(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] kilo chat acct=%s: upstream %d %s body=%s",
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

// kiloModelEntry Kilo /gateway/models 目录里的单条模型（OpenRouter 形态）。
// pricing 是**字符串**（"0" / "0.0000012"），不是数字——按 string 收再解析，
// 避免上游把 "0" 写成字符串时整条解析失败。
type kiloModelEntry struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int64  `json:"context_length"`
	IsFree        bool   `json:"isFree"`
	Pricing       struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	TopProvider struct {
		ContextLength int64 `json:"context_length"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities  []string `json:"input_modalities"`
		OutputModalities []string `json:"output_modalities"`
	} `json:"architecture"`
}

// kiloModelIsFree 判定一条目录模型是否属于免费层。三条判据取或：
// 显式 isFree 旗标、pricing 双零、id 带 ":free" 后缀——上游三种标记都用过。
func kiloModelIsFree(m kiloModelEntry) bool {
	if m.IsFree {
		return true
	}
	p, c := strings.TrimSpace(m.Pricing.Prompt), strings.TrimSpace(m.Pricing.Completion)
	if p != "" && c != "" && isZeroPrice(p) && isZeroPrice(c) {
		return true
	}
	return strings.Contains(strings.ToLower(m.ID), ":free")
}

// isZeroPrice 价格串是否为零（"0" / "0.0" / "0.000000" / "$0" 都算）。
func isZeroPrice(v string) bool {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "$"))
	if v == "" {
		return false
	}
	for _, r := range v {
		if r != '0' && r != '.' {
			return false
		}
	}
	return true
}

// FetchKiloModels 拉 Kilo 上游的模型清单并**只保留免费层**（实时权威，1h 缓存由调用方
// 维护）。上游没网/拉失败时返回错误，由调用方决定是否回落。
func (c *Client) FetchKiloModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.kiloModelsURL(a), nil)
	if err != nil {
		return nil, err
	}
	c.kiloHeaders(req, a)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, &Error{Kind: classifyKilo(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	// 形态：{"data":[{...}]}（OpenRouter 兼容）。也兼容裸数组（个别镜像）。
	var env struct {
		Data []kiloModelEntry `json:"data"`
	}
	var list []kiloModelEntry
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 {
		list = env.Data
	} else if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("kilo models: 无法解析响应: %w", err)
	}
	out := make([]ModelInfo, 0, 32)
	for _, m := range list {
		id := strings.TrimSpace(m.ID)
		if id == "" || !kiloModelIsFree(m) {
			continue
		}
		mi := ModelInfo{ID: id, Name: firstNonEmptyStr(m.Name, id)}
		if m.ContextLength > 0 {
			mi.ContextWindow = m.ContextLength
		} else if m.TopProvider.ContextLength > 0 {
			mi.ContextWindow = m.TopProvider.ContextLength
		}
		for _, mod := range m.Architecture.InputModalities {
			if strings.EqualFold(strings.TrimSpace(mod), "image") {
				mi.SupportsImages = true
				break
			}
		}
		out = append(out, mi)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("kilo models: 上游没有免费层模型（或目录形态变了）")
	}
	return out, nil
}

// ProbeKiloLane 探一把 Kilo 匿名通道（**不消耗额度**：模型目录是只读接口）。
// 传 nil client 时用包内一次性 client。
func ProbeKiloLane(ctx context.Context, client *http.Client, base, key string) (int, []string, error) {
	url := defaultKiloModelsBase + kiloModelsPath
	if strings.TrimSpace(base) != "" {
		url = strings.TrimRight(strings.TrimSpace(base), "/") + kiloModelsPath
	}
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", kiloUserAgent)
	k := strings.TrimSpace(key)
	if k == "" {
		k = kiloAnonymousKey
	}
	req.Header.Set("Authorization", "Bearer "+k)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		Data []kiloModelEntry `json:"data"`
	}
	var ids []string
	if json.Unmarshal(raw, &env) == nil {
		for _, m := range env.Data {
			if id := strings.TrimSpace(m.ID); id != "" && kiloModelIsFree(m) {
				ids = append(ids, id)
			}
		}
	}
	if resp.StatusCode >= 400 {
		return resp.StatusCode, ids, fmt.Errorf("kilo models: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	return resp.StatusCode, ids, nil
}

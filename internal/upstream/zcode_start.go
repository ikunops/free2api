// zcode_start.go 把 ZCode 的 Start Plan 接成可用的 Anthropic Messages 上游：
// 入站仍是网关统一的 OpenAI Chat 形态，出站前按 ZCode 官方客户端协议 dress 成
// 三段 cached system + context prefix + metadata.user_id，回程再把 Anthropic SSE
// 翻回 OpenAI Chat SSE，供现有 Aggregate / StreamHint / Responses 出口复用。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/session"
)

//go:embed zcode_start_prompt.json
var zcodeStartPromptJSON []byte

var zcodeStartMessagesURL = "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"

const (
	zcodeStartModel = "GLM-5.3-Flash"
	zcodeStartUA    = "ZCode/3.16.3 ai-sdk/anthropic/3.0.81"
)

type zcodeStartPromptDoc struct {
	CLIPrefix         string   `json:"cliPrefix"`
	StableSections    []string `json:"stableSections"`
	BeforeEnvironment string   `json:"beforeEnvironment"`
	AfterEnvironment  string   `json:"afterEnvironment"`
	Environment       struct {
		Heading        string `json:"heading"`
		InvokedLine    string `json:"invokedLine"`
		CwdLabel       string `json:"cwdLabel"`
		GitLabel       string `json:"gitLabel"`
		GitNo          string `json:"gitNo"`
		PlatformLabel  string `json:"platformLabel"`
		OSVersionLabel string `json:"osVersionLabel"`
		PoweredByLine  string `json:"poweredByLine"`
	} `json:"environment"`
	Context struct {
		Intro              string `json:"intro"`
		Outro              string `json:"outro"`
		CurrentDateHeading string `json:"currentDateHeading"`
		CurrentDateLine    string `json:"currentDateLine"`
	} `json:"context"`
}

var zcodeStartPrompt = mustZCodeStartPrompt()

func mustZCodeStartPrompt() zcodeStartPromptDoc {
	var p zcodeStartPromptDoc
	if err := json.Unmarshal(zcodeStartPromptJSON, &p); err != nil {
		panic("zcode start prompt: " + err.Error())
	}
	return p
}

// zcodeStartOn 只接管已经实测有权益的 GLM-5.3-Flash。其他 ZCode 模型仍走
// open.bigmodel.cn 的静态 key 通道，避免把未验证模型整体切到 Start Plan。
func (c *Client) zcodeStartOn(a *auth.Auth, body []byte) bool {
	if !c.zcodeOn(a) || strings.TrimSpace(a.ZCodeJWT()) == "" {
		return false
	}
	var req struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Model), "glm-5.3-flash")
}

// zcodeStartDressBody OpenAI Chat -> ZCode Start Plan 的 Anthropic Messages。
// 保留客户端的 system 内容，但按 ZCode 客户端只发三块 system 的硬约束，把它并入
// 第三块 dynamic；首条 user 固定为 currentDate 的 <system-reminder>。
func zcodeStartDressBody(body []byte, a *auth.Auth, meta ChatMeta) ([]byte, string, error) {
	var in map[string]any
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, "", err
	}
	model, _ := in["model"].(string)
	model = canonicalZCodeStartModel(model)
	if model == "" {
		return nil, "", fmt.Errorf("zcode start: empty model")
	}
	msgs, _ := in["messages"].([]any)
	ownSystem := make([]string, 0, 2)
	outMsgs := make([]any, 0, len(msgs)+1)
	for _, raw := range msgs {
		m, _ := raw.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		switch role {
		case "system", "developer":
			if txt := openAIContentText(m["content"]); strings.TrimSpace(txt) != "" {
				ownSystem = append(ownSystem, txt)
			}
		default:
			am, err := openAIMessageToAnthropic(m)
			if err != nil {
				return nil, "", err
			}
			outMsgs = append(outMsgs, am)
		}
	}
	outMsgs = append([]any{zcodeStartContextPrefix()}, outMsgs...)
	last := lastNonSystemMessage(outMsgs)
	for _, raw := range outMsgs {
		m, _ := raw.(map[string]any)
		if m == nil || m["role"] == "system" {
			continue
		}
		stripCacheControl(m)
	}
	if last != nil {
		if s, ok := last["content"].(string); ok {
			last["content"] = []any{zcodeCachedText(s)}
		} else if parts, ok := last["content"].([]any); ok && len(parts) > 0 {
			if p, _ := parts[len(parts)-1].(map[string]any); p != nil {
				p["cache_control"] = map[string]any{"type": "ephemeral"}
			}
		}
	}
	device := strings.TrimSpace(a.ZCodeDeviceMID())
	sid := zcodeStartSessionID(a, meta)
	dyn := []string{zcodeStartPrompt.BeforeEnvironment, zcodeStartEnvironment(model), zcodeStartPrompt.AfterEnvironment}
	dyn = append(dyn, ownSystem...)
	out := map[string]any{
		"model":    model,
		"stream":   true,
		"messages": outMsgs,
		"system": []any{
			zcodeCachedText(zcodeStartPrompt.CLIPrefix),
			zcodeCachedText(strings.Join(zcodeStartPrompt.StableSections, "\n\n")),
			zcodeCachedText("\n\n" + strings.Join(dyn, "\n\n")),
		},
		"metadata": map[string]any{"user_id": zcodeStartUserID(device, sid)},
	}
	if v, ok := in["max_completion_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	} else if v, ok := in["max_tokens"]; ok && v != nil {
		out["max_tokens"] = v
	} else {
		out["max_tokens"] = 8192
	}
	for _, k := range []string{"temperature", "top_p", "top_k"} {
		if v, ok := in[k]; ok && v != nil {
			out[k] = v
		}
	}
	if v, ok := in["stop"]; ok && v != nil {
		out["stop_sequences"] = v
	}
	toolChoice := any(nil)
	if tc, ok := in["tool_choice"]; ok && tc != nil {
		toolChoice = openAIToolChoiceToAnthropic(tc)
	}
	toolNone := false
	if s, ok := in["tool_choice"].(string); ok && strings.EqualFold(strings.TrimSpace(s), "none") {
		toolNone = true
	}
	if !toolNone {
		if tools, ok := in["tools"].([]any); ok && len(tools) > 0 {
			out["tools"] = openAIToolsToAnthropic(tools)
		}
		if toolChoice != nil {
			out["tool_choice"] = toolChoice
		}
	}
	raw, err := json.Marshal(out)
	return raw, sid, err
}

func canonicalZCodeStartModel(s string) string {
	if strings.EqualFold(strings.TrimSpace(s), zcodeStartModel) {
		return zcodeStartModel
	}
	return strings.TrimSpace(s)
}

func zcodeCachedText(s string) map[string]any {
	return map[string]any{"type": "text", "text": s, "cache_control": map[string]any{"type": "ephemeral"}}
}

func zcodeStartContextPrefix() map[string]any {
	p := zcodeStartPrompt.Context
	date := time.Now().Format("2006-01-02")
	body := strings.Join([]string{
		p.Intro,
		p.CurrentDateHeading + "\n" + strings.ReplaceAll(p.CurrentDateLine, "{date}", date),
		"",
		p.Outro,
	}, "\n")
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>" + body + "</system-reminder>"}}}
}

func zcodeStartEnvironment(model string) string {
	e := zcodeStartPrompt.Environment
	lines := []string{e.Heading, e.InvokedLine,
		fmt.Sprintf("- %s: %s", e.PlatformLabel, runtime.GOOS),
		fmt.Sprintf("- %s: %s %s", e.OSVersionLabel, runtime.GOOS, runtime.GOARCH),
	}
	if model != "" {
		lines = append(lines, strings.ReplaceAll(strings.ReplaceAll(e.PoweredByLine, "{provider}", "zai-api"), "{model}", model))
	}
	return strings.Join(lines, "\n")
}

func zcodeStartSessionID(a *auth.Auth, meta ChatMeta) string {
	seed := "zcode-session-device\x00" + strings.TrimSpace(a.ZCodeDeviceMID())
	if v := strings.TrimSpace(meta.ConversationID); v != "" {
		seed = "zcode-session\x00" + v
	} else if v := strings.TrimSpace(meta.ConversationRequestID); v != "" {
		seed = "zcode-session\x00" + v
	}
	h := sha256.Sum256([]byte(seed))
	var b [16]byte
	copy(b[:], h[:16])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	x := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", x[:8], x[8:12], x[12:16], x[16:20], x[20:32])
}

func zcodeStartUserID(device, sid string) string {
	b, _ := json.Marshal(map[string]string{"device_id": device, "account_uuid": "", "session_id": sid})
	return string(b)
}

func lastNonSystemMessage(msgs []any) map[string]any {
	for i := len(msgs) - 1; i >= 0; i-- {
		if m, _ := msgs[i].(map[string]any); m != nil && m["role"] != "system" {
			return m
		}
	}
	return nil
}

func stripCacheControl(m map[string]any) {
	switch c := m["content"].(type) {
	case string:
		m["content"] = c
	case []any:
		for _, raw := range c {
			if p, _ := raw.(map[string]any); p != nil {
				delete(p, "cache_control")
			}
		}
	}
}

func openAIContentText(v any) string {
	switch c := v.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, raw := range c {
			p, _ := raw.(map[string]any)
			if p == nil {
				continue
			}
			typ, _ := p["type"].(string)
			if typ == "" || typ == "text" || typ == "input_text" {
				if s, ok := p["text"].(string); ok {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

func openAIMessageToAnthropic(m map[string]any) (map[string]any, error) {
	role, _ := m["role"].(string)
	switch role {
	case "assistant":
		out := map[string]any{"role": "assistant", "content": openAIAssistantBlocks(m)}
		return out, nil
	case "tool":
		id, _ := m["tool_call_id"].(string)
		content := openAIContentText(m["content"])
		return map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": content}}}, nil
	default:
		return map[string]any{"role": "user", "content": openAIUserBlocks(m["content"])}, nil
	}
}

func openAIUserBlocks(v any) []any {
	switch c := v.(type) {
	case string:
		return []any{map[string]any{"type": "text", "text": c}}
	case []any:
		out := make([]any, 0, len(c))
		for _, raw := range c {
			p, _ := raw.(map[string]any)
			if p == nil {
				continue
			}
			typ, _ := p["type"].(string)
			switch typ {
			case "image_url":
				url := ""
				if iu, _ := p["image_url"].(map[string]any); iu != nil {
					url, _ = iu["url"].(string)
				}
				if strings.HasPrefix(url, "data:") {
					if comma := strings.IndexByte(url, ','); comma >= 0 {
						meta, data := url[:comma], url[comma+1:]
						media := strings.TrimPrefix(strings.TrimPrefix(meta, "data:"), "image/")
						if semi := strings.IndexByte(media, ';'); semi >= 0 {
							media = media[:semi]
						}
						out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/" + media, "data": data}})
					}
				} else if url != "" {
					out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}})
				}
			default:
				if s, ok := p["text"].(string); ok {
					out = append(out, map[string]any{"type": "text", "text": s})
				}
			}
		}
		return out
	default:
		return []any{map[string]any{"type": "text", "text": openAIContentText(v)}}
	}
}

func openAIAssistantBlocks(m map[string]any) []any {
	out := make([]any, 0, 2)
	if rc, _ := m["reasoning_content"].(string); rc != "" {
		out = append(out, map[string]any{"type": "thinking", "thinking": rc, "signature": ""})
	}
	if txt := openAIContentText(m["content"]); txt != "" {
		out = append(out, map[string]any{"type": "text", "text": txt})
	}
	if calls, ok := m["tool_calls"].([]any); ok {
		for _, raw := range calls {
			c, _ := raw.(map[string]any)
			if c == nil {
				continue
			}
			fn, _ := c["function"].(map[string]any)
			name, _ := fn["name"].(string)
			args := fn["arguments"]
			var input any = map[string]any{}
			switch a := args.(type) {
			case string:
				if strings.TrimSpace(a) != "" {
					_ = json.Unmarshal([]byte(a), &input)
				}
			case map[string]any:
				input = a
			}
			out = append(out, map[string]any{"type": "tool_use", "id": c["id"], "name": name, "input": input})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"type": "text", "text": ""})
	}
	return out
}

func openAIToolsToAnthropic(tools []any) []any {
	out := make([]any, 0, len(tools))
	for _, raw := range tools {
		t, _ := raw.(map[string]any)
		if t == nil {
			continue
		}
		fn, _ := t["function"].(map[string]any)
		if fn == nil {
			fn = t
		}
		name, _ := fn["name"].(string)
		desc, _ := fn["description"].(string)
		params := fn["parameters"]
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{"name": name, "description": desc, "input_schema": params})
	}
	return out
}

func openAIToolChoiceToAnthropic(v any) any {
	switch c := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "auto":
			return map[string]any{"type": "auto"}
		case "required", "any":
			return map[string]any{"type": "any"}
		case "none":
			return nil
		}
	case map[string]any:
		fn, _ := c["function"].(map[string]any)
		if name, _ := fn["name"].(string); name != "" {
			return map[string]any{"type": "tool", "name": name}
		}
	}
	return nil
}

// zcodeStartChatStreamContext 发 Start Plan 请求。返回体已经是 OpenAI Chat SSE，
// 下游无需知道上游是 Anthropic Messages。
func (c *Client) zcodeStartChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	jwt := strings.TrimSpace(a.ZCodeJWT())
	if jwt == "" {
		return nil, 0, nil, &Error{Kind: ErrSessionDead, Status: http.StatusUnauthorized, Msg: "zcode start: missing zcode_jwt"}
	}
	dressed, sid, err := zcodeStartDressBody(body, a, meta)
	if err != nil {
		return nil, 0, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, zcodeStartMessagesURL, bytes.NewReader(dressed))
	if err != nil {
		return nil, 0, nil, err
	}
	zcodeStartHeaders(req, jwt, sid)
	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] zcode start acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
		roundTripCloseIdle(c.chatHTTP().Transport)
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		if rerr != nil {
			return nil, 0, nil, fmt.Errorf("read body: %w", rerr)
		}
		kind := classifyZCodeStart(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] zcode start acct=%s: upstream %d %s body=%s",
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
	converted := newZCodeStartSSEConverter(resp.Body)
	// 关键：不能先把 200 响应头交给 handler。无 Start Plan 权益的 ZCode 号会返回
	// HTTP 200 + 0 个 SSE 帧；若等到 handler 已写响应头才发现，客户端只能看到
	// 200 Content-Length: 0，且无法换号。这里先预读首个有效事件：
	//   - 空流/超时 → 返回 502 分类错误，handler 可正常轮转；
	//   - 有事件 → 把已读字节放回流头，不丢首帧。
	prefix, preErr := preReadZCodeStart(converted, c.IdleTimeout)
	if preErr != nil {
		_ = resp.Body.Close()
		cancel()
		log.Printf("WARN: [upstream] zcode start acct=%s: %v", logfmt.Label(a.UID, a.Nickname), preErr)
		return nil, http.StatusBadGateway, nil, &Error{
			Kind: ErrServer, Status: http.StatusBadGateway,
			Msg: "zcode start: empty upstream stream (no Start Plan entitlement or upstream returned 0 frames)",
		}
	}
	return monitorBody(&prefixReadCloser{prefix: prefix, rc: converted}, c.IdleTimeout, cancel), resp.StatusCode, nil, nil
}

func zcodeStartHeaders(req *http.Request, jwt, sid string) {
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	req.Header.Set("User-Agent", zcodeStartUA)
	req.Header.Set("X-ZCode-App-Version", "3.16.3")
	req.Header.Set("X-Title", "Z Code@cli")
	req.Header.Set("X-Release-Channel", "production")
	req.Header.Set("X-Client-Language", "zh-CN")
	req.Header.Set("X-Client-Timezone", "Asia/Shanghai")
	req.Header.Set("X-ZCode-Agent", "glm")
	req.Header.Set("X-Platform", zcodeStartPlatform())
	req.Header.Set("X-Os-Category", zcodeStartOSCategory())
	req.Header.Set("X-Os-Version", "10.0.26100")
	req.Header.Set("x-request-id", session.NewUUIDv7())
	req.Header.Set("x-zcode-session-type", "main")
	req.Header.Set("x-zcode-trace-id", session.NewUUIDv7())
	req.Header.Set("x-zcode-query-source", "main_turn")
	req.Header.Set("x-session-id", sid)
	// Chat 请求不发 X-Device-Mid；余额查询才发（见 zcode.go 的 FetchZCodeBalance）。
}

func zcodeStartPlatform() string {
	if runtime.GOOS == "windows" {
		return "win32-" + runtime.GOARCH
	}
	return runtime.GOOS + "-" + runtime.GOARCH
}

func zcodeStartOSCategory() string {
	switch runtime.GOOS {
	case "windows":
		return "windows"
	case "darwin":
		return "macos"
	default:
		return "linux"
	}
}

func classifyZCodeStart(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if status == http.StatusUnauthorized || hasBusinessCode(body, "1001") ||
		strings.Contains(lower, "token expired") || strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "authentication") || strings.Contains(lower, "unauthorized") {
		return ErrSessionDead
	}
	if hasBusinessCode(body, "1113") || hasBusinessCode(body, "1005") ||
		strings.Contains(body, "余额不足") || strings.Contains(lower, "insufficient balance") ||
		strings.Contains(lower, "insufficient quota") {
		return ErrHardCredit
	}
	if status == http.StatusMethodNotAllowed || hasBusinessCode(body, "3012") ||
		strings.Contains(lower, "unusual activity") || strings.Contains(lower, "request has been blocked") {
		return ErrAccountFault
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	return classifyZCode(status, body)
}

// preReadZCodeStart 预读转换后的首个有效 SSE 块。必须在 handler 写响应头之前调用，
// 否则空流会被伪装成 200。IdleTimeout<=0 时给一个保守的 30s 上限，避免永久挂住。
func preReadZCodeStart(rc io.ReadCloser, idle time.Duration) ([]byte, error) {
	wait := idle
	if wait <= 0 {
		wait = 30 * time.Second
	}
	type result struct {
		b   []byte
		err error
	}
	ch := make(chan result, 1)
	go func() {
		buf := make([]byte, 32*1024)
		n, err := rc.Read(buf)
		if n > 0 {
			ch <- result{b: append([]byte(nil), buf[:n]...)}
			return
		}
		ch <- result{err: err}
	}()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case r := <-ch:
		if len(r.b) > 0 {
			return r.b, nil
		}
		if r.err == nil {
			r.err = errEmptyStream
		}
		return nil, r.err
	case <-t.C:
		return nil, fmt.Errorf("zcode start: no first SSE event within %s", wait)
	}
}

// prefixReadCloser 把预读字节放回读流头，后续 Read 先吐 prefix 再读底流。
type prefixReadCloser struct {
	prefix []byte
	rc     io.ReadCloser
}

func (p *prefixReadCloser) Read(b []byte) (int, error) {
	if len(p.prefix) > 0 {
		n := copy(b, p.prefix)
		p.prefix = p.prefix[n:]
		return n, nil
	}
	return p.rc.Read(b)
}

func (p *prefixReadCloser) Close() error { return p.rc.Close() }

type zcodeStartSSEConverter struct {
	src       io.ReadCloser
	br        *bufio.Reader
	pending   []byte
	dataLines []string
	id        string
	model     string
	created   int64
	started   bool
	done      bool
	valid     int
	err       error
}

func newZCodeStartSSEConverter(src io.ReadCloser) *zcodeStartSSEConverter {
	return &zcodeStartSSEConverter{src: src, br: bufio.NewReaderSize(src, 64*1024), created: time.Now().Unix()}
}

func (c *zcodeStartSSEConverter) Read(p []byte) (int, error) {
	for len(c.pending) == 0 && !c.done && c.err == nil {
		c.nextEvent()
	}
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	if c.err != nil {
		return 0, c.err
	}
	if c.valid == 0 {
		return 0, errEmptyStream
	}
	return 0, io.EOF
}

// Close 在调用方提前结束时也清理底层连接；正常读完时底流已 EOF，重复 Close 无害。
func (c *zcodeStartSSEConverter) Close() error { return c.src.Close() }

func (c *zcodeStartSSEConverter) nextEvent() {
	line, err := c.br.ReadString('\n')
	if err != nil && err != io.EOF {
		c.err = err
		return
	}
	line = strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(line, "data:") {
		c.dataLines = append(c.dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
	}
	if line == "" && len(c.dataLines) > 0 {
		payload := strings.Join(c.dataLines, "\n")
		c.dataLines = c.dataLines[:0]
		c.handleEvent(payload)
	}
	if err == io.EOF {
		if len(c.dataLines) > 0 {
			payload := strings.Join(c.dataLines, "\n")
			c.dataLines = c.dataLines[:0]
			c.handleEvent(payload)
		}
		c.done = true
	}
}

func (c *zcodeStartSSEConverter) handleEvent(payload string) {
	if payload == "" || payload == "[DONE]" {
		return
	}
	var ev map[string]any
	if json.Unmarshal([]byte(payload), &ev) != nil {
		return
	}
	typ, _ := ev["type"].(string)
	switch typ {
	case "message_start":
		msg, _ := ev["message"].(map[string]any)
		if msg != nil {
			c.id, _ = msg["id"].(string)
			c.model, _ = msg["model"].(string)
		}
		if c.id == "" {
			c.id = "chatcmpl-zcode-" + session.NewMessageID()
		}
		if c.model == "" {
			c.model = zcodeStartModel
		}
		if !c.started {
			c.started = true
			c.emit(map[string]any{
				"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
			})
		}
	case "content_block_start":
		block, _ := ev["content_block"].(map[string]any)
		if block != nil && block["type"] == "tool_use" {
			idx := intFromAny(ev["index"])
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			c.emitDelta(map[string]any{"tool_calls": []any{map[string]any{
				"index": idx, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": ""},
			}}})
		}
	case "content_block_delta":
		delta, _ := ev["delta"].(map[string]any)
		if delta == nil {
			return
		}
		switch delta["type"] {
		case "text_delta":
			if s, _ := delta["text"].(string); s != "" {
				c.emitDelta(map[string]any{"content": s})
			}
		case "thinking_delta":
			if s, _ := delta["thinking"].(string); s != "" {
				c.emitDelta(map[string]any{"reasoning_content": s})
			}
		case "input_json_delta":
			if s, _ := delta["partial_json"].(string); s != "" {
				idx := intFromAny(ev["index"])
				c.emitDelta(map[string]any{"tool_calls": []any{map[string]any{
					"index": idx, "function": map[string]any{"arguments": s},
				}}})
			}
		}
	case "message_delta":
		delta, _ := ev["delta"].(map[string]any)
		finish := "stop"
		if delta != nil {
			switch delta["stop_reason"] {
			case "max_tokens":
				finish = "length"
			case "tool_use":
				finish = "tool_calls"
			case "end_turn", "stop_sequence", nil:
				finish = "stop"
			}
		}
		chunk := map[string]any{
			"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}},
		}
		if usage, _ := ev["usage"].(map[string]any); usage != nil {
			chunk["usage"] = zcodeStartUsage(usage)
		}
		c.emit(chunk)
	case "message_stop":
		if !c.started {
			c.started = true
			c.emit(map[string]any{
				"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": ""}, "finish_reason": nil}},
			})
		}
		c.pending = append(c.pending, []byte("data: [DONE]\n\n")...)
		c.done = true
	case "error":
		e, _ := ev["error"].(map[string]any)
		msg := "zcode start stream error"
		code := ""
		if e != nil {
			if s, _ := e["message"].(string); s != "" {
				msg = s
			}
			if v, ok := e["code"]; ok {
				code = fmt.Sprint(v)
			}
		}
		frame := map[string]any{"error": map[string]any{"message": msg, "type": "upstream_error", "code": code}}
		c.emit(frame)
		c.err = fmt.Errorf("zcode start stream error: %s", msg)
		c.pending = append(c.pending, []byte("data: [DONE]\n\n")...)
		c.done = true
	}
}

func (c *zcodeStartSSEConverter) emitDelta(delta map[string]any) {
	c.emit(map[string]any{
		"id": c.id, "object": "chat.completion.chunk", "created": c.created, "model": c.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})
}

func (c *zcodeStartSSEConverter) emit(v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.pending = append(c.pending, []byte("data: "+string(raw)+"\n\n")...)
	c.valid++
}

func zcodeStartUsage(u map[string]any) map[string]any {
	in := intFromAny(u["input_tokens"])
	out := intFromAny(u["output_tokens"])
	read := intFromAny(u["cache_read_input_tokens"])
	write := intFromAny(u["cache_creation_input_tokens"])
	miss := in - read
	if miss < 0 {
		miss = 0
	}
	return map[string]any{
		"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
		"prompt_cache_hit_tokens": read, "prompt_cache_miss_tokens": miss, "prompt_cache_write_tokens": write,
	}
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	}
	return 0
}

// qoder.go Qoder（CN）上游适配器：把 Qoder 官方客户端的 COSY 协议接成标准
// OpenAI Chat Completions 端点。
//
// 协议来源：magpie 的 @magpie-community/opencode-qoder-auth（MIT）。用本机真实
// Qoder CN 登录态实测：
//
//	GET  /algo/api/v2/model/list?Encode=1                    → 200（COSY 签名 + machine id）
//	POST /algo/api/v2/service/pro/sse/agent_chat_generation   → 200 SSE，原生 tool_calls
//
// 与 zcode/opencode/kilo 的差别：
//   - 鉴权不是一枚静态 Bearer，而是每请求现签的 COSY 信封（AES-128-CBC userBlob +
//     RSA 包裹会话钥 + md5 签名），machine id 必须是登录客户端的真实值；
//   - 请求体要走 Qoder 自定义的 base64 变体（字母表不同 + 前后三分之一交换）；
//   - 响应是 SSE envelope：{"body":"<OpenAI chunk json>","statusCodeValue":200}，
//     需要解包后再交给网关出口编码器。
//
// 模型目录按账号实时拉取（1h 缓存）。目录里的模型对象（model_config）必须原样放进
// 请求体——Qoder 不接受只写模型 id。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
)

const (
	defaultQoderChatBase = "https://gateway.qoder.com.cn"
	defaultQoderOpenAPI  = "https://openapi.qoder.com.cn"

	qoderModelsPath  = "/algo/api/v2/model/list?Encode=1"
	qoderChatPath    = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	qoderRefreshPath = "/api/v1/deviceToken/refresh"

	// qoderCosyVersion 桌面端 worker runtime 的信封版本。Qoder 客户端升级后若协议
	// 变更，改这一处；用 CLI 的版本号会被网关按非桌面端拒绝。
	qoderCosyVersion = "1.1.69"

	qoderSystemPrompt = "You are a Qoder agent. Use the instructions below and the tools available to you to assist the user."
)

// ProducerQoder Qoder 生产者标识（与 internal/source.ProducerQoder 同值）。
const ProducerQoder = "qoder"

// qoderRSAPublicKey Qoder 客户端内置的 1024-bit RSA 公钥，用来包裹每请求的 AES 会话钥。
const qoderRSAPublicKey = `-----BEGIN PUBLIC KEY-----
MIGfMA0GCSqGSIb3DQEBAQUAA4GNADCBiQKBgQDA8iMH5c02LilrsERw9t6Pv5Nc
4k6Pz1EaDicBMpdpxKduSZu5OANqUq8er4GM95omAGIOPOh+Nx0spthYA2BqGz+l
6HRkPJ7S236FZz73In/KVuLnwI8JJ2CbuJap8kvheCCZpmAWpb/cPx/3Vr/J6I17
XcW+ML9FoCI6AOvOzwIDAQAB
-----END PUBLIC KEY-----`

// qoderAlphabet Qoder 请求体编码的 64 字符表（与标准 base64 不同）。
const qoderAlphabet = "_doRTgHZBKcGVjlvpC,@aFSx#DPuNJme&i*MzLOEn)sUrthbf%Y^w.(kIQyXqWA!"

var qoderRSAKey struct {
	once sync.Once
	key  *rsa.PublicKey
	err  error
}

func qoderPublicKey() (*rsa.PublicKey, error) {
	qoderRSAKey.once.Do(func() {
		block, _ := pem.Decode([]byte(qoderRSAPublicKey))
		if block == nil {
			qoderRSAKey.err = fmt.Errorf("qoder: RSA 公钥解析失败")
			return
		}
		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			qoderRSAKey.err = fmt.Errorf("qoder: RSA 公钥解析失败: %w", err)
			return
		}
		rk, ok := pub.(*rsa.PublicKey)
		if !ok {
			qoderRSAKey.err = fmt.Errorf("qoder: 公钥不是 RSA")
			return
		}
		qoderRSAKey.key = rk
	})
	return qoderRSAKey.key, qoderRSAKey.err
}

// qoderOn 报告账号是否走 Qoder 上游。
func (c *Client) qoderOn(a *auth.Auth) bool {
	return a != nil && a.Producer() == ProducerQoder
}

// qoderBase 生效的 Qoder base：凭证自带 upstream_base > Client 覆盖 > 内置缺省。
func (c *Client) qoderBase(a *auth.Auth) string {
	if a != nil {
		if b := strings.TrimSpace(a.UpstreamBase()); b != "" {
			return strings.TrimRight(b, "/")
		}
	}
	if b := strings.TrimSpace(c.ChatBaseQoder); b != "" {
		return strings.TrimRight(b, "/")
	}
	return defaultQoderChatBase
}

// ---------- COSY 信封 ----------

func qoderHexID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// qoderMachineOS 与 Qoder 桌面端一致的机器标识串（x64_win32）。
func qoderMachineOS() string {
	arch := map[string]string{"amd64": "x64", "386": "x86", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" {
		arch = runtime.GOARCH
	}
	osName := map[string]string{"windows": "win32", "darwin": "darwin", "linux": "linux"}[runtime.GOOS]
	if osName == "" {
		osName = runtime.GOOS
	}
	return arch + "_" + osName
}

// qoderUserBlob 生成 COSY 信封里的 userBlob：AES-128-CBC（key 即 IV）加密用户 JSON，
// 再用 Qoder 的 RSA 公钥 PKCS#1 v1.5 包裹这把 16 字节会话钥。
func qoderUserBlob(a *auth.Auth) (info string, wrappedKey string, err error) {
	uid := strings.TrimPrefix(a.UID, "qoder-")
	payload := struct {
		UID                string `json:"uid"`
		AID                string `json:"aid"`
		Name               string `json:"name"`
		Email              string `json:"email"`
		SecurityOAuthToken string `json:"security_oauth_token"`
	}{UID: uid, AID: "", Name: a.Nickname, Email: "", SecurityOAuthToken: a.AccessTokenValue()}
	plain, merr := json.Marshal(payload)
	if merr != nil {
		return "", "", merr
	}
	key := []byte(qoderHexID()[:16])
	block, berr := aes.NewCipher(key)
	if berr != nil {
		return "", "", berr
	}
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	out := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, key).CryptBlocks(out, padded)
	pub, kerr := qoderPublicKey()
	if kerr != nil {
		return "", "", kerr
	}
	wrapped, rerr := rsa.EncryptPKCS1v15(rand.Reader, pub, key)
	if rerr != nil {
		return "", "", rerr
	}
	return base64.StdEncoding.EncodeToString(out), base64.StdEncoding.EncodeToString(wrapped), nil
}

// qoderHeaders 为一请求生成 COSY 头组。body 必须是在线形态（编码后的字节）。
func (c *Client) qoderHeaders(req *http.Request, a *auth.Auth, body []byte) error {
	info, wrapped, err := qoderUserBlob(a)
	if err != nil {
		return err
	}
	payloadJSON, _ := json.Marshal(map[string]any{
		"version": "v1", "requestId": qoderHexID(), "info": info,
		"cosyVersion": qoderCosyVersion, "ideVersion": "",
	})
	payload := base64.StdEncoding.EncodeToString(payloadJSON)
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	path := req.URL.Path
	if strings.HasPrefix(path, "/algo") {
		path = path[len("/algo"):]
	}
	sum := md5.Sum([]byte(payload + "\n" + wrapped + "\n" + ts + "\n" + string(body) + "\n" + path))
	sig := hex.EncodeToString(sum[:])

	mid := strings.TrimSpace(a.QoderMachineID())
	uid := strings.TrimPrefix(a.UID, "qoder-")
	h := req.Header
	h.Set("Accept", "application/json")
	h.Set("Accept-Encoding", "identity")
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Bearer COSY."+payload+"."+sig)
	h.Set("Cosy-Business-Product", "app")
	h.Set("Cosy-Business-Type", "agent")
	h.Set("Cosy-ClientIp", mid)
	h.Set("Cosy-ClientType", "10")
	h.Set("Cosy-Data-Policy", "disagree")
	h.Set("Cosy-Date", ts)
	h.Set("Cosy-Key", wrapped)
	h.Set("Cosy-MachineId", mid)
	h.Set("Cosy-MachineToken", mid)
	h.Set("Cosy-MachineType", "5")
	h.Set("Cosy-MachineOS", qoderMachineOS())
	h.Set("Cosy-Scene", "app")
	h.Set("Cosy-User", uid)
	h.Set("Cosy-Version", qoderCosyVersion)
	h.Set("Login-Version", "v2")
	return nil
}

// ---------- 请求体编码 ----------

// qoderSegmentEncode 按 Qoder 字母表做 6-bit 编码，`$` 补齐到 4 的倍数。
func qoderSegmentEncode(data []byte) string {
	var sb strings.Builder
	sb.Grow((len(data)+2)/3*4 + 4)
	acc, nb := 0, 0
	for _, b := range data {
		acc = ((acc << 8) | int(b)) & 0xffffff
		nb += 8
		for nb >= 6 {
			nb -= 6
			sb.WriteByte(qoderAlphabet[(acc>>nb)&63])
		}
	}
	if nb > 0 {
		sb.WriteByte(qoderAlphabet[(acc<<(6-nb))&63])
	}
	for sb.Len()%4 != 0 {
		sb.WriteByte('$')
	}
	return sb.String()
}

// qoderSwapThirds 把字符串前 1/3 与后 1/3 交换。
func qoderSwapThirds(s string) string {
	t := len(s) / 3
	if t == 0 {
		return s
	}
	return s[len(s)-t:] + s[t:len(s)-t] + s[:t]
}

// qoderEncodeBody 在线形态（发给 Qoder 的 body 字符串）。
func qoderEncodeBody(text string) string { return qoderSwapThirds(qoderSegmentEncode([]byte(text))) }

// qoderDecodeBody 解码（仅测试与排障用）。
func qoderDecodeBody(wire string) (string, error) {
	s := qoderSwapThirds(wire)
	var out []byte
	for g := 0; g+4 <= len(s); g += 4 {
		vals := make([]int, 0, 4)
		for i := 0; i < 4; i++ {
			ch := s[g+i]
			if ch == '$' {
				continue
			}
			idx := strings.IndexByte(qoderAlphabet, ch)
			if idx < 0 {
				return "", fmt.Errorf("qoder: 非法编码字符 %q", ch)
			}
			vals = append(vals, idx)
		}
		x := 0
		for _, v := range vals {
			x = x*64 + v
		}
		n := 6 * len(vals) / 8
		for k := 0; k < n; k++ {
			out = append(out, byte((x>>uint(6*len(vals)-8*(k+1)))&255))
		}
	}
	return string(out), nil
}

// ---------- 模型目录 ----------

// QoderCatalog 一个账号的 Qoder 模型目录：对外 ModelInfo + 每条模型的原始 model_config。
type QoderCatalog struct {
	Models []ModelInfo
	Raw    map[string]json.RawMessage
}

// qoderModelEntry 目录单条。model_config 必须是原对象，不能只写 id。
type qoderModelEntry struct {
	Key            string  `json:"key"`
	DisplayName    string  `json:"display_name"`
	Source         string  `json:"source"`
	Enable         bool    `json:"enable"`
	IsFree         bool    `json:"is_free"`
	IsVL           bool    `json:"is_vl"`
	IsReasoning    bool    `json:"is_reasoning"`
	PriceFactor    float64 `json:"price_factor"`
	MaxInputTokens int64   `json:"max_input_tokens"`
	ContextConfig  map[string]struct {
		TokenCount int64 `json:"token_count"`
		IsDefault  bool  `json:"is_default"`
	} `json:"context_config"`
	ThinkingConfig struct {
		Disabled json.RawMessage `json:"disabled"`
		Enabled  *struct {
			Efforts map[string]struct {
				IsDefault bool `json:"is_default"`
			} `json:"efforts"`
		} `json:"enabled"`
	} `json:"thinking_config"`
}

var qoderEffortRank = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}

func qoderEffortIndex(s string) int {
	for i, v := range qoderEffortRank {
		if v == s {
			return i
		}
	}
	return len(qoderEffortRank)
}

// qoderParseCatalog 解析 /algo/api/v2/model/list 的返回。app scene 优先，chat 补漏。
func qoderParseCatalog(raw []byte) (*QoderCatalog, error) {
	var env struct {
		App  []json.RawMessage `json:"app"`
		Chat []json.RawMessage `json:"chat"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("qoder models: 解析失败: %w", err)
	}
	list := env.App
	if len(list) == 0 {
		list = env.Chat
	} else {
		seen := map[string]bool{}
		for _, r := range env.App {
			var k struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(r, &k) == nil {
				seen[k.Key] = true
			}
		}
		for _, r := range env.Chat {
			var k struct {
				Key string `json:"key"`
			}
			if json.Unmarshal(r, &k) == nil && !seen[k.Key] {
				list = append(list, r)
			}
		}
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("qoder models: 上游返回空清单")
	}
	cat := &QoderCatalog{Raw: map[string]json.RawMessage{}}
	for _, r := range list {
		var e qoderModelEntry
		if err := json.Unmarshal(r, &e); err != nil {
			continue
		}
		key := strings.TrimSpace(e.Key)
		if key == "" || !e.Enable || key == "auto" || key == "default" {
			continue
		}
		mi := ModelInfo{
			ID:                key,
			Name:              firstNonEmptyStr(e.DisplayName, key),
			ContextWindow:     qoderMaxWindow(e),
			SupportsImages:    e.IsVL,
			SupportsReasoning: e.IsReasoning,
			SupportsToolCall:  true,
			Free:              e.IsFree || e.PriceFactor == 0,
			Vendor:            e.Source,
			DevBase:           defaultQoderChatBase,
		}
		if mi.Free {
			mi.Credits = "x0.00"
		} else {
			mi.Credits = "x" + strconv.FormatFloat(e.PriceFactor, 'f', -1, 64)
		}
		if tc := e.ThinkingConfig; tc.Enabled != nil {
			for name, v := range tc.Enabled.Efforts {
				mi.Efforts = append(mi.Efforts, name)
				if v.IsDefault {
					mi.DefaultEffort = name
				}
			}
			sort.Slice(mi.Efforts, func(i, j int) bool {
				return qoderEffortIndex(mi.Efforts[i]) < qoderEffortIndex(mi.Efforts[j])
			})
		}
		cat.Models = append(cat.Models, mi)
		cat.Raw[key] = r
	}
	if len(cat.Models) == 0 {
		return nil, fmt.Errorf("qoder models: 上游没有可用模型")
	}
	return cat, nil
}

// qoderMaxWindow 目录里最大的上下文窗口（对外展示真实能力，例如 1M）。
func qoderMaxWindow(e qoderModelEntry) int64 {
	var max int64
	for _, w := range e.ContextConfig {
		if w.TokenCount > max {
			max = w.TokenCount
		}
	}
	if max == 0 {
		max = e.MaxInputTokens
	}
	return max
}

// qoderDefaultWindow 请求默认窗口（context_config 的 is_default，否则 max_input_tokens）。
func qoderDefaultWindow(e qoderModelEntry) int64 {
	for _, w := range e.ContextConfig {
		if w.IsDefault && w.TokenCount > 0 {
			return w.TokenCount
		}
	}
	return e.MaxInputTokens
}

// qoderCatalogCache 目录缓存：按账号 UID 分桶，1h TTL + 5min 负缓存。
var qoderCatalogCache = struct {
	sync.Mutex
	byUID map[string]*qoderCatalogEntry
}{byUID: map[string]*qoderCatalogEntry{}}

type qoderCatalogEntry struct {
	cat      *QoderCatalog
	fetched  time.Time
	lastFail time.Time
}

const (
	qoderCatalogTTL     = time.Hour
	qoderCatalogFailTTL = 5 * time.Minute
)

// FetchQoderCatalog 拉取（或读缓存）某账号的模型目录。
func (c *Client) FetchQoderCatalog(ctx context.Context, a *auth.Auth) (*QoderCatalog, error) {
	if a == nil {
		return nil, fmt.Errorf("qoder models: 缺少账号")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	qoderCatalogCache.Lock()
	ent := qoderCatalogCache.byUID[a.UID]
	if ent != nil {
		if ent.cat != nil && time.Since(ent.fetched) < qoderCatalogTTL {
			cat := ent.cat
			qoderCatalogCache.Unlock()
			return cat, nil
		}
		if !ent.lastFail.IsZero() && time.Since(ent.lastFail) < qoderCatalogFailTTL {
			qoderCatalogCache.Unlock()
			return nil, fmt.Errorf("qoder models: 近期拉取失败，冷却中")
		}
	}
	qoderCatalogCache.Unlock()

	url := c.qoderBase(a) + qoderModelsPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if err := c.qoderHeaders(req, a, nil); err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		c.noteQoderCatalogFail(a.UID)
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		c.noteQoderCatalogFail(a.UID)
		return nil, err
	}
	if resp.StatusCode >= 400 {
		c.noteQoderCatalogFail(a.UID)
		return nil, &Error{Kind: classifyQoder(resp.StatusCode, string(raw)), Status: resp.StatusCode,
			Msg: truncate(string(raw), 200)}
	}
	cat, err := qoderParseCatalog(raw)
	if err != nil {
		c.noteQoderCatalogFail(a.UID)
		return nil, err
	}
	qoderCatalogCache.Lock()
	qoderCatalogCache.byUID[a.UID] = &qoderCatalogEntry{cat: cat, fetched: time.Now()}
	qoderCatalogCache.Unlock()
	return cat, nil
}

func (c *Client) noteQoderCatalogFail(uid string) {
	qoderCatalogCache.Lock()
	ent := qoderCatalogCache.byUID[uid]
	if ent == nil {
		ent = &qoderCatalogEntry{}
		qoderCatalogCache.byUID[uid] = ent
	}
	ent.lastFail = time.Now()
	qoderCatalogCache.Unlock()
}

// QoderCatalogModels 便捷包装：只要 ModelInfo（server 的 /v1/models 用）。
func (c *Client) QoderCatalogModels(ctx context.Context, a *auth.Auth) ([]ModelInfo, error) {
	cat, err := c.FetchQoderCatalog(ctx, a)
	if err != nil {
		return nil, err
	}
	return cat.Models, nil
}

// ---------- 对话 ----------

type qoderChatRequest struct {
	Model               string             `json:"model"`
	Messages            []qoderChatMessage `json:"messages"`
	Tools               []qoderTool        `json:"tools"`
	ToolChoice          json.RawMessage    `json:"tool_choice"`
	MaxTokens           int64              `json:"max_tokens"`
	MaxCompletionTokens int64              `json:"max_completion_tokens"`
	ReasoningEffort     string             `json:"reasoning_effort"`
	PromptCacheKey      string             `json:"prompt_cache_key"`
	User                string             `json:"user"`
}

type qoderChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []qoderToolCall `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
}

type qoderToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type qoderTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

// qoderTextBlocks 把 OpenAI content（字符串或 parts 数组）转成 Qoder 的 text/image 块。
func qoderTextBlocks(raw json.RawMessage) (string, []map[string]any) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == "" {
			return "", nil
		}
		return s, []map[string]any{{"type": "text", "text": s}}
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL any    `json:"image_url"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil
	}
	var text strings.Builder
	blocks := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				text.WriteString(p.Text)
				blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
			}
		case "image_url":
			url := ""
			switch v := p.ImageURL.(type) {
			case string:
				url = v
			case map[string]any:
				if u, ok := v["url"].(string); ok {
					url = u
				}
			}
			if url != "" {
				blocks = append(blocks, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			}
		}
	}
	return text.String(), blocks
}

// qoderBuildBody 把 OpenAI chat 请求翻译成 agent_chat_generation 的明文 body。
func qoderBuildBody(chat *qoderChatRequest, cfg json.RawMessage, e qoderModelEntry, meta ChatMeta) ([]byte, error) {
	var sysParts []string
	messages := make([]map[string]any, 0, len(chat.Messages)+1)
	for _, m := range chat.Messages {
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			txt, _ := qoderTextBlocks(m.Content)
			if strings.TrimSpace(txt) != "" {
				sysParts = append(sysParts, txt)
			}
		case "user":
			_, blocks := qoderTextBlocks(m.Content)
			if len(blocks) > 0 {
				messages = append(messages, map[string]any{"role": "user", "content": blocks})
			}
		case "assistant":
			txt, blocks := qoderTextBlocks(m.Content)
			msg := map[string]any{"role": "assistant", "content": blocks}
			if len(blocks) == 0 && txt == "" {
				msg["content"] = []map[string]any{}
			}
			if len(m.ToolCalls) > 0 {
				calls := make([]map[string]any, 0, len(m.ToolCalls))
				for _, c := range m.ToolCalls {
					id := strings.TrimSpace(c.ID)
					if id == "" {
						sum := sha256.Sum256([]byte(c.Function.Name + c.Function.Arguments))
						id = "call_" + hex.EncodeToString(sum[:])[:24]
					}
					calls = append(calls, map[string]any{
						"id": id, "type": "function",
						"function": map[string]any{"name": c.Function.Name, "arguments": c.Function.Arguments},
					})
				}
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
		case "tool":
			txt, _ := qoderTextBlocks(m.Content)
			messages = append(messages, map[string]any{
				"role": "tool", "tool_call_id": m.ToolCallID, "content": txt,
			})
		}
	}
	sysText := qoderSystemPrompt
	if len(sysParts) > 0 {
		sysText += "\n\n" + strings.Join(sysParts, "\n\n")
	}
	sysBlock := map[string]any{"type": "text", "text": sysText}
	messages = append([]map[string]any{{"role": "system", "content": []map[string]any{sysBlock}}}, messages...)

	maxTokens := chat.MaxCompletionTokens
	if maxTokens <= 0 {
		maxTokens = chat.MaxTokens
	}
	if maxTokens <= 0 {
		maxTokens = 32000
	}
	params := map[string]any{"max_tokens": maxTokens}
	alwaysThink := e.ThinkingConfig.Enabled != nil && len(e.ThinkingConfig.Disabled) == 0
	want := strings.ToLower(strings.TrimSpace(chat.ReasoningEffort))
	switch {
	case alwaysThink:
		params["enable_thinking"] = true
	case e.ThinkingConfig.Enabled != nil:
		params["enable_thinking"] = want != "" && want != "none" && want != "off" && want != "disabled"
	default:
		params["enable_thinking"] = false
	}
	if want != "" && want != "none" && want != "off" && want != "disabled" && e.ThinkingConfig.Enabled != nil {
		params["reasoning_effort"] = qoderFitEffort(want, e)
	}
	if w := qoderWindowFor(chat, e, maxTokens); w > 0 {
		params["context_length"] = w
	}

	reqID := qoderHexID()
	taskID := qoderHexID()
	body := map[string]any{
		"request_id":     reqID,
		"request_set_id": taskID,
		"chat_record_id": reqID,
		"session_id":     qoderSessionOf(chat, meta),
		"parameters":     params,
		"business": map[string]any{
			"product": "app", "version": qoderCosyVersion, "type": "agent",
			"id": taskID, "name": qoderTaskName(chat),
			"begin_at": time.Now().UnixMilli(), "stage": "start",
		},
		"agent_id":     "agent_common",
		"task_id":      "common",
		"session_type": "app",
		"model_config": cfg,
		"system":       []map[string]any{sysBlock},
		"messages":     messages,
	}
	if len(chat.Tools) > 0 {
		tools := make([]map[string]any, 0, len(chat.Tools))
		for _, t := range chat.Tools {
			if t.Type != "function" || strings.TrimSpace(t.Function.Name) == "" {
				continue
			}
			p := t.Function.Parameters
			if len(p) == 0 {
				p = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Function.Name, "description": t.Function.Description, "parameters": p,
				},
			})
		}
		if len(tools) > 0 {
			body["tools"] = tools
		}
	}
	return json.Marshal(body)
}

// qoderFitEffort 把客户端档位收敛到模型支持档位（取最接近一档；ultra→max）。
func qoderFitEffort(want string, e qoderModelEntry) string {
	if e.ThinkingConfig.Enabled == nil {
		return want
	}
	if want == "ultra" {
		if _, ok := e.ThinkingConfig.Enabled.Efforts["ultra"]; !ok {
			want = "max"
		}
	}
	if _, ok := e.ThinkingConfig.Enabled.Efforts[want]; ok {
		return want
	}
	at := qoderEffortIndex(want)
	best, dist := "", len(qoderEffortRank)+1
	for name := range e.ThinkingConfig.Enabled.Efforts {
		i := qoderEffortIndex(name)
		if i >= len(qoderEffortRank) {
			continue
		}
		d := i - at
		if d < 0 {
			d = -d
		}
		if d < dist || (d == dist && i > at) {
			best, dist = name, d
		}
	}
	if best == "" {
		return want
	}
	return best
}

// qoderWindowFor 选出请求要用的 context_length：默认窗口装得下就用默认，
// 否则用能装下的最小窗口，最大不超过目录最大窗口。
func qoderWindowFor(chat *qoderChatRequest, e qoderModelEntry, reply int64) int64 {
	type win struct{ size int64 }
	var wins []win
	for _, w := range e.ContextConfig {
		if w.TokenCount > 0 {
			wins = append(wins, win{w.TokenCount})
		}
	}
	if len(wins) == 0 {
		return e.MaxInputTokens
	}
	sort.Slice(wins, func(i, j int) bool { return wins[i].size < wins[j].size })
	def := qoderDefaultWindow(e)
	if def <= 0 {
		def = wins[0].size
	}
	need := int64(0)
	if raw, err := json.Marshal(map[string]any{"m": chat.Messages, "t": chat.Tools}); err == nil {
		need = int64(len(raw)+2) / 3
	}
	need += reply
	if need <= def {
		return def
	}
	for _, w := range wins {
		if w.size >= need {
			return w.size
		}
	}
	return wins[len(wins)-1].size
}

// qoderSessionOf 生成稳定 session_id：优先客户端会话键，否则用 system+首条 user 哈希。
func qoderSessionOf(chat *qoderChatRequest, meta ChatMeta) string {
	seed := ""
	switch {
	case meta.ConversationID != "":
		seed = "session\x00" + meta.ConversationID
	case strings.TrimSpace(chat.PromptCacheKey) != "":
		seed = "key\x00" + chat.PromptCacheKey
	case strings.TrimSpace(chat.User) != "":
		seed = "user\x00" + chat.User
	default:
		var sys []string
		var first any
		for _, m := range chat.Messages {
			if m.Role == "system" || m.Role == "developer" {
				txt, _ := qoderTextBlocks(m.Content)
				sys = append(sys, txt)
			}
			if m.Role == "user" && first == nil {
				first = m.Content
			}
		}
		b, _ := json.Marshal([]any{sys, first})
		seed = "chat\x00" + string(b)
	}
	sum := sha256.Sum256([]byte(seed))
	h := hex.EncodeToString(sum[:16])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// qoderTaskName 任务名：最后一条 user 文本的前 10 个字符（Qoder 客户端同口径）。
func qoderTaskName(chat *qoderChatRequest) string {
	for i := len(chat.Messages) - 1; i >= 0; i-- {
		if chat.Messages[i].Role == "user" {
			txt, _ := qoderTextBlocks(chat.Messages[i].Content)
			r := []rune(txt)
			if len(r) > 10 {
				r = r[:10]
			}
			return string(r)
		}
	}
	return ""
}

// classifyQoder 错误分类：401/403 除明确的登录过期码外不一律当 session dead
// （Qoder 会把排队、限额也用 403 发）；10605 排队走软限流。
func classifyQoder(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	if strings.Contains(body, "10605") || strings.Contains(lower, "isqueued") {
		return ErrSoftRate
	}
	if strings.Contains(body, `"code":105`) || strings.Contains(body, `"code": 105`) || strings.Contains(lower, "login expired") {
		return ErrSessionDead
	}
	if status == http.StatusUnauthorized {
		return ErrSessionDead
	}
	return Classify(status, body)
}

// ---------- SSE envelope 解包 ----------

// qoderSSEStream 把 Qoder 的 SSE envelope 流转换成标准 OpenAI SSE 流：
//
//	data: {"body":"{\"choices\":[...]}","statusCodeValue":200}
//	  ↓
//	data: {"choices":[...]}
//
// body 字段本身就是标准 OpenAI chunk，不需要再解码。
type qoderSSEStream struct {
	body   io.ReadCloser
	rd     *bufio.Reader
	out    bytes.Buffer
	seed   []string // 预读阶段已消费的 data 行（保证一字节不丢）
	cancel context.CancelFunc
}

func newQoderSSEStream(body io.ReadCloser, rd *bufio.Reader, seed []string, cancel context.CancelFunc) *qoderSSEStream {
	return &qoderSSEStream{body: body, rd: rd, seed: seed, cancel: cancel}
}

// nextLine 取下一行原始文本（优先 seed）。
func (s *qoderSSEStream) nextLine() (string, error) {
	if len(s.seed) > 0 {
		line := s.seed[0]
		s.seed = s.seed[1:]
		return line, nil
	}
	line, err := s.rd.ReadString('\n')
	return line, err
}

// Read 逐帧解包。
func (s *qoderSSEStream) Read(p []byte) (int, error) {
	for s.out.Len() == 0 {
		line, rerr := s.nextLine()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(trimmed[len("data:"):])
			// 裸 [DONE]（非 envelope 形态）：直接透传。上游多数时候把 [DONE] 包在
			// {"body":"[DONE]"} 里，但不能假定——漏发结束帧会让客户端挂到超时。
			if payload == "[DONE]" {
				s.out.WriteString("data: [DONE]\n\n")
				if rerr != nil {
					break
				}
				continue
			}
			var env struct {
				Body            string `json:"body"`
				StatusCodeValue int    `json:"statusCodeValue"`
				StatusCode      string `json:"statusCode"`
			}
			if json.Unmarshal([]byte(payload), &env) == nil {
				if env.StatusCodeValue != 0 && env.StatusCodeValue != 200 {
					msg := strings.TrimSpace(env.Body)
					if msg == "" {
						msg = payload
					}
					s.out.WriteString("data: " + qoderErrorFrame(msg, env.StatusCodeValue) + "\n\n")
					continue
				}
				inner := strings.TrimSpace(env.Body)
				switch {
				case inner == "":
					// 心跳/空帧：跳过
				case inner == "[DONE]":
					s.out.WriteString("data: [DONE]\n\n")
				case json.Valid([]byte(inner)):
					s.out.WriteString("data: " + inner + "\n\n")
				}
			}
		}
		if rerr != nil {
			if s.out.Len() > 0 {
				break
			}
			return 0, rerr
		}
	}
	n := copy(p, s.out.Bytes())
	s.out.Next(n)
	return n, nil
}

// Close 关闭上游体并释放请求 ctx。
func (s *qoderSSEStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return s.body.Close()
}

// qoderErrorFrame 把上游错误包成 OpenAI error 帧。
func qoderErrorFrame(msg string, status int) string {
	var parsed any
	message := any(msg)
	if json.Unmarshal([]byte(msg), &parsed) == nil {
		message = parsed
	}
	b, _ := json.Marshal(map[string]any{"error": map[string]any{
		"message": message, "type": "qoder_error", "code": status,
	}})
	return string(b)
}

// qoderChatStreamContext Qoder 的 chat 出站：签 COSY 头、发编码体、解 SSE envelope。
func (c *Client) qoderChatStreamContext(ctx context.Context, a *auth.Auth, body []byte, meta ChatMeta) (io.ReadCloser, int, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var chat qoderChatRequest
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, 0, nil, fmt.Errorf("qoder: 请求体解析失败: %w", err)
	}
	modelKey := strings.TrimSpace(chat.Model)
	if modelKey == "" {
		return nil, 0, nil, fmt.Errorf("qoder: 缺少 model")
	}
	cat, err := c.FetchQoderCatalog(ctx, a)
	if err != nil {
		return nil, 0, nil, err
	}
	cfg, ok := cat.Raw[modelKey]
	if !ok {
		msg := []byte(`{"error":{"message":"qoder: 模型不在目录：` + modelKey + `","type":"model_not_found"}}`)
		return nil, http.StatusNotFound, msg, &Error{Kind: ErrNotFound, Status: http.StatusNotFound, Msg: "qoder: model not found: " + modelKey}
	}
	var entry qoderModelEntry
	_ = json.Unmarshal(cfg, &entry)
	plain, err := qoderBuildBody(&chat, cfg, entry, meta)
	if err != nil {
		return nil, 0, nil, err
	}
	wire := qoderEncodeBody(string(plain))
	url := c.qoderBase(a) + qoderChatPath
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(wire))
	if err != nil {
		return nil, 0, nil, err
	}
	if err := c.qoderHeaders(req, a, []byte(wire)); err != nil {
		return nil, 0, nil, err
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("X-Model-Key", modelKey)
	src := entry.Source
	if src == "" {
		src = "system"
	}
	req.Header.Set("X-Model-Source", src)

	reqCtx, cancel := context.WithCancel(ctx)
	req = req.WithContext(reqCtx)
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		cancel()
		log.Printf("ERR: [upstream] qoder chat acct=%s: transport error: %v", logfmt.Label(a.UID, a.Nickname), err)
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
		kind := classifyQoder(resp.StatusCode, string(raw))
		log.Printf("WARN: [upstream] qoder chat acct=%s: upstream %d %s body=%s",
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
	// 预读第一个 data 帧：Qoder 常用 200 + envelope.statusCodeValue 非 200 表示排队/限额。
	// 这时还没向客户端写过任何字节，应当返回错误让 handler 换号，而不是先 200 再在流里报错。
	rd := bufio.NewReaderSize(resp.Body, 64*1024)
	var seed []string
	for i := 0; i < 64; i++ {
		line, rerr := rd.ReadString('\n')
		if line != "" {
			seed = append(seed, line)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "data:") {
			payload := strings.TrimSpace(strings.TrimSpace(line)[len("data:"):])
			var env struct {
				Body            string `json:"body"`
				StatusCodeValue int    `json:"statusCodeValue"`
			}
			if json.Unmarshal([]byte(payload), &env) == nil && env.StatusCodeValue != 0 && env.StatusCodeValue != 200 {
				raw := []byte(payload)
				resp.Body.Close()
				cancel()
				kind := classifyQoder(env.StatusCodeValue, string(raw))
				return nil, env.StatusCodeValue, raw, &Error{Kind: kind, Status: env.StatusCodeValue,
					Msg: truncate(string(raw), 200)}
			}
			break
		}
		if rerr != nil {
			break
		}
	}
	return newQoderSSEStream(resp.Body, rd, seed, cancel), resp.StatusCode, nil, nil
}

// ---------- token 刷新 ----------

// qoderRefreshToken 用 refreshToken 换新的 device token。Qoder 的 refresh token 是一次性
// 的（轮换），所以成功后必须由调用方 SaveAtomic 落盘。
func (c *Client) qoderRefreshToken(a *auth.Auth) error {
	rt := strings.TrimSpace(a.RefreshTokenValue())
	if rt == "" {
		return fmt.Errorf("qoder refresh: no refreshToken")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": rt})
	ctx, cancel := context.WithTimeout(context.Background(), refreshIOTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, defaultQoderOpenAPI+qoderRefreshPath, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("qoder refresh: HTTP %d: %s", resp.StatusCode, truncate(string(raw), 160))
	}
	var tok struct {
		Token        string `json:"token"`
		DeviceToken  string `json:"device_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresAt    string `json:"expires_at"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &tok); err != nil {
		return fmt.Errorf("qoder refresh: 解析失败: %w", err)
	}
	nt := firstNonEmptyStr(tok.Token, tok.DeviceToken)
	if nt == "" {
		return fmt.Errorf("qoder refresh: 响应里没有新 token")
	}
	a.Lock()
	a.AccessToken = nt
	if tok.RefreshToken != "" {
		a.RefreshToken = tok.RefreshToken
	}
	if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(tok.ExpiresAt)); perr == nil {
		a.ExpiresAt = t.Unix()
	} else if tok.ExpiresIn > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	}
	a.Unlock()
	return nil
}

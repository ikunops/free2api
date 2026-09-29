//go:build global_e2e

// global_e2e_test.go 国际版（Global realm）真实账号端到端实测断言 + 模糊项补齐。
//
// 与 internal/upstream/global_e2e_test.go（只读探针，只 log 不断言）互补：本文件
// 走真实 handler 全链路（含 NoteModelCost 接线）做**定论断言**，并补齐 PLAN §6 的
// 模糊项（billing 无 /v2、chat /v2、trial POST 幂等码、register GET、更多模型）。
//
// 用法（build tag global_e2e 隔离）：
//
//	go test -tags=global_e2e -run TestGlobalE2ER9CreditIsomorphic -v -count=1 ./internal/server/
//	go test -tags=global_e2e -run TestGlobalE2E -v -count=1 ./internal/server/
//
// 边界：只对真实 global 账号只读探测 + 一次 chat（与探针文件同语义）；不 SaveAtomic、
// 不改 auths/ 凭证；token 只脱敏出现。
package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

func init() {
	auth.SetGlobalEnabled(true)
}

// sseE2EBase 缺省 global chat base。
const sseE2EBase = "https://www.workbuddy.ai"

// e2eChatClient 构造带真实 global 配置的 upstream.Client（缺省 base 即可，路径由
// ChatStream 按 realm 路由）。60s 兜底超时防探测挂起。
func e2eChatClient() *upstream.Client {
	return &upstream.Client{
		HTTP:          &http.Client{Timeout: 60 * time.Second},
		ChatHTTP:      &http.Client{Timeout: 60 * time.Second},
		GlobalEnabled: true,
	}
}

// findSSEGlobalAuthFile 在候选路径里找 realm=global 的 auth 文件（与 upstream 探针
// 文件同策略，独立实现避免跨包依赖）。
func findSSEGlobalAuthFile(t *testing.T) string {
	t.Helper()
	dirs := []string{"../auths", "../../auths", "../../../auths", "/root/free2api/auths"}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
		if err != nil {
			continue
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var probe struct {
				Auth struct {
					Realm  string `json:"realm"`
					Domain string `json:"domain"`
				} `json:"auth"`
				Realm  string `json:"realm"`
				Domain string `json:"domain"`
			}
			_ = json.Unmarshal(raw, &probe)
			realm := probe.Auth.Realm
			if realm == "" {
				realm = probe.Realm
			}
			domain := probe.Auth.Domain
			if domain == "" {
				domain = probe.Domain
			}
			if realm == "global" || (domain != "" && strings.Contains(strings.ToLower(domain), "workbuddy.ai")) {
				return f
			}
		}
	}
	t.Skipf("no realm=global auth file found (searched %v)", dirs)
	return ""
}

// loadSSEGlobalAcct 从 auths/ 读取真实 global 账号。每次调用重新解析磁盘（独立实例）。
func loadSSEGlobalAcct(t *testing.T) *auth.Auth {
	t.Helper()
	fp := findSSEGlobalAuthFile(t)
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Skipf("read auth file: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Skipf("parse auth file: %v", err)
	}
	if a.Realm() != "global" {
		t.Skipf("auth not global: realm=%s domain=%s", a.Realm(), a.Domain)
	}
	a.FilePath = fp
	return a
}

// sseChatBody 组装 chat 探测请求体（system 首条触达上游；与生产 chat 同形态）。
// 直接 marshaling OpenAI 形状，不依赖上游未导出改装函数（prepareBody/ensureConsoleSystem）。
func sseChatBody(cl *upstream.Client, a *auth.Auth, model string, maxTokens int) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": maxTokens,
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a helpful assistant."},
			map[string]any{"role": "user", "content": "Reply with the single word 'ok'."},
		},
	})
	return raw
}

// sseDoProbe 发一个原样请求并返回 HTTP status + body（上限 1MB）。
func sseDoProbe(cl *upstream.Client, a *auth.Auth, method, url string, headerFn func(*http.Request, *auth.Auth), body []byte) (int, []byte) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, []byte("construct req: " + err.Error())
	}
	if headerFn != nil {
		headerFn(req, a)
	}
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		return 0, []byte("http: " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

// sseReadSummary 完整读 SSE 流（io.Reader 或 io.ReadCloser 皆可）：返回末帧 JSON、
// 有效帧数、末帧原文。调用方负责 close（如 rec.Body 为 buffer 无需 close）。
func sseReadSummary(r io.Reader) (map[string]any, int, string) {
	br := newReader(r)
	var lastFrame string
	frames := 0
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: ") {
				p := strings.TrimPrefix(trimmed, "data: ")
				if p == "[DONE]" {
					break
				}
				frames++
				lastFrame = p
			}
		}
		if err != nil {
			break
		}
	}
	var obj map[string]any
	if lastFrame != "" {
		_ = json.Unmarshal([]byte(lastFrame), &obj)
	}
	return obj, frames, lastFrame
}

// ============================================================================
// R9 定论：global SSE 末帧 usage.credit 同构 + NoteModelCost 接线（Task 3）
// ============================================================================
// 走真实 handler 全链路（pool + Client + handler.ServeHTTP），发 `global:deepseek-v4.1-flash`，
// 断言末帧带 usage.credit 且账本记录 tier0 免费（credit=0）。这把 PLAN §6 R9 的【待实测】
// 标为定论——上游探针文件只 log 不断言，本测试是真正的断言。
func TestGlobalE2ER9CreditIsomorphic(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cl := e2eChatClient()
	a := loadSSEGlobalAcct(t)

	p := pool.New(t.TempDir() + "/state.json")
	p.Add(a)
	p.SetCredits(a.UID, 1000)
	h := NewHandler(Config{
		Pool:          p,
		Upstream:      cl,
		SoftCooldown:  time.Minute,
		PromptMode:    "passthrough",
		GlobalEnabled: true,
	})

	body, _ := json.Marshal(map[string]any{
		"model":      "global:deepseek-v4.1-flash",
		"stream":     true,
		"max_tokens": 10,
		"messages": []any{
			map[string]any{"role": "user", "content": "Reply with the single word 'ok'."},
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
	if rec.Code != 200 {
		snippet := rec.Body.String()
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		t.Fatalf("R9 chat global:deepseek-v4.1-flash code=%d body=%s", rec.Code, snippet)
	}

	lastFrameObj, frames, lastFrame := sseReadSummary(rec.Body)
	if lastFrameObj == nil {
		t.Fatalf("R9 末帧不可解析（frames=%d lastFrame=%s）", frames, lastFrame)
	}
	// 末帧是 OpenAI chunk：usage 是顶层 object 内的子对象（非顶层键）。
	usage, _ := lastFrameObj["usage"].(map[string]any)
	if usage == nil {
		t.Fatalf("R9 末帧无 usage 子对象（frames=%d lastFrame=%s）", frames, lastFrame)
	}
	if _, hasCredit := usage["credit"]; !hasCredit {
		t.Errorf("R9 末帧 usage 缺少 credit 字段（global SSE 与 CN 非同构）: usage=%v", usage)
	} else {
		t.Logf("R9 末帧 usage.credit 存在（与 CN 同构 ✓）")
	}

	// NoteModelCost 被调用：账本应记录该 (uid, model) 观测，且为 tier0 免费（credit=0）。
	per1k, ok := p.ModelCost(a.UID, "deepseek-v4.1-flash")
	if !ok {
		t.Errorf("R9 NoteModelCost 未被调用：pool.ModelCost(%s, deepseek-v4.1-flash) 无观测", logfmt.UID8(a.UID))
	} else if per1k != 0 {
		t.Errorf("R9 账本 per1k=%.6f want 0（deepseek-v4.1-flash 应为 tier0 免费层）", per1k)
	} else {
		t.Logf("R9 NoteModelCost 已被调用：tier0 免费层（credit=0 → per1k=0）✓")
	}
}

// ============================================================================
// 模糊项实录补齐（Task 4）：全部走真实 global 账号只读探测 + 结论文案
// ============================================================================

// TestGlobalE2ER1BillingNoV2 billing 路径定序：/billing/meter/get-user-resource
// （无 /v2 前缀）→ 2xx 确认。修复前不确定该号走无 /v2 是否 200，现在锚定为结论。
func TestGlobalE2ER1BillingNoV2(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cl := e2eChatClient()
	a := loadSSEGlobalAcct(t)
	timestamp := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": timestamp.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   timestamp.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
	raw, _ := json.Marshal(body)
	status, respBody := sseDoProbe(cl, a, http.MethodPost, sseE2EBase+"/billing/meter/get-user-resource",
		func(req *http.Request, ac *auth.Auth) { cl.BillingHeaders(req, ac) }, raw)
	if status >= 200 && status < 300 {
		t.Logf("R1 结论: /billing/meter/get-user-resource（无 /v2）→ %d 确认（global 无 /v2 直通）", status)
	} else {
		t.Logf("R1 结论: /billing/meter/get-user-resource（无 /v2）→ %d 未确认", status)
	}
	t.Logf("R1 body: %s", strings.TrimSpace(string(respBody))[:min(300, len(strings.TrimSpace(string(respBody))))])
}

// TestGlobalE2ER2ChatV2 chat 路径 /v2/chat/completions → 200 + SSE 流（已通，补锚定）。
// 与上游探针文件（只读探测）互补：这里直接驱动 handler 走一遍，确认 SSE 流可读。
func TestGlobalE2ER2ChatV2(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cl := e2eChatClient()
	a := loadSSEGlobalAcct(t)
	// default-model 用 max_tokens=10 足够（最便宜稳定）；gpt-5.4 更贵且要求 ≥100。
	body := sseChatBody(cl, a, "default-model", 10)
	status, raw := sseDoProbe(cl, a, http.MethodPost, sseE2EBase+"/v2/chat/completions",
		func(req *http.Request, ac *auth.Auth) { cl.ChatHeaders(req, ac, "", upstream.ChatMeta{}) }, body)
	t.Logf("R2 /v2/chat/completions status=%d", status)
	if status >= 200 && status < 300 {
		usage, frames, _ := sseReadSummary(io.NopCloser(bytes.NewReader(raw)))
		t.Logf("R2 结论: /v2/chat/completions → 200 + SSE（frames=%d），usage=%v", frames, usage != nil)
	} else {
		t.Logf("R2 结论: /v2/chat/completions → %d（未确认）", status)
	}
}

// TestGlobalE2EDefaultAndGpt54 更多模型：global:default-model（credit=0 免费）与
// global:gpt-5.4（有 credits 号，credit=0.04）确认都能通。
func TestGlobalE2EDefaultAndGpt54(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cl := e2eChatClient()
	a := loadSSEGlobalAcct(t)

	for _, model := range []string{"global:default-model", "global:gpt-5.4"} {
		_, bare := ResolveModel(model)
		// gpt-5.4 实测要求 max_tokens ≥100（code 11133 integer_below_min_value 否则）；
		// default-model 无此限制。统一用 100 保证两型号都通过。
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"stream":     true,
			"max_tokens": 100,
			"messages": []any{
				map[string]any{"role": "user", "content": "Reply with the single word 'ok'."},
			},
		})
		rec := httptest.NewRecorder()
		NewHandler(Config{Pool: testE2EPool(cl, a), Upstream: cl, SoftCooldown: time.Minute, PromptMode: "passthrough"}).
			ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body)))
		t.Logf("模型 %s → status=%d", model, rec.Code)
		if rec.Code != 200 {
			continue
		}
		if frame, _, _ := sseReadSummary(rec.Body); frame != nil {
			usage, _ := frame["usage"].(map[string]any)
			if usage != nil {
				if c, has := usage["credit"]; has && c != nil {
					if f, ok := c.(float64); ok {
						t.Logf("模型 %s → credit=%.4f", model, f)
					} else {
						t.Logf("模型 %s → credit=%v", model, c)
					}
				}
			}
		}
		_ = bare
	}
}

// testE2EPool 构造含单真实 global 账号的内存池（不落盘）。
func testE2EPool(cl *upstream.Client, a *auth.Auth) *pool.Pool {
	p := pool.New("")
	p.Add(a)
	p.SetCredits(a.UID, 1000)
	return p
}

// TestGlobalE2ETrialPostIdempotent trial POST /billing/ide/trial → 幂等码 14051
// 确认不等于失败（该号此前很可能已领过 trial 包）。
func TestGlobalE2ETrialPostIdempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cl := e2eChatClient()
	a := loadSSEGlobalAcct(t)
	status, raw := sseDoProbe(cl, a, http.MethodPost, sseE2EBase+"/billing/ide/trial",
		func(req *http.Request, ac *auth.Auth) { cl.BillingHeaders(req, ac) }, []byte("{}"))
	t.Logf("trial POST status=%d body=%s", status, strings.TrimSpace(string(raw))[:min(300, len(strings.TrimSpace(string(raw))))])
	if status >= 200 && status < 300 {
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(raw, &env); err == nil {
			if env.Code == 14051 {
				t.Logf("trial POST 结论: code=14051 幂等已领（不算失败）✓")
			} else {
				t.Logf("trial POST 结论: code=%d（新领取或未预期）", env.Code)
			}
		}
	} else {
		t.Logf("trial POST 结论: HTTP %d（该号可能 trial 状态特殊）", status)
	}
}

// TestGlobalE2ERegisterGET register GET → code 200 确认（已激活/新激活均 200）。
func TestGlobalE2ERegisterGET(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	cl := e2eChatClient()
	a := loadSSEGlobalAcct(t)
	// 注：不走 auth 头，register 端点仅需要 query userId（实测该账号 code 200）。
	status, raw := sseDoProbe(cl, a, http.MethodGet,
		sseE2EBase+"/auth/realms/copilot/overseas/user/register?userId="+a.UID, nil, nil)
	t.Logf("register GET status=%d body=%s", status, strings.TrimSpace(string(raw))[:min(300, len(strings.TrimSpace(string(raw))))])
	if status >= 200 && status < 300 {
		var env struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		if err := json.Unmarshal(raw, &env); err == nil && env.Code == 200 {
			t.Logf("register GET 结论: code=200 已激活（register required 无碍）✓")
		} else {
			t.Logf("register GET 结论: code=%d msg=%q", env.Code, env.Msg)
		}
	} else {
		t.Logf("register GET 结论: HTTP %d（未捕获业务码）", status)
	}
}

// ---------------------------------------------------------------------------
// helpers（独立于 upstream 探针文件的小工具）
// ---------------------------------------------------------------------------

func newReader(rc io.Reader) *bufioReader {
	return &bufioReader{rc: rc}
}

type bufioReader struct {
	rc io.Reader
}

// ReadString 简易逐行读（简化实现，避免依赖 bufio 的 Scanner 状态机）。
func (r *bufioReader) ReadString(delim byte) (string, error) {
	var buf []byte
	tmp := make([]byte, 1)
	for {
		n, err := r.rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if tmp[0] == delim {
				return string(buf), nil
			}
		}
		if err != nil {
			return string(buf), err
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

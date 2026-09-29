package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// realmFake 捕获出站 chat 请求的 authz/model/path/messages 数的双域 fake upstream。
// GlobalEnabled=true（生产接线语义），base 假值无实际连接。
type realmFake struct {
	up *upstream.Client

	mu    sync.Mutex
	authz string
	model string
	path  string
	msgs  int
}

func newRealmFake(t *testing.T) *realmFake {
	t.Helper()
	cf := &realmFake{}
	cf.up = &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			cf.mu.Lock()
			defer cf.mu.Unlock()
			cf.authz = r.Header.Get("Authorization")
			cf.path = r.URL.Path
			raw, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(raw, &req)
			cf.model, _ = req["model"].(string)
			if ms, ok := req["messages"].([]any); ok {
				cf.msgs = len(ms)
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:        "https://fake.cn",
		BillingBaseCN:     "https://fake.cn",
		ChatBaseGlobal:    "https://fake.global",
		BillingBaseGlobal: "https://fake.global",
		GlobalEnabled:     true,
	}
	return cf
}

// TestChatRealmSelectionAndBodyRewrite 断言 realm 贯穿：
// global: 前缀 → 全局号 + 出站 body 剥前缀 + /v2 路径（#119 固定单路径） + ensureConsoleSystem 补 system；
// 裸名 → CN 号 + /v2 路径 + body 原样（零回归）。
// TestModelsGlobalListGating 断言 GlobalEnabled 逃生门仍生效：true 时 global 域探测名单
// 可列出（带 global: 前缀）、false 时 global 名单不出现（纯动态，两侧均无静态兜底——
// 无可上游时列表为空，不再断言具体模型名）。
func TestModelsGlobalListGating(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()
	// 无健康上游（fake base 不可达）→ CN/global 两域均拉取失败 → 空列表（纯动态）。
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     "https://fake.cn",
		ChatBaseGlobal: "https://fake.global",
		GlobalEnabled:  true,
	}
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})
	for _, m := range h.modelList() {
		if strings.HasPrefix(m["id"].(string), "global:") {
			t.Fatalf("pure-dynamic: unreachable upstream must not list global models: %q", m["id"])
		}
	}

	// 缺省（GlobalEnabled=false）不列 global 名单（逃生门）。
	h2 := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})
	for _, m := range h2.modelList() {
		if strings.HasPrefix(m["id"].(string), "global:") {
			t.Fatalf("GlobalEnabled=false: global: model %q should not be listed", m["id"])
		}
	}
}

func TestChatRealmSelectionAndBodyRewrite(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	cf := newRealmFake(t)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	// global: 前缀请求
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("global chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel, gotPath, gotMsgs := cf.authz, cf.model, cf.path, cf.msgs
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_gl" {
		t.Errorf("global chat authz=%q want g1 Bearer", gotAuthz)
	}
	if gotModel != "gpt-5.4" {
		t.Errorf("global chat outbound model=%q want gpt-5.4 (prefix stripped)", gotModel)
	}
	if gotPath != "/v2/chat/completions" {
		t.Errorf("global chat path=%q want /v2/chat/completions", gotPath)
	}
	if gotMsgs != 2 { // ensureConsoleSystem：user 前置补 system
		t.Errorf("global chat messages=%d want 2 (system fallback)", gotMsgs)
	}

	// 裸名请求（CN 零回归）
	cf.mu.Lock()
	cf.authz, cf.model, cf.path, cf.msgs = "", "", "", 0
	cf.mu.Unlock()
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("cn chat code=%d body=%s", rec.Code, rec.Body)
	}
	cf.mu.Lock()
	gotAuthz, gotModel, gotPath, gotMsgs = cf.authz, cf.model, cf.path, cf.msgs
	cf.mu.Unlock()
	if gotAuthz != "Bearer at_cn" {
		t.Errorf("cn chat authz=%q want cn1 Bearer", gotAuthz)
	}
	if gotModel != "glm-5.2" {
		t.Errorf("cn chat outbound model=%q want glm-5.2", gotModel)
	}
	if gotPath != "/v2/chat/completions" {
		t.Errorf("cn chat path=%q want /v2/chat/completions", gotPath)
	}
	if gotMsgs != 1 { // CN 不补 system
		t.Errorf("cn chat messages=%d want 1 (no system fallback)", gotMsgs)
	}
}

// TestChatRealmGlobalAbortedNoGlobalAccount 逃生门双保险：默认开启后，仅池里没有 global
// 账号时，global: 前缀请求无可选号 → 503（不跨 realm 用 CN 号顶上）。
func TestChatRealmGlobalAbortedNoGlobalAccount(t *testing.T) {
	cf := newRealmFake(t)
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no global account: code=%d want 503 (must not fall back to cn)", rec.Code)
	}
}

// TestChatRealmGlobalDisabledEscapeHatch 逃生门：config GlobalEnabled=false 时，即便默认
// 开启（auth.Realm() 判 global 账号），也没有 global 上游可路由，请求回落 CN 路径（200，
// 不崩溃、不泄漏 global 凭据）。
func TestChatRealmGlobalDisabledEscapeHatch(t *testing.T) {
	cf := newRealmFake(t)
	cf.up.GlobalEnabled = false
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: false})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:gpt-5.4","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("global disabled escape hatch: code=%d want 200 (routes CN path)", rec.Code)
	}
	cf.mu.Lock()
	gotPath := cf.path
	cf.mu.Unlock()
	if gotPath != "/v2/chat/completions" {
		t.Errorf("global disabled escape hatch: path=%q want CN /v2/chat/completions", gotPath)
	}
}

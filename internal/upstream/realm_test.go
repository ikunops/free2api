package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// globalTestClient 构造带 global 配置的 Client（GlobalEnabled=true + global base 指向 httptest 两段
// chat/console 与 billing/meter），并确保 auth globalEnabled 开关开启（缺省）。
func globalTestClient(t *testing.T, chatSrv, billingSrv *httptest.Server) *Client {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	return &Client{
		HTTP:             &http.Client{}, // DefaultTransport → 走 httptest 服务器真实地址
		ChatBaseCN:       "https://chat.example",
		BillingBaseCN:    "https://billing.example",
		ChatBaseGlobal:   strings.TrimSuffix(chatSrv.URL, "/"),
		BillingBaseGlobal: strings.TrimSuffix(billingSrv.URL, "/"),
		GlobalEnabled:     true,
	}
}

// globalAcct 构造一个 realm=global 的账号（显式 domain fallback：www.workbuddy.ai）。
func globalAcct() *auth.Auth {
	return &auth.Auth{AccessToken: "at", RefreshToken: "rt", UID: "g1", Domain: "www.workbuddy.ai"}
}

// TestGlobalChatUsesV2PathAndBase 断言 global 账号的 chat 打到 ChatBaseGlobal + /v2/chat/completions
// （#119：/console 挂腾讯云 WAF body 内容规则，固定 /v2 单路径），
// 且首条消息非 system 时自动补兜底 system（ensureConsoleSystem）。
func TestGlobalChatUsesV2PathAndBase(t *testing.T) {
	var gotPath, gotOrigin, gotModel string
	var calls []string
	var gotMsgs []map[string]any
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		gotPath = r.URL.Path
		gotOrigin = r.Header.Get("Origin")
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		gotModel, _ = req["model"].(string)
		if msgs, ok := req["messages"].([]any); ok {
			for _, m := range msgs {
				if mm, ok := m.(map[string]any); ok {
					gotMsgs = append(gotMsgs, mm)
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer chatSrv.Close()
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer billSrv.Close()

	c := globalTestClient(t, chatSrv, billSrv)
	a := globalAcct()
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()

	if gotPath != "/v2/chat/completions" {
		t.Errorf("global chat path=%q want /v2/chat/completions", gotPath)
	}
	// 单路径：出站只打一次，不存在 fallback 二次请求。
	if len(calls) != 1 {
		t.Errorf("global chat calls=%v want exactly 1 outbound request (/v2 single path)", calls)
	}
	if gotOrigin != "https://www.workbuddy.ai" {
		t.Errorf("global chat Origin=%q want https://www.workbuddy.ai", gotOrigin)
	}
	if gotModel != "gpt-5.4" {
		t.Errorf("model=%q", gotModel)
	}
	// ensureConsoleSystem：首条 user → 前置 system 兜底。
	if len(gotMsgs) != 2 || gotMsgs[0]["role"] != "system" {
		t.Errorf("expected prepended system fallback, got %v", gotMsgs)
	}
}

// TestGlobalChatNoFallbackOn404 断言 /v2 单路径下 404 时不再发起第二次请求
// （#119：global chat 固定 /v2，旧 [console→v2] fallback 链已移除）。
func TestGlobalChatNoFallbackOn404(t *testing.T) {
	var calls []string
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":404,"msg":"nope"}`))
	}))
	defer chatSrv.Close()
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer billSrv.Close()

	c := globalTestClient(t, chatSrv, billSrv)
	_, status, _, err := c.ChatStream(globalAcct(), []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrNotFound || status != 404 {
		t.Fatalf("chat 404: want status=404 + *Error{not_found}, got status=%d err=%v", status, err)
	}
	if len(calls) != 1 || calls[0] != "/v2/chat/completions" {
		t.Errorf("chat 404 calls=%v want exactly [/v2/chat/completions] (no fallback retry)", calls)
	}
}

// TestGlobalBillingUsesBillingMeterThenV2Fallback 断言 global billing 先打 /billing/meter/*
// 404 时 fallback /v2/billing/meter/*；CN billing 直接 /v2/billing/meter/*（现状逐字）。
func TestGlobalBillingUsesBillingMeterThenV2Fallback(t *testing.T) {
	var billingCalls []string
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		billingCalls = append(billingCalls, r.URL.Path)
		if r.URL.Path == "/billing/meter/get-user-resource" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"msg":"nope"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`))
	}))
	defer billSrv.Close()
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer chatSrv.Close()

	c := globalTestClient(t, chatSrv, billSrv)
	remain, err := c.UserResource(globalAcct())
	if err != nil {
		t.Fatalf("global billing: %v", err)
	}
	if remain != 0 {
		t.Errorf("remain=%d want 0", remain)
	}
	if len(billingCalls) != 2 ||
		billingCalls[0] != "/billing/meter/get-user-resource" ||
		billingCalls[1] != "/v2/billing/meter/get-user-resource" {
		t.Errorf("global billing fallback calls=%v want [/billing/meter/get-user-resource, /v2/billing/meter/get-user-resource]", billingCalls)
	}
}

// TestGlobalRefreshUsesGlobalChatBase 断言 global 账号 refresh 打到 ChatBaseGlobal + /v2/plugin/auth/token/refresh。
func TestGlobalRefreshUsesGlobalChatBase(t *testing.T) {
	var gotPath, gotHost string
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHost = r.Host
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{"accessToken":"newat","expiresIn":3600}}`))
	}))
	defer chatSrv.Close()
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer billSrv.Close()

	c := globalTestClient(t, chatSrv, billSrv)
	a := globalAcct()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("global refresh: %v", err)
	}
	if gotPath != "/v2/plugin/auth/token/refresh" {
		t.Errorf("global refresh path=%q", gotPath)
	}
	if !strings.HasPrefix(gotHost, "127.0.0.1") {
		t.Errorf("global refresh host=%q want httptest server", gotHost)
	}
}

// TestCNChatPathUnchanged 零回归：CN 账号（realm=cn）chat 仍打 CN base + /v2/chat/completions，
// 无首条 system 兜底（ensureConsoleSystem 只对 global）。
func TestCNChatPathUnchanged(t *testing.T) {
	var gotPath, gotMsgsJSON string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		gotMsgsJSON = string(raw)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("cn chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if gotPath != "/v2/chat/completions" {
		t.Errorf("cn chat path=%q", gotPath)
	}
	// 零回归：body 不因 ensureConsoleSystem 注入 system。
	if bytes.Contains([]byte(gotMsgsJSON), []byte(`"system"`)) {
		t.Errorf("cn chat body should not be modified with system fallback: %s", gotMsgsJSON)
	}
}

// TestGlobalChatServerFallbackErrorCode 断言 500 时直接返回错误、不发起第二次请求
// （#119 后 global 单路径，语义与旧「fallback 只在 404/405」收窄为「无 fallback」）。
func TestGlobalChatServerFallbackErrorCode(t *testing.T) {
	var calls []string
	chatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer chatSrv.Close()
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer billSrv.Close()

	c := globalTestClient(t, chatSrv, billSrv)
	_, status, _, err := c.ChatStream(globalAcct(), []byte(`{"model":"gpt-5.4","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`), "", ChatMeta{})
	// 5xx 错误路径现在返回**已分类的** *Error（Kind=ErrServer，见 ChatStreamContext
	// 注释）：err 非 nil 是新契约，传输层错误（非 *Error）仍按抖动处理。
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrServer {
		t.Fatalf("chat 500: want classified *Error{server}, got %v", err)
	}
	if status != 500 {
		t.Errorf("status=%d want 500", status)
	}
	if len(calls) != 1 || calls[0] != "/v2/chat/completions" {
		t.Errorf("500 calls=%v want exactly [/v2/chat/completions] (no retry)", calls)
	}
}
// TestEffortsKeyedByRealm efforts 缓存按 realm 隔离：CN 探测写入的 supportedEfforts
// 不得被 global 同模型名请求复用（C-2）。global 侧无 efforts 探测 → 原样透传不降级；
// 同 Client 上 CN 请求仍按 CN 探测结果降级。
func TestEffortsKeyedByRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var globalBody, cnBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
			// CN 探测（FetchModels 走 chatBase(cn)）= CN base + 该路径。
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"medium","supportedEfforts":["low","medium"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`))
		case strings.HasSuffix(r.URL.Path, "/v2/chat/completions"):
			// #119 后 global/cn chat 均打 /v2：global 先到（RealmAcct）——按 Bearer 区分
			// 归属桶（globalAcct/cn 共用同一 chat base 指向本 srv）。
			switch r.Header.Get("Authorization") {
			case "Bearer at-global":
				globalBody, _ = io.ReadAll(r.Body)
			default:
				cnBody, _ = io.ReadAll(r.Body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		default:
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"msg":"nope"}`))
		}
	}))
	defer srv.Close()

	base := strings.TrimSuffix(srv.URL, "/")
	c := &Client{
		HTTP:               http.DefaultClient,
		ChatBaseCN:         base,
		BillingBaseCN:      "https://billing.example",
		ChatBaseGlobal:     base,
		GlobalEnabled:      true,
		SanitizeFingerprints: true,
	}
	cn := &auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"}

	// step 1：CN 探测写 cn 桶（glm-5.2 → [low, medium]）。
	if _, err := c.FetchModels(cn); err != nil {
		t.Fatalf("cn fetch models: %v", err)
	}

	// step 2：global 账号同模型名请求 → 不应命中 CN 探测的 supportedEfforts，原样透传 high。
	glb := globalAcct()
	glb.AccessToken = "at-global"
	rc, status, _, err := c.ChatStream(glb, []byte(`{"model":"glm-5.2","reasoning_effort":"high","messages":[{"role":"system","content":"s"}]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("global chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(globalBody, &m); err != nil {
		t.Fatalf("unmarshal global outbound: %v (%s)", err, globalBody)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("global reasoning_effort=%v want high (CN efforts must not contaminate global)", m["reasoning_effort"])
	}

	// step 3：同 Client 的 CN 请求仍按 CN 桶降级（high 不在 [low,medium] → 降至 medium）。
	rc, status, _, err = c.ChatStream(cn, []byte(`{"model":"glm-5.2","reasoning_effort":"high","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("cn chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if err := json.Unmarshal(cnBody, &m); err != nil {
		t.Fatalf("unmarshal cn outbound: %v (%s)", err, cnBody)
	}
	if got, _ := m["reasoning_effort"].(string); got != "medium" {
		t.Errorf("cn reasoning_effort=%v want medium (cn bucket still applies within realm)", m["reasoning_effort"])
	}
}

func cloneBody(r *http.Request) []byte {
	raw, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return raw
}

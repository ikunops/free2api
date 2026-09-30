// handler_model_echo_test.go 端到端锚定 /v1/chat/completions 的 model 回显：
// 客户端请求名必须出现在响应里，而不是上游回程的裸名。回归背景：上游只认裸名
// （出站前 rewriteModel 剥前缀），同源多逻辑名（cn:x-x0.11 / global:x-free）时
// 裸名回程会被严格校验的 harness 判为模型错配（DSH invalidReplay）。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// TestChatNonStreamEchoesClientModel 非流式响应 model = 客户端请求名（含前缀）。
func TestChatNonStreamEchoesClientModel(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	const want = "cn:deepseek-v4.1-flash-x0.11"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+want+`","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	// 上游 sseOK 的裸名是 glm-5.2；出口必须回显客户端请求名。
	if resp["model"] != want {
		t.Errorf("model=%v want %q (client request name)", resp["model"], want)
	}
}

// TestChatStreamEchoesClientModel 流式每一帧 model = 客户端请求名。
func TestChatStreamEchoesClientModel(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	const want = "cn:deepseek-v4.1-flash-x0.11"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+want+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var seen int
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.HasPrefix(ln, "data: [DONE]") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
			t.Fatalf("bad frame %q: %v", ln, err)
		}
		if obj["model"] != want {
			t.Errorf("frame model=%v want %q", obj["model"], want)
		}
		seen++
	}
	if seen == 0 {
		t.Fatal("no data frames parsed from stream")
	}
}

// TestResponsesStreamEchoesClientModel Responses 出口同口径（回归保护：该路径已正确）。
func TestResponsesStreamEchoesClientModel(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	const want = "cn:glm-5.3-x0.79"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/responses",
		strings.NewReader(`{"model":"`+want+`","stream":true,"input":"hi"}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, want) {
		t.Errorf("responses stream should carry client model name %q; body=%s", want, body)
	}
}

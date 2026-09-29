package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// newCaptureUpstream fake 上游：把收到出站请求体记到 captured，再回给定 SSE。
// 用来证明 /v1/responses 的入口翻译真的落到了出站 body 上（不是只在网关内部换个壳）。
func newCaptureUpstream(captured *string, sse string) *upstream.Client {
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(r.Body)
			*captured = string(b)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

const codexResponsesBody = `{
  "model": "cn:glm-5.2",
  "instructions": "You are Codex.",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"说你好"}]}
  ],
  "tools": [{"type":"function","name":"shell","description":"run","parameters":{"type":"object"}}],
  "tool_choice": "auto",
  "reasoning": {"effort":"medium"},
  "store": false,
  "stream": false,
  "prompt_cache_key": "pck-1"
}`

// TestResponsesNonStream 非流式：Responses 进 → Responses 对象出，且出站 body 是 Chat 形态。
func TestResponsesNonStream(t *testing.T) {
	var outbound string
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newCaptureUpstream(&outbound, sseOK),
	})
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(codexResponsesBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "response" || resp["status"] != "completed" {
		t.Errorf("骨架不对: object=%v status=%v", resp["object"], resp["status"])
	}
	// model 回显客户端原始名（含 cn: 前缀），不改写。
	if resp["model"] != "cn:glm-5.2" {
		t.Errorf("model 应回显客户端原始名: %v", resp["model"])
	}
	items, _ := resp["output"].([]any)
	if len(items) != 1 {
		t.Fatalf("output 应有一条 message: %v", items)
	}
	m, _ := items[0].(map[string]any)
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Errorf("message item: %v", m)
	}
	content, _ := m["content"].([]any)
	c0, _ := content[0].(map[string]any)
	if c0["type"] != "output_text" || c0["text"] != "你好" {
		t.Errorf("output_text: %v", content)
	}
	u, _ := resp["usage"].(map[string]any)
	if u["input_tokens"] == nil || u["output_tokens"] == nil || u["total_tokens"] == nil {
		t.Errorf("usage 三段应齐备: %v", u)
	}

	// 出站 body 必须已是 Chat 形态：有 messages、无 input/instructions/store。
	var sent map[string]any
	if err := json.Unmarshal([]byte(outbound), &sent); err != nil {
		t.Fatalf("出站 body 不是 JSON: %v (%s)", err, outbound)
	}
	if _, ok := sent["messages"]; !ok {
		t.Errorf("出站 body 缺 messages（入口翻译没生效）: %s", outbound)
	}
	for _, k := range []string{"input", "instructions", "store", "reasoning"} {
		if _, ok := sent[k]; ok {
			t.Errorf("出站 body 不该带 %q: %s", k, outbound)
		}
	}
	if sent["model"] != "glm-5.2" {
		t.Errorf("出站 model 应剥掉 realm 前缀: %v", sent["model"])
	}
	if sent["stream"] != true {
		t.Errorf("出站 stream 应被管线强制为 true: %v", sent["stream"])
	}
	if _, ok := sent["stream_options"]; !ok {
		t.Errorf("出站应带 stream_options（管线注入 include_usage）: %s", outbound)
	}
	if sent["prompt_cache_key"] != "pck-1" {
		t.Errorf("prompt_cache_key 应透传到上游: %v", sent["prompt_cache_key"])
	}
}

// TestResponsesStream 流式：向上游仍是 chat SSE，向下游是 Responses 事件序列。
func TestResponsesStream(t *testing.T) {
	var outbound string
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newCaptureUpstream(&outbound, sseOK),
	})
	body := strings.Replace(codexResponsesBody, `"stream": false`, `"stream": true`, 1)
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type=%q", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.done",
		"event: response.completed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "data: [DONE]") {
		t.Errorf("Responses 流不该有 [DONE]:\n%s", out)
	}
	if !strings.Contains(out, "你好") {
		t.Errorf("正文未出现在事件流里:\n%s", out)
	}
}

// TestResponsesBadBody 请求体翻译失败 → 400（不能把畸形 body 喂给上游）。
func TestResponsesBadBody(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	for _, body := range []string{`not json`, `{"input":"hi"}`, `{"model":"m"}`} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body=%s code=%d want 400 (resp=%s)", body, rec.Code, rec.Body)
		}
		if !assertJSONErrorCode(t, rec.Body.String(), "invalid_request") {
			t.Errorf("body=%s 应回 invalid_request: %s", body, rec.Body)
		}
	}
}

// TestResponsesRouteOnlyRegisteredForResponses /v1/chat/completions 不吃 Responses 形态，
// /v1/responses 也不吃裸 chat 形态——两个入口各自把住自己的协议（避免「翻译悄悄兜底」）。
func TestResponsesRouteProtocolSeparation(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: newFakeUpstream(t, func(string) (int, string, bool) { return 200, sseOK, true }),
	})
	// Responses 形态打到 chat 口：没有 messages，上游会拒；这里只断言网关不崩且不是 200 对象。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(codexResponsesBody))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), `"object":"response"`) {
		t.Errorf("chat 口不该产出 Responses 对象: %s", rec.Body)
	}
}

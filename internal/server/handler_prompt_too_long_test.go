package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// handler_prompt_too_long_test.go 11115 prompt too long 端到端验收（任务书
// ptl-passthrough-restore：只恢复透传语义，无出站预估——用户裁定移除预估拦截）：
// 上游真实 11115 → 分类不罚号不轮转、透传原文（非固定文案）；空 body → 兜底短文案。

// TestChatPromptTooLongPassesThroughUpstreamBody 上游 400 + 11115（含真实 token
// 数/上限值/requestId 原文）→ 不轮转（多账号池 calls=1，换号同样超限白扔配额）、
// 不罚号（无冷却/禁用/熔断计数）、error.message 逐字透传上游原文（上游原文是
// 最有价值的错误信息，客户端必须看到，禁止固定词覆盖）。
func TestChatPromptTooLongPassesThroughUpstreamBody(t *testing.T) {
	const raw = `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"req-11115-abc"}`
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return 400, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "prompt_too_long" {
		t.Errorf("code=%q want prompt_too_long", e.Error.Code)
	}
	// message 必须逐字等于上游原文（透传，非固定文案）。
	if e.Error.Message != raw {
		t.Errorf("message=%q want raw upstream body passthrough %q", e.Error.Message, raw)
	}
	if !strings.Contains(e.Error.Message, "120000 tokens > 65536 maximum") || !strings.Contains(e.Error.Message, "req-11115-abc") {
		t.Errorf("message must preserve real token count/limit/requestId: %s", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, "all accounts are temporarily unavailable") {
		t.Errorf("message must NOT be the fixed local scheduling text: %s", e.Error.Message)
	}
	// 不轮转：多账号池也只打第一个号（换号同样超限，白扔配额）。
	if calls["Bearer at1"]+calls["Bearer at2"] != 1 {
		t.Errorf("upstream calls=%v want exactly 1 (no rotation on 11115)", calls)
	}
	// 不罚号：a1 无冷却/无禁用/无熔断/无连败计数。
	st, _ := p.Status("a1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("11115 must not penalize account (request-level error): %+v", st)
	}
}

// TestChatImageInvalidPassesThroughWithoutRotation 上游图片错误是请求级确定性
// 错误：多账号池只调用一次，不冷却/禁用/累计失败，并逐字透传上游原文。
func TestChatImageInvalidPassesThroughWithoutRotation(t *testing.T) {
	const raw = `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2: json: cannot unmarshal string into Go value of type v2.ImageContent","requestId":"req-image-abc"}`
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		return http.StatusBadRequest, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "img-a1", AccessToken: "at-img-1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "img-a2", AccessToken: "at-img-2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"cn:deepseek-v4.1-flash","messages":[{"role":"user","content":[{"type":"image_url","image_url":"data:image/png;base64,QUJD"}]}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%s want 400", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "image_invalid" {
		t.Errorf("code=%q want image_invalid", e.Error.Code)
	}
	if e.Error.Message != raw {
		t.Errorf("message=%q want raw upstream body passthrough %q", e.Error.Message, raw)
	}
	if calls["Bearer at-img-1"]+calls["Bearer at-img-2"] != 1 {
		t.Errorf("upstream calls=%v want exactly 1 (no rotation for invalid image)", calls)
	}
	for _, uid := range []string{"img-a1", "img-a2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
			t.Errorf("%s must not be penalized for request-level image error: %+v", uid, st)
		}
	}
}

// TestChatPromptTooLongSingleAccount 413/404 状态码上的 11115 同样走透传分支
// （promptTooLongRule 认请求级 4xx 家族）。
func TestChatPromptTooLongSingleAccount(t *testing.T) {
	for _, status := range []int{400, 404, 413} {
		raw := `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			return status, raw, false
		})
		h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("status=%d: resp not json: %v body=%s", status, err, rec.Body)
		}
		if e.Error.Code != "prompt_too_long" {
			t.Errorf("status=%d: code=%q want prompt_too_long", status, e.Error.Code)
		}
		if e.Error.Message != raw {
			t.Errorf("status=%d: message=%q want passthrough %q", status, e.Error.Message, raw)
		}
	}
}

// TestPromptTooLongMessageFallback 空 body 兜底（单元级）：分类要求 body 命中
// 11115 marker，真走到透传分支时 body 必非空——兜底是防御分支（未来分类口径
// 变化等），无上游原文可透传时保留可读分类短文案（不编造原文、不用本地调度
// 固定词覆盖）；非空 body 原样返回。
func TestPromptTooLongMessageFallback(t *testing.T) {
	const raw = `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`
	for _, tc := range []struct {
		name, in, want string
	}{
		{"raw passthrough", raw, raw},
		{"empty", "", "prompt is too long"},
		{"whitespace only", " \n\t ", "prompt is too long"},
	} {
		if got := promptTooLongMessage(tc.in); got != tc.want {
			t.Errorf("%s: promptTooLongMessage(%q)=%q want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

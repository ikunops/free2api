package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// TestRateSuffixShapes 后缀口径：credit 档按模型自身倍率，零倍率当免费，无倍率不加；
// free 档一律 -free，off 档不加。
func TestRateSuffixShapes(t *testing.T) {
	cases := []struct{ hint, credits, want string }{
		{RateCredit, "x0.11", "-x0.11"},
		{RateCredit, "x0.11 credits", "-x0.11"},
		{RateCredit, "0.29", "-x0.29"},
		{RateCredit, "x0.00", "-free"},
		{RateCredit, "x0", "-free"},
		{RateCredit, "", ""},
		{RateCredit, "   ", ""},
		{RateCredit, "auto", ""}, // 非数字倍率原文不猜
		{RateFree, "x0.11", "-free"},
		{RateFree, "", "-free"},
		{RateOff, "x0.11", ""},
		{RateOff, "", ""},
	}
	for _, c := range cases {
		if got := RateSuffix(c.hint, c.credits); got != c.want {
			t.Errorf("RateSuffix(%q,%q)=%q want %q", c.hint, c.credits, got, c.want)
		}
	}
}

// TestStripRateSuffix 只认我们加上的两种形状；真实模型名尾巴（-x、-flashx 等）不误剥。
func TestStripRateSuffix(t *testing.T) {
	cases := []struct{ in, want string }{
		{"glm-4.6v-x0.11", "glm-4.6v"},
		{"glm-4.6v-free", "glm-4.6v"},
		{"glm-4.6v", "glm-4.6v"},
		{"hy3-x", "hy3-x"},                   // 结尾 -x 后无数字：不剥
		{"glm-5.3-flashx", "glm-5.3-flashx"}, // 结尾是 x 不是 -x：不剥
		{"deepseek-v3-1-volc", "deepseek-v3-1-volc"},
		{"-free", "-free"}, // 剥完是空串 → 保护，不剥
		{"x0.11", "x0.11"},
		{"", ""},
	}
	for _, c := range cases {
		if got := StripRateSuffix(c.in); got != c.want {
			t.Errorf("StripRateSuffix(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestRateSuffixRoundTrip 后缀端到端：/v1/models 的 id 带费率后缀；客户端把带后缀的名字
// 回传时必须剥掉再路由（出站 body 拿裸名），旧配置里的裸名也仍然能调。
func TestRateSuffixRoundTrip(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	var mu sync.Mutex
	var outboundChat []string
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reply := func(ct, body string) (*http.Response, error) {
				return &http.Response{StatusCode: 200,
					Header: http.Header{"Content-Type": []string{ct}},
					Body:   io.NopCloser(strings.NewReader(body))}, nil
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/v3/config"),
				strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"):
				return reply("application/json", fullFieldsModelsBody)
			}
			if r.Body != nil {
				if raw, err := io.ReadAll(r.Body); err == nil && strings.Contains(string(raw), "messages") {
					mu.Lock()
					outboundChat = append(outboundChat, string(raw))
					mu.Unlock()
				}
			}
			return reply("text/event-stream", sseOK)
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false, Output: NewOutputStore("")})

	// 目录里 hy3 的 credits=x0.05 → 对外 id 必须带 -x0.05。
	var published string
	for _, m := range h.modelList() {
		if id, _ := m["id"].(string); strings.HasPrefix(id, "cn:hy3") {
			published = id
		}
	}
	if published != "cn:hy3-x0.05" {
		t.Fatalf("published id = %q, want cn:hy3-x0.05", published)
	}

	// 带后缀与裸名都要能调；出站 body 一律是裸名 hy3。
	for _, name := range []string{"cn:hy3-x0.05", "cn:hy3"} {
		mu.Lock()
		before := len(outboundChat)
		mu.Unlock()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+name+`","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
		if rec.Code != 200 {
			t.Fatalf("model=%q code=%d body=%s want 200", name, rec.Code, rec.Body)
		}
		mu.Lock()
		sent := outboundChat[before:]
		mu.Unlock()
		if len(sent) == 0 {
			t.Fatalf("model=%q 没有出站 chat 请求", name)
		}
		if !strings.Contains(sent[len(sent)-1], `"model":"hy3"`) {
			t.Errorf("model=%q 出站 body 未剥成裸名: %s", name, sent[len(sent)-1])
		}
	}
}

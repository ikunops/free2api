// handler_noqueue_test.go 锚定「池里真没可服务号时不排队」：SlotWait 很大也应立即 503，
// 不做无意义的等待（排队只用于「有健康号但名额占满」的形态）。
package server

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

func TestChatNoQueueWhenNoHealthyAccount(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.Disable("u1", "test-disabled") // 禁用号不参与兜底，池里确无可服务对象
	h := NewHandler(Config{
		Pool:     p,
		Upstream: newFakeUpstream(t, func(authz string) (int, string, bool) { return 200, sseOK, true }),
		SlotWait: 30 * time.Second,
	})
	rec := httptest.NewRecorder()
	begin := time.Now()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if elapsed := time.Since(begin); elapsed > 2*time.Second {
		t.Errorf("no servable account: should return fast, took %v", elapsed)
	}
}

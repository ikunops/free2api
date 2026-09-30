// handler_slotwait_test.go 锚定单账号池在途占满时的排队背压：并发突发不再成片
// 503，而是排队等名额释放（同时对上游维持 max_in_flight 的并发压制）。
package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestChatSlotWaitQueuesInsteadOf503 单账号 + max_in_flight=2 + SlotWait 开启：
// 6 个并发请求全部应成功（此前第 3 个起立即 503）。
func TestChatSlotWaitQueuesInsteadOf503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		time.Sleep(80 * time.Millisecond) // 拉长在途占用，制造名额竞争
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(2)
	h := NewHandler(Config{
		Pool:     p,
		Upstream: up,
		SlotWait: 10 * time.Second, // 足够等到全部完成
	})

	const n = 6
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != 200 {
			t.Errorf("request %d code=%d want 200 (queued, not rejected)", i, c)
		}
	}
}

// TestChatSlotWaitDisabled503WhenFull SlotWait=0（旧行为）：第 3 个并发立即 503。
func TestChatSlotWaitDisabled503WhenFull(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		time.Sleep(150 * time.Millisecond)
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	h := NewHandler(Config{Pool: p, Upstream: up, SlotWait: -1}) // 负值 = 显式关闭

	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]int, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
			results[i] = rec.Code
		}(i)
	}
	close(start)
	wg.Wait()

	okCount := 0
	for _, c := range results {
		if c == 200 {
			okCount++
		}
	}
	if okCount != 1 {
		t.Fatalf("only the slot holder should succeed when SlotWait disabled, ok=%d results=%v", okCount, results)
	}
}

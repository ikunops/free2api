// stats_stream_test.go GET /v1/stats/stream 的契约测试。
//
// 用 httptest.NewServer + 真实 http 客户端读流，而不是直接调 handler：
// SSE 是长连接，直接调会卡在 for-select 里，只有真 server 才能测到
// 「连上就推」「有观测就推」「断开就收尾」这三件事。
//
// 覆盖：
//  1. 首帧立即推全量（连上就有数据，不等下一次请求落地）。
//  2. 新观测落地会推新帧 —— 这是它相对轮询的全部价值。
//  3. 载荷与 GET /v1/stats 同口径。
//  4. api_key：EventSource 不能自定义头，只能走查询串，所以这条路由要
//     放行 ?api_key=；但口子只对这条路由开，别的一律 401。
//  5. 客户端断开后 handler 自行返回。
package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

// sseFrame 一条 SSE 事件。
type sseFrame struct {
	event string
	data  MetricsSnapshot
}

// nextSSEFrame 阻塞读一帧 data；超时返回 ok=false。
// 注意：整帧必须在一个 goroutine 里连续读，不能中途放弃——SSE 是字节流，
// 读一半就 return 会把下一帧的开头吃掉。
func nextSSEFrame(t *testing.T, br *bufio.Reader, wait time.Duration) (sseFrame, bool) {
	t.Helper()
	type res struct {
		f  sseFrame
		ok bool
	}
	ch := make(chan res, 1)
	go func() {
		ev := ""
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				ch <- res{}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(line, "event:"):
				ev = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				var snap MetricsSnapshot
				if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &snap); err != nil {
					t.Errorf("data 不是合法 JSON: %v (%s)", err, line)
					ch <- res{}
					return
				}
				ch <- res{sseFrame{event: ev, data: snap}, true}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.f, r.ok
	case <-time.After(wait):
		return sseFrame{}, false
	}
}

// openSSE 起一个真 server，连上 stream，返回已读好的首帧与 reader。
func openSSE(t *testing.T, h *Handler, query string) (*httptest.Server, *bufio.Reader, sseFrame) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, err := http.NewRequest("GET", srv.URL+"/v1/stats/stream"+query, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	ctx, cancel := context.WithCancel(req.Context())
	t.Cleanup(cancel)
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("code=%d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type=%q want text/event-stream", ct)
	}
	if v := resp.Header.Get("X-Accel-Buffering"); v != "no" {
		t.Errorf("X-Accel-Buffering=%q want no（否则反代缓冲会把它退化回轮询）", v)
	}
	br := bufio.NewReader(resp.Body)
	first, ok := nextSSEFrame(t, br, 3*time.Second)
	if !ok {
		t.Fatal("连上后应立刻收到首帧（首帧 = 全量快照）")
	}
	if first.event != "stats" {
		t.Fatalf("event=%q want stats", first.event)
	}
	return srv, br, first
}

// TestStatsStreamFirstFrameIsFullSnapshot 首帧就是当前全量，不必等新请求。
func TestStatsStreamFirstFrameIsFullSnapshot(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)

	_, _, first := openSSE(t, h, "?range=7d")
	if first.data.Total.Requests != 1 {
		t.Errorf("首帧 requests=%d want 1", first.data.Total.Requests)
	}
	if len(first.data.Models) != 1 || first.data.Models[0].Model != "cn:hy3" {
		t.Errorf("首帧应带全量模型行，得到 %+v", first.data.Models)
	}
}

// TestStatsStreamPayloadMatchesOneShot 推送帧与 GET /v1/stats 同口径。
func TestStatsStreamPayloadMatchesOneShot(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)

	_, _, first := openSSE(t, h, "?range=7d")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/stats?range=7d", nil))
	var direct MetricsSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &direct); err != nil {
		t.Fatalf("decode /v1/stats: %v", err)
	}
	if direct.Total.Requests != first.data.Total.Requests ||
		direct.Total.CompletionTokens != first.data.Total.CompletionTokens {
		t.Errorf("推送帧 %+v 与 /v1/stats %+v 口径不一致",
			first.data.Total, direct.Total)
	}
}

// TestStatsStreamPushesOnNewObservation 新观测落地立刻推新帧。
func TestStatsStreamPushesOnNewObservation(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})

	_, br, first := openSSE(t, h, "?range=today")
	if first.data.Total.Requests != 0 {
		t.Fatalf("前置：首帧应为 0 请求，得到 %d", first.data.Total.Requests)
	}

	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)

	second, ok := nextSSEFrame(t, br, 1500*time.Millisecond)
	if !ok {
		t.Fatal("新观测落地后 1.5s 内没收到推送帧 —— 这正是 SSE 的核心契约")
	}
	if second.data.Total.Requests != 1 {
		t.Errorf("第二帧 requests=%d want 1", second.data.Total.Requests)
	}
}

// TestStatsStreamCoalescesBurst 突发请求不会把帧数放大成请求数。
//
// 信号量容量 1 的意义就在这儿：连着落 20 条观测，下游不会被唤醒 20 次。
// 注意断言不是「只推一帧」——SSE 是最终一致的：写第 11 条时 handler 完全
// 可能已经醒来、快照到 11 就推出去，这是正确行为（早推早可见）。
// 真正要保证的两条是：
//  1. 帧数被显著压住（远小于 20），即合并生效；
//  2. 最后一帧收敛到 20，即没有观测被信号合并丢掉。
func TestStatsStreamCoalescesBurst(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})

	_, br, _ := openSSE(t, h, "?range=today")
	for i := 0; i < 20; i++ {
		recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)
	}

	frames := 0
	last := int64(-1)
	// 静默窗口内持续读，读到没数据为止。
	for {
		f, ok := nextSSEFrame(t, br, 400*time.Millisecond)
		if !ok {
			break
		}
		frames++
		last = f.data.Total.Requests
		if last == 20 {
			// 已收敛，剩下的都是重复帧，不必再等。
			break
		}
	}
	if last != 20 {
		t.Fatalf("最后一帧 requests=%d want 20（有观测被合并丢了）", last)
	}
	if frames > 10 {
		t.Errorf("20 条突发产生 %d 帧，合并没生效（应远少于 20）", frames)
	}
	t.Logf("20 条突发 → %d 帧（合并生效）", frames)
}

// TestStatsStreamRangeIsHonored range 参数真的分流（today 看不到昨天）。
func TestStatsStreamRangeIsHonored(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})

	// 直接往 days 桶里塞一条昨天 / 一条今天。
	yKey := time.Now().AddDate(0, 0, -1).Format(dayKeyFmt)
	todayKey := time.Now().Format(dayKeyFmt)
	globalMetrics.mu.Lock()
	for _, k := range []string{yKey, todayKey} {
		b := globalMetrics.days[k]
		if b == nil {
			b = newDayBucket()
			globalMetrics.days[k] = b
		}
		b.ByModel["cn:hy3"] = &modelMetrics{Requests: 1}
	}
	globalMetrics.mu.Unlock()

	_, _, sevenDay := openSSE(t, h, "?range=7d")
	if sevenDay.data.Total.Requests != 2 {
		t.Errorf("7d 应含昨天+今天 = 2，得到 %d", sevenDay.data.Total.Requests)
	}
	_, _, today := openSSE(t, h, "?range=today")
	if today.data.Total.Requests != 1 {
		t.Errorf("today 只该含今天 = 1，得到 %d", today.data.Total.Requests)
	}
}

// TestStatsStreamQueryKeyOnlyOnThisRoute api_key 查询串放行只对这条路由生效。
func TestStatsStreamQueryKeyOnlyOnThisRoute(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"}), APIKey: "secret"})

	// 对照组：别的路由带 ?api_key= 必须仍然 401（别把口子开成全局）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/stats?api_key=secret", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("/v1/stats?api_key=secret code=%d want 401（查询串鉴权不许外扩）", rec.Code)
	}

	// 无 key：SSE 也该 401。
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/stats/stream?range=today", nil))
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("无 key 的 SSE code=%d want 401", rec2.Code)
	}

	// 错 key：401（常量时间比较，别退回 != 短路）。
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest("GET", "/v1/stats/stream?range=today&api_key=wrong", nil))
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("错 key 的 SSE code=%d want 401", rec3.Code)
	}

	// 正确 key：放行。EventSource 只能靠这条路。
	srv := httptest.NewServer(h)
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/v1/stats/stream?range=today&api_key=secret", nil)
	ctx, cancel := context.WithTimeout(req.Context(), 3*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("正确 api_key 的 SSE code=%d want 200（EventSource 需要这条通路）", resp.StatusCode)
	}
	if _, ok := nextSSEFrame(t, bufio.NewReader(resp.Body), 2*time.Second); !ok {
		t.Error("放行后应收得到首帧")
	}
}

// TestStatsStreamResetAlsoPushes 手动清统计也要推一帧（否则页面数字停在旧值）。
func TestStatsStreamResetAlsoPushes(t *testing.T) {
	resetMetricsForTest(t)
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1"})})
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)

	_, br, first := openSSE(t, h, "?range=today")
	if first.data.Total.Requests != 1 {
		t.Fatalf("前置：首帧应有 1 条，得到 %d", first.data.Total.Requests)
	}

	ResetMetrics()
	f, ok := nextSSEFrame(t, br, 2*time.Second)
	if !ok {
		t.Fatal("ResetMetrics 后应推一帧（否则页面数字停在旧值）")
	}
	if f.data.Total.Requests != 0 {
		t.Errorf("重置帧 requests=%d want 0", f.data.Total.Requests)
	}
}

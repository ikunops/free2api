// live_window_test.go 短窗口速率的契约测试。
package server

import (
	"testing"
	"time"

	"free2api/internal/auth"
)

// resetLiveForTest 把全局 store 的窗口清空（复用 metrics 的 reset 钩子）。
func resetLiveForTest(t *testing.T) {
	t.Helper()
	ResetMetrics()
	t.Cleanup(ResetMetrics)
}

func TestLiveWindowEmptyIsZero(t *testing.T) {
	resetLiveForTest(t)
	r := globalMetrics.live.snapshot(time.Now())
	if r.Requests != 0 || r.Samples != 0 || r.TokensPerSec != 0 {
		t.Fatalf("空窗口应全零，得到 %+v", r)
	}
	if r.WindowSec != liveWindowSec {
		t.Errorf("window_sec=%d want %d", r.WindowSec, liveWindowSec)
	}
}

// TestLiveWindowCountsRecent 窗口内的观测被计入。
func TestLiveWindowCountsRecent(t *testing.T) {
	resetLiveForTest(t)
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "stream", status: 200, toks: 100, hasUsage: true}, 2*time.Second)
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 500, toks: 50, hasUsage: true}, time.Second)

	r := globalMetrics.live.snapshot(time.Now())
	if r.Requests != 2 || r.Samples != 2 {
		t.Fatalf("requests=%d samples=%d want 2/2", r.Requests, r.Samples)
	}
	if r.Success != 1 || r.Failed != 1 {
		t.Errorf("success=%d failed=%d want 1/1", r.Success, r.Failed)
	}
	if r.SuccessRate != 0.5 {
		t.Errorf("success_rate=%v want 0.5", r.SuccessRate)
	}
	if r.CompTok != 150 {
		t.Errorf("completion=%d want 150", r.CompTok)
	}
}

// TestLiveWindowExpiresOld 窗口外的观测被淘汰——这是它能当「最近 60 秒」的原因。
func TestLiveWindowExpiresOld(t *testing.T) {
	resetLiveForTest(t)
	// start 是 90s 前：chatStat.start 决定落点时间。
	recordChatMetric(&chatStat{
		model: "cn:hy3", mode: "sync", status: 200, toks: 100, hasUsage: true,
		start: time.Now().Add(-90 * time.Second),
	}, time.Second)

	r := globalMetrics.live.snapshot(time.Now())
	if r.Requests != 0 {
		t.Fatalf("90s 前的观测不该还在最近 60s 窗口里，得到 requests=%d", r.Requests)
	}
}

// TestLiveWindowKeepsRecent 窗口内的旧观测仍在。
func TestLiveWindowKeepsRecent(t *testing.T) {
	resetLiveForTest(t)
	recordChatMetric(&chatStat{
		model: "cn:hy3", mode: "sync", status: 200, toks: 100, hasUsage: true,
		start: time.Now().Add(-30 * time.Second),
	}, time.Second)
	r := globalMetrics.live.snapshot(time.Now())
	if r.Requests != 1 {
		t.Fatalf("30s 前的观测应仍在窗口里，得到 requests=%d", r.Requests)
	}
}

// TestLiveWindowBounded 内存有界：超过 liveWindowMax 只留最近的。
func TestLiveWindowBounded(t *testing.T) {
	resetLiveForTest(t)
	for i := 0; i < liveWindowMax+40; i++ {
		recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 1, hasUsage: true}, time.Second)
	}
	if n := len(globalMetrics.live.pts); n > liveWindowMax {
		t.Errorf("窗口点数=%d 超过上限 %d（内存泄漏）", n, liveWindowMax)
	}
	r := globalMetrics.live.snapshot(time.Now())
	if r.Requests != int64(liveWindowMax) {
		t.Errorf("requests=%d want %d（应保留满窗口）", r.Requests, liveWindowMax)
	}
}

// TestLiveTokensPerSec 只用生成阶段时长做分母。
func TestLiveTokensPerSec(t *testing.T) {
	resetLiveForTest(t)
	// 流式 200 token，TTFB 1s、总耗时 5s → 生成阶段约 4s。
	recordChatMetric(&chatStat{
		model: "cn:hy3", mode: "stream", status: 200,
		toks: 200, hasUsage: true,
		ttfb: time.Second,
	}, 5*time.Second)

	r := globalMetrics.live.snapshot(time.Now())
	// 200 / 4 = 50 tok/s。允许一点点浮点误差。
	if r.TokensPerSec < 45 || r.TokensPerSec > 55 {
		t.Errorf("tokens_per_sec=%v want ≈50（200 token / 4s 生成阶段）", r.TokensPerSec)
	}
}

// TestLiveIndependentOfRange live 不随区间变：today 与 7d 的 live 应当一致。
// 这是设计约束——它是「现在多快」，不是「这个区间多快」。
func TestLiveIndependentOfRange(t *testing.T) {
	resetLiveForTest(t)
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)

	today := metricsSnapshotRange(parseRangeSpec("today"))
	seven := metricsSnapshotRange(parseRangeSpec("7d"))
	if today.Live.Requests != seven.Live.Requests {
		t.Errorf("live 随区间变了：today=%d 7d=%d", today.Live.Requests, seven.Live.Requests)
	}
	if today.Live.Requests != 1 {
		t.Errorf("live.requests=%d want 1", today.Live.Requests)
	}
}

// TestLiveResetClears 重置统计也要清窗口，否则重置后 60s 内还显示旧速率。
func TestLiveResetClears(t *testing.T) {
	resetLiveForTest(t)
	recordChatMetric(&chatStat{model: "cn:hy3", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)
	if globalMetrics.live.snapshot(time.Now()).Requests != 1 {
		t.Fatal("前置：应有 1 条")
	}
	ResetMetrics()
	if r := globalMetrics.live.snapshot(time.Now()); r.Requests != 0 {
		t.Errorf("重置后 live.requests=%d want 0", r.Requests)
	}
}

// TestLiveNilWindowSafe 零值 metricsStore 不该 panic。
func TestLiveNilWindowSafe(t *testing.T) {
	var m metricsStore
	if r := m.live.snapshot(time.Now()); r.Requests != 0 {
		t.Errorf("nil 窗口应返回空快照，得到 %+v", r)
	}
}

var _ = auth.Auth{}

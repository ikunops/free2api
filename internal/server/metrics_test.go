package server

import (
	"testing"
	"time"
)

// resetMetricsForTest 隔离用例间的全局聚合状态。
func resetMetricsForTest(t *testing.T) {
	t.Helper()
	ResetMetrics()
	t.Cleanup(ResetMetrics)
}

// TestMetricsAggregatesByModel 同一模型的多次请求累加，派生字段按口径折算。
func TestMetricsAggregatesByModel(t *testing.T) {
	resetMetricsForTest(t)

	// 两次成功 + 一次失败，同一模型。
	mk := func(status int, ttfbMS, toks, prompt, hit, miss, wr int, credit float64, mode string) *chatStat {
		return &chatStat{
			model: "global:deepseek-v4.1-flash", mode: mode, status: status,
			ttfb: time.Duration(ttfbMS) * time.Millisecond,
			toks: toks, hasUsage: true, prompt: prompt,
			cacheHit: hit, cacheMiss: miss, cacheWr: wr,
			credit: credit, hasCredit: true,
		}
	}
	recordChatMetric(mk(200, 1000, 100, 50, 800, 200, 0, 0.02, "stream"), 5*time.Second)
	recordChatMetric(mk(200, 2000, 200, 60, 900, 100, 0, 0.04, "stream"), 7*time.Second)
	recordChatMetric(mk(429, 0, -1, 0, 0, 0, 0, 0, "sync"), 1*time.Second)

	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 {
		t.Fatalf("models=%d want 1", len(snap.Models))
	}
	m := snap.Models[0]
	if m.Requests != 3 || m.Success != 2 || m.Failed != 1 {
		t.Errorf("req/succ/fail = %d/%d/%d want 3/2/1", m.Requests, m.Success, m.Failed)
	}
	if m.Streaming != 2 {
		t.Errorf("streaming=%d want 2", m.Streaming)
	}
	// 端到端均值 = (5000+7000+1000)/3 = 4333.33ms
	if got := m.AvgLatencyMS; got < 4333 || got > 4334 {
		t.Errorf("avg_latency=%.2f want ~4333.33", got)
	}
	// TTFB 只统计有观测的两次：(1000+2000)/2 = 1500ms
	if got := m.AvgTTFBMS; got != 1500 {
		t.Errorf("avg_ttfb=%.2f want 1500", got)
	}
	// token 只累加 hasUsage 的两次
	if m.PromptTokens != 110 || m.CompletionTokens != 300 {
		t.Errorf("prompt/comp = %d/%d want 110/300", m.PromptTokens, m.CompletionTokens)
	}
	// 命中率 = 1700/(1700+300) = 0.85
	if got := m.CacheHitRate; got < 0.8499 || got > 0.8501 {
		t.Errorf("cache_hit_rate=%.4f want 0.85", got)
	}
	if m.Credit != 0.06 {
		t.Errorf("credit=%.4f want 0.06", m.Credit)
	}
}

// TestMetricsMissingUsageNotCountedAsZero usage 缺失时不得把 0 计进 token/缓存。
func TestMetricsMissingUsageNotCountedAsZero(t *testing.T) {
	resetMetricsForTest(t)

	// hasUsage=false（上游没回 usage）：toks=-1 是哨兵，不该被当成 token 累加。
	recordChatMetric(&chatStat{
		model: "m1", mode: "sync", status: 200, toks: -1,
	}, time.Second)
	// hasUsage=true 且显式全 0：合法观测，参与累加（分母不为零才有意义）。
	recordChatMetric(&chatStat{
		model: "m1", mode: "sync", status: 200, toks: 0, hasUsage: true,
	}, time.Second)

	snap := MetricsSnapshotOf()
	m := snap.Models[0]
	if m.Requests != 2 {
		t.Fatalf("requests=%d want 2", m.Requests)
	}
	if m.CompletionTokens != 0 {
		t.Errorf("completion=%d want 0（-1 哨兵不得计入）", m.CompletionTokens)
	}
	if m.CacheHitRate != 0 {
		t.Errorf("cache_hit_rate=%f want 0（无观测时不做除法）", m.CacheHitRate)
	}
}

// TestMetricsTotalIsSumOfModels total 必须等于各模型累加，不另算一份。
func TestMetricsTotalIsSumOfModels(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "a", mode: "sync", status: 200, toks: 10, hasUsage: true, prompt: 5}, time.Second)
	recordChatMetric(&chatStat{model: "b", mode: "sync", status: 500, toks: 20, hasUsage: true, prompt: 7}, time.Second)

	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 2 || snap.Total.Success != 1 || snap.Total.Failed != 1 {
		t.Errorf("total req/succ/fail = %d/%d/%d want 2/1/1",
			snap.Total.Requests, snap.Total.Success, snap.Total.Failed)
	}
	if snap.Total.PromptTokens != 12 || snap.Total.CompletionTokens != 30 {
		t.Errorf("total tokens = %d/%d want 12/30", snap.Total.PromptTokens, snap.Total.CompletionTokens)
	}
}

// TestMetricsResetClears 重置后归零。
func TestMetricsResetClears(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "a", mode: "sync", status: 200, toks: 10, hasUsage: true}, time.Second)
	if MetricsSnapshotOf().Total.Requests != 1 {
		t.Fatal("前置：应有 1 条")
	}
	ResetMetrics()
	snap := MetricsSnapshotOf()
	if snap.Total.Requests != 0 || len(snap.Models) != 0 {
		t.Errorf("重置后 requests=%d models=%d want 0/0", snap.Total.Requests, len(snap.Models))
	}
}

// TestMetricsEmptyModelFallsBack 空模型名归入 "-"，不丢弃观测。
func TestMetricsEmptyModelFallsBack(t *testing.T) {
	resetMetricsForTest(t)

	recordChatMetric(&chatStat{model: "", mode: "sync", status: 200}, time.Second)
	snap := MetricsSnapshotOf()
	if len(snap.Models) != 1 || snap.Models[0].Model != "-" {
		t.Fatalf("空模型名应归入 \"-\"，得到 %+v", snap.Models)
	}
	if snap.Total.Requests != 1 {
		t.Errorf("观测不得因模型名为空而丢弃")
	}
}

// TestMetricsCapBounded 超容量上限时丢弃新键且不 panic。
func TestMetricsCapBounded(t *testing.T) {
	resetMetricsForTest(t)

	for i := 0; i < metricsCap+50; i++ {
		recordChatMetric(&chatStat{model: string(rune('a'+i%26)) + string(rune('0'+i%10)) + string(rune('A'+i/260)), mode: "sync", status: 200}, time.Second)
	}
	snap := MetricsSnapshotOf()
	if len(snap.Models) > metricsCap {
		t.Errorf("models=%d 超过上限 %d", len(snap.Models), metricsCap)
	}
}

package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ─── 区间视图 / 按来源 / 热力图 / 逐日持久化 ────────────────────────────────

// hardResetMetrics 模拟「进程重启」：连 loaded/path 一起复位，好让 InitMetricsPersist
// 再跑一次载入（ResetMetrics 只清数据、保留已载入标记）。
func hardResetMetrics() {
	m := globalMetrics
	m.mu.Lock()
	m.byModel = map[string]*modelMetrics{}
	m.byProducer = map[string]*modelMetrics{}
	m.days = map[string]*dayBucket{}
	m.since = time.Now()
	m.warned = false
	m.path = ""
	m.loaded = false
	m.dirty = false
	m.lastFlush = time.Time{}
	m.mu.Unlock()
}

// statAt 构造一条带明确时间与来源的观测（区间与日桶都按 s.start 归档）。
func statAt(at time.Time, model, producer string, status int) *chatStat {
	return &chatStat{
		start: at, model: model, producer: producer,
		mode: "stream", status: status, toks: 10, hasUsage: true, prompt: 5,
	}
}

// TestMetricsByProducer 按来源维度：同一模型名、两个来源各自独立累加。
func TestMetricsByProducer(t *testing.T) {
	resetMetricsForTest(t)
	now := time.Now()

	recordChatMetric(statAt(now, "cn:glm-4.6", "zcode", 200), time.Second)
	recordChatMetric(statAt(now, "cn:glm-4.6", "zcode", 429), time.Second)
	recordChatMetric(statAt(now, "cn:auto", "workbuddy", 200), time.Second)

	snap := metricsSnapshotRange(parseRangeSpec("all"))
	if len(snap.Producers) != 2 {
		t.Fatalf("producers=%d want 2: %+v", len(snap.Producers), snap.Producers)
	}
	// 请求数降序：zcode 2 条在前。
	if snap.Producers[0].Producer != "zcode" || snap.Producers[0].Requests != 2 ||
		snap.Producers[0].Success != 1 || snap.Producers[0].Failed != 1 {
		t.Errorf("zcode 行 = %+v, want req=2 succ=1 fail=1", snap.Producers[0])
	}
	if snap.Producers[1].Producer != "workbuddy" || snap.Producers[1].Requests != 1 {
		t.Errorf("workbuddy 行 = %+v, want req=1", snap.Producers[1])
	}
	// 来源维度不能影响 total：3 条全算。
	if snap.Total.Requests != 3 {
		t.Errorf("total=%d want 3", snap.Total.Requests)
	}
}

// TestMetricsEmptyProducerFoldsToWorkbuddy 空来源归一为 workbuddy（与路由层同口径）。
func TestMetricsEmptyProducerFoldsToWorkbuddy(t *testing.T) {
	resetMetricsForTest(t)
	recordChatMetric(statAt(time.Now(), "cn:auto", "", 200), time.Second)

	snap := metricsSnapshotRange(parseRangeSpec("all"))
	if len(snap.Producers) != 1 || snap.Producers[0].Producer != "workbuddy" {
		t.Fatalf("producers=%+v, want 单行 workbuddy", snap.Producers)
	}
}

// TestMetricsRangeWindows 区间窗口：今天 / 近 7 天 / 近 30 天各只收窗口内的日桶。
func TestMetricsRangeWindows(t *testing.T) {
	resetMetricsForTest(t)
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 10, 0, 0, 0, now.Location())

	recordChatMetric(statAt(today, "m", "workbuddy", 200), time.Second)                          // 今天
	recordChatMetric(statAt(today.AddDate(0, 0, -3), "m", "workbuddy", 200), time.Second)        // 4 天前
	recordChatMetric(statAt(today.AddDate(0, 0, -10), "m", "workbuddy", 200), time.Second)       // 11 天前
	recordChatMetric(statAt(today.AddDate(0, 0, -40), "m", "workbuddy", 200), time.Second)       // 41 天前

	cases := []struct {
		rng  string
		want int64
	}{
		{"today", 1},
		{"7d", 2},
		{"30d", 3},
		{"all", 4},
	}
	for _, c := range cases {
		snap := metricsSnapshotRange(parseRangeSpec(c.rng))
		if snap.Total.Requests != c.want {
			t.Errorf("range=%s requests=%d want %d", c.rng, snap.Total.Requests, c.want)
		}
		if snap.Range != c.rng {
			t.Errorf("range=%s 回显 Range=%q", c.rng, snap.Range)
		}
	}
}

// TestMetricsUnknownRangeFallsBackToAll 未知取值按「全部」，不给空表。
func TestMetricsUnknownRangeFallsBackToAll(t *testing.T) {
	resetMetricsForTest(t)
	recordChatMetric(statAt(time.Now(), "m", "workbuddy", 200), time.Second)

	snap := metricsSnapshotRange(parseRangeSpec("bogus"))
	if snap.Range != "all" || snap.Total.Requests != 1 {
		t.Errorf("未知 range 应回落 all 且带数据，得到 range=%q req=%d", snap.Range, snap.Total.Requests)
	}
}

// TestMetricsSeriesAndHeatmap 逐日序列与「周几 × 小时」热力图都从日桶派生。
func TestMetricsSeriesAndHeatmap(t *testing.T) {
	resetMetricsForTest(t)
	now := time.Now()
	// 挑一个明确的时刻：今天的 09 点，避免跨日竞态。
	at := time.Date(now.Year(), now.Month(), now.Day(), 9, 30, 0, 0, now.Location())

	recordChatMetric(statAt(at, "m", "workbuddy", 200), time.Second)
	recordChatMetric(statAt(at, "m", "workbuddy", 200), time.Second)

	snap := metricsSnapshotRange(parseRangeSpec("all"))
	if len(snap.Series) != 1 {
		t.Fatalf("series=%d want 1（只今天有数据）", len(snap.Series))
	}
	if p := snap.Series[0]; p.Day != at.Format("2006-01-02") || p.Requests != 2 || p.Tokens != 30 {
		t.Errorf("series[0] = %+v, want day=%s req=2 tokens=30", p, at.Format("2006-01-02"))
	}
	// 热力图对应格（周几, 9 点）= 2；其余全 0。
	wd := int(at.Weekday())
	if got := snap.Heatmap[wd][9]; got != 2 {
		t.Errorf("heatmap[%d][9] = %d want 2", wd, got)
	}
	var sum int64
	for d := 0; d < 7; d++ {
		for h := 0; h < 24; h++ {
			sum += snap.Heatmap[d][h]
		}
	}
	if sum != 2 {
		t.Errorf("heatmap 总和 = %d want 2（不能把同一笔算到多个格子）", sum)
	}
}

// TestMetricsPersistRoundTrip 逐日落盘 + 载入：跨「重启」后区间视图仍在。
func TestMetricsPersistRoundTrip(t *testing.T) {
	resetMetricsForTest(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "stats.json")

	now := time.Now()
	recordChatMetric(statAt(now, "cn:hy3", "zcode", 200), time.Second)
	FlushMetricsTo(path) // 见下方 helper：测试里显式指定路径，不碰全局 path

	// 模拟重启：清内存 → 重新载入。
	hardResetMetrics()
	InitMetricsPersist(path)
	t.Cleanup(func() { hardResetMetrics() })

	snap := metricsSnapshotRange(parseRangeSpec("all"))
	if snap.Total.Requests != 1 {
		t.Fatalf("载入后 requests=%d want 1（逐日历史没落盘/没读回）", snap.Total.Requests)
	}
	if len(snap.Producers) != 1 || snap.Producers[0].Producer != "zcode" {
		t.Errorf("载入后 producers=%+v, want zcode", snap.Producers)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("落盘文件不存在: %v", err)
	}
}

// TestMetricsRetainPrunesOldDays 超出保留期的日桶被裁掉。
func TestMetricsRetainPrunesOldDays(t *testing.T) {
	resetMetricsForTest(t)
	now := time.Now()
	old := now.AddDate(0, 0, -(statsRetainDays + 5))
	recordChatMetric(statAt(old, "m", "workbuddy", 200), time.Second)
	recordChatMetric(statAt(now, "m", "workbuddy", 200), time.Second)

	globalMetrics.mu.Lock()
	n := len(globalMetrics.days)
	globalMetrics.mu.Unlock()
	if n != 1 {
		t.Errorf("days=%d want 1（过期日桶应被裁掉）", n)
	}
	if snap := metricsSnapshotRange(parseRangeSpec("all")); snap.Total.Requests != 1 {
		t.Errorf("all requests=%d want 1", snap.Total.Requests)
	}
}

// TestMetricsResetClearsDays 重置要连逐日历史一起清（否则下次载入把旧数据带回来）。
func TestMetricsResetClearsDays(t *testing.T) {
	resetMetricsForTest(t)
	recordChatMetric(statAt(time.Now(), "m", "workbuddy", 200), time.Second)
	ResetMetrics()

	globalMetrics.mu.Lock()
	n := len(globalMetrics.days)
	globalMetrics.mu.Unlock()
	if n != 0 {
		t.Errorf("重置后 days=%d want 0", n)
	}
	if snap := metricsSnapshotRange(parseRangeSpec("all")); snap.Total.Requests != 0 {
		t.Errorf("重置后 requests=%d want 0", snap.Total.Requests)
	}
}

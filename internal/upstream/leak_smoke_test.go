package upstream

// leak_smoke_test.go 代码健康审计（任务书第 3 条）泄漏维度：N 轮 goroutine 计数
// 冒烟。既有退出路径断言已覆盖 pool flusher（close_test.go）/ idle monitor
// （TestMonitorBodyCloseStopsGoroutine）/ scheduler Run（TestRunAllDisabledNoSpin
// + travel/activity_test 的 Run+done 模式）/ redisstore Close（close_test.go 族）；
// 本文件补「同进程反复构建-销毁 N 轮，goroutine 总数不增长」的进程级回归——
// 单轮泄漏会在 N 轮放大后显形。
import (
	"runtime"
	"testing"
	"time"
)

// waitGoroutines 轮询等待 goroutine 数回落到 baseline 以下（有界，防挂死）。
func waitGoroutines(t *testing.T, baseline int, d time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= baseline
}

// TestModelsDevFetchGoroutineExitsNRound modelsdev 异步拉取 goroutine 退出：
// N 轮「触发 ensureDocAsync → 等拉取完成」循环，goroutine 数不逐轮增长
// （fetchDoc 是短生命周期 goroutine，拉完即退；若泄漏，每轮 +1，N 轮后
// 总数显著超基线）。
func TestModelsDevFetchGoroutineExitsNRound(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	fake := newModelsDevServer(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"leak-probe-model": {2000000, 262144}},
	}))
	defer fake.ts.Close()
	httpc := fake.client()

	// 基线：先做一轮完整拉取（建立 doc 索引），等一切落定后的 goroutine 数。
	if got := ContextWindowListingV4("leak-probe-model", 0, httpc); got != DefaultContextWindow {
		t.Fatalf("first lookup: %d want 1M", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok := modelCatalogGet("leak-probe-model"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	baseline := runtime.NumGoroutine()

	// N 轮再触发：doc 已就绪，ensureDocAsync 直接返回（不再起 goroutine）；
	// 即便走冷却过期重拉路径，fetchDoc 也必须退出。
	const rounds = 20
	for i := 0; i < rounds; i++ {
		ContextWindowListingV4("leak-probe-model", 0, httpc)
		ContextWindowListingV4("another-unknown-"+string(rune('a'+i%26)), 0, httpc)
	}
	if !waitGoroutines(t, baseline, 2*time.Second) {
		t.Errorf("goroutines after %d rounds: baseline=%d now=%d (fetchDoc 泄漏?)",
			rounds, baseline, runtime.NumGoroutine())
	}
}

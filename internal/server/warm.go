package server

// 模型目录预热 / 周期刷新。
//
// 背景：/v1/models 各来源目录都是「首次请求才拉、缓存 1h」（dynamicModelsCache /
// globalModels 探测桶 / zcodeCatalogCache / opencodeCatalogCache / kiloCatalogCache）。
// 带来的两个问题：
//   - 冷启动后第一次拉目录的用户要等一次上游往返；
//   - 上游模型上下架后，最多要等 1h TTL 过期才反映到客户端目录。
//
// WarmCatalog 启动时把所有来源的目录重打一遍；RefreshCatalogLoop 之后按 interval
// 周期重打，自动同步上游上下架。刷新只改缓存内容、不影响可达性——某来源拉取失败时
// 旧名单仍在（各 fetch 的失败分支不清既有数据），客户端目录不会凭空变空。
import (
	"context"
	"log"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"free2api/internal/upstream"
)

// WarmCatalog 把所有已接入来源的模型目录预热一遍（启动时后台调一次）。
func (h *Handler) WarmCatalog() {
	h.refreshCatalogs()
}

// RefreshCatalogLoop 周期刷新模型目录，直到 ctx 结束（启动时 go 出去）。
// interval <= 0 = 关闭周期刷新（仍由 WarmCatalog 做一次启动预热）。
func (h *Handler) RefreshCatalogLoop(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.refreshCatalogs()
		}
	}
}

// refreshCatalogs 逐来源重打目录（绕过各自 TTL 与负缓存）。每来源独立、互不阻塞：
// 某家没接入（producerAllowed=false）或拉取失败都不影响其余来源。
func (h *Handler) refreshCatalogs() {
	if h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return
	}
	// CN workbuddy：清时间戳后走既有 fetch（成功写回，失败保留旧数据）。
	if h.producerAllowed("workbuddy") {
		dynamicModelsCache.Lock()
		dynamicModelsCache.fetched = time.Time{}
		dynamicModelsCache.lastFail = time.Time{}
		dynamicModelsCache.Unlock()
		h.fetchDynamicModels()
	}
	// global（国际版）：仅在开着时刷（与 modelList 分支同口径）。
	if h.cfg.GlobalEnabled && h.producerAllowed("workbuddy") {
		h.cfg.Upstream.InvalidateGlobalModelsCache()
		h.fetchGlobalModels()
	}
	if h.producerAllowed(upstream.ProducerZCode) {
		zcodeCatalogCache.Lock()
		zcodeCatalogCache.fetched = time.Time{}
		zcodeCatalogCache.lastFail = time.Time{}
		zcodeCatalogCache.Unlock()
		h.fetchZCodeCatalog()
	}
	if h.producerAllowed(upstream.ProducerOpenCode) {
		opencodeCatalogCache.Lock()
		opencodeCatalogCache.fetched = time.Time{}
		opencodeCatalogCache.lastFail = time.Time{}
		opencodeCatalogCache.Unlock()
		h.fetchOpenCodeCatalog()
	}
	if h.producerAllowed(upstream.ProducerKilo) {
		kiloCatalogCache.Lock()
		kiloCatalogCache.fetched = time.Time{}
		kiloCatalogCache.lastFail = time.Time{}
		kiloCatalogCache.Unlock()
		h.fetchKiloCatalog()
	}
	log.Printf("INFO: [server] 模型目录已刷新")
}

// ---------- 手动同步 ----------

// catalogCounts 当前各来源目录的模型条数。只读既有缓存（不触发上游请求），
// 所以刷新前后各调一次就能给出「这次同步增减了多少」的诚实差值。
func (h *Handler) catalogCounts() map[string]int {
	m := map[string]int{
		"workbuddy": len(h.fetchDynamicModels()),
	}
	if h.cfg.GlobalEnabled {
		ids, _ := h.fetchGlobalModels()
		m["global"] = len(ids)
	}
	if h.producerAllowed(upstream.ProducerZCode) {
		m[upstream.ProducerZCode] = len(h.fetchZCodeCatalog())
	}
	if h.producerAllowed(upstream.ProducerOpenCode) {
		m[upstream.ProducerOpenCode] = len(h.fetchOpenCodeCatalog())
	}
	if h.producerAllowed(upstream.ProducerKilo) {
		m[upstream.ProducerKilo] = len(h.fetchKiloCatalog())
	}
	return m
}

// catalogRefreshing 手动同步防重入：一次刷新要串行打多家上游，面板上连点会叠出
// 好几轮外网请求。与包级目录缓存同风格（dynamicModelsCache 等也是包级）。
var catalogRefreshing atomic.Bool

// adminCatalogRefresh POST /admin/catalog/refresh —— 立刻重打各来源模型目录。
//
// 存在的理由：目录有 30m 周期刷新，但「上游刚上了新模型」这件事用户是当场就知道的，
// 让他等 TTL 或重启网关才能用，等于把已知能力藏起来。这里给面板一个显式入口。
func (h *Handler) adminCatalogRefresh(w http.ResponseWriter, r *http.Request) {
	if !catalogRefreshing.CompareAndSwap(false, true) {
		writeOpenAIErrorHint(w, http.StatusConflict, "catalog_refresh_in_progress",
			"已有一轮目录同步在跑", "等上一轮结束再试；各来源独立，卡住的只有打不通的那家")
		return
	}
	before := h.catalogCounts()
	h.refreshCatalogs()
	after := h.catalogCounts()
	catalogRefreshing.Store(false)

	deltas := make([]map[string]any, 0, len(after))
	for _, k := range sortedProducerKeys(before, after) {
		if after[k] == before[k] {
			continue
		}
		deltas = append(deltas, map[string]any{"producer": k, "before": before[k], "after": after[k]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"before":  before,
		"after":   after,
		"changed": deltas,
		"note":    "各来源独立刷新：拉取失败的一路保留旧名单，不会把目录清空",
	})
}

// sortedProducerKeys 两家差集的稳定顺序（map 遍历无序，响应里顺序变了会让前端抖动）。
func sortedProducerKeys(before, after map[string]int) []string {
	seen := make(map[string]bool, len(before)+len(after))
	out := make([]string, 0, len(before)+len(after))
	for _, m := range []map[string]int{before, after} {
		for k := range m {
			if !seen[k] {
				seen[k] = true
				out = append(out, k)
			}
		}
	}
	sort.Strings(out)
	return out
}

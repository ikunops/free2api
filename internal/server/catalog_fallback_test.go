package server

import (
	"testing"
	"time"

	"free2api/internal/upstream"
)

// staleTwo 两条假模型，用来验证"旧名单"是否被保住。
func staleTwo() []upstream.ModelInfo {
	return []upstream.ModelInfo{{ID: "m-a"}, {ID: "m-b"}}
}

// TestCatalogFailCooldownKeepsStaleList 目录负缓存期内必须回落到**旧名单**，而不是清空。
//
// 现场：国际线路一次 TLS 抖动（Get opencode.ai/zen/go/v1/models: handshake timeout）后，
// 负缓存命中分支 `return nil`，发布清单从 114 个掉到 58 个——workbuddy/zcode 还在，
// opencode/kilo 整家消失。用户看到的是"模型凭空没了"，而 warm.go 的注释本来就承诺
// "拉取失败时旧名单仍在，客户端目录不会凭空变空"。
//
// 四个缓存（workbuddy CN / zcode / opencode / kilo）同构，这里逐一钉住：
// 有旧数据 → 回旧数据；没有旧数据（冷启动就失败）→ 仍是空，行为不变。
func TestCatalogFailCooldownKeepsStaleList(t *testing.T) {
	h := &Handler{}
	cooldownStarted := time.Now()

	cases := []struct {
		name string
		seed func()
		get  func() int
	}{
		{
			name: "workbuddy CN",
			seed: func() {
				dynamicModelsCache.Lock()
				dynamicModelsCache.ids = staleTwo()
				dynamicModelsCache.fetched = time.Time{}
				dynamicModelsCache.lastFail = cooldownStarted
				dynamicModelsCache.Unlock()
			},
			get: func() int { return len(h.fetchDynamicModels()) },
		},
		{
			name: "zcode",
			seed: func() {
				zcodeCatalogCache.Lock()
				zcodeCatalogCache.infos = staleTwo()
				zcodeCatalogCache.fetched = time.Time{}
				zcodeCatalogCache.lastFail = cooldownStarted
				zcodeCatalogCache.Unlock()
			},
			get: func() int { return len(h.fetchZCodeCatalog()) },
		},
		{
			name: "opencode",
			seed: func() {
				opencodeCatalogCache.Lock()
				opencodeCatalogCache.infos = staleTwo()
				opencodeCatalogCache.fetched = time.Time{}
				opencodeCatalogCache.lastFail = cooldownStarted
				opencodeCatalogCache.Unlock()
			},
			get: func() int { return len(h.fetchOpenCodeCatalog()) },
		},
		{
			name: "kilo",
			seed: func() {
				kiloCatalogCache.Lock()
				kiloCatalogCache.infos = staleTwo()
				kiloCatalogCache.fetched = time.Time{}
				kiloCatalogCache.lastFail = cooldownStarted
				kiloCatalogCache.Unlock()
			},
			get: func() int { return len(h.fetchKiloCatalog()) },
		},
	}

	for _, c := range cases {
		c.seed()
		if got := c.get(); got != 2 {
			t.Errorf("%s：负缓存期内应回落到旧名单（2 条），实得 %d 条——目录被清空了", c.name, got)
		}
	}
}

// TestCatalogFailCooldownColdStartStaysEmpty 负缓存 + 没有任何旧数据（冷启动即失败）
// 时仍返回空：不能凭空编出模型来。
func TestCatalogFailCooldownColdStartStaysEmpty(t *testing.T) {
	h := &Handler{}
	opencodeCatalogCache.Lock()
	opencodeCatalogCache.infos = nil
	opencodeCatalogCache.fetched = time.Time{}
	opencodeCatalogCache.lastFail = time.Now()
	opencodeCatalogCache.Unlock()

	if got := len(h.fetchOpenCodeCatalog()); got != 0 {
		t.Errorf("冷启动失败时不该有任何模型，实得 %d", got)
	}
}

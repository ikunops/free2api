package server

import (
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// ─── handler.modelFree：选号热路径的「零积分模型」判定（只读快照）────────────
//
// 三条要锚定的契约：
//  1. 目录里标了 Free 的模型 → true（免费分支：不按余额加权）；
//  2. 目录里存在但没标 Free → false（按收费处理，退回余额加权）；
//  3. 缓存冷 / 缺失 / 模型不在目录 → false（保守回退，绝不把不确定当免费）。
// 另外两条：producer=="" 时四家目录都查（任一家免费即免费）；该函数**不得**发起上游调用。

// seedFreeCatalog 直接写各目录的只读缓存（不触发任何上游调用）。
func seedFreeCatalog(t *testing.T) {
	t.Helper()
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{
		{ID: "hy3-free", Free: true},
		{ID: "hy3", Credits: "x0.05", Free: false},
	}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()

	zcodeCatalogCache.Lock()
	zcodeCatalogCache.infos = []upstream.ModelInfo{{ID: "glm-5.3-flash", Free: false}}
	zcodeCatalogCache.fetched = time.Now()
	zcodeCatalogCache.Unlock()

	opencodeCatalogCache.Lock()
	opencodeCatalogCache.infos = []upstream.ModelInfo{
		{ID: "space-bunny-free", Free: true},
		{ID: "claude-fable-5", Free: false},
	}
	opencodeCatalogCache.fetched = time.Now()
	opencodeCatalogCache.Unlock()

	kiloCatalogCache.Lock()
	kiloCatalogCache.infos = []upstream.ModelInfo{{ID: "stepfun/step-3.7-flash:free", Free: true}}
	kiloCatalogCache.fetched = time.Now()
	kiloCatalogCache.Unlock()
}

func TestModelFreeReadsCatalogSnapshot(t *testing.T) {
	seedFreeCatalog(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, fullFieldsModelsBody, false })
	h := NewHandler(Config{Pool: p, Upstream: up})

	cases := []struct {
		realm, producer, bare string
		want                  bool
		why                   string
	}{
		{"cn", "workbuddy", "hy3-free", true, "CN 目录标了 Free"},
		{"cn", "workbuddy", "hy3", false, "CN 目录存在但未标 Free（x0.05）"},
		{"cn", "opencode", "space-bunny-free", true, "opencode 免费层命名"},
		{"cn", "opencode", "claude-fable-5", false, "opencode 付费模型"},
		{"cn", "kilo", "stepfun/step-3.7-flash:free", true, "kilo 目录恒免费"},
		{"cn", "zcode", "glm-5.3-flash", false, "zcode 目录未标 Free"},
		{"cn", "workbuddy", "not-in-catalog", false, "目录外模型 → 保守按收费"},
		{"cn", "workbuddy", "", false, "空裸名 → false"},
		{"cn", "", "space-bunny-free", true, "producer 未知：四家都查，命中 opencode 免费层"},
		{"cn", "", "hy3-free", true, "producer 未知：命中 CN 免费模型"},
	}
	for _, c := range cases {
		if got := h.modelFree(c.realm, c.producer, c.bare); got != c.want {
			t.Errorf("modelFree(%q,%q,%q) = %v, want %v（%s）", c.realm, c.producer, c.bare, got, c.want, c.why)
		}
	}
}

// TestAvailableOutputModelsExposesFree 发布清单必须把 Free 透出去，前端「只看免费」才能筛。
// 管理页在 opencode Zen 的整家目录（40 个里 31 个收费）里挑号，不给这个字段就只能靠
// 名字猜哪行免费——那是猜的，不是判定。
func TestAvailableOutputModelsExposesFree(t *testing.T) {
	seedFreeCatalog(t)
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(string) (int, string, bool) { return 200, fullFieldsModelsBody, false })
	h := NewHandler(Config{Pool: p, Upstream: up})

	got := map[string]any{}
	for _, e := range h.availableOutputModels() {
		key, _ := e["key"].(string)
		if key != "" {
			got[key] = e["free"]
		}
	}
	cases := []struct {
		key  string
		want bool
	}{
		{"hy3-free", true},              // CN 目录标了 Free
		{"hy3", false},                  // CN 目录存在但未标 Free
		{"opencode:space-bunny-free", true},
		{"opencode:claude-fable-5", false},
		{"kilo:stepfun/step-3.7-flash:free", true},
		{"zcode:glm-5.3-flash", false},
	}
	for _, c := range cases {
		v, ok := got[c.key]
		if !ok {
			t.Errorf("发布清单缺少条目 %q", c.key)
			continue
		}
		if v != c.want {
			t.Errorf("%q free = %v, want %v", c.key, v, c.want)
		}
	}
}

// TestModelFreeColdCacheIsConservative 缓存冷 → 一律 false（按收费处理），且零上游调用。
func TestModelFreeColdCacheIsConservative(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	calls := 0
	up := newFakeUpstream(t, func(string) (int, string, bool) {
		calls++
		return 200, fullFieldsModelsBody, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true})

	for _, bare := range []string{"hy3-free", "space-bunny-free", "glm-5.3-flash"} {
		if h.modelFree("cn", "", bare) {
			t.Errorf("冷缓存下 modelFree(%q) = true, want false（保守回退）", bare)
		}
	}
	if calls != 0 {
		t.Errorf("modelFree 发起 %d 次上游调用，want 0（只读快照，绝不探测）", calls)
	}
}

// TestModelFreeGlobalsRealm global realm 走 global 目录（GlobalModelInfosSnapshot）。
func TestModelFreeGlobalsRealm(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	cf := newGlobalModelsHandlerFake(t, 200, fullFieldsModelsBody)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
	// 预热 global 目录（一次 modelList 触发探测并落 Client 缓存）。
	found := false
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok && strings.HasPrefix(id, "global:hy3") {
			found = true
		}
	}
	if !found {
		t.Fatalf("预热失败：global 目录没落缓存")
	}

	if h.modelFree("global", "workbuddy", "hy3") {
		t.Errorf("global:hy3 在 fullFieldsModelsBody 里是 x0.05，不应判免费")
	}
	if h.modelFree("cn", "workbuddy", "hy3") {
		t.Errorf("cn 口径下 global 目录不该被读到（realm=cn 查 CN 快照 → false）")
	}
	if h.modelFree("global", "workbuddy", "hy3-free") {
		t.Errorf("global 目录里没有 hy3-free 条目，应 false")
	}
}

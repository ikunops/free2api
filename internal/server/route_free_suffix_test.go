package server

import (
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// TestResolveRouteKeepsNativeFreeSuffix 回归：上游模型名自身以 "-free" 结尾时，入站
// 解析不得把它当费率后缀剥掉。
//
// 背景：resolveModelPrefixed 为「客户端带后缀回传还能路由」会无条件剥尾部 "-free"。
// opencode 免费层一大批模型名自带 "-free"（mimo-v2.6-flash-free / space-bunny-free…），
// 被剥成 "mimo-v2.6-flash" 后上游回 400 Model is unavailable，并被误标池级死模型。
// 修复后按目录裁决：剥完在目录里才认，否则保留原串（见 reconcileRateSuffix）。
func TestResolveRouteKeepsNativeFreeSuffix(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	// 造目录：opencode 侧既有自带 "-free" 的免费层，也有普通付费名；workbuddy 侧放一个
	// 用来验证「客户端自己加的 -free 后缀仍会被剥」。
	opencodeCatalogCache.Lock()
	opencodeCatalogCache.infos = []upstream.ModelInfo{
		{ID: "mimo-v2.6-flash-free"},
		{ID: "space-bunny-free"},
		{ID: "big-pickle"},
		{ID: "glm-5.3-flash"},
		// 冲突对：jev-1.13 与 jev-1.13-free 在目录里同时存在。用户点名 -free 版时
		// 必须路由到 -free，不能因为「剥完的 jev-1.13 也在目录里」而被吞掉。
		{ID: "jev-1.13"},
		{ID: "jev-1.13-free"},
	}
	opencodeCatalogCache.fetched = time.Now()
	opencodeCatalogCache.Unlock()

	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "glm-5.2"}}
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.Unlock()

	// 裸名解析会经过 modelOwnerEx → fetchZCodeCatalog，需要非 nil 的池；池里没有 zcode
	// 账号时该函数早返回 nil，不会打网络。
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Output: NewOutputStore("")})

	cases := []struct {
		in       string
		wantBare string
		why      string
	}{
		{"cn:opencode:mimo-v2.6-flash-free", "mimo-v2.6-flash-free", "模型名自带 -free，不能被剥"},
		{"cn:opencode:space-bunny-free", "space-bunny-free", "模型名自带 -free，不能被剥"},
		{"cn:opencode:big-pickle", "big-pickle", "无后缀，原样"},
		{"cn:glm-5.2-free", "glm-5.2", "客户端加的费率后缀，剥掉后命中 workbuddy 目录"},
		{"cn:opencode:glm-5.3-flash-free", "glm-5.3-flash", "客户端加的费率后缀，剥掉后命中 opencode 目录"},
		{"cn:opencode:jev-1.13-free", "jev-1.13-free", "目录里 jev-1.13-free 与 jev-1.13 并存，必须保留 -free"},
		{"cn:opencode:jev-1.13", "jev-1.13", "非 -free 版本，原样"},
	}
	for _, c := range cases {
		_, _, bare := h.resolveRoute(c.in)
		if bare != c.wantBare {
			t.Errorf("resolveRoute(%q) bare=%q want %q（%s）", c.in, bare, c.wantBare, c.why)
		}
	}
}

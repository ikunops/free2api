package server

import "testing"

// TestPublishAllowsBareIDFallback 老清单里存的是裸 id，而 publishAllows 按
// modelKey（"zcode:<id>"）去找，键口径分叉导致白名单勾了等于没勾——ZCode 专用口
// （7870）吐 0 个模型就是这个。回归守着裸 id 回退。
func TestPublishAllowsBareIDFallback(t *testing.T) {
	h := &Handler{}
	pub := map[string]bool{"glm-5.3-flash": true}

	if !h.publishAllows(pub, "cn", "zcode", "glm-5.3-flash") {
		t.Error("zcode 段老清单（裸 id）应放行")
	}
	if h.publishAllows(pub, "cn", "zcode", "glm-4.6") {
		t.Error("白名单外的模型不该放行")
	}
	// workbuddy 本来就是裸 id，不受影响
	if !h.publishAllows(pub, "cn", "workbuddy", "glm-5.3-flash") {
		t.Error("workbuddy 裸 id 应放行")
	}
	if h.publishAllows(pub, "cn", "", "glm-5.3-flash") != true {
		t.Error("空 producer 视同 workbuddy，裸 id 应放行")
	}
	// 新形态（带 producer 段）优先，不该被回退掩盖
	pub2 := map[string]bool{"zcode:glm-5.3-flash": true, "glm-4.6": true}
	if !h.publishAllows(pub2, "cn", "zcode", "glm-5.3-flash") {
		t.Error("带 producer 段的键应放行")
	}
	if h.publishAllows(pub2, "cn", "zcode", "glm-4.6") {
		t.Error("只勾了别的来源的同名裸 id，不该放行 zcode 段")
	}
	// nil = 全放
	if !h.publishAllows(nil, "cn", "zcode", "任意") {
		t.Error("nil 白名单应全放")
	}
}

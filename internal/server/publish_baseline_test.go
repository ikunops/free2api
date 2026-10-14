package server

import "testing"

// TestPublishAllowsAutoAllowsNewModels 发布清单的「上游新增自动放行」基线机制。
//
// 回归的现场问题：白名单是**静默失效**的——保存过一份清单之后，上游新上的模型既不出现在
// /v1/models、请求又直接 404，而面板上根本看不见它们，用户无从察觉自己那份清单已经把
// 新东西挡在外面了（历史上 global 域 29 个模型全部未勾选就是这么来的）。
//
// 判据三条：
//   - 有基线 + 该模型不在基线里  → 上游新增，放行；
//   - 有基线 + 该模型在基线里但没勾 → 用户明确不要，继续屏蔽；
//   - 没基线（老清单）           → 维持严格白名单，绝不因为升级突然放开。
func TestPublishAllowsAutoAllowsNewModels(t *testing.T) {
	h := &Handler{}

	// 场景：用户勾了 auto 与 fast-model，那时上游还没有 brand-new / zcode:glm-new。
	pub := map[string]bool{"auto": true, "zcode:glm-5.3-flash": true}
	known := map[string]bool{
		"auto": true, "deep-model": true, "zcode:glm-5.3-flash": true, "zcode:glm-4.6": true,
	}

	// ① 不勾也不在基线里 = 上游新增 → 放行
	if !h.publishAllows(pub, known, "cn", "workbuddy", "brand-new") {
		t.Error("基线外的 workbuddy 新型号应自动放行")
	}
	if !h.publishAllows(pub, known, "cn", "zcode", "brand-new") {
		t.Error("基线外的 zcode 新型号应自动放行")
	}
	if !h.publishAllows(pub, known, "global", "workbuddy", "gpt-new") {
		t.Error("基线外的 global 新型号应自动放行")
	}

	// ② 在基线里但没勾 = 用户明确不要 → 继续屏蔽（这是基线的全部意义）
	if h.publishAllows(pub, known, "cn", "workbuddy", "deep-model") {
		t.Error("基线内且未勾选的模型不该放行——那等于把用户的选择还给上游")
	}
	if h.publishAllows(pub, known, "cn", "zcode", "glm-4.6") {
		t.Error("基线内且未勾选的 zcode 模型不该放行")
	}

	// ③ 老清单（无基线）严格判定：不在白名单一律不放
	if h.publishAllows(pub, nil, "cn", "workbuddy", "brand-new") {
		t.Error("老清单没有基线时必须维持严格白名单，不能因为升级就放开")
	}
	if h.publishAllows(pub, map[string]bool{}, "cn", "workbuddy", "brand-new") {
		t.Error("空基线等同无基线（老清单），必须严格")
	}

	// ④ 全放不受影响
	if !h.publishAllows(nil, known, "cn", "workbuddy", "任意") {
		t.Error("nil 白名单 = 全放")
	}
}

// TestPublishKeyForms 基线的键形态必须与面板保存时的 key 一致，否则自动放行会全错：
// CN 用 modelKey（workbuddy 是裸 id、其余带 producer 段），国际版用 "global:<裸 id>"。
func TestPublishKeyForms(t *testing.T) {
	cases := []struct{ realm, producer, id, want string }{
		{"cn", "workbuddy", "auto", "auto"},
		{"cn", "", "auto", "auto"},
		{"cn", "zcode", "glm-5.3-flash", "zcode:glm-5.3-flash"},
		{"cn", "kilo", "k", "kilo:k"},
		{"global", "workbuddy", "fast-model", "global:fast-model"},
	}
	for _, c := range cases {
		if got := publishKey(c.realm, c.producer, c.id); got != c.want {
			t.Errorf("publishKey(%q,%q,%q) = %q, want %q", c.realm, c.producer, c.id, got, c.want)
		}
	}
}

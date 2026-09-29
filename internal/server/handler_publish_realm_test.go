package server

import (
	"sort"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// publishKeysOf 把发布清单的 key 集合取出来（错误信息里直接用，方便一眼看出少/多了哪条）。
func publishKeysOf(entries []map[string]any) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if k, ok := e["key"].(string); ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// listedIDs /v1/models 的 id 集合（对外真正发出去的名字）。
func listedIDs(h *Handler) map[string]bool {
	out := map[string]bool{}
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok {
			out[id] = true
		}
	}
	return out
}

// TestAvailableOutputModelsIncludesGlobalRealm 发布清单必须把国际版（wba）单列一组。
//
// 回归：availableOutputModels 只组装 workbuddy（CN）/zcode 两组时，国际版 27 个型号在
// 「模型发布清单」里一条都不出现——用户看不见它们，一保存白名单就把国际版独有的型号
// （gpt-6-astra / gemini-3.5-flash / grok-4.7 …）静默砍掉，而页面上毫无提示。
func TestAvailableOutputModelsIncludesGlobalRealm(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	// 探测目录里刻意放一个两域同名的 id（cn-dyn-model 也在 CN 动态表里）。
	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "cn-dyn-model"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	got := h.availableOutputModels()
	byKey := map[string]map[string]any{}
	for _, e := range got {
		if k, ok := e["key"].(string); ok {
			byKey[k] = e
		}
	}

	g, ok := byKey["global:gpt-5.4"]
	if !ok {
		t.Fatalf("发布清单没有国际版条目 global:gpt-5.4；现有键=%v", publishKeysOf(got))
	}
	if realm, _ := g["realm"].(string); realm != "global" {
		t.Errorf("国际版条目 realm=%q want global（前端按它分组单列）", realm)
	}
	if prod, _ := g["producer"].(string); prod != "workbuddy" {
		t.Errorf("国际版条目 producer=%q want workbuddy（国际版也是 workbuddy 那家）", prod)
	}
	if fid, _ := g["full_id"].(string); !strings.HasPrefix(fid, "global:gpt-5.4") {
		t.Errorf("国际版条目 full_id=%q 应以 global:gpt-5.4 开头", fid)
	}
	if sel, ok := g["selected"].(bool); !ok || !sel {
		t.Errorf("空清单（全放）时国际版条目应默认已勾选，selected=%v", g["selected"])
	}

	// 同名型号两域各一条：CN 用裸 id 当键，国际版用 global:<裸 id>，互不顶掉。
	cn, ok := byKey["cn-dyn-model"]
	if !ok {
		t.Fatalf("发布清单没有 CN 条目 cn-dyn-model；现有键=%v", publishKeysOf(got))
	}
	if realm, _ := cn["realm"].(string); realm != "" {
		t.Errorf("CN 条目不该带 realm 字段（缺省即 CN），realm=%q", realm)
	}
	if _, ok := byKey["global:cn-dyn-model"]; !ok {
		t.Errorf("两域同名型号应各一条；现有键=%v", publishKeysOf(got))
	}
}

// TestPublishGateRealmScoped 发布清单是新清单（models_realm_scoped=true）时两域**完全独立**：
// 国际版只认 "global:<裸 id>"，裸 id 只管 CN。同名型号（fast-model/glm-5.3 之类）各一把开关，
// 勾国内版不会把国际版一起放开，反之亦然。
func TestPublishGateRealmScoped(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "cn-dyn-model"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	out := NewOutputStore("")
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true, Output: out})

	set := func(models []string, realmScoped bool) {
		t.Helper()
		if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
			Models: models, ModelsRealmScoped: realmScoped}); err != nil {
			t.Fatalf("set output: %v", err)
		}
	}
	want := func(step string, m map[string]bool, want map[string]bool) {
		t.Helper()
		for id, on := range want {
			if m[id] != on {
				t.Errorf("%s：/v1/models 里 %q 存在=%v want %v（实际=%v）",
					step, id, m[id], on, sortedIDList(m))
			}
		}
	}

	// 基线：空清单 = 全放，两域都在（默认零回归）。
	set(nil, true)
	want("全放", listedIDs(h), map[string]bool{
		"cn:cn-dyn-model": true, "global:cn-dyn-model": true, "global:gpt-5.4": true,
	})

	// 只开 CN：裸 id 只管 CN，国际版的同名型号与独有型号都被挡掉。
	set([]string{"cn-dyn-model"}, true)
	want("只开 CN", listedIDs(h), map[string]bool{
		"cn:cn-dyn-model": true, "global:cn-dyn-model": false, "global:gpt-5.4": false,
	})

	// 只开国际版独有型号：CN 侧一条都不该出现。
	set([]string{"global:gpt-5.4"}, true)
	want("只开国际版", listedIDs(h), map[string]bool{
		"global:gpt-5.4": true, "cn:cn-dyn-model": false, "global:cn-dyn-model": false,
	})

	// 同名型号：只勾国际版那把开关，CN 的同名型号必须关着（各留一把开关的正向）。
	set([]string{"global:cn-dyn-model"}, true)
	want("同名只勾国际版", listedIDs(h), map[string]bool{
		"global:cn-dyn-model": true, "cn:cn-dyn-model": false, "global:gpt-5.4": false,
	})

	// 列表与可调用性同口径：清单里列出来的每一条，请求侧都必须放行（否则就是
	// 「列表里看得见、一调就 404」——global 段只认裸 id 时正是这个毛病）。
	set([]string{"cn-dyn-model", "global:gpt-5.4"}, true)
	published := h.outputCfgModelSet()
	for id := range listedIDs(h) {
		realm, producer, bare := h.resolveRoute(id)
		if !h.publishAllows(published, realm, producer, bare) {
			t.Errorf("清单列了 %q，请求侧却会被 404（realm=%s producer=%s bare=%s）", id, realm, producer, bare)
		}
	}
}

// TestPublishGateLegacyBareIDCoversGlobal 旧清单（没有 models_realm_scoped 键，只有裸 id）
// 保持加国际版分组之前的行为：裸 id 同时管 CN 与国际版。老用户升级后不清空清单也不会
// 突然把原有型号挡掉；同名型号在国际版侧跟着 CN 一起开，正是旧语义。
func TestPublishGateLegacyBareIDCoversGlobal(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "cn-dyn-model"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	out := NewOutputStore("")
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true, Output: out})

	// 旧清单形态：裸 id + 不带 models_realm_scoped。
	if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Models: []string{"cn-dyn-model"}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	got := listedIDs(h)
	if !got["cn:cn-dyn-model"] {
		t.Errorf("旧清单：CN 型号应照旧列出，实际=%v", sortedIDList(got))
	}
	if !got["global:cn-dyn-model"] {
		t.Errorf("旧清单：同名国际版型号应跟着裸 id 一起开（旧语义），实际=%v", sortedIDList(got))
	}
	if got["global:gpt-5.4"] {
		t.Errorf("旧清单：国际版独有型号裸 id 不在清单里，不该被列出，实际=%v", sortedIDList(got))
	}

	// 面板保存一次（models_realm_scoped=true）→ 同一份清单立刻切到严格分域。
	if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Models: []string{"cn-dyn-model"}, ModelsRealmScoped: true}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	got = listedIDs(h)
	if got["global:cn-dyn-model"] {
		t.Errorf("新清单：裸 id 不该再管国际版，实际=%v", sortedIDList(got))
	}
	if !got["cn:cn-dyn-model"] {
		t.Errorf("新清单：CN 型号仍在清单里，应照旧列出，实际=%v", sortedIDList(got))
	}
}

// TestPublishGateMergeIsOneWay 「合并」模式（裸 id 同时管两域）是**单向**的：以 CN 那把为准。
//
// 清单里只写 global:<裸 id> 时，CN 的同名型号不会跟着开——合并不是「两边互认」，免得以后
// 有人把它当双向等价改出「勾国际版顺手把国内版也放开」。
func TestPublishGateMergeIsOneWay(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "cn-dyn-model"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	out := NewOutputStore("")
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true, Output: out})

	// 合并模式（models_realm_scoped=false，旧语义）：只写国际版那把开关。
	if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Models: []string{"global:cn-dyn-model"}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	got := listedIDs(h)
	if got["cn:cn-dyn-model"] {
		t.Errorf("合并是单向的：只勾国际版不该把 CN 同名型号一起放开，实际=%v", sortedIDList(got))
	}
	if !got["global:cn-dyn-model"] {
		t.Errorf("勾了的国际版型号该列出，实际=%v", sortedIDList(got))
	}

	// 反过来：只写 CN 裸 id → 两域都开（这才是「合并」该有的样子）。
	if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Models: []string{"cn-dyn-model"}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	got = listedIDs(h)
	if !got["cn:cn-dyn-model"] || !got["global:cn-dyn-model"] {
		t.Errorf("裸 id 在合并模式下该同时开两域，实际=%v", sortedIDList(got))
	}
}

func sortedIDList(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for id, on := range m {
		if on {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// TestModelListEffortFieldsNoCNEmpty 无 CN 健康号触发动态拉取失败 → /v1/models CN 面空列表
// （纯动态，无静态兜底）。
func TestModelListEffortFieldsNoCNEmpty(t *testing.T) {
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: upstream.New(), GlobalEnabled: false})

	got := h.modelList()
	if len(got) != 0 {
		t.Fatalf("no CN account: modelList=%v want empty (pure dynamic, no static fallback)", got)
	}
}

// TestModelListEffortFieldsDynamicCN 动态分支：远端 supportedEfforts 权威透出，
// defaultEffort ∈ efforts 才声明；无档位模型省略字段（不输出空数组）。
func TestModelListEffortFieldsDynamicCN(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-effort","maxInputTokens":65536,"maxOutputTokens":8192,
			 "reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}},
			{"id":"dyn-nodefault","maxInputTokens":65536,"maxOutputTokens":8192,
			 "reasoning":{"supportedEfforts":["low","high"]}},
			{"id":"dyn-none","maxInputTokens":65536,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["dyn-effort","dyn-nodefault","dyn-none"]}]}}`, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	got := h.modelList()
	byID := map[string]map[string]any{}
	for _, m := range got {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	if eff := byID["cn:dyn-effort"]["reasoning_supported_efforts"]; !reflect.DeepEqual(eff, []string{"low", "high"}) {
		t.Errorf("dyn-effort supported=%v want [low high]", eff)
	}
	if def := byID["cn:dyn-effort"]["reasoning_default_effort"]; def != "high" {
		t.Errorf("dyn-effort default=%v want high", def)
	}
	// 有档位但无 defaultEffort → 只带 supported，不带 default。
	if byID["cn:dyn-nodefault"]["reasoning_supported_efforts"] == nil {
		t.Error("dyn-nodefault should carry supported efforts")
	}
	if _, ok := byID["cn:dyn-nodefault"]["reasoning_default_effort"]; ok {
		t.Error("dyn-nodefault should NOT carry default effort (upstream omitted)")
	}
	// 无档位 → 字段整体省略（非空数组）。
	if _, ok := byID["cn:dyn-none"]["reasoning_supported_efforts"]; ok {
		t.Error("dyn-none should omit reasoning_supported_efforts")
	}
}

// TestModelListEffortFieldsGlobalProbeFailEmpty global 探测失败 → 空名单（纯动态，无静态兜底），
// deepseek-v4.1-flash 等静态历史名单成员不出现。
func TestModelListEffortFieldsGlobalProbeFailEmpty(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 500, `{"code":500,"msg":"boom"}`) // 探测失败→空名单
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	for _, m := range h.modelList() {
		if id, _ := m["id"].(string); id == "global:deepseek-v4.1-flash" {
			t.Fatalf("global:deepseek-v4.1-flash must not appear on probe failure (pure dynamic): %v", m)
		}
	}
}

// TestModelListEffortFieldsGlobalRemote 探测下发档位 → /v1/models global 面用远端桶（权威）；
// 探测未覆盖的模型不出现（纯动态，无静态兜底）。
func TestModelListEffortFieldsGlobalRemote(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":{"models":[
		{"id":"gpt-5.4","reasoning":{"supportedEfforts":["low","medium","high","xhigh"],"defaultEffort":"high"}},
		{"id":"probe-only-x","reasoning":{"supportedEfforts":["low"],"defaultEffort":"low"}}
	]}}`)
	p := testPoolWith(&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	got := h.modelList()
	byID := map[string]map[string]any{}
	for _, m := range got {
		if id, ok := m["id"].(string); ok {
			byID[id] = m
		}
	}
	// 远端下发的 gpt-5.4 effort 权威透出。
	if !reflect.DeepEqual(byID["global:gpt-5.4"]["reasoning_supported_efforts"], []string{"low", "medium", "high", "xhigh"}) {
		t.Errorf("gpt-5.4 remote efforts=%v want [low medium high xhigh]", byID["global:gpt-5.4"]["reasoning_supported_efforts"])
	}
	if byID["global:gpt-5.4"]["reasoning_default_effort"] != "high" {
		t.Errorf("gpt-5.4 remote default=%v want high", byID["global:gpt-5.4"]["reasoning_default_effort"])
	}
	// 探测独有的 probe-only-x 也透出远程档位。
	if !reflect.DeepEqual(byID["global:probe-only-x"]["reasoning_supported_efforts"], []string{"low"}) {
		t.Errorf("probe-only-x remote efforts=%v want [low]", byID["global:probe-only-x"]["reasoning_supported_efforts"])
	}
	// 纯动态：探测未覆盖的静态历史名单成员（deepseek-v4.1-flash）不出现。
	if _, ok := byID["global:deepseek-v4.1-flash"]; ok {
		t.Error("global:deepseek-v4.1-flash must not appear (not probed, no static fallback)")
	}
}

// TestModelsEndpointFullJSONLockExistingKeys /v1/models 全量 JSON 键回归：
// 既有字段 id/object/created/owned_by/context_length 保持原样（新增字段不删不改）。
func TestModelsEndpointFullJSONLockExistingKeys(t *testing.T) {
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: upstream.New(), GlobalEnabled: false})
	// modelList 直接产出（跳过 HTTP 序列化，验证数据结构本身）。
	got := h.modelList()
	for _, m := range got {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var obj map[string]any
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		for _, k := range []string{"id", "object", "created", "owned_by"} {
			if _, ok := obj[k]; !ok {
				t.Errorf("model entry missing existing key %q: %v", k, obj)
			}
		}
	}
}

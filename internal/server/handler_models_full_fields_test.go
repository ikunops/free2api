package server

import (
	"testing"

	"free2api/internal/auth"
)

// fullFieldsModelsBody CN /console 动态目录全字段样本（任务书 hy3 抓取样本逐字，
// 与 internal/upstream/models_full_fields_test.go 同 fixture 源）。
const fullFieldsModelsBody = `{"code":0,"data":{"models":[
	{"id":"hy3","name":"Hy3","descriptionZh":"混元思考模型，具有增强的推理能力","credits":"x0.05","tags":["craft","badge:限时免费:#FF0000"],"vendor":"j","isDefault":false,"maxAllowedSize":192000,"maxInputTokens":192000,"maxOutputTokens":64000,"onlyReasoning":true,"supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"reasoning":{"effort":"high","summary":"auto","defaultEffort":"high","supportedEfforts":["low","high"]}}
],"agents":[{"name":"cli","models":["hy3"]}]}}`

// TestModelListFullFieldsDynamicCN CN 动态分支全字段透出：/v1/models 条目带
// description/credits/tags/vendor/is_default/supports_reasoning/supports_tool_call/
// only_reasoning/max_allowed_size/reasoning_effort/reasoning_summary（空值省略口径不破坏）。
func TestModelListFullFieldsDynamicCN(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, fullFieldsModelsBody, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	got := h.modelList()
	var entry map[string]any
	for _, m := range got {
		if id, ok := m["id"].(string); ok && id == "cn:hy3-x0.05" {
			entry = m
			break
		}
	}
	if entry == nil {
		t.Fatalf("cn:hy3-x0.05 not found in %v", got)
	}
	// 费率不再进描述：描述原文透出，费率改由 id 后缀 + credits 承载。
	if entry["description"] != "混元思考模型，具有增强的推理能力" {
		t.Errorf("description=%v want 混元思考模型，具有增强的推理能力（费率只走 id 后缀）", entry["description"])
	}
	if entry["credits"] != "x0.05" {
		t.Errorf("credits=%v want x0.05", entry["credits"])
	}
	if tags, ok := entry["tags"].([]string); !ok || len(tags) != 2 || tags[0] != "craft" {
		t.Errorf("tags=%v want [craft badge:限时免费:#FF0000]", entry["tags"])
	}
	if entry["vendor"] != "j" {
		t.Errorf("vendor=%v want j", entry["vendor"])
	}
	if entry["is_default"] != nil {
		// isDefault:false 与其他旗标同口径：false 整体省略（applyModelInfoFields
		// 只在 true 时写出），省略路径由本断言 + omitted 用例共同锁定。
		t.Errorf("is_default=%v want omitted (false)", entry["is_default"])
	}
	if entry["supports_reasoning"] != true {
		t.Errorf("supports_reasoning=%v want true", entry["supports_reasoning"])
	}
	if entry["supports_tool_call"] != true {
		t.Errorf("supports_tool_call=%v want true", entry["supports_tool_call"])
	}
	if entry["only_reasoning"] != true {
		t.Errorf("only_reasoning=%v want true", entry["only_reasoning"])
	}
	if entry["max_allowed_size"] != int64(192000) {
		t.Errorf("max_allowed_size=%v want 192000", entry["max_allowed_size"])
	}
	if entry["reasoning_effort"] != "high" {
		t.Errorf("reasoning_effort=%v want high", entry["reasoning_effort"])
	}
	if entry["reasoning_summary"] != "auto" {
		t.Errorf("reasoning_summary=%v want auto", entry["reasoning_summary"])
	}
	// 既有字段回归锚点。
	if entry["name"] != "Hy3" || entry["context_length"] != int64(192000) || entry["max_output_tokens"] != int64(64000) {
		t.Errorf("base fields wrong: name=%v ctx=%v maxout=%v", entry["name"], entry["context_length"], entry["max_output_tokens"])
	}
}

// TestModelListFullFieldsDynamicCNOmitted 上游不下发新字段的模型 → 对应字段整体省略
// （不输出空串/空数组/false 之外的伪值，不编造）。
func TestModelListFullFieldsDynamicCNOmitted(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"bare-model","maxInputTokens":65536,"maxOutputTokens":8192}
		],"agents":[{"name":"cli","models":["bare-model"]}]}}`, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok && id != "cn:bare-model" {
			continue
		}
		for _, field := range []string{"name", "description", "credits", "tags", "vendor", "is_default",
			"supports_reasoning", "supports_tool_call", "only_reasoning", "max_allowed_size",
			"reasoning_effort", "reasoning_summary"} {
			if _, ok := m[field]; ok {
				t.Errorf("bare-model should omit %s (upstream omitted): got %v", field, m[field])
			}
		}
	}
}

// TestModelListFullFieldsGlobalRich global 分支：/v2 探测对象形态全字段命中 →
// global: 条目透出富字段；探测 200 与名单共用同一次探测（零额外上游请求）。
func TestModelListFullFieldsGlobalRich(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, fullFieldsModelsBody)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	var entry map[string]any
	for _, m := range h.modelList() {
		if id, ok := m["id"].(string); ok && id == "global:hy3-x0.05" {
			entry = m
			break
		}
	}
	if entry == nil {
		t.Fatal("global:hy3-x0.05 not found in modelList output")
	}
	if entry["name"] != "Hy3" {
		t.Errorf("name=%v want Hy3", entry["name"])
	}
	if entry["description"] != "混元思考模型，具有增强的推理能力" {
		t.Errorf("description=%v", entry["description"])
	}
	if entry["credits"] != "x0.05" {
		t.Errorf("credits=%v want x0.05", entry["credits"])
	}
	if entry["vendor"] != "j" {
		t.Errorf("vendor=%v want j", entry["vendor"])
	}
	if entry["supports_tool_call"] != true {
		t.Errorf("supports_tool_call=%v want true", entry["supports_tool_call"])
	}
	if entry["only_reasoning"] != true {
		t.Errorf("only_reasoning=%v want true", entry["only_reasoning"])
	}
	if entry["max_allowed_size"] != int64(192000) {
		t.Errorf("max_allowed_size=%v want 192000", entry["max_allowed_size"])
	}
	if entry["reasoning_effort"] != "high" || entry["reasoning_summary"] != "auto" {
		t.Errorf("reasoning_effort=%v reasoning_summary=%v want high/auto", entry["reasoning_effort"], entry["reasoning_summary"])
	}
	// 富字段全局命中时 context_length/max_output_tokens 走真实值（非 131072 兜底）。
	if entry["context_length"] != int64(192000) || entry["max_output_tokens"] != int64(64000) {
		t.Errorf("ctx=%v maxout=%v want 192000/64000", entry["context_length"], entry["max_output_tokens"])
	}
	// 一次 modelList 只触发一次探测（names 与 infos 共享缓存）。
	// v3-config-merge：单次探测 = v3/config + /v2 企业路并发 = 2 个请求。
	cnt, _, _, _ := cf.snapshot()
	if cnt != 2 {
		t.Errorf("probe calls=%d want 2 (v3 + v2 concurrent, names+infos shared cache)", cnt)
	}
}

// TestModelListFullFieldsGlobalNarrowOmitted 窄表探测（只有 ID 名单）→ 条目保持裸形态：
// 富字段全部省略（数据源没给，不编造；与 TestModelListNameFieldGlobalNarrow 同口径，
// 扩到全字段集合）。
func TestModelListFullFieldsGlobalNarrowOmitted(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, `{"code":0,"data":["gpt-5.4","narrow-only"]}`)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	entries := 0
	for _, m := range h.modelList() {
		id, ok := m["id"].(string)
		if !ok || len(id) < 7 || id[:7] != "global:" {
			continue
		}
		entries++
		for _, field := range []string{"name", "description", "credits", "tags", "vendor",
			"supports_tool_call", "only_reasoning", "max_allowed_size",
			"reasoning_effort", "reasoning_summary"} {
			if _, ok := m[field]; ok {
				t.Errorf("global narrow entry %v should omit %s (no data source)", id, field)
			}
		}
	}
	if entries == 0 {
		t.Fatal("narrow probe should yield global entries")
	}
}

// TestFetchGlobalModelsReturnsAccount fetchGlobalModels 返回被探测账号（供
// FetchGlobalModelInfos 同账号共享缓存）；无 global 号 → (空名单, nil)。
func TestFetchGlobalModelsReturnsAccount(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, fullFieldsModelsBody)
	gAcct := &auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	p := testPoolWith(gAcct)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	names, acct := h.fetchGlobalModels()
	if acct == nil || acct.UID != gAcct.UID {
		t.Fatalf("acct=%v want picked global account %s", acct, gAcct.UID)
	}
	if !contains(names, "hy3") {
		t.Errorf("names missing probed hy3: %v", names)
	}
	// nil 账号路径（无 global 号）：FetchGlobalModelInfos(nil) 安稳返回 nil。
	if got := h.cfg.Upstream.FetchGlobalModelInfos(nil); got != nil {
		t.Errorf("FetchGlobalModelInfos(nil)=%v want nil", got)
	}
}

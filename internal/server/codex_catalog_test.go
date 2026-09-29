package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"free2api/internal/auth"
)

// codexRequiredFields 实测出来的 Codex 目录最小字段集（见 README「Codex 目录分支」）：
// 少任何一个，Codex 都会整份解析失败、退回内置的 8 个模型。
var codexRequiredFields = []string{
	"slug", "display_name", "base_instructions", "experimental_supported_tools",
	"priority", "shell_type", "support_verbosity", "supported_in_api",
	"supported_reasoning_levels", "supports_parallel_tool_calls",
	"supports_reasoning_summaries", "truncation_policy", "visibility",
}

// TestModelsEndpointSplitsByClientVersion 同一个 /v1/models 必须按 client_version 分流：
// 普通客户端拿通用 {"data":[...]}，Codex 拿 {"models":[...]}，且两份的模型名集合一致。
//
// 回归：只有通用格式时，Codex 拉不到目录、静默退回内置 8 个模型——用户在选择器里
// 根本看不到号池里的模型（实测：指到通用格式的口，拿到的是 gpt-5.6-sol 那 8 个）。
func TestModelsEndpointSplitsByClientVersion(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	// 通用客户端：{"object":"list","data":[...]}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	var generic struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &generic); err != nil {
		t.Fatalf("通用格式解析失败: %v body=%s", err, rec.Body.String())
	}
	if generic.Object != "list" || generic.Data == nil {
		t.Fatalf("通用格式应为 {object:list,data:[...]}，实际 body=%s", rec.Body.String())
	}
	if generic.Models != nil {
		t.Errorf("通用格式不该带 models 键")
	}
	ids := map[string]bool{}
	for _, m := range generic.Data {
		if id, ok := m["id"].(string); ok {
			ids[id] = true
		}
	}

	// Codex：{"models":[...]}，字段齐、slug 集合与通用格式一致
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("GET", "/v1/models?client_version=0.144.2", nil))
	var codex struct {
		Object string           `json:"object"`
		Data   []map[string]any `json:"data"`
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &codex); err != nil {
		t.Fatalf("Codex 目录解析失败: %v body=%s", err, rec2.Body.String())
	}
	if codex.Models == nil {
		t.Fatalf("Codex 分支必须回 {models:[...]}，实际 body=%s", rec2.Body.String())
	}
	if codex.Data != nil {
		t.Errorf("Codex 分支不该带 data 键")
	}
	if len(codex.Models) == 0 {
		t.Fatalf("Codex 目录为空；CN 面应有 cn-dyn-model")
	}
	slugs := map[string]bool{}
	for _, m := range codex.Models {
		slug, _ := m["slug"].(string)
		if slug == "" {
			t.Errorf("条目缺 slug: %v", m)
			continue
		}
		slugs[slug] = true
		for _, f := range codexRequiredFields {
			if _, ok := m[f]; !ok {
				t.Errorf("条目 %s 缺必需字段 %s", slug, f)
			}
		}
	}
	for id := range ids {
		if !slugs[id] {
			t.Errorf("通用格式有 %q，Codex 目录里没有（两份口径必须同源）", id)
		}
	}
	for slug := range slugs {
		if !ids[slug] {
			t.Errorf("Codex 目录有 %q，通用格式里没有（两份口径必须同源）", slug)
		}
	}
}

// TestCodexDisplayName 显示名 = 上游中文名 + 「域 费率」标签；slug 才是路由依据。
func TestCodexDisplayName(t *testing.T) {
	cases := []struct{ id, name, want string }{
		{"cn:auto", "Auto", "Auto [CN]"},
		{"cn:fast-model-x0.21", "快速", "快速 [CN x0.21]"},
		{"global:deepseek-v4.1-flash-free", "Deepseek-V4.1-Flash", "Deepseek-V4.1-Flash [GLOBAL free]"},
		{"cn:zcode:glm-5.3-flash", "GLM-5.3-Flash", "GLM-5.3-Flash [CN]"},
		{"gpt-5.2", "gpt-5.2", "gpt-5.2"},
	}
	for _, c := range cases {
		if got := codexDisplayName(c.id, c.name); got != c.want {
			t.Errorf("codexDisplayName(%q,%q)=%q want %q", c.id, c.name, got, c.want)
		}
	}
}

// TestCodexReasoningLevels 上游给了档位就照搬，没给就退回默认三档（不让客户端选不了档位）。
func TestCodexReasoningLevels(t *testing.T) {
	got := codexReasoningLevels(map[string]any{})
	if len(got) != 3 {
		t.Fatalf("无档位时应退回默认三档，实际 %d 个", len(got))
	}
	got2 := codexReasoningLevels(map[string]any{"reasoning_supported_efforts": []string{"low", "max"}})
	if len(got2) != 2 {
		t.Fatalf("应照搬上游两档，实际 %d 个", len(got2))
	}
	if got2[0]["effort"] != "low" || got2[1]["effort"] != "max" {
		t.Errorf("档位顺序/取值不对: %v", got2)
	}
	for _, lv := range got2 {
		if d, _ := lv["description"].(string); d == "" {
			t.Errorf("档位 %v 缺 description", lv["effort"])
		}
	}
}

// TestCodexCatalogServiceTierAndModelMessages 消掉 Codex 客户端的两条噪音告警
// （实测：见 README「Codex 目录」一节的复现步骤）：
//
//   - "Configured service tier `priority` is not advertised as supported ... and will
//     be omitted from requests." → 目录必须广告 priority 档；
//   - "Model personality requested but model_messages is missing, falling back to base
//     instructions." → 目录必须给 model_messages.instructions_template。
//
// 两条都是 Codex 侧的纯噪音：service_tier 经 responses.RequestToChat 的字段白名单
// 本来就到不了上游，instructions_template 的内容与 base_instructions 相同。
func TestCodexCatalogServiceTierAndModelMessages(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models?client_version=0.144.2", nil))
	var got struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("Codex 目录解析失败: %v body=%s", err, rec.Body.String())
	}
	if len(got.Models) == 0 {
		t.Fatalf("Codex 目录为空")
	}
	for _, m := range got.Models {
		slug, _ := m["slug"].(string)

		tiers, _ := m["service_tiers"].([]any)
		if len(tiers) == 0 {
			t.Errorf("%s: service_tiers 空 → Codex 每次请求都会打 priority warning", slug)
		}
		found := false
		for _, tl := range tiers {
			if tm, ok := tl.(map[string]any); ok && tm["id"] == "priority" {
				if nm, _ := tm["name"].(string); nm == "" {
					t.Errorf("%s: priority 档缺 name（Codex 选择器要显示它）", slug)
				}
				found = true
			}
		}
		if !found {
			t.Errorf("%s: service_tiers 里没有 id=priority：%v", slug, tiers)
		}

		mm, ok := m["model_messages"].(map[string]any)
		if !ok {
			t.Errorf("%s: 缺 model_messages → Codex 会回落并打 personality warning", slug)
			continue
		}
		if len(mm) != 3 {
			t.Errorf("%s: model_messages 应与官方同形（3 个键），实际 %v", slug, mm)
		}
		if v, ok := mm["instructions_variables"]; !ok || v != nil {
			t.Errorf("%s: instructions_variables 必须存在且为 null，实际 %v", slug, v)
		}
		if _, ok := mm["approvals"]; !ok {
			t.Errorf("%s: approvals 字段位必须存在（官方同形）", slug)
		}
		tmpl, _ := mm["instructions_template"].(string)
		if tmpl != codexBaseInstructions {
			t.Errorf("%s: instructions_template 应等于 base_instructions，实际 %q", slug, tmpl)
		}
		if bi, _ := m["base_instructions"].(string); tmpl != bi {
			t.Errorf("%s: instructions_template(%q) 与 base_instructions(%q) 不一致", slug, tmpl, bi)
		}
	}
}

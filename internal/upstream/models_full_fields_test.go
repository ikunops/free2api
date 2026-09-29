package upstream

import (
	"net/http"
	"testing"

	"free2api/internal/auth"
)

// fullFieldsEntry 上游全字段模型对象样本（任务书 hy3 抓取样本逐字）。
// CN /console 与 global /v2 实测同构（2026-09-15 global 真实账号 /v2 探测确认，
// 见 .claude/reports/models-full-fields.md），两域解析共用此 fixture。
const fullFieldsEntry = `{"id":"hy3","name":"Hy3","descriptionZh":"混元思考模型，具有增强的推理能力","credits":"x0.05","tags":["craft","badge:限时免费:#FF0000"],"vendor":"j","isDefault":false,"maxAllowedSize":192000,"maxInputTokens":192000,"maxOutputTokens":64000,"onlyReasoning":true,"supportsImages":true,"supportsReasoning":true,"supportsToolCall":true,"reasoning":{"effort":"high","summary":"auto","defaultEffort":"high","supportedEfforts":["low","high"]}}`

// TestFetchModelsParsesFullFields CN 动态模型目录全字段解析：
// description/credits/tags/vendor/能力旗标/maxAllowedSize/reasoning.effort+summary
// 全部落入 ModelInfo（既有字段同时回归锚定）。
func TestFetchModelsParsesFullFields(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"models":[`+fullFieldsEntry+`],
			"agents":[{"name":"cli","models":["hy3"]}]}}`), nil
	})
	infos, err := c.FetchModels(authStub())
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v want 1", infos)
	}
	mi := infos[0]
	if mi.Description != "混元思考模型，具有增强的推理能力" {
		t.Errorf("Description=%q want 混元思考模型，具有增强的推理能力", mi.Description)
	}
	if mi.Credits != "x0.05" {
		t.Errorf("Credits=%q want x0.05", mi.Credits)
	}
	if len(mi.Tags) != 2 || mi.Tags[0] != "craft" || mi.Tags[1] != "badge:限时免费:#FF0000" {
		t.Errorf("Tags=%v want [craft badge:限时免费:#FF0000]", mi.Tags)
	}
	if mi.Vendor != "j" {
		t.Errorf("Vendor=%q want j", mi.Vendor)
	}
	if mi.IsDefault {
		t.Error("IsDefault=true want false（上游 isDefault:false）")
	}
	if !mi.SupportsReasoning {
		t.Error("SupportsReasoning=false want true")
	}
	if !mi.SupportsToolCall {
		t.Error("SupportsToolCall=false want true")
	}
	if !mi.OnlyReasoning {
		t.Error("OnlyReasoning=false want true")
	}
	if mi.MaxAllowedSize != 192000 {
		t.Errorf("MaxAllowedSize=%d want 192000", mi.MaxAllowedSize)
	}
	if mi.ReasoningEffort != "high" {
		t.Errorf("ReasoningEffort=%q want high", mi.ReasoningEffort)
	}
	if mi.ReasoningSummary != "auto" {
		t.Errorf("ReasoningSummary=%q want auto", mi.ReasoningSummary)
	}
	// 既有字段回归锚点。
	if mi.ContextWindow != 192000 || mi.MaxTokens != 64000 {
		t.Errorf("ContextWindow=%d MaxTokens=%d want 192000/64000", mi.ContextWindow, mi.MaxTokens)
	}
	if !mi.SupportsImages {
		t.Error("SupportsImages=false want true")
	}
	if mi.DefaultEffort != "high" || !sameStrings(mi.Efforts, []string{"low", "high"}) {
		t.Errorf("DefaultEffort=%q Efforts=%v want high/[low high]", mi.DefaultEffort, mi.Efforts)
	}
}

// TestParseGlobalModelNamesFullFields global /v2 对象形态全字段：
// 解析产出 names + 完整 ModelInfo（富字段）+ effort 桶（数组优先）。
func TestParseGlobalModelNamesFullFields(t *testing.T) {
	raw := `{"code":0,"data":{"models":[` + fullFieldsEntry + `]}}`
	names, infos, efforts, defaults, err := parseGlobalModelNames([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(names) != 1 || names[0] != "hy3" {
		t.Fatalf("names=%v want [hy3]", names)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v want 1", infos)
	}
	mi := infos[0]
	if mi.ID != "hy3" || mi.Name != "Hy3" {
		t.Errorf("ID=%q Name=%q want hy3/Hy3", mi.ID, mi.Name)
	}
	if mi.Description != "混元思考模型，具有增强的推理能力" {
		t.Errorf("Description=%q", mi.Description)
	}
	if mi.Credits != "x0.05" {
		t.Errorf("Credits=%q want x0.05", mi.Credits)
	}
	if len(mi.Tags) != 2 || mi.Tags[0] != "craft" {
		t.Errorf("Tags=%v", mi.Tags)
	}
	if mi.Vendor != "j" {
		t.Errorf("Vendor=%q want j", mi.Vendor)
	}
	if !mi.SupportsToolCall || !mi.SupportsReasoning || !mi.OnlyReasoning {
		t.Errorf("capability flags wrong: %+v", mi)
	}
	if mi.MaxAllowedSize != 192000 {
		t.Errorf("MaxAllowedSize=%d want 192000", mi.MaxAllowedSize)
	}
	if mi.ReasoningEffort != "high" || mi.ReasoningSummary != "auto" {
		t.Errorf("ReasoningEffort=%q ReasoningSummary=%q want high/auto", mi.ReasoningEffort, mi.ReasoningSummary)
	}
	if mi.ContextWindow != 192000 || mi.MaxTokens != 64000 {
		t.Errorf("ContextWindow=%d MaxTokens=%d want 192000/64000", mi.ContextWindow, mi.MaxTokens)
	}
	if !sameStrings(efforts["hy3"], []string{"low", "high"}) {
		t.Errorf("efforts[hy3]=%v want [low high]（数组优先于单档）", efforts["hy3"])
	}
	if defaults["hy3"] != "high" {
		t.Errorf("defaults[hy3]=%v want high", defaults["hy3"])
	}
}

// TestFetchGlobalModelInfosProbe 富 ModelInfo 探测：对象形态 200 →
// FetchGlobalModelInfos 返回全字段条目；FetchGlobalModels 共享同一次探测
// （名单照常合并）；二次调用命中 1h 缓存零新上游请求。
func TestFetchGlobalModelInfosProbe(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[` + fullFieldsEntry + `]}}`
	})
	defer srv.Close()
	c := globalModelsClient(t, srv)

	infos := c.FetchGlobalModelInfos(globalAcct())
	if len(infos) != 1 {
		t.Fatalf("infos=%+v want 1", infos)
	}
	if infos[0].Description != "混元思考模型，具有增强的推理能力" || infos[0].Credits != "x0.05" {
		t.Errorf("rich fields missing: %+v", infos[0])
	}
	if infos[0].ContextWindow != 192000 {
		t.Errorf("ContextWindow=%d want 192000", infos[0].ContextWindow)
	}
	// 名单路径共享同一探测缓存：零额外上游请求（v3-config-merge 后首探 = v3+v2 两路）。
	names := c.FetchGlobalModels(globalAcct())
	if len(calls) != 2 || !containsStr(calls, "/v3/config") || !containsStr(calls, "/v2/enterprises/personal/models") {
		t.Fatalf("probe calls=%v want [/v3/config /v2/enterprises/personal/models] (cache shared)", calls)
	}
	hasHy3 := false
	for _, id := range names {
		if id == "hy3" {
			hasHy3 = true
		}
	}
	if !hasHy3 {
		t.Errorf("merged names missing probed hy3: %v", names)
	}
	// 二次调用命中缓存。
	infos2 := c.FetchGlobalModelInfos(globalAcct())
	if len(infos2) != 1 || infos2[0].ID != "hy3" {
		t.Fatalf("cached infos=%+v want 1 hy3", infos2)
	}
	if len(calls) != 2 {
		t.Errorf("cache: probe calls=%d want 2 (second hit 1h cache)", len(calls))
	}
}

// TestFetchGlobalModelInfosNarrowNil 窄表形态：names 可解析但无对象字段 →
// infos 返回 nil（调用方回落纯 ID 名单条目，不编造字段）。
func TestFetchGlobalModelInfosNarrowNil(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":["gpt-5.4","narrow-only"]}`
	})
	defer srv.Close()
	c := globalModelsClient(t, srv)

	if got := c.FetchGlobalModelInfos(globalAcct()); got != nil {
		t.Errorf("narrow table: infos=%v want nil", got)
	}
	if names := c.FetchGlobalModels(globalAcct()); len(names) == 0 {
		t.Error("narrow table: names should still merge")
	}
}

// TestFetchGlobalModelInfosFailureNil 探测失败（家族全 500）→ 负缓存 + infos nil；
// CN 账号（不路由 global）→ infos nil（逃生门兜底）。
func TestFetchGlobalModelInfosFailureNil(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 500, `{"code":500,"msg":"boom"}`
	})
	defer srv.Close()
	c := globalModelsClient(t, srv)

	if got := c.FetchGlobalModelInfos(globalAcct()); got != nil {
		t.Errorf("probe failure: infos=%v want nil", got)
	}
	// 非路由账号（CN realm）→ 不探测，infos nil。
	if got := c.FetchGlobalModelInfos(authStub()); got != nil {
		t.Errorf("cn account: infos=%v want nil (globalOn gate)", got)
	}
}

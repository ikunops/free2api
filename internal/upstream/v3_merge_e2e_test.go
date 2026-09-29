//go:build v3_e2e

// v3_merge_e2e_test.go v3-config-merge 真实账号端到端实测探针（任务书验收 4）。
//
// 用法：
//
//	go test -tags=v3_e2e -run TestV3MergeE2E -v -count=1 ./internal/upstream/
//
// 日常 `go test ./...` 不编译本文件（build tag 隔离）。只读探测：GET 模型目录端点，
// 不写上游、不落盘。token 只以脱敏形式出现在日志。
package upstream

import (
	"net/http"
	"testing"
	"time"
)

// TestV3MergeE2EGlobal 真实 global 账号全链路：FetchGlobalModels 走
// probeGlobalModels（v3/config 主 + v2 企业补充并发），断言：
//   - 并集 = 22 个模型（v3 21 ∪ v2 18，任务书验收 4 的预期清单）；
//   - v3 独有 4 模型（deepseek-v4.1-flash / gpt-6-astra / hy4-preview-f / kimi-k2.8-preview）在列；
//   - v2 独有 gpt-5.3-codex 作为补充在列；
//   - 富字段条目（credits/description）非空——v3 条目字段权威。
func TestV3MergeE2EGlobal(t *testing.T) {
	a := loadGlobalAcct(t)
	c := &Client{
		HTTP:          &http.Client{Timeout: 60 * time.Second},
		GlobalEnabled: true,
	}

	names := c.FetchGlobalModels(a)
	if len(names) == 0 {
		t.Fatal("FetchGlobalModels returned empty (probe failed)")
	}
	t.Logf("E2E global: merged %d models: %v", len(names), names)

	want22 := []string{
		"balanced-model", "deep-model", "default-model", "fast-model",
		"gemini-3.5-flash", "glm-5.2", "glm-5.3", "gpt-5.3-codex", "gpt-5.4",
		"gpt-5.5", "gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-astra",
		"hy3", "hy4-preview", "hy4-preview-f", "deepseek-v4.1-flash",
		"kimi-k2.6", "kimi-k2.8-preview", "kimi-k3", "primary-model",
	}
	if len(names) != 22 {
		t.Errorf("merged count=%d want 22 (v3 21 ∪ v2 18)", len(names))
	}
	for _, id := range want22 {
		if !containsStr(names, id) {
			t.Errorf("merged names missing %s (expected in 22-model union)", id)
		}
	}

	infos := c.FetchGlobalModelInfos(a)
	if len(infos) == 0 {
		t.Fatal("FetchGlobalModelInfos returned nil (object form expected)")
	}
	byID := map[string]ModelInfo{}
	for _, mi := range infos {
		byID[mi.ID] = mi
	}
	// v3 独有模型必须是富条目（v3 响应下发全字段）。
	for _, id := range []string{"deepseek-v4.1-flash", "gpt-6-astra", "hy4-preview-f", "kimi-k2.8-preview"} {
		mi, ok := byID[id]
		if !ok {
			t.Errorf("infos missing v3-only model %s", id)
			continue
		}
		if mi.Description == "" || mi.Credits == "" {
			t.Errorf("%s rich fields empty (v3 entry should carry full metadata): %+v", id, mi)
		}
		t.Logf("E2E %s: credits=%s desc=%.30s ctx=%d maxout=%d", id, mi.Credits, mi.Description, mi.ContextWindow, mi.MaxTokens)
	}
	// v2 补充模型（gpt-5.3-codex）带 v2 的字段。
	if mi, ok := byID["gpt-5.3-codex"]; ok {
		t.Logf("E2E gpt-5.3-codex (v2 supplement): credits=%s desc=%.30s", mi.Credits, mi.Description)
	} else {
		t.Errorf("infos missing v2-only supplement gpt-5.3-codex")
	}
}

// TestV3MergeE2EEndpointsReachable 独立 curl 等价验证：两端点经 CommonHeaders
// （三段式 CLI UA）+ Bearer 均 200（任务书验收 4：实测确认仍 200）。
func TestV3MergeE2EEndpointsReachable(t *testing.T) {
	a := loadGlobalAcct(t)
	c := &Client{
		HTTP:          &http.Client{Timeout: 60 * time.Second},
		GlobalEnabled: true,
	}
	for _, path := range []string{v3ConfigPath, globalModelsPath} {
		names, infos, _, _, err := c.globalModelsOnce(a, path)
		if err != nil {
			t.Errorf("endpoint %s: %v (want 200 + parse ok)", path, err)
			continue
		}
		t.Logf("E2E %s: 200 OK, %d models, infos=%d (Bearer+CLI UA gate passed)", path, len(names), len(infos))
	}
}

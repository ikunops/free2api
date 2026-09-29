package upstream

import (
	"testing"
)

// TestInjectThinkingUsesModelDefaultEffort P1：缓存有 defaultEffort=medium 的模型
// → 注入 medium 而非硬编码 high。
func TestInjectThinkingUsesModelDefaultEffort(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, nil,
		map[string]string{"deepseek-v4-flash": "medium"})
	eff, _ := objFieldString(t, out, "reasoning_effort")
	if eff != "medium" {
		t.Errorf("reasoning_effort=%q want medium (模型默认档) (out=%s)", eff, out)
	}
}

// TestInjectThinkingFallsBackToHardcoded P1：缓存无该模型 → 用硬编码 high（向后兼容）。
func TestInjectThinkingFallsBackToHardcoded(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, nil, nil)
	eff, _ := objFieldString(t, out, "reasoning_effort")
	if eff != "high" {
		t.Errorf("reasoning_effort=%q want high (硬编码兜底) (out=%s)", eff, out)
	}
}

// TestInjectThinkingEmptyDefaultEffortFallsBack P1：defaultEfforts 有该模型但值为空串
// → 回退硬编码 high。
func TestInjectThinkingEmptyDefaultEffortFallsBack(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false, nil,
		map[string]string{"deepseek-v4-flash": ""})
	eff, _ := objFieldString(t, out, "reasoning_effort")
	if eff != "high" {
		t.Errorf("reasoning_effort=%q want high (空串回退) (out=%s)", eff, out)
	}
}

// TestInjectThinkingDefaultEffortDowngrades P1：模型默认档 medium + supportedEfforts
// 不含 medium（只 low/high）→ 经降级管线落到 ≤medium 的最高支持档。
// 此用例验证默认档也走降级：medium 在 [low,high] 中无匹配，向下取 low。
func TestInjectThinkingDefaultEffortDowngrades(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault(
		[]byte(`{"model":"deepseek-v4-flash","messages":[]}`),
		false,
		map[string][]string{"deepseek-v4-flash": {"low", "high"}},
		map[string]string{"deepseek-v4-flash": "medium"})
	eff, _ := objFieldString(t, out, "reasoning_effort")
	// medium 不在 [low,high] 中，降级到 ≤medium 的最高支持档 = low
	if eff != "low" {
		t.Errorf("reasoning_effort=%q want low (medium 在 [low,high] 降级) (out=%s)", eff, out)
	}
}

// objFieldString 见 thinking_test.go（复用）。


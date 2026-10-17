// model_cap_fields.go 统一的模型目录能力字段解析。
//
// 不同上游（OpenCode / Kilo / ZCode / 镜像站）的 /models 响应字段并不统一：
// 同一项能力可能叫 context_length、context_window、max_input_tokens 或
// limit.context；图片能力可能来自 supportsImages、vision 或
// architecture.input_modalities。这里集中做“有就取、没有就留未知”的宽容解析，
// 不按模型名硬编码任何能力值。未知字段保持零值，由 /v1/models 侧省略。
package upstream

import (
	"encoding/json"
	"strings"
)

// modelCapFields 可嵌入各家 /models 条目结构，解析常见同义字段。
// 所有字段都是可选：上游没声明时保持零值。
type modelCapFields struct {
	ContextLength         int64 `json:"context_length"`
	ContextWindow         int64 `json:"context_window"`
	MaxInputTokens        int64 `json:"max_input_tokens"`
	MaxContextLength      int64 `json:"max_context_length"`
	MaxInputTokensCamel   int64 `json:"maxInputTokens"`
	MaxContextLengthCamel int64 `json:"maxContextLength"`
	MaxOutputTokens       int64 `json:"max_output_tokens"`
	MaxOutput             int64 `json:"max_output"`
	MaxTokens             int64 `json:"max_tokens"`
	MaxCompletionTokens   int64 `json:"max_completion_tokens"`
	MaxOutputTokensCamel  int64 `json:"maxOutputTokens"`

	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	TopProvider struct {
		ContextLength       int64 `json:"context_length"`
		MaxCompletionTokens int64 `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`

	SupportsImages      bool `json:"supportsImages"`
	SupportsImagesSnake bool `json:"supports_images"`
	Vision              bool `json:"vision"`
	SupportsVision      bool `json:"supports_vision"`

	SupportsReasoning      bool `json:"supportsReasoning"`
	SupportsReasoningSnake bool `json:"supports_reasoning"`
	SupportsToolCall       bool `json:"supportsToolCall"`
	SupportsToolCallSnake  bool `json:"supports_tool_call"`
	SupportsTools          bool `json:"supports_tools"`

	// 有些 OpenRouter 兼容目录把 tools / reasoning 写成 bool，另一些写成对象或
	// 数组。RawMessage 避免“数组塞进 bool”导致整条目录解析失败；只有字面 bool
	// 才当作能力声明。
	ToolsRaw     json.RawMessage `json:"tools"`
	ToolCallRaw  json.RawMessage `json:"tool_call"`
	ReasoningRaw json.RawMessage `json:"reasoning"`
}

// apply 把目录里实际声明的能力写进 ModelInfo。只覆盖正值/true，不把缺失写成 false。
func (f modelCapFields) apply(mi *ModelInfo) {
	if mi == nil {
		return
	}
	if mi.ContextWindow <= 0 {
		for _, v := range []int64{
			f.ContextLength, f.ContextWindow, f.MaxInputTokens, f.MaxContextLength,
			f.MaxInputTokensCamel, f.MaxContextLengthCamel, f.Limit.Context,
			f.TopProvider.ContextLength,
		} {
			if v > 0 {
				mi.ContextWindow = v
				break
			}
		}
	}
	if mi.MaxTokens <= 0 {
		for _, v := range []int64{
			f.MaxOutputTokens, f.MaxOutput, f.MaxTokens, f.MaxCompletionTokens,
			f.MaxOutputTokensCamel, f.Limit.Output, f.TopProvider.MaxCompletionTokens,
		} {
			if v > 0 {
				mi.MaxTokens = v
				break
			}
		}
	}
	if f.SupportsImages || f.SupportsImagesSnake || f.Vision || f.SupportsVision || hasImageModality(f.Architecture.InputModalities) {
		mi.SupportsImages = true
	}
	if f.SupportsReasoning || f.SupportsReasoningSnake || rawBool(f.ReasoningRaw) {
		mi.SupportsReasoning = true
	}
	if f.SupportsToolCall || f.SupportsToolCallSnake || f.SupportsTools || rawBool(f.ToolsRaw) || rawBool(f.ToolCallRaw) {
		mi.SupportsToolCall = true
	}
}

func hasImageModality(mods []string) bool {
	for _, mod := range mods {
		if strings.EqualFold(strings.TrimSpace(mod), "image") {
			return true
		}
	}
	return false
}

func rawBool(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

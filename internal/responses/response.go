package responses

import (
	"encoding/json"
	"strings"
	"time"
)

// FromChat 把聚合后的 chat.completion 翻译成 Responses 的 response 对象（非流式）。
// model 由调用方传入客户端**原始**请求里的模型名（含网关前缀），原样回显——
// 客户端拿它做断言/日志，改写会让它认不出自己的请求。
func FromChat(chat map[string]any, model string) map[string]any {
	created := time.Now().Unix()
	if v, ok := number(chat["created"]); ok && v > 0 {
		created = int64(v)
	}
	return map[string]any{
		"id":                  newID("resp"),
		"object":              "response",
		"created_at":          created,
		"status":              "completed",
		"model":               model,
		"output":              outputItems(chat),
		"usage":               usageFromChat(chat),
		"error":               nil,
		"incomplete_details":  nil,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"metadata":            map[string]any{},
	}
}

// outputItems 把 chat.completion 的 choices[0].message 翻成 Responses 的 output 数组。
// 顺序：先正文 message，再逐个 function_call（与客户端读到的顺序一致）。
func outputItems(chat map[string]any) []any {
	out := []any{}
	msg := firstChoiceMessage(chat)
	if msg == nil {
		return out
	}
	if txt := messageText(msg["content"]); strings.TrimSpace(txt) != "" {
		out = append(out, messageItemOut(newID("msg"), txt))
	}
	// 官方 tool_calls（含 arguments 原文）逐个翻成 function_call item。
	if tcs, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range tcs {
			tc, _ := raw.(map[string]any)
			if tc == nil {
				continue
			}
			out = append(out, functionCallItem(newID("fc"), toolCallID(tc), toolCallName(tc), toolCallArgs(tc), "completed"))
		}
	}
	// 兼容上游把单次调用放在老式 function_call 字段的形态。
	if fc, ok := msg["function_call"].(map[string]any); ok && fc != nil {
		name, _ := fc["name"].(string)
		args, _ := fc["arguments"].(string)
		if name != "" || args != "" {
			out = append(out, functionCallItem(newID("fc"), newID("call"), name, args, "completed"))
		}
	}
	return out
}

// firstChoiceMessage 取 choices[0].message（缺失/畸形 → nil）。
func firstChoiceMessage(chat map[string]any) map[string]any {
	choices, _ := chat["choices"].([]any)
	if len(choices) == 0 {
		return nil
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return nil
	}
	msg, _ := c["message"].(map[string]any)
	// 部分上游把聚合结果直接放在 delta 上（非流式聚合兜底形态）。
	if msg == nil {
		msg, _ = c["delta"].(map[string]any)
	}
	return msg
}

// messageText 把 message.content 拉平成文本：string 直接用；数组形态拼接其中的 text 片段。
func messageText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var b strings.Builder
		for _, raw := range t {
			if p, ok := raw.(map[string]any); ok {
				if s, _ := p["text"].(string); s != "" {
					b.WriteString(s)
				}
			}
		}
		return b.String()
	}
	return ""
}

// messageItemOut 构造 Responses 的 assistant message item。
func messageItemOut(id, text string) map[string]any {
	return map[string]any{
		"id":     id,
		"type":   "message",
		"role":   "assistant",
		"status": "completed",
		"content": []any{
			map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		},
	}
}

// functionCallItem 构造 Responses 的 function_call item。
func functionCallItem(id, callID, name, args, status string) map[string]any {
	if callID == "" {
		callID = newID("call")
	}
	return map[string]any{
		"id":        id,
		"type":      "function_call",
		"call_id":   callID,
		"name":      name,
		"arguments": args,
		"status":    status,
	}
}

// toolCallID / toolCallName / toolCallArgs 取 Chat tool_call 的三个字段（缺省给安全值）。
func toolCallID(tc map[string]any) string {
	if v, _ := tc["id"].(string); v != "" {
		return v
	}
	return newID("call")
}

func toolCallName(tc map[string]any) string {
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	v, _ := fn["name"].(string)
	return v
}

func toolCallArgs(tc map[string]any) string {
	fn, _ := tc["function"].(map[string]any)
	if fn == nil {
		return ""
	}
	v, _ := fn["arguments"].(string)
	return v
}

// usageFromChat 把 Chat 的 usage 翻成 Responses 的 usage。
// 上游没给 usage 时返回全零结构（不省略字段：严格客户端按存在性取值）。
func usageFromChat(chat map[string]any) map[string]any {
	u, _ := chat["usage"].(map[string]any)
	in, _ := number(u["prompt_tokens"])
	out, _ := number(u["completion_tokens"])
	total, hasTotal := number(u["total_tokens"])
	if !hasTotal {
		total = in + out
	}
	cached, _ := number(u["prompt_cache_hit_tokens"])
	// 类型断言必须带 ok：上游通常不发 completion_tokens_details，直断言会 panic。
	var reasoning float64
	if det, ok := u["completion_tokens_details"].(map[string]any); ok {
		reasoning, _ = number(det["reasoning_tokens"])
	}
	return map[string]any{
		"input_tokens":  int64(in),
		"output_tokens": int64(out),
		"total_tokens":  int64(total),
		"input_tokens_details": map[string]any{
			"cached_tokens": int64(cached),
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": int64(reasoning),
		},
	}
}

// zeroUsage 全零 usage（响应骨架用）。
func zeroUsage() map[string]any {
	return map[string]any{
		"input_tokens":  0,
		"output_tokens": 0,
		"total_tokens":  0,
		"input_tokens_details": map[string]any{
			"cached_tokens": 0,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": 0,
		},
	}
}

// number 把 JSON 数字（float64 / int 家族 / json.Number）统一成 float64。
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

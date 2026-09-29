// Package responses 在 OpenAI Responses API 与 Chat Completions 之间做双向翻译。
//
// 存在理由：Codex CLI 只认 Responses（config.toml 里 wire_api = "responses"），
// 而 WorkBuddy 上游只认 Chat Completions。此前这条翻译由外部 CLIProxyAPI（8317）
// 承担——多一个进程、多一个端口、多一份配置、多一跳本机转发。本包把它收进网关本体：
// 一个进程同时对外提供 /v1/chat/completions 与 /v1/responses 两种协议，出口格式由
// 用户在选择「输出格式」时决定（见 internal/server/output.go 的 formats 注册表）。
//
// 本文件的职责是**请求向**：Responses 请求体 → Chat 请求体。
package responses

import (
	"encoding/json"
	"fmt"
	"strings"
)

// RequestToChat 把 Responses API 请求体翻译成 Chat Completions 请求体。
//
// 只翻译语义等价的字段；Responses 专有字段（store / include / stream_options /
// text / reasoning.summary / metadata 等）一律丢弃——Chat 上游不认它们，
// 原样透传只会换来上游的字段白名单 400。
//
// 翻译后的 body 仍会走网关既有的出站预处理管线（upstream.PrepareBody）：
// 强制 stream、max_completion_tokens → max_tokens、tool_choice 归一化、
// developer → system、reasoning_effort 按模型支持档降级等，都在那一层完成，
// 本函数不做重复工作。
func RequestToChat(raw []byte) ([]byte, error) {
	var in struct {
		Model             string           `json:"model"`
		Instructions      json.RawMessage  `json:"instructions"`
		Input             json.RawMessage  `json:"input"`
		Tools             []map[string]any `json:"tools"`
		ToolChoice        json.RawMessage  `json:"tool_choice"`
		ParallelToolCalls *bool            `json:"parallel_tool_calls"`
		Stream            bool             `json:"stream"`
		Temperature       *float64         `json:"temperature"`
		TopP              *float64         `json:"top_p"`
		MaxOutputTokens   *float64         `json:"max_output_tokens"`
		Reasoning         json.RawMessage  `json:"reasoning"`
		// PromptCacheKey 会原样透传给上游（upstream.InjectPromptCacheKey 认它），
		// 同时也是 session.ExtractKey 的第 5 优先级会话键——Codex 每轮都带同一个值，
		// 于是「会话粘性」在 Responses 协议下天然可用（不必回落到首条 user 消息派生）。
		PromptCacheKey string `json:"prompt_cache_key"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("请求体不是合法 JSON: %w", err)
	}
	if strings.TrimSpace(in.Model) == "" {
		return nil, fmt.Errorf("缺少 model 字段")
	}

	out := map[string]any{"model": in.Model, "stream": in.Stream}

	msgs := make([]any, 0, 8)
	if txt := textOf(in.Instructions); strings.TrimSpace(txt) != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": txt})
	}
	msgs = append(msgs, inputMessages(in.Input)...)
	if len(msgs) == 0 {
		return nil, fmt.Errorf("input 里没有可发送的消息")
	}
	out["messages"] = msgs

	if tools := chatTools(in.Tools); len(tools) > 0 {
		out["tools"] = tools
	}
	if tc := translateToolChoice(in.ToolChoice); tc != nil {
		out["tool_choice"] = tc
	}
	if in.ParallelToolCalls != nil {
		out["parallel_tool_calls"] = *in.ParallelToolCalls
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if in.MaxOutputTokens != nil && *in.MaxOutputTokens > 0 {
		// 上游管线把 max_completion_tokens 翻成 max_tokens（只认后者）。
		out["max_completion_tokens"] = int64(*in.MaxOutputTokens)
	}
	if eff := reasoningEffort(in.Reasoning); eff != "" {
		out["reasoning_effort"] = eff
	}
	if k := strings.TrimSpace(in.PromptCacheKey); k != "" {
		out["prompt_cache_key"] = k
	}
	return json.Marshal(out)
}

// textOf 把「字符串 / 内容数组 / 缺失」三种形态的文本字段拉平成纯文本。
// instructions 在官方规范里是 string，但部分客户端发 [{"type":"input_text",...}]。
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		if t, _ := p["text"].(string); t != "" {
			b.WriteString(t)
		}
	}
	return b.String()
}

// inputMessages 把 Responses 的 input 数组翻译成 Chat messages。
//
// 逐项映射（官方 Responses 规范的 item 类型）：
//   - message              → {role, content}（developer 归一为 system，与上游 role 白名单一致）
//   - function_call        → assistant 消息 + tool_calls（Chat 里工具调用挂在 assistant 上）
//   - function_call_output → {role:"tool", tool_call_id, content}
//   - reasoning / item_reference / 未知类型 → 丢弃。
//
// 丢弃 reasoning 是刻意的：Chat 上游没有对应的请求侧结构，且「谁调用、谁返回」的
// 配对关系只由 function_call / function_call_output 承载——丢推理不破坏配对。
//
// 悬空配对（orphan）也要丢：客户端回合被中断（如 Codex 的 turn_aborted）或历史
// 压缩后，input 里会留下有 call 无 output 的 function_call / 有 output 无 call 的
// function_call_output。前者翻成带 tool_calls 的 assistant 却等不到 tool 结果、
// 后者翻成没有 tool_calls 在前的 tool 消息，Chat 上游（WorkBuddy）一律 400
// （11148「tool calls and tool results do not match」/ 11133「Invalid request
// parameters」），且换哪个账号都一样——客户端只剩无限重试。预扫配对、只放行
// 双侧齐全的 call_id，被丢弃的半截对模型语义没有贡献，丢掉不损失信息。
//
// 连续的 function_call 还要**合并成一条 assistant**：Codex 一轮发多个工具调用时，
// input 里是并列的多个 function_call item；若逐条各翻一条 assistant，上游看到的
// 是「assistant(call A)、assistant(call B)、tool(A)…」——tool 结果没有紧跟携带
// 它的 assistant，上游按同样两个错误码拒绝（实测复现）。Chat 的正确形态是
// 一条 assistant 挂 tool_calls:[A,B]，随后 tool 结果逐条跟随。
func inputMessages(raw json.RawMessage) []any {
	if len(raw) == 0 {
		return nil
	}
	// input 可以是裸字符串（{"input":"hi"}）——等价于单条 user 消息。
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if strings.TrimSpace(s) == "" {
			return nil
		}
		return []any{map[string]any{"role": "user", "content": s}}
	}
	var items []map[string]any
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	paired := pairedCallIDs(items)
	var (
		out     = make([]any, 0, len(items))
		pending []map[string]any // 连续 function_call 攒成的 tool_calls，遇到非 call 项落盘
	)
	flush := func() {
		if len(pending) == 0 {
			return
		}
		// 紧邻的前一条是纯文本 assistant（Codex「先说结论、同轮接着调工具」）时，
		// 把 tool_calls 并进去——规范 Chat 形态是一条 assistant 同时带 content 和
		// tool_calls，也避免出现连续两条 assistant。
		if n := len(out); n > 0 {
			if last, ok := out[n-1].(map[string]any); ok &&
				last["role"] == "assistant" && last["tool_calls"] == nil && last["content"] != nil {
				last["tool_calls"] = pending
				pending = nil
				return
			}
		}
		out = append(out, map[string]any{
			"role":       "assistant",
			"content":    nil,
			"tool_calls": pending,
		})
		pending = nil
	}
	for _, it := range items {
		switch itemType(it) {
		case "function_call":
			if !paired[pairIDOf(it)] {
				continue
			}
			pending = append(pending, toolCallEntry(it))
		case "function_call_output":
			flush()
			if !paired[pairIDOf(it)] {
				continue
			}
			out = append(out, functionCallOutputMessage(it))
		case "reasoning", "item_reference":
			// reasoning/引用类：Chat 无对应结构，丢弃；它们夹在连续 function_call
			// 之间时不算打断——同一轮的多个调用仍应合并进同一条 assistant。
		default:
			flush()
			if m := messageItem(it); m != nil {
				out = append(out, m)
			}
		}
	}
	flush()
	return out
}

// toolCallEntry 把单个 function_call item 翻成 tool_calls 数组的成员。
func toolCallEntry(it map[string]any) map[string]any {
	name, _ := it["name"].(string)
	args, _ := it["arguments"].(string)
	return map[string]any{
		"id":   callIDOf(it),
		"type": "function",
		"function": map[string]any{
			"name":      name,
			"arguments": args,
		},
	}
}

// pairIDOf 取配对 ID。与 callIDOf 的差别：缺字段时不生成新 ID——
// 现场造出来的 ID 任何 output 都配不上，注定是孤儿。
func pairIDOf(it map[string]any) string {
	if v, _ := it["call_id"].(string); v != "" {
		return v
	}
	if v, _ := it["id"].(string); v != "" {
		return v
	}
	return ""
}

// pairedCallIDs 返回同一请求里 call 与 output 双侧齐全的 call_id 集合。
func pairedCallIDs(items []map[string]any) map[string]bool {
	calls := map[string]bool{}
	outs := map[string]bool{}
	for _, it := range items {
		switch itemType(it) {
		case "function_call":
			if id := pairIDOf(it); id != "" {
				calls[id] = true
			}
		case "function_call_output":
			if id := pairIDOf(it); id != "" {
				outs[id] = true
			}
		}
	}
	paired := make(map[string]bool, len(calls))
	for id := range calls {
		if outs[id] {
			paired[id] = true
		}
	}
	return paired
}

// itemType 取 item 的 type；缺 type 但有 role 的按 message 处理
// （官方历史回灌里 message item 有时省略 type）。
func itemType(it map[string]any) string {
	if t, _ := it["type"].(string); t != "" {
		return t
	}
	if _, ok := it["role"]; ok {
		return "message"
	}
	return ""
}

// messageItem 把一条 Responses message item 翻成 Chat message。
// 返回 nil 表示这条 item 没有可发送的内容（调用方跳过）。
func messageItem(it map[string]any) map[string]any {
	role, _ := it["role"].(string)
	if role == "" {
		return nil
	}
	content, ok := chatContent(it["content"])
	if !ok {
		return nil
	}
	return map[string]any{"role": normalizeRole(role), "content": content}
}

// normalizeRole Responses 的 developer 是 system 的别名（上游 role 白名单不含 developer）。
func normalizeRole(role string) string {
	if role == "developer" {
		return "system"
	}
	return role
}

// chatContent 把 Responses 的 content 数组翻成 Chat 的 content。
//   - 纯文本且只有一个片段 → 直接给字符串（最兼容的形态，上游与各类客户端都认）；
//   - 含图片 → 给 [{type:"text"},{type:"image_url"}] 数组；
//   - 空/无法识别 → ok=false。
func chatContent(v any) (any, bool) {
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil, false
		}
		return s, true
	}
	parts, ok := v.([]any)
	if !ok || len(parts) == 0 {
		return nil, false
	}
	var (
		text   strings.Builder
		out    = make([]any, 0, len(parts))
		images int
	)
	for _, raw := range parts {
		p, _ := raw.(map[string]any)
		if p == nil {
			continue
		}
		typ, _ := p["type"].(string)
		switch typ {
		case "input_text", "output_text", "text", "summary_text":
			if t, _ := p["text"].(string); t != "" {
				text.WriteString(t)
				out = append(out, map[string]any{"type": "text", "text": t})
			}
		case "input_image":
			if url := imageURLOf(p); url != "" {
				images++
				out = append(out, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": url},
				})
			}
		case "refusal":
			// 拒答片段：Chat 无对应结构，丢弃（内容为空时整条消息会被跳过）。
		}
	}
	if len(out) == 0 {
		return nil, false
	}
	if images == 0 {
		// 全文本：给字符串形态（比数组形态兼容面更宽）。
		if s := text.String(); strings.TrimSpace(s) != "" {
			return s, true
		}
		return nil, false
	}
	return out, true
}

// imageURLOf 取 input_image 的 url：官方是字符串，部分实现给 {"url": ...} 对象。
func imageURLOf(p map[string]any) string {
	switch v := p["image_url"].(type) {
	case string:
		return v
	case map[string]any:
		if s, _ := v["url"].(string); s != "" {
			return s
		}
	}
	if s, _ := p["url"].(string); s != "" {
		return s
	}
	return ""
}

// functionCallOutputMessage 把 Responses 的 function_call_output item 翻成 Chat 的
// tool 消息（role:"tool" + tool_call_id 配对）。
func functionCallOutputMessage(it map[string]any) map[string]any {
	out := outputText(it["output"])
	return map[string]any{
		"role":         "tool",
		"tool_call_id": callIDOf(it),
		"content":      out,
	}
}

// callIDOf 取工具的配对 ID：官方字段是 call_id，历史回灌里可能是 id。
func callIDOf(it map[string]any) string {
	if v, _ := it["call_id"].(string); v != "" {
		return v
	}
	if v, _ := it["id"].(string); v != "" {
		return v
	}
	return newID("call")
}

// outputText 把 function_call_output 的 output 拉平成字符串：
// 官方允许 string，也允许 [{type:"input_text",text:...}] 数组。
func outputText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if parts, ok := v.([]any); ok {
		var b strings.Builder
		for _, raw := range parts {
			if p, ok := raw.(map[string]any); ok {
				if t, _ := p["text"].(string); t != "" {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(raw)
}

// chatTools 把 Responses 的扁平工具定义翻成 Chat 的 {"type":"function","function":{...}}。
// 已经不是 function 类型的工具（web_search / file_search / computer_use 等）
// 一律丢弃：Chat 上游没有这些内置工具的入口，透传只会 400。
func chatTools(tools []map[string]any) []any {
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		if typ, _ := t["type"].(string); typ != "" && typ != "function" {
			continue
		}
		// 已是 Chat 形态（含 function 子对象）：取子对象，形状原样保留。
		if fn, ok := t["function"].(map[string]any); ok {
			out = append(out, map[string]any{"type": "function", "function": fn})
			continue
		}
		name, _ := t["name"].(string)
		if strings.TrimSpace(name) == "" {
			continue
		}
		fn := map[string]any{"name": name}
		for _, k := range []string{"description", "parameters", "strict"} {
			if v, ok := t[k]; ok && v != nil {
				fn[k] = v
			}
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// translateToolChoice 把 Responses 的 tool_choice 翻成 Chat 形态。
// 标量（auto/none/required）原样；对象形态统一成 {"type":"function","function":{"name":...}}
// ——上游预处理管线两种形态都认（也认扁平的 name），这里只保证是规范 Chat 形态。
func translateToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s = strings.TrimSpace(s); s == "" {
			return nil
		}
		return s
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	if fn, ok := m["function"].(map[string]any); ok {
		return map[string]any{"type": "function", "function": fn}
	}
	name, _ := m["name"].(string)
	if strings.TrimSpace(name) == "" {
		return m
	}
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
}

// reasoningEffort 取 Responses 的 reasoning.effort（minimal/low/medium/high/xhigh）。
// 上游管线按模型 supportedEfforts 降级，未知模型/未知档位一律透传。
func reasoningEffort(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var r struct {
		Effort string `json:"effort"`
	}
	if json.Unmarshal(raw, &r) != nil {
		return ""
	}
	return strings.TrimSpace(r.Effort)
}

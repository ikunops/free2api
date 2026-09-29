package responses

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRequestToChatCodexShape 覆盖 Codex CLI 真实发出的 Responses 请求形态：
// instructions + input（message / function_call / function_call_output）+ tools
// （扁平 function）+ tool_choice 对象 + reasoning.effort + prompt_cache_key。
func TestRequestToChatCodexShape(t *testing.T) {
	raw := []byte(`{
	  "model": "cn:wb-deepseek-v4.1-flash-x011",
	  "instructions": "You are Codex.",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"列出当前目录"}]},
	    {"type":"reasoning","summary":[{"type":"summary_text","text":"thinking..."}]},
	    {"type":"function_call","call_id":"call_1","name":"shell","arguments":"{\"command\":\"ls\"}"},
	    {"type":"function_call_output","call_id":"call_1","output":"a.go\nb.go"},
	    {"type":"item_reference","id":"ref_1"}
	  ],
	  "tools": [{"type":"function","name":"shell","description":"run a command",
	             "parameters":{"type":"object"},"strict":false}],
	  "tool_choice": {"type":"function","name":"shell"},
	  "parallel_tool_calls": true,
	  "reasoning": {"effort":"medium","summary":"auto"},
	  "store": false,
	  "stream": true,
	  "include": ["reasoning.encrypted_content"],
	  "prompt_cache_key": "pck-42"
	}`)
	out, err := RequestToChat(raw)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("翻译结果不是合法 JSON: %v", err)
	}
	if got["model"] != "cn:wb-deepseek-v4.1-flash-x011" {
		t.Errorf("model 未原样保留: %v", got["model"])
	}
	if got["stream"] != true {
		t.Errorf("stream 应为 true: %v", got["stream"])
	}
	// Responses 专有字段一律不许透传（上游字段白名单会 400）。
	for _, k := range []string{"instructions", "input", "include", "store", "reasoning"} {
		if _, ok := got[k]; ok {
			t.Errorf("Responses 专有字段 %q 不该出现在 Chat body 里", k)
		}
	}
	if got["prompt_cache_key"] != "pck-42" {
		t.Errorf("prompt_cache_key 应透传（会话粘性 + 上游前缀缓存）: %v", got["prompt_cache_key"])
	}
	if got["reasoning_effort"] != "medium" {
		t.Errorf("reasoning.effort 应翻成 reasoning_effort: %v", got["reasoning_effort"])
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages 条数：system + user + function_call + tool = 4，实得 %d: %v", len(msgs), msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" || m0["content"] != "You are Codex." {
		t.Errorf("instructions 应成为首条 system: %v", m0)
	}
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "列出当前目录" {
		t.Errorf("user 消息应为纯字符串形态: %v", m1)
	}
	m2, _ := msgs[2].(map[string]any)
	if m2["role"] != "assistant" {
		t.Errorf("function_call 应翻成 assistant 消息: %v", m2)
	}
	if _, ok := m2["content"]; !ok {
		t.Errorf("assistant 消息应显式带 content:null: %v", m2)
	}
	tcs, _ := m2["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant 应有 1 个 tool_call: %v", m2)
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Errorf("tool_call.id 应为 call_id: %v", tc)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "shell" {
		t.Errorf("tool_call.function.name: %v", fn)
	}
	m3, _ := msgs[3].(map[string]any)
	if m3["role"] != "tool" || m3["tool_call_id"] != "call_1" {
		t.Errorf("function_call_output 应翻成 role=tool + tool_call_id: %v", m3)
	}
	if m3["content"] != "a.go\nb.go" {
		t.Errorf("tool 结果内容应拉平成字符串: %v", m3["content"])
	}
	// tools 扁平 → 嵌套。
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools 应保留 1 个: %v", got["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	tfn, _ := tool["function"].(map[string]any)
	if tool["type"] != "function" || tfn["name"] != "shell" || tfn["description"] != "run a command" {
		t.Errorf("tools 应翻成 {type:function, function:{...}}: %v", tool)
	}
	// tool_choice 对象 → Chat 规范形态。
	tcOut, _ := got["tool_choice"].(map[string]any)
	tcFn, _ := tcOut["function"].(map[string]any)
	if tcOut["type"] != "function" || tcFn["name"] != "shell" {
		t.Errorf("tool_choice 应翻成 {type:function,function:{name}}: %v", got["tool_choice"])
	}
}

// TestRequestToChatStringInput 裸字符串 input + developer 角色归一。
func TestRequestToChatStringInput(t *testing.T) {
	out, err := RequestToChat([]byte(`{"model":"m","input":"hi","instructions":[{"type":"input_text","text":"be nice"}]}`))
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应为 system + user 两条: %v", msgs)
	}
	m0, _ := msgs[0].(map[string]any)
	if m0["content"] != "be nice" {
		t.Errorf("instructions 数组形态应拉平成字符串: %v", m0)
	}
	m1, _ := msgs[1].(map[string]any)
	if m1["role"] != "user" || m1["content"] != "hi" {
		t.Errorf("裸字符串 input 应成为 user 消息: %v", m1)
	}
}

// TestRequestToChatDeveloperRole developer 是 system 的别名（上游 role 白名单不含它）。
func TestRequestToChatDeveloperRole(t *testing.T) {
	out, err := RequestToChat([]byte(`{"model":"m","input":[{"type":"message","role":"developer","content":"rules"}]}`))
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	msgs, _ := got["messages"].([]any)
	m0, _ := msgs[0].(map[string]any)
	if m0["role"] != "system" {
		t.Errorf("developer 应归一为 system: %v", m0)
	}
}

// TestRequestToChatErrors 缺 model / 非 JSON / 空 input 都要报错（handler 翻成 400）。
func TestRequestToChatErrors(t *testing.T) {
	cases := map[string]string{
		"非 JSON":   `not json`,
		"缺 model":  `{"input":"hi"}`,
		"input 为空": `{"model":"m"}`,
	}
	for name, body := range cases {
		if _, err := RequestToChat([]byte(body)); err == nil {
			t.Errorf("%s：应当报错", name)
		}
	}
}

// TestFromChat 非流式：chat.completion（正文 + 工具调用）→ Responses 对象。
func TestFromChat(t *testing.T) {
	chat := map[string]any{
		"id": "chatcmpl-x", "object": "chat.completion", "created": float64(1700000000),
		"model": "bare",
		"choices": []any{map[string]any{
			"index": float64(0),
			"message": map[string]any{
				"role":    "assistant",
				"content": "先看一下目录。",
				"tool_calls": []any{map[string]any{
					"id": "call_9", "type": "function",
					"function": map[string]any{"name": "shell", "arguments": `{"command":"ls"}`},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{
			"prompt_tokens": float64(10), "completion_tokens": float64(7),
			"total_tokens": float64(17), "prompt_cache_hit_tokens": float64(4),
		},
	}
	got := FromChat(chat, "cn:pref-model-x011")
	if got["object"] != "response" || got["status"] != "completed" {
		t.Fatalf("response 骨架不对: %v", got)
	}
	if got["model"] != "cn:pref-model-x011" {
		t.Errorf("model 应回显客户端原始名: %v", got["model"])
	}
	if got["created_at"] != int64(1700000000) {
		t.Errorf("created_at 应沿用上游 created: %v", got["created_at"])
	}
	out, _ := got["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output 应为 message + function_call 两条: %v", out)
	}
	m, _ := out[0].(map[string]any)
	if m["type"] != "message" || m["role"] != "assistant" || m["status"] != "completed" {
		t.Errorf("message item 形态: %v", m)
	}
	content, _ := m["content"].([]any)
	c0, _ := content[0].(map[string]any)
	if c0["type"] != "output_text" || c0["text"] != "先看一下目录。" {
		t.Errorf("content 应为 output_text 片段: %v", content)
	}
	if _, ok := c0["annotations"]; !ok {
		t.Errorf("output_text 应带 annotations 数组: %v", c0)
	}
	fc, _ := out[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_9" || fc["name"] != "shell" {
		t.Errorf("function_call item 形态: %v", fc)
	}
	if fc["arguments"] != `{"command":"ls"}` {
		t.Errorf("arguments 原文应保留: %v", fc["arguments"])
	}
	u, _ := got["usage"].(map[string]any)
	if u["input_tokens"] != int64(10) || u["output_tokens"] != int64(7) || u["total_tokens"] != int64(17) {
		t.Errorf("usage 三段应换算: %v", u)
	}
	det, _ := u["input_tokens_details"].(map[string]any)
	if det["cached_tokens"] != int64(4) {
		t.Errorf("缓存命中应进 input_tokens_details.cached_tokens: %v", det)
	}
	if _, ok := u["output_tokens_details"]; !ok {
		t.Errorf("usage 应带 output_tokens_details: %v", u)
	}
}

// TestStreamChatEvents 流式：chat SSE → Responses 事件序列。
func TestStreamChatEvents(t *testing.T) {
	sse := strings.Join([]string{
		`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","created":1700000000,"model":"bare","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"你好"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"reasoning_content":"内心戏"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"，世界"},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_7","type":"function","function":{"name":"shell","arguments":"{\"command\":"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]},"finish_reason":null}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		``,
		`data: {"id":"chatcmpl-1","choices":[],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	if err := StreamChat(rec, strings.NewReader(sse), "cn:m-x011", nil); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	body := rec.Body.String()
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type: %q", ct)
	}
	if strings.Contains(body, "[DONE]") {
		t.Errorf("Responses 流不该发 data: [DONE]：\n%s", body)
	}
	for _, want := range []string{
		"event: response.created",
		"event: response.output_item.added",
		"event: response.content_part.added",
		`event: response.output_text.delta`,
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		"event: response.completed",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少事件 %q", want)
		}
	}
	// reasoning_content 必须被丢弃（不发不可解析的 reasoning 事件）。
	if strings.Contains(body, "内心戏") {
		t.Errorf("reasoning_content 不该出现在 Responses 流里：\n%s", body)
	}
	// 正文拼接正确。
	var completed map[string]any
	for _, blk := range strings.Split(body, "\n\n") {
		if !strings.HasPrefix(blk, "event: response.completed") {
			continue
		}
		data := blk[strings.Index(blk, "data: ")+len("data: "):]
		if err := json.Unmarshal([]byte(data), &completed); err != nil {
			t.Fatalf("response.completed 不是合法 JSON: %v (%q)", err, data)
		}
	}
	if completed == nil {
		t.Fatalf("没有解析到 response.completed:\n%s", body)
	}
	resp, _ := completed["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("completed 里的 status: %v", resp["status"])
	}
	if resp["model"] != "cn:m-x011" {
		t.Errorf("completed 里的 model 应回显客户端原始名: %v", resp["model"])
	}
	items, _ := resp["output"].([]any)
	if len(items) != 2 {
		t.Fatalf("output 应为 message + function_call: %v", items)
	}
	m, _ := items[0].(map[string]any)
	mc, _ := m["content"].([]any)
	mc0, _ := mc[0].(map[string]any)
	if mc0["text"] != "你好，世界" {
		t.Errorf("正文拼接：%v", mc0["text"])
	}
	fc, _ := items[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_7" {
		t.Errorf("function_call item: %v", fc)
	}
	if fc["arguments"] != `{"command":"ls"}` {
		t.Errorf("arguments 拼接：%v", fc["arguments"])
	}
	u, _ := resp["usage"].(map[string]any)
	if u["input_tokens"] != float64(11) || u["output_tokens"] != float64(5) || u["total_tokens"] != float64(16) {
		t.Errorf("usage 应取上游末帧: %v", u)
	}
}

// TestStreamChatUpstreamError 上游 error 帧 → response.failed（且带 gateway_hint）。
func TestStreamChatUpstreamError(t *testing.T) {
	sse := "data: {\"error\":{\"code\":6004,\"message\":\"rate limited\"}}\n\n"
	rec := httptest.NewRecorder()
	hint := func(string) string { return "rate limited by upstream; retry after reset" }
	if err := StreamChat(rec, strings.NewReader(sse), "m", hint); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("应发 response.failed:\n%s", body)
	}
	if !strings.Contains(body, "rate limited") || !strings.Contains(body, "gateway_hint") {
		t.Errorf("failed 事件应带上游原文与 gateway_hint:\n%s", body)
	}
	if strings.Contains(body, "response.completed") {
		t.Errorf("失败流不该再发 response.completed:\n%s", body)
	}
}

// TestStreamChatEmpty 上游 200 但零有效帧 → response.failed + 空流哨兵。
func TestStreamChatEmpty(t *testing.T) {
	rec := httptest.NewRecorder()
	err := StreamChat(rec, strings.NewReader("data: [DONE]\n\n"), "m", nil)
	if err == nil {
		t.Fatalf("空流应返回错误")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: response.failed") {
		t.Fatalf("空流应给明确失败事件:\n%s", body)
	}
	if !strings.Contains(body, "empty upstream stream") {
		t.Errorf("失败原因应可读:\n%s", body)
	}
}

// TestStreamChatWritesSSEHeaders 头已发出（200）时仍要保证 SSE 三件套齐全。
func TestStreamChatWritesSSEHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\ndata: [DONE]\n\n"
	if err := StreamChat(rec, strings.NewReader(sse), "m", nil); err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if rec.Code != http.StatusOK {
		t.Errorf("状态码: %d", rec.Code)
	}
	for _, k := range []string{"Cache-Control", "X-Accel-Buffering"} {
		if rec.Header().Get(k) == "" {
			t.Errorf("缺少响应头 %s", k)
		}
	}
}

package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// 孤儿 function_call（有 call 无 output）翻出去会被 Chat 上游以
// 「11148 tool calls and tool results do not match」/「11133 Invalid request
// parameters」拒绝，且换哪个账号都一样。真实来源：Codex 回合被中断后历史里
// 留下的悬空调用（store:false 全量重发，每一轮都带着）。必须在翻译层丢弃，
// 且不能连累同请求里正常配对的调用。
func TestRequestToChatDropsOrphanFunctionCall(t *testing.T) {
	raw := []byte(`{
	  "model": "m1",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"列出目录"}]},
	    {"type":"function_call","call_id":"call_ok","name":"shell","arguments":"{\"command\":\"ls\"}"},
	    {"type":"function_call_output","call_id":"call_ok","output":"a.go"},
	    {"type":"function_call","call_id":"call_lost","name":"shell","arguments":"{\"command\":\"restart\"}"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
	  ]
	}`)
	out, err := RequestToChat(raw)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 期望 4 条：user、assistant(tool_calls=call_ok)、tool(call_ok)、user。
	if len(got.Messages) != 4 {
		t.Fatalf("messages = %d 条, want 4: %s", len(got.Messages), out)
	}
	if got.Messages[1]["tool_calls"] == nil {
		t.Fatalf("配对的 tool_calls 被误删: %s", out)
	}
	if s := string(out); strings.Contains(s, "call_lost") || strings.Contains(s, "restart") {
		t.Fatalf("孤儿 call 泄漏进翻译结果: %s", s)
	}
}

// 反向孤儿：有 output 无 call（历史压缩截断的另一半）同样是上游 400 源，也要丢。
func TestRequestToChatDropsOrphanFunctionCallOutput(t *testing.T) {
	raw := []byte(`{
	  "model": "m1",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
	    {"type":"function_call_output","call_id":"call_ghost","output":"残留结果"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
	  ]
	}`)
	out, err := RequestToChat(raw)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("messages = %d 条, want 2（两条 user）: %s", len(got.Messages), out)
	}
	for _, m := range got.Messages {
		if m["role"] == "tool" {
			t.Fatalf("孤立 tool 消息未丢弃: %s", out)
		}
	}
}

// 极端情况：input 里全是没有 id 字段的孤儿——丢弃后仍剩 user 消息可发，
// 不能因为全丢而报「input 里没有可发送的消息」。
func TestRequestToChatAllOrphansStillSendsUserMessage(t *testing.T) {
	raw := []byte(`{
	  "model": "m1",
	  "input": [
	    {"type":"function_call","name":"shell","arguments":"{}"},
	    {"type":"function_call_output","output":"残骸"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"只回复OK"}]}
	  ]
	}`)
	out, err := RequestToChat(raw)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Messages) != 1 || got.Messages[0]["role"] != "user" {
		t.Fatalf("应只剩单条 user 消息: %s", out)
	}
}

// call_id 缺失时以 id 字段兜底配对（历史回灌形态）：同值即视为配对成功。
func TestRequestToChatPairsByIDFallback(t *testing.T) {
	raw := []byte(`{
	  "model": "m1",
	  "input": [
	    {"type":"function_call","id":"fc_1","name":"shell","arguments":"{}"},
	    {"type":"function_call_output","id":"fc_1","output":"ok"}
	  ]
	}`)
	out, err := RequestToChat(raw)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(got.Messages) != 2 {
		t.Fatalf("id 兜底配对失败, messages = %d 条: %s", len(got.Messages), out)
	}
}

// 同一轮的多个 function_call 必须合并成一条 assistant 挂 tool_calls:[A,B]。
// 若逐条各翻一条 assistant，上游会看到 tool(A) 跟在 assistant(B) 之后——
// 按「tool calls and tool results do not match」拒绝（实测 11148/11133）。
func TestRequestToChatMergesConsecutiveFunctionCalls(t *testing.T) {
	raw := []byte(`{
	  "model": "m1",
	  "input": [
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"探测两个端口"}]},
	    {"type":"function_call","call_id":"call_A","name":"shell","arguments":"{\"command\":\"probe 8317\"}"},
	    {"type":"function_call","call_id":"call_B","name":"shell","arguments":"{\"command\":\"probe 7864\"}"},
	    {"type":"function_call_output","call_id":"call_A","output":"A结果"},
	    {"type":"function_call_output","call_id":"call_B","output":"B结果"},
	    {"type":"message","role":"user","content":[{"type":"input_text","text":"汇总"}]}
	  ]
	}`)
	out, err := RequestToChat(raw)
	if err != nil {
		t.Fatalf("RequestToChat: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	// 期望 5 条：user、assistant(tool_calls=[A,B])、tool(A)、tool(B)、user。
	if len(got.Messages) != 5 {
		t.Fatalf("messages = %d 条, want 5: %s", len(got.Messages), out)
	}
	tcs, _ := got.Messages[1]["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("assistant 应挂 2 个 tool_calls, got %d: %s", len(tcs), out)
	}
	if got.Messages[2]["role"] != "tool" || got.Messages[3]["role"] != "tool" {
		t.Fatalf("tool 结果应连续跟随合并后的 assistant: %s", out)
	}
	// 绝不允许出现连续两条 assistant。
	for i := 1; i < len(got.Messages); i++ {
		if got.Messages[i]["role"] == "assistant" && got.Messages[i-1]["role"] == "assistant" {
			t.Fatalf("出现连续 assistant 消息: %s", out)
		}
	}
}

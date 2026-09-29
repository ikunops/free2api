package upstream

import (
	"strings"
	"testing"
)

// 本文件覆盖 issue #142：Aggregate 的 delta / message 两路 content latch 对**空串**
// 的误置位。两路同一口径：content 非空才认「已取到正文」并 latch；空串不占名额。
//
// 缺陷形态（两处，均为空串帧吞 latch → 后续真正文被 !gotAnyContent 守卫静默拒绝）：
//   - delta 分支：`if txt, ok := delta["content"].(string); ok` 空串也 WriteString + 置位。
//     OpenAI 风格 role-only 首帧（delta.content="" 或缺省）即触发——后续整条
//     message.content="真正文" 被拒，输出 ""。
//   - message 分支：`if txt, ok := msg["content"].(string); ok` 同样空串置位。
//     帧1 message.content="" 吞 latch，帧2 真正文 message.content="Hello" 被拒。
//
// 规约（PR 正文同口径）：
//  S1 content 非空才认「已取到正文」并 latch；空串不占名额（两路同一口径）。
//  S2 delta 优先语义不变：delta 已取到非空正文后，message 回退分支整体跳过。
//  S3 message 整条只采一次（#134/#137 成果不回退）。
//  S4 空 content 帧的 role/reasoning_content/tool_calls 照常合并（不因 content 空丢其它字段）。
//  S5 全流皆空帧：正文为空串、不报错（既有行为）。

// wantContent 断言聚合结果的 message.content 等于 want。
func wantContent(t *testing.T, raw string, want string) {
	t.Helper()
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	msg, _ := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg == nil {
		t.Fatalf("message missing: %#v", resp["choices"])
	}
	if got, _ := msg["content"].(string); got != want {
		t.Errorf("content=%q want %q", got, want)
	}
}

// S1a（RED 实证之一，issue #142 message 路缺陷）：帧1 整条 message.content=""，
// 帧2 整条 message.content="Hello"。缺陷下帧1 吞 latch → 帧2 被 !gotAnyContent 拒，
// 输出 ""。修复后空串不置位，帧2 正常采入。
func TestAggregateEmptyMessageFrameDoesNotLatch(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"Hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"Hello")
}

// S1b（RED 实证之二，issue #142 delta 路缺陷）：首帧 OpenAI 风格 role-only
// delta（content=""），后续整条 message.content="真正文"。缺陷下首帧吞 latch →
// message 被拒，输出 ""。修复后 message 正常采入。#134/#137 未覆盖此缺口。
func TestAggregateEmptyDeltaFrameDoesNotLatch(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"真正文\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"真正文")
}

// S1c：空 delta content 帧不 latch 对纯 delta 流同样成立——role-only 首帧（content
// 键缺失，与既有 fixture 同形态）后跟正常 delta 正文流，正文完整拼接。
func TestAggregateDeltaStreamWithEmptyFirstFrame(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hel\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"Hello")
}

// S1d：空串与真 delta 正文交错——空串帧不追加任何字节，也不影响后续拼接。
func TestAggregateDeltaStreamInterleavedEmptyContent(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"He\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"llo\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"Hello")
}

// S2 delta 优先：delta 先给非空正文，之后的整条 message 不再追加（#134 latch
// 语义保持），且前置的空 message 帧不 latch、不吞掉后续 delta 正文。
func TestAggregateDeltaPriorityOverMessage(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"delta\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"message\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"delta")
}

// S3 守卫不回退（#134 成果）：整条 message 采到非空正文后 latch，后续整条
// message 不重复追加——空 content 帧（首帧空 message）不得绕过该守卫。
func TestAggregateMessageOnlyTakenOnceGuardKept(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"abc\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":5}}\n\n"+
			"data: [DONE]\n\n",
		"abc")
}

// S4 空 content 帧的其它字段照常合并：空 message 帧带 role / reasoning_content /
// tool_calls 时，正文 latch 不受影响、字段不丢（content 空 ≠ 丢字段）。
func TestAggregateEmptyFrameFieldsStillMerged(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\",\"reasoning_content\":\"think\"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\",\"tool_calls\":[{\"id\":\"tc1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if got, _ := msg["content"].(string); got != "done" {
		t.Errorf("content=%q want %q", got, "done")
	}
	if got, _ := msg["reasoning_content"].(string); got != "think" {
		t.Errorf("reasoning_content=%q want %q（空 content 帧的 reasoning 不应丢）", got, "think")
	}
	// 生产代码聚合 tool_calls 为 []map[string]any（sse.go calls 构造），按此类型断言。
	tcs, _ := msg["tool_calls"].([]map[string]any)
	if len(tcs) != 1 {
		t.Fatalf("tool_calls len=%d want 1（空 content 帧的 tool_calls 不应丢）: %#v", len(tcs), msg["tool_calls"])
	}
	tc := tcs[0]
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("tool name=%v want get_weather", fn["name"])
	}
}

// S5 全流皆空帧：正文为空串、不报错（既有行为保持；validEvents>0 不触发空流哨兵）。
func TestAggregateAllEmptyContentFramesYieldsEmptyContent(t *testing.T) {
	wantContent(t,
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n"+
			"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":1}}\n\n"+
			"data: [DONE]\n\n",
		"")
}

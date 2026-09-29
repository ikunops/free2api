package upstream

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestCleanupOrphanToolCallsNoTraffic 无工具流量 → 零改动（changed=false）。
func TestCleanupOrphanToolCallsNoTraffic(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "hi"},
		map[string]any{"role": "user", "content": "hello"},
		map[string]any{"role": "assistant", "content": "hi there"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("no-traffic should be unchanged")
	}
	if &out[0] != &messages[0] {
		t.Fatal("no-traffic should return the original slice")
	}
}

// TestCleanupOrphanToolCallWithoutResult 孤儿 tool_call（无结果）→ 删除 tool_calls 键。
func TestCleanupOrphanToolCallWithoutResult(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "Grep", "arguments": "{}"}},
		}},
		map[string]any{"role": "user", "content": "continue"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("orphan tool_call should be cleaned")
	}
	asst := out[0].(map[string]any)
	if _, ok := asst["tool_calls"]; ok {
		t.Fatalf("tool_calls should be removed, got %#v", asst)
	}
}

// TestCleanupOrphanPartialBatch 一批两个 tool_call，只有 c1 拿到结果 → 按 keepCalls
// 对称裁剪：调用侧只留 c1、无结果的 c2 被剔，两侧不残留半截配对。
func TestCleanupOrphanPartialBatch(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
			map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "ok"},
		map[string]any{"role": "user", "content": "next"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("partial batch should be cleaned")
	}
	asst := out[0].(map[string]any)
	tcs, ok := asst["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("只应保留有结果的 c1，实际 %#v", asst)
	}
	if id, _ := tcs[0].(map[string]any)["id"].(string); id != "c1" {
		t.Fatalf("保留的应是 c1，实际 %s", id)
	}
	// 关键不变式：出站任何 tool 结果都必须有对应 tool_call，任何 tool_call 都必须有
	// 结果——否则上游判 11148（tool calls and tool results do not match）。
	assertPairingSymmetric(t, out)
}

// assertPairingSymmetric 断言出站消息两侧配对对称：每个 tool_call id 都有结果，
// 每个 tool 结果的 id 都有调用。task 要求「只留有结果配对的调用、孤儿结果整删，
// 两侧同口径」——该断言是两侧同口径的直接表达。
func assertPairingSymmetric(t *testing.T, msgs []any) {
	t.Helper()
	allCalls := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if role, _ := mm["role"].(string); role != "assistant" {
			continue
		}
		tcs, ok := mm["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if id, _ := tc["id"].(string); id != "" {
				allCalls[id] = true
			}
		}
	}
	resultIDs := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if role, _ := mm["role"].(string); role != "tool" {
			continue
		}
		id, _ := mm["tool_call_id"].(string)
		resultIDs[id] = true
		if !allCalls[id] {
			t.Fatalf("残留孤儿 tool 结果 %s——上游会判 11148：%#v", id, msgs)
		}
	}
	for id := range allCalls {
		if !resultIDs[id] {
			t.Fatalf("残留无结果 tool_call %s——上游会判 11148：%#v", id, msgs)
		}
	}
}

// TestCleanupOrphanResultOnly 孤儿 tool 结果（无对应 tool_call）→ 整条消息删除。
func TestCleanupOrphanResultOnly(t *testing.T) {
	messages := []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "tool", "tool_call_id": "ghost", "content": "orphan"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if !changed {
		t.Fatal("orphan result should be cleaned")
	}
	if len(out) != 1 || out[0].(map[string]any)["role"] != "user" {
		t.Fatalf("orphan tool message should be removed, got %#v", out)
	}
}

// TestCleanupOrphanPairingPreserved 正例零改动：完整配对的多 tool_call 轮 + 正常文本轮，
// 所有字段原样保留。
func TestCleanupOrphanPairingPreserved(t *testing.T) {
	grepArgs := `{"pattern":"foo","path":"."}`
	readArgs := `{"file_path":"a.go"}`
	messages := []any{
		map[string]any{"role": "user", "content": "search"},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "Grep", "arguments": grepArgs}},
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "Read", "arguments": readArgs}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "3 matches"},
		map[string]any{"role": "tool", "tool_call_id": "call_2", "content": "file body"},
		map[string]any{"role": "user", "content": "keep going"},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("fully paired round-trip must be zero-change")
	}
	if len(out) != 5 {
		t.Fatalf("message count changed: %d", len(out))
	}
	asst := out[1].(map[string]any)
	tcs := asst["tool_calls"].([]any)
	if len(tcs) != 2 {
		t.Fatalf("tool_calls dropped from paired round-trip: %#v", asst)
	}
	// 函数参数原样保留（引用不变）。
	fn := tcs[0].(map[string]any)["function"].(map[string]any)
	if fn["arguments"] != grepArgs {
		t.Fatalf("call_1 arguments mutated: %v", fn["arguments"])
	}
}

// TestCleanupOrphanOutOfOrderToolBeforeResult 乱序：tool 结果消息出现在 assistant
// tool_call 之前（不按顺序但 id 齐全）→ 仍保留（按 id 集合配对，与顺序无关）。
func TestCleanupOrphanOutOfOrderToolBeforeResult(t *testing.T) {
	messages := []any{
		map[string]any{"role": "tool", "tool_call_id": "call_9", "content": "res"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "call_9", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
	}
	out, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("id-complete out-of-order pairing must be preserved")
	}
	if len(out) != 2 {
		t.Fatalf("message count changed: %d", len(out))
	}
}

// TestCleanupOrphanDuplicateID 重复 tool_call id（两处引用同一结果 id）：
// 结果侧唯一、调用侧重复——每次按集合取并，保持「id 命中结果即保留」的最宽口径。
func TestCleanupOrphanDuplicateID(t *testing.T) {
	messages := []any{
		map[string]any{"role": "tool", "tool_call_id": "dup", "content": "r"},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "dup", "type": "function", "function": map[string]any{"name": "a", "arguments": "{}"}},
		}},
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "dup", "type": "function", "function": map[string]any{"name": "b", "arguments": "{}"}},
		}},
	}
	_, changed := cleanupOrphanToolCalls(messages)
	if changed {
		t.Fatal("duplicate id referencing an existing result must be preserved (widest keep)")
	}
}

// TestCleanupOrphanThroughPrepareBody 集成：孤儿 tool_call 经 PrepareBodyOpt 全链路被剔除，
// 且与消息顺序无关。
func TestCleanupOrphanThroughPrepareBody(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"assistant","tool_calls":[{"id":"bad","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"user","content":"hi"}
	]}`
	out := PrepareBodyOpt([]byte(body), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	asst := msgs[0].(map[string]any)
	if _, ok := asst["tool_calls"]; ok {
		t.Fatalf("orphan tool_calls should be stripped by PrepareBodyOpt, got %#v", asst)
	}
}

// TestRepackToolResultBlocksInsertedNotice 复刻真实会话的卡死结构：Codex 的
// <image_resize_notice> 作为 developer 消息插在并行 tool 结果中间，上游判
// 11148 tool_call_sequence_broken。修复后结果必须连续、插入物后移、内容不变。
func TestRepackToolResultBlocksInsertedNotice(t *testing.T) {
	notice := "<image_resize_notice>resized</image_resize_notice>"
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c00", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
			map[string]any{"id": "c01", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "img0"},
		map[string]any{"role": "developer", "content": notice},
		map[string]any{"role": "tool", "tool_call_id": "c01", "content": "img1"},
		map[string]any{"role": "user", "content": "next"},
	}
	out, changed := repackToolResultBlocks(messages)
	if !changed {
		t.Fatal("插入物应触发重排")
	}
	if len(out) != 5 {
		t.Fatalf("消息数不应变化，实际 %d", len(out))
	}
	// 只调顺序不改内容：developer 消息原 map 对象后移（引用不变，深比较外的同一实例）。
	if out[3].(map[string]any)["content"] != notice {
		t.Fatalf("插入物内容被改动：%#v", out[3])
	}
	// reflect.ValueOf 同 interface 值的 Pointer 比较同一 map 实例（&out[3] != &messages[2]
	// 只是比较 slice 元素地址，恒不等，不能这么断言）。
	if reflect.ValueOf(out[3]).Pointer() != reflect.ValueOf(messages[2]).Pointer() {
		t.Fatalf("插入物应是原 map 对象（只调顺序不改内容）：%#v", out[3])
	}
	// 顺序：assistant 之后紧跟两条 tool 结果，developer 被移到其后。
	wantRoles := []string{"assistant", "tool", "tool", "developer", "user"}
	for i, w := range wantRoles {
		got, _ := out[i].(map[string]any)["role"].(string)
		if got != w {
			t.Fatalf("out[%d] 角色应为 %s，实际 %s（%#v）", i, w, got, out)
		}
	}
	// 结果顺序保持 c00 -> c01。
	if id, _ := out[1].(map[string]any)["tool_call_id"].(string); id != "c00" {
		t.Fatalf("第一份结果应为 c00，实际 %s", id)
	}
	if id, _ := out[2].(map[string]any)["tool_call_id"].(string); id != "c01" {
		t.Fatalf("第二份结果应为 c01，实际 %s", id)
	}
}

// TestRepackToolResultBlocksNoInsert 无插入物（完整连续配对）→ 零改动（返回原 slice）。
func TestRepackToolResultBlocksNoInsert(t *testing.T) {
	messages := []any{
		map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"},
		map[string]any{"role": "user", "content": "n"},
	}
	out, changed := repackToolResultBlocks(messages)
	if changed {
		t.Fatal("完整配对不应改动")
	}
	if &out[0] != &messages[0] {
		t.Fatal("零改动应返回原 slice")
	}
}

// TestRepackToolResultBlocksNextGroupHeadNotSwallowed 回归（真实会话 msg[181] 形态）：
// 下一组 assistant.tool_calls 紧跟上一组结果时，绝不能被上一组的收集循环当「插入物」
// 吞掉——否则它自己那批结果永远得不到重排，上游照旧判 11148。
func TestRepackToolResultBlocksNextGroupHeadNotSwallowed(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "user", "content": "go"},
		// 第 1 组：exec_command ×2（结果连续，无插入物）。
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c00", "type": "function", "function": map[string]any{"name": "exec_command", "arguments": "{}"}},
			map[string]any{"id": "c01", "type": "function", "function": map[string]any{"name": "exec_command", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c00", "content": "ok0"},
		map[string]any{"role": "tool", "tool_call_id": "c01", "content": "ok1"},
		// 第 2 组：view_image ×2，紧邻上一组结果，且自身结果被 notice 打断。
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c10", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
			map[string]any{"id": "c11", "type": "function", "function": map[string]any{"name": "view_image", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c10", "content": "img0"},
		map[string]any{"role": "developer", "content": "<image_resize_notice>n"},
		map[string]any{"role": "tool", "tool_call_id": "c11", "content": "img1"},
		map[string]any{"role": "developer", "content": "<image_resize_notice>n"},
	}
	out, changed := repackToolResultBlocks(msgs)
	if !changed {
		t.Fatalf("changed=false，第二组未被重排")
	}
	if len(out) != len(msgs) {
		t.Fatalf("长度变化：%d -> %d", len(msgs), len(out))
	}
	// 期望顺序：[user][a1][tool c00][tool c01][a2][tool c10][tool c11][dev][dev]
	want := []string{"", "", "c00", "c01", "", "c10", "c11", "", ""}
	for i, m := range out {
		mm, _ := m.(map[string]any)
		id, _ := mm["tool_call_id"].(string)
		if id != want[i] {
			t.Fatalf("[%d] tool_call_id=%q，期望 %q；实际顺序 %v", i, id, want[i], repackSeqOf(out))
		}
	}
	assertRepackPairsContiguous(t, out)
}

// TestRepackToolResultBlocksThreeConsecutiveGroups 回归：连续多组、仅末组含插入物
// ——确保组头识别在连续场景下不退化。
func TestRepackToolResultBlocksThreeConsecutiveGroups(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "a0", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "a0", "content": "r"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "b0", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
			map[string]any{"id": "b1", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "b0", "content": "r"},
		map[string]any{"role": "tool", "tool_call_id": "b1", "content": "r"},
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "c0", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "x", "arguments": "{}"}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "c0", "content": "r"},
		map[string]any{"role": "developer", "content": "<notice>"},
		map[string]any{"role": "tool", "tool_call_id": "c1", "content": "r"},
		map[string]any{"role": "developer", "content": "<notice>"},
	}
	out, changed := repackToolResultBlocks(msgs)
	if !changed {
		t.Fatalf("changed=false")
	}
	if len(out) != len(msgs) {
		t.Fatalf("长度变化：%d -> %d", len(msgs), len(out))
	}
	assertRepackPairsContiguous(t, out)
}

func repackSeqOf(msgs []any) []string {
	var s []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		if id, _ := mm["tool_call_id"].(string); id != "" {
			s = append(s, role+":"+id)
		} else {
			s = append(s, role)
		}
	}
	return s
}

// assertRepackPairsContiguous 断言每个 assistant.tool_calls 的结果在其后连续出现。
func assertRepackPairsContiguous(t *testing.T, msgs []any) {
	t.Helper()
	for i := 0; i < len(msgs); i++ {
		mm, _ := msgs[i].(map[string]any)
		tcs, ok := mm["tool_calls"].([]any)
		if !ok || len(tcs) == 0 {
			continue
		}
		want := map[string]bool{}
		for _, tci := range tcs {
			tc, _ := tci.(map[string]any)
			if id, _ := tc["id"].(string); id != "" {
				want[id] = true
			}
		}
		got := map[string]bool{}
		j := i + 1
		for j < len(msgs) {
			nxt, _ := msgs[j].(map[string]any)
			if r, _ := nxt["role"].(string); r != "tool" {
				break
			}
			if id, _ := nxt["tool_call_id"].(string); id != "" {
				got[id] = true
			}
			j++
		}
		if len(got) != len(want) {
			t.Fatalf("assistant[%d] 结果不连续：want=%d got=%d 序列=%v", i, len(want), len(got), repackSeqOf(msgs))
		}
	}
}

// TestRepackThenCleanupRealShape 集成（真实会话卡死形态）：多 tool 并行 + 图片
// resize notice 插在结果中间，经 repack + cleanup 后出站载荷零违规——结果连续、
// 配对对称。走的是 PrepareBodyOpt 生产转换路径，而非只调内部函数。
func TestRepackThenCleanupRealShape(t *testing.T) {
	body := `{"model":"glm-5.2","messages":[
		{"role":"user","content":"go"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"c00","type":"function","function":{"name":"exec_command","arguments":"{}"}},
			{"id":"c01","type":"function","function":{"name":"exec_command","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"c00","content":"ok0"},
		{"role":"tool","tool_call_id":"c01","content":"ok1"},
		{"role":"assistant","content":"","tool_calls":[
			{"id":"i00","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"a.png\"}"}},
			{"id":"i01","type":"function","function":{"name":"view_image","arguments":"{\"path\":\"b.png\"}"}}]},
		{"role":"tool","tool_call_id":"i00","content":"img0"},
		{"role":"developer","content":"<image_resize_notice>resized</image_resize_notice>"},
		{"role":"tool","tool_call_id":"i01","content":"img1"},
		{"role":"user","content":"next"}
	]}`
	out := PrepareBodyOpt([]byte(body), false)
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 9 {
		t.Fatalf("消息数不应变化，实际 %d", len(msgs))
	}
	// developer 归一为 system（normalizeRoles 既有行为），插在 notice 位次的它
	// 应已被挪到两条 tool 结果之后。
	seq := repackSeqOf(msgs)
	// i00/i01 两条结果连续（中间不再夹 developer/system）。
	idx := -1
	for i, s := range seq {
		if s == "tool:i00" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(seq) || seq[idx+1] != "tool:i01" {
		t.Fatalf("i00/i01 结果应连续，实际序列 %v", seq)
	}
	// notice 消息在 i01 之后（后移到位）。
	if notice := msgs[idx+2].(map[string]any); notice["content"] != "<image_resize_notice>resized</image_resize_notice>" {
		t.Fatalf("插入物应在结果之后且内容不变，实际 %#v", notice)
	}
	assertRepackPairsContiguous(t, msgs)
	assertPairingSymmetric(t, msgs)
}

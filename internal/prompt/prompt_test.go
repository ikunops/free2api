package prompt

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// systemRoles 提取 messages 中所有 role 值，用于断言改写后的角色序列。
func systemRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("msg not map: %v", m)
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

func TestRewriteReplacesSystemAndDeveloper(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"被替换的旧提示词"},
			{"role":"developer","content":"开发者指令"},
			{"role":"user","content":"你好"}
		],
		"metadata":{"conversation_id":"c1"}
	}`)
	out := Rewrite(in, "我是自有提示词")
	roles := systemRoles(t, out)
	wantRoles := []string{"system", "user"}
	if len(roles) != len(wantRoles) {
		t.Fatalf("roles=%v want %v", roles, wantRoles)
	}
	for i, r := range roles {
		if r != wantRoles[i] {
			t.Fatalf("roles[%d]=%q want %q (all=%v)", i, r, wantRoles[i], roles)
		}
	}
	// 头部 system 内容恰为自有提示词，旧 system/developer 内容零残留。
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	first := msgs[0].(map[string]any)
	if first["content"] != "我是自有提示词" {
		t.Errorf("first system content=%v", first["content"])
	}
	if strings.Contains(string(out), "被替换的旧提示词") || strings.Contains(string(out), "开发者指令") {
		t.Errorf("old system/developer content leaked: %s", out)
	}
}

func TestRewriteKeepsUserAssistantToolUntouched(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"user","content":"u-content"},
			{"role":"assistant","content":"a-content"},
			{"role":"tool","tool_call_id":"t1","content":"tool-result"}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	// 预期：system(新) + user + assistant + tool，顺序保留。
	if len(msgs) != 4 {
		t.Fatalf("len=%d", len(msgs))
	}
	roles := systemRoles(t, out)
	want := []string{"system", "user", "assistant", "tool"}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
	// user/assistant/tool 字段逐字不动。
	userMsg := msgs[1].(map[string]any)
	if userMsg["content"] != "u-content" {
		t.Errorf("user content changed: %v", userMsg["content"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "tool-result" {
		t.Errorf("tool changed: %v", toolMsg)
	}
}

func TestRewriteKeepsMetadataAndOtherFields(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"metadata":{"conversation_id":"c1","user_id":"u9"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if obj["model"] != "glm-5.2" {
		t.Errorf("model changed: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Errorf("stream changed: %v", obj["stream"])
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" || meta["user_id"] != "u9" {
		t.Errorf("metadata changed: %v", obj["metadata"])
	}
}

func TestRewriteInjectsSystemWhenAbsent(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := Rewrite(in, "SYS")
	roles := systemRoles(t, out)
	want := []string{"system", "user"}
	if len(roles) != len(want) {
		t.Fatalf("roles=%v want %v", roles, want)
	}
	for i, r := range roles {
		if r != want[i] {
			t.Fatalf("roles=%v want %v", roles, want)
		}
	}
}

func TestRewriteMultimodalContentUntouched(t *testing.T) {
	// user content 为多模态数组（text + image_url），Rewrite 只动 messages 层级，
	// 不应改动 content 内部结构。
	in := []byte(`{
		"messages":[
			{"role":"system","content":"old"},
			{"role":"user","content":[
				{"type":"text","text":"看图"},
				{"type":"image_url","image_url":{"url":"data:..."}}
			]}
		]
	}`)
	out := Rewrite(in, "SYS")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("len=%d", len(msgs))
	}
	userMsg := msgs[1].(map[string]any)
	arr, ok := userMsg["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multimodal content changed: %v", userMsg["content"])
	}
	textPart := arr[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "看图" {
		t.Errorf("text part changed: %v", textPart)
	}
}

func TestRewriteInvalidJSONReturnedAsIs(t *testing.T) {
	in := []byte(`{not valid json`)
	out := Rewrite(in, "SYS")
	if string(out) != string(in) {
		t.Errorf("invalid json should return as-is: got %s", out)
	}
}

func TestRewriteEmptyBodyReturnedAsIs(t *testing.T) {
	out := Rewrite([]byte{}, "SYS")
	if len(out) != 0 {
		t.Errorf("empty body should return as-is: got %s", out)
	}
}

func TestRewriteEmptyPromptReturnedAsIs(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"old"}]}`)
	out := Rewrite(in, "")
	// systemPrompt 空 → 不改写，原样返回。
	if string(out) != string(in) {
		t.Errorf("empty prompt should return as-is: got %s", out)
	}
}

func TestLoadDefaultWhenFileEmpty(t *testing.T) {
	got, err := Load("custom", "")
	if err != nil {
		t.Fatal(err)
	}
	if got != defaultPrompt {
		t.Errorf("Load() returned non-default prompt (len=%d vs %d)", len(got), len(defaultPrompt))
	}
	if len(got) == 0 {
		t.Error("default prompt is empty")
	}
}

func TestLoadFileOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "my.md")
	want := "这是我的自定义人格入口。"
	os.WriteFile(fp, []byte(want), 0o600)
	got, err := Load("custom", fp)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("Load()=%q want %q", got, want)
	}
}

func TestLoadFileMissingFailsFast(t *testing.T) {
	if _, err := Load("custom", "/nonexistent/promp.md"); err == nil {
		t.Fatal("missing file should return error (fail fast)")
	}
}

// ---- Append（issue #129：append 模式，M1-M10 语义矩阵） ----

// appendRoles 提取改写后 messages 的角色序列（非 map 消息以占位符呈现，
// 用于 M7 边界形态断言）。
func appendRoles(t *testing.T, body []byte) []string {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, body)
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		t.Fatalf("messages not []any: %v", obj["messages"])
	}
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, "<non-map>")
			continue
		}
		r, _ := mm["role"].(string)
		out = append(out, r)
	}
	return out
}

// wantAppendRoles 逐位比对角色序列（含占位符）。
func wantAppendRoles(t *testing.T, body []byte, want []string) {
	t.Helper()
	got := appendRoles(t, body)
	if len(got) != len(want) {
		t.Fatalf("roles=%v want %v", got, want)
	}
	for i, r := range got {
		if r != want[i] {
			t.Fatalf("roles[%d]=%q want %q (all=%v)", i, r, want[i], got)
		}
	}
}

// TestAppendInsertsAfterLeadingRun M2 核心场景：[sys, dev, user] → [sys, dev, GW, user]，
// 块内容逐字保留，GW 内容恰为 PromptText。
func TestAppendInsertsAfterLeadingRun(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"messages":[
			{"role":"system","content":"项目规范：所有回复用中文"},
			{"role":"developer","content":"工具约定：调用前必须确认"},
			{"role":"user","content":"你好"}
		]
	}`)
	out := Append(in, "网关提示词")
	wantAppendRoles(t, out, []string{"system", "developer", "system", "user"})
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	gw := msgs[2].(map[string]any)
	if gw["content"] != "网关提示词" {
		t.Errorf("GW content=%v", gw["content"])
	}
	// 开头块内容逐字保留。
	if msgs[0].(map[string]any)["content"] != "项目规范：所有回复用中文" {
		t.Errorf("leading system content changed: %v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "工具约定：调用前必须确认" {
		t.Errorf("leading developer content changed: %v", msgs[1])
	}
}

// TestAppendNoSystemInsertsAtHead M1：开头无 system → GW 插最前。
func TestAppendNoSystemInsertsAtHead(t *testing.T) {
	in := []byte(`{"messages":[{"role":"user","content":"u"},{"role":"assistant","content":"a"}]}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "user", "assistant"})
}

// TestAppendMidStreamSystemUntouched M3：中途 system 原样不动、不触发二次插入。
func TestAppendMidStreamSystemUntouched(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"system","content":"头"},
			{"role":"user","content":"u1"},
			{"role":"system","content":"中途sys"},
			{"role":"user","content":"u2"}
		]
	}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "system", "user", "system", "user"})
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if msgs[3].(map[string]any)["content"] != "中途sys" {
		t.Errorf("mid-stream system content changed: %v", msgs[3])
	}
}

// TestAppendEmptyMessages M4：messages=[] → [GW]。
func TestAppendEmptyMessages(t *testing.T) {
	in := []byte(`{"model":"m","messages":[]}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system"})
}

// TestAppendNoMessagesField M5：无 messages 键 → messages=[GW]，其余字段原样。
func TestAppendNoMessagesField(t *testing.T) {
	in := []byte(`{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"c1"}}`)
	out := Append(in, "GW")
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["model"] != "glm-5.2" {
		t.Errorf("model changed: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Errorf("stream changed: %v", obj["stream"])
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" {
		t.Errorf("metadata changed: %v", obj["metadata"])
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) != 1 {
		t.Fatalf("messages=%v want [GW]", obj["messages"])
	}
	gw := msgs[0].(map[string]any)
	if gw["role"] != "system" || gw["content"] != "GW" {
		t.Errorf("GW=%v", gw)
	}
}

// TestAppendOnlyToolMessages M6：[tool] → [GW, tool]，tool_call_id/content 不动。
func TestAppendOnlyToolMessages(t *testing.T) {
	in := []byte(`{"messages":[{"role":"tool","tool_call_id":"t1","content":"tool-result"}]}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "tool"})
	var obj map[string]any
	json.Unmarshal(out, &obj)
	toolMsg := obj["messages"].([]any)[1].(map[string]any)
	if toolMsg["tool_call_id"] != "t1" || toolMsg["content"] != "tool-result" {
		t.Errorf("tool changed: %v", toolMsg)
	}
}

// TestAppendNonMapMessageStopsRun M7：非 map 消息 = 边界停止条件。
func TestAppendNonMapMessageStopsRun(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"头"},"garbage",{"role":"user","content":"u"}]}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "system", "<non-map>", "user"})
}

// TestAppendMultimodalSystemUntouched M8：开头 system content 为多模态数组 → 结构逐字不动。
func TestAppendMultimodalSystemUntouched(t *testing.T) {
	in := []byte(`{
		"messages":[
			{"role":"system","content":[
				{"type":"text","text":"多模态系统提示"},
				{"type":"image_url","image_url":{"url":"data:..."}}
			]},
			{"role":"user","content":"看图"}
		]
	}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "system", "user"})
	var obj map[string]any
	json.Unmarshal(out, &obj)
	sysMsg := obj["messages"].([]any)[0].(map[string]any)
	arr, ok := sysMsg["content"].([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("multimodal system content changed: %v", sysMsg["content"])
	}
	textPart := arr[0].(map[string]any)
	if textPart["type"] != "text" || textPart["text"] != "多模态系统提示" {
		t.Errorf("text part changed: %v", textPart)
	}
	imgPart := arr[1].(map[string]any)
	if imgPart["type"] != "image_url" {
		t.Errorf("image part changed: %v", imgPart)
	}
}

// TestAppendEmptyPromptAsIs M9 守卫一：空 Prompt 原样返回。
func TestAppendEmptyPromptAsIs(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"old"}]}`)
	out := Append(in, "")
	if string(out) != string(in) {
		t.Errorf("empty prompt should return as-is: got %s", out)
	}
}

// TestAppendInvalidJSONAsIs M9 守卫二：坏 JSON 原样返回。
func TestAppendInvalidJSONAsIs(t *testing.T) {
	in := []byte(`{not valid json`)
	out := Append(in, "GW")
	if string(out) != string(in) {
		t.Errorf("invalid json should return as-is: got %s", out)
	}
}

// TestAppendEmptyBodyAsIs M9 守卫三：空 body 原样返回。
func TestAppendEmptyBodyAsIs(t *testing.T) {
	out := Append([]byte{}, "GW")
	if len(out) != 0 {
		t.Errorf("empty body should return as-is: got %s", out)
	}
}

// TestAppendKeepsOtherFields A10：model/stream/metadata 原样。
func TestAppendKeepsOtherFields(t *testing.T) {
	in := []byte(`{
		"model":"glm-5.2",
		"stream":true,
		"metadata":{"conversation_id":"c1","user_id":"u9"},
		"messages":[{"role":"user","content":"hi"}]
	}`)
	out := Append(in, "GW")
	var obj map[string]any
	json.Unmarshal(out, &obj)
	if obj["model"] != "glm-5.2" {
		t.Errorf("model changed: %v", obj["model"])
	}
	if obj["stream"] != true {
		t.Errorf("stream changed: %v", obj["stream"])
	}
	meta, ok := obj["metadata"].(map[string]any)
	if !ok || meta["conversation_id"] != "c1" || meta["user_id"] != "u9" {
		t.Errorf("metadata changed: %v", obj["metadata"])
	}
}

// TestAppendUnknownRoleStopsRun A11/M10：未知角色（function）≠ system → 边界停；
// 精确匹配不命中变体（含大小写/空白 " System "）同样视为非 system。
func TestAppendUnknownRoleStopsRun(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"头"},{"role":"function","content":"f"},{"role":"user","content":"u"}]}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "system", "function", "user"})
}

// TestAppendRoleVariantStopsRun M10：role 大小写/空白变体精确匹配不命中 → 边界停。
func TestAppendRoleVariantStopsRun(t *testing.T) {
	in := []byte(`{"messages":[{"role":"system","content":"头"},{"role":" System ","content":"变体"},{"role":"user","content":"u"}]}`)
	out := Append(in, "GW")
	wantAppendRoles(t, out, []string{"system", "system", " System ", "user"})
	var obj map[string]any
	json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	if msgs[2].(map[string]any)["role"] != " System " {
		t.Errorf("role variant changed: %v", msgs[2])
	}
}

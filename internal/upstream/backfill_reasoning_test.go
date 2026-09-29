package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// assistantReasoningField 提取输出 messages 里各 assistant 消息的 reasoning 字段。
// 与 assistantRC 同构：缺字段返回 "<absent>"，非 string 值返回 "<non-string>"，
// 其余返回原值（含空串）。返回切片与 messages 中 assistant 消息一一对应。
func assistantReasoningField(t *testing.T, out []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal: %v (out=%s)", err, out)
	}
	var got []string
	msgs, _ := m["messages"].([]any)
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		v, ok := msg["reasoning"]
		if !ok {
			got = append(got, "<absent>")
		} else if s, ok := v.(string); ok {
			got = append(got, s)
		} else {
			got = append(got, "<non-string>")
		}
	}
	return got
}

// TestBackfillReasoningFieldNonEmpty issue #165 追评 R7（RED 锚）：零痕迹多轮
// deepseek（enabled 注入后）→ 出站每条 assistant 消息的 reasoning 字段存在且
// 非空。旧行为只写 reasoning_content 不写 reasoning → 缺字段，RED 抓住。
// 依据：haocheng23 判定表（部分账号校验 len(reasoning)>0——空串 400 空白串 200）
// 与官方 CLI 行为（itemsToMessages 的 applyPendingReasoning 给 assistant 挂
// reasoning 文本）——「每条 assistant 保证 reasoning 非空」正是官方形态。
func TestBackfillReasoningFieldNonEmpty(t *testing.T) {
	body := `{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u1"},
		{"role":"assistant","content":"a1"},
		{"role":"user","content":"u2"},
		{"role":"assistant","content":"a2"},
		{"role":"user","content":"u3"}]}`
	out := PrepareBodyOptWithEfforts([]byte(body), false, nil)
	got := assistantReasoningField(t, out)
	if len(got) != 2 {
		t.Fatalf("assistant 消息数 = %d want 2 (out=%s)", len(got), out)
	}
	for i, r := range got {
		if r == "<absent>" || r == "<non-string>" || r == "" {
			t.Errorf("零痕迹 enabled 形态下 assistant[%d].reasoning = %q want 存在且非空 (out=%s)", i, r, out)
		}
	}
}

// TestBackfillReasoningMirrorsReasoningContent R8：镜像写入优先级——
// reasoning 已是非空 string → 不动；reasoning 缺失/null/空串且 rc 有来源文本 →
// 写入来源文本；两者皆无 → 补单个空格 " "（上游 len>0 不 trim 校验：空白串认、
// 空串不认——空白串是官方 Moonshot 占位规则 "-" 的同款先例）。
func TestBackfillReasoningMirrorsReasoningContent(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"rc 有值 + reasoning 缺失 → 镜像复制",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning_content":"source text"}]}`,
			[]string{"source text"}},
		{"rc 有值 + reasoning null → 归一化为来源文本",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":null,"reasoning_content":"source text"}]}`,
			[]string{"source text"}},
		{"rc 有值 + reasoning 空串 → 覆盖为来源文本",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":"","reasoning_content":"source text"}]}`,
			[]string{"source text"}},
		{"两者皆无 → 补单个空格（空白串过闸、空串不过）",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a"}]}`,
			[]string{" "}},
		{"reasoning 已非空 → 原样保留不覆盖",
			`{"model":"deepseek-v4-flash","messages":[
				{"role":"assistant","content":"a","reasoning":"kept","reasoning_content":"other"}]}`,
			[]string{"kept"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantReasoningField(t, out)
			if len(got) != len(c.want) {
				t.Fatalf("assistant 消息数 = %d want %d (out=%s)", len(got), len(c.want), out)
			}
			for i, want := range c.want {
				if got[i] != want {
					t.Errorf("assistant[%d].reasoning = %q want %q (out=%s)", i, got[i], want, out)
				}
			}
		})
	}
}

// TestBackfillReasoningFieldNonDeepSeek R9：非 deepseek 模型零改动——
// reasoning 字段保持原样（含缺失形态不补）。
func TestBackfillReasoningFieldNonDeepSeek(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []string // "<absent>" 表示应保持缺失
	}{
		{"缺失不补", `{"model":"glm-5.2","messages":[{"role":"assistant","content":"a"}]}`, []string{"<absent>"}},
		{"已有原样保留", `{"model":"glm-5.2","messages":[{"role":"assistant","content":"a","reasoning":"kept"}]}`, []string{"kept"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := PrepareBodyOptWithEfforts([]byte(c.body), false, nil)
			got := assistantReasoningField(t, out)
			if len(got) != 1 || got[0] != c.want[0] {
				t.Errorf("非 deepseek reasoning 应零改动: got %v want %v (out=%s)", got, c.want, out)
			}
		})
	}
}

// TestBackfillReasoningFieldGateComposes 门控组合：disabled + 零痕迹 → reasoning
// 也不补（与 rc 同门 thinkingEnabled||hasTrace，四分支一致）；disabled + 有痕迹 →
// 照补（hasTrace 半边）。
func TestBackfillReasoningFieldGateComposes(t *testing.T) {
	// disabled + 零痕迹 → 零改动。
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"user","content":"u"},{"role":"assistant","content":"plain"}]}`), false, nil)
	for _, r := range assistantReasoningField(t, out) {
		if r != "<absent>" {
			t.Errorf("disabled+零痕迹 不应补 reasoning, got %q (out=%s)", r, out)
		}
	}
	// disabled + 有痕迹（rc 键存在即痕迹）→ 照补（镜像复制）。
	out = PrepareBodyOptWithEfforts([]byte(`{"model":"deepseek-v4-flash","thinking":{"type":"disabled"},"messages":[
		{"role":"assistant","content":"a","reasoning_content":"trace"}]}`), false, nil)
	got := assistantReasoningField(t, out)
	if len(got) != 1 || got[0] != "trace" {
		t.Errorf("disabled+有痕迹 应照补 reasoning（镜像 rc）: got %v (out=%s)", got, out)
	}
}

// TestBackfillReasoningFieldWhitespaceOnlySanitize sanitize 管线下占位空格的
// 存活：sanitizeText 不得把占位 " " 改写为空串（sanitize 改的是指纹文本，
// 不触碰占位符）。若未来 sanitize 收紧 whitespace，本用例守住「出站 reasoning
// 非空」的最终语义。
func TestBackfillReasoningFieldWhitespaceOnlySanitize(t *testing.T) {
	out := PrepareBodyOptWithEfforts([]byte(`{"model":"deepseek-v4-flash","messages":[
		{"role":"user","content":"u"},{"role":"assistant","content":"a"}]}`), true, nil)
	got := assistantReasoningField(t, out)
	if len(got) != 1 {
		t.Fatalf("assistant 消息数 = %d want 1 (out=%s)", len(got), out)
	}
	if strings.TrimSpace(got[0]) == "" && got[0] != " " {
		t.Errorf("sanitize 管线下占位 reasoning 应保持非空: got %q (out=%s)", got[0], out)
	}
	if got[0] == "" || got[0] == "<absent>" {
		t.Errorf("占位 reasoning 被清空: got %q (out=%s)", got[0], out)
	}
}

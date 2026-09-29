package upstream

import (
	"encoding/json"
	"testing"
)

// payload_max_completion_tokens_test.go max_completion_tokens → max_tokens 翻译
// （吸收 PR #116，Closes #117）。用例：PR 原 2 例（别名翻译 / 显式优先）+
// 边界补强（0/null/负数/浮尾不翻译、两字段并存删别名、int 形态）。

// TestPrepareBodyConvertsMaxCompletionTokens（PR #116 用例 1）：
// 别名 128000 → max_tokens=128000，别名删除。
func TestPrepareBodyConvertsMaxCompletionTokens(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault([]byte(`{"model":"deepseek-v4.1-flash","messages":[],"max_completion_tokens":128000}`), false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(128000) {
		t.Fatalf("max_tokens=%v, want 128000", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Fatal("alias remains")
	}
}

// TestPrepareBodyPrefersMaxTokens（PR #116 用例 2）：
// 两字段并存取显式 max_tokens（64000），别名删除。
func TestPrepareBodyPrefersMaxTokens(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault([]byte(`{"messages":[],"max_tokens":64000,"max_completion_tokens":128000}`), false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got["max_tokens"] != float64(64000) {
		t.Fatalf("max_tokens=%v, want 64000", got["max_tokens"])
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Fatal("alias remains")
	}
}

// TestMaxCompletionTokensNonPositiveNotTranslated 边界（任务书 prompt-too-long §3）：
// 0/null/负数/浮尾/非数值别名不翻译（走上游默认/由上游报参数错），别名仍删除。
func TestMaxCompletionTokensNonPositiveNotTranslated(t *testing.T) {
	cases := []struct {
		name      string
		aliasJSON string
	}{
		{"zero", `"max_completion_tokens":0`},
		{"null", `"max_completion_tokens":null`},
		{"negative", `"max_completion_tokens":-5`},
		{"float tail", `"max_completion_tokens":1.5`},     // 非整数：不翻译（不把小数尾巴搬进 max_tokens）
		{"non-numeric", `"max_completion_tokens":"128000"`}, // 字符串畸形：不翻译（上游 11101 自会报）
	}
	for _, c := range cases {
		src := []byte(`{"messages":[],` + c.aliasJSON + `}`)
		out := PrepareBodyOptWithEffortsAndDefault(src, false, nil, nil)
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("%s: unmarshal: %v", c.name, err)
		}
		if _, ok := got["max_tokens"]; ok {
			t.Errorf("%s: max_tokens must NOT be set from non-positive/invalid alias, got %v", c.name, got["max_tokens"])
		}
		if _, ok := got["max_completion_tokens"]; ok {
			t.Errorf("%s: alias must be deleted", c.name)
		}
	}
}

// TestMaxCompletionTokensExplicitZeroKept 显式 max_tokens=0 与别名并存：
// 显式字段原样保留（0 语义 = 上游默认，不覆盖成别名值），别名删除。
func TestMaxCompletionTokensExplicitZeroKept(t *testing.T) {
	out := PrepareBodyOptWithEffortsAndDefault([]byte(`{"messages":[],"max_tokens":0,"max_completion_tokens":128000}`), false, nil, nil)
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if v, ok := got["max_tokens"]; !ok || v != float64(0) {
		t.Fatalf("explicit max_tokens=0 must be preserved verbatim, got %v ok=%v", v, ok)
	}
	if _, ok := got["max_completion_tokens"]; ok {
		t.Error("alias remains")
	}
}

// TestTranslateMaxCompletionTokensIntForms 手构造 map 的 int 形态（非 JSON 解码路径）
// 防御性兼容：正 int/int64 翻译，非正不翻译。
func TestTranslateMaxCompletionTokensIntForms(t *testing.T) {
	obj := map[string]any{"max_completion_tokens": int(42)}
	translateMaxCompletionTokens(obj)
	if v, ok := obj["max_tokens"].(int64); !ok || v != 42 {
		t.Errorf("int alias: max_tokens=%v want 42", obj["max_tokens"])
	}
	obj = map[string]any{"max_completion_tokens": int64(-1)}
	translateMaxCompletionTokens(obj)
	if _, ok := obj["max_tokens"]; ok {
		t.Errorf("negative int64 must not translate, got %v", obj["max_tokens"])
	}
}

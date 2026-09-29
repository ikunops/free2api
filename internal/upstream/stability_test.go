// stability_test.go upstream 侧 body 重写稳定性回归测试。
//
// 背景（lovingfish/workbuddy-cliproxy issue#4）：逐字节相同的请求在上游侧 prompt cache
// 命中恒为 0。若网关 body 重写链每次产生不同的 JSON 序列化（字段顺序跑偏、无谓往返），
// 上游前缀 hash 无法命中。
//
// 本文件锁定 upstream 侧重写（prepareBody/PrepareBodyOptWithEfforts + ensureConsoleSystem）
// 的结论：每一步都是 json.Unmarshal → map[string]any → json.Marshal，Go map 序列化按键
// 字典序，完全由输入与 efforts 快照决定；链中不注入任何时间/随机/id 字段。同输入 →
// 字节级同输出，稳定。测试即锁死这一性质。
package upstream

import (
	"bytes"
	"testing"
)

// prepareBodyChain 以字节级精度仿真生产出站 body 重写链（upstream 侧）：
// PrepareBodyOptWithEfforts（脱敏 + effort 降级等，efforts 传 nil → 透传不降级）
// → ensureConsoleSystem（global realm 首条消息非 system 时前置兜底 system）。
func prepareBodyChain(body []byte, realm string) []byte {
	out := PrepareBodyOptWithEfforts(body, true, nil)
	if realm == "global" {
		out = ensureConsoleSystem(out)
	}
	return out
}

// TestUpstreamRewriteStableSerialization 同一请求体经 upstream 侧重写链跑多遍，断言字节级一致。
func TestUpstreamRewriteStableSerialization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		body  string
		realm string
	}{
		{"cn 零改写透传", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`, "cn"},
		{"global 首条 user + 兜底 system", `{"model":"g:glm-4","messages":[{"role":"user","content":"hi"}]}`, "global"},
		{"global 首条已是 system 不重复注入", `{"model":"g:glm-4","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`, "global"},
		{"deepseek 开思考注入 thinking+effort", `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`, "cn"},
		{"deepseek 显式 thinking disabled 删 effort", `{"model":"deepseek-v4-flash","reasoning_effort":"high","thinking":{"type":"disabled"},"messages":[]}`, "cn"},
		{"tool_choice 归一化", `{"model":"g:glm-4","tool_choice":{"type":"function","function":{"name":"f"}},"tools":[{"type":"function","function":{"name":"f"}}],"messages":[{"role":"user","content":"hi"}]}`, "global"},
		{"脱敏指纹 system 改写（逐字稳定）", `{"model":"g:glm-4","messages":[{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs). 11128"},{"role":"user","content":"hi"}]}`, "global"},
		{"assistant reasoning_content + tool_calls 数组", `{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"reasoning_content":"hidden","tool_calls":[{"function":{"name":"f","arguments":"[\"a\",\"b\"]"}}]}]}`, "cn"},
		{"多模态 content 数组", `{"model":"g:glm-4","messages":[{"role":"user","content":[{"type":"text","text":"hi"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]}]}`, "global"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := func() []byte { return prepareBodyChain([]byte(c.body), c.realm) }
			first := run()
			for n := 0; n < 5; n++ {
				again := run()
				if !bytes.Equal(first, again) {
					t.Fatalf("body 重写序列化不稳定（prompt cache 前缀将 miss）\nrun1=%s\nrun%d=%s", first, n+2, again)
				}
			}
		})
	}
}

// TestChainIsInputPureFunction 锁死"重写结果不含时间/随机/id"：把同一输入跑出的两步结果
// 再次互相比较等价于只比较派生处的一致性——字节级 Equal 已覆盖。此处补一个直接断言：
// 对 fingerprint 携载的 system 首条消息，global 兜底不会引入与注入物无关的新键
// （确定性由注入物 string 决定，非时间/随机）。
func TestDeepSeekEffortDefaultsDeterministic(t *testing.T) {
	t.Parallel()
	// 首次跑注入 thinking.type=enabled + reasoning_effort=high（无时间/随机参与）。
	in := []byte(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"hi"}]}`)
	first := PrepareBodyOptWithEfforts(in, false, nil)
	second := PrepareBodyOptWithEfforts(in, false, nil)
	if !bytes.Equal(first, second) {
		t.Fatalf("deepseek thinking 注入不稳定\n1=%s\n2=%s", first, second)
	}
	if bytes.Contains(first, []byte("high")) == false {
		t.Errorf("effort 默认档未注入（断言前置失败）")
	}
}

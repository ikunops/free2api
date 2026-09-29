// stability_test.go 网关侧 body 重写稳定性回归测试。
//
// 背景（lovingfish/workbuddy-cliproxy issue#4）：逐字节相同的请求在上游侧 prompt cache
// 命中恒为 0。若网关 body 重写链每次产生不同的 JSON 序列化（字段顺序跑偏、无谓往返），
// 上游前缀 hash 无法命中。
//
// 本文件锁定 server 侧重写（prompt.Rewrite → rewriteModel）的结论：
// 每一步都是 json.Unmarshal → map[string]any → json.Marshal，Go map 序列化按键字典序，
// 完全由输入决定，链中不注入任何时间/随机/id 字段。同输入 → 字节级同输出，稳定。
// upstream 侧 prepareBody/ensureConsoleSystem 的稳定性见 internal/upstream 对应测试。
package server

import (
	"bytes"
	"testing"

	"free2api/internal/prompt"
	"free2api/internal/upstream"
)

// rewriteChain server 侧出站 body 重写链（与 handler.chatCompletions 一致）：
//   - prompt.Rewrite：custom 提示词替换 system/developer（passthrough 降级期同理只换提示词文本）
//   - rewriteModel：realm 前缀剥除（bare != 原 model 时改写）
//   - PrepareBodyOptWithEfforts：脱敏等（出站统一在 upstream.ChatStream 内执行；efforts 传 nil
//     与空缓存等价——透传不降级，见 payload.go normalizeReasoningEffort）
func rewriteChain(body []byte, custom string, bare string) []byte {
	out := body
	if custom != "" {
		out = prompt.Rewrite(out, custom)
	}
	out = rewriteModel(out, bare)
	return upstream.PrepareBodyOptWithEfforts(out, true, nil)
}

// TestServerRewriteStableSerialization 同一请求体经 server 侧重写链跑多遍，断言字节级一致。
func TestServerRewriteStableSerialization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		body   string
		custom string
		bare   string
	}{
		{"cn 裸模型名（零改写透传）", `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`, "", "glm-5.2"},
		{"cn realm 前缀剥除", `{"model":"cn:glm-4","messages":[{"role":"user","content":"hi"}]}`, "", "glm-4"},
		{"custom 提示词替换 system/developer", `{"model":"g:glm-4","messages":[{"role":"system","content":"You are Claude Code"},{"role":"developer","content":"dev"},{"role":"user","content":"hi"}]}`, "You are a helpful assistant.", "glm-4"},
		{"assistant 带 reasoning 痕迹 + tool_calls 数组", `{"model":"deepseek-v4-flash","messages":[{"role":"assistant","content":null,"reasoning":"hidden","tool_calls":[{"function":{"name":"f","arguments":"{}"}}]}]}`, "", "deepseek-v4-flash"},
		{"多模态 / 嵌套对象", `{"model":"g:glm-4","messages":[{"role":"user","content":[{"type":"text","text":"你好"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`, "", "glm-4"},
		{"大嵌套结构", `{"model":"ds:deepseek-v4-pro","messages":[{"role":"user","content":"x"},{"role":"assistant","content":"y","reasoning_content":""}],"tools":[{"type":"function","function":{"name":"a","parameters":{"properties":{"p":{"type":"string"}},"type":"object"}}}]}`, "", "deepseek-v4-pro"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run := func() []byte { return rewriteChain([]byte(c.body), c.custom, c.bare) }
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

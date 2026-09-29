package upstream

import (
	"net/http"
	"testing"
)

// 智谱侧 1211「模型不存在」与 workbuddy 11102 同为权威性「无此模型」答复，
// 必须进同一分类（池级死模型标记依赖它；漏判会让已下架模型永远留在目录里）。
func TestIsModelBlockedZcode1211(t *testing.T) {
	// 实测透传体：嵌套 error 子对象 + string code + 中文 message。
	nested := `{"error":{"code":"1211","message":"模型不存在，请检查模型代码。","requestId":"x"}}`
	if !IsModelBlocked(http.StatusBadRequest, nested) {
		t.Fatal("嵌套 error.code=1211 应判为 model blocked")
	}
	// 顶层形态（防上游换壳）。
	top := `{"code":"1211","msg":"模型不存在，请检查模型代码。"}`
	if !IsModelBlocked(http.StatusBadRequest, top) {
		t.Fatal("顶层 code=1211 应判为 model blocked")
	}
	// 只命中「模型不存在」措辞、无 code（上游换壳保留措辞的情形）。
	msgOnly := `{"error":{"message":"模型不存在，请检查模型代码。"}}`
	if !IsModelBlocked(http.StatusBadRequest, msgOnly) {
		t.Fatal("message 命中「模型不存在」应判为 model blocked")
	}
	// 429 带 1211 仍是限流语义，不进 blocked 判定。
	if IsModelBlocked(http.StatusTooManyRequests, nested) {
		t.Fatal("429 不在 blocked 判定范围")
	}
	// 无关错误不误判。
	if IsModelBlocked(http.StatusBadRequest, `{"error":{"code":"11148","message":"tool calls and tool results do not match"}}`) {
		t.Fatal("11148 不是 model blocked")
	}
	if IsModelBlocked(http.StatusBadRequest, `{"error":{"code":"1200","message":"内部错误"}}`) {
		t.Fatal("无关错误不应误判")
	}
}

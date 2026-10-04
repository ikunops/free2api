package webui

import (
	"regexp"
	"strings"
	"testing"
)

// TestNoGoConstantNamesInJS 页面 JS 里不得引用 Go 侧的**数据项 ID 常量**。
//
// 为什么需要它：踩过一次 —— viewSettings 里写了 floatItemClick / floatItemTopmost。
// 这两个名字是 cmd/desktop 包里的 Go 常量（值分别是 "passthru" / "topmost"），
// JS 侧从来没有定义过。node --check 过得去（合法标识符引用），但运行到那一行
// 才抛 ReferenceError，而它在 render() 里 —— 整段脚本当帧失效，用户看到的只是
// 「设置页什么都打不开」，报错还跟设置页毫无字面关系。
//
// 白名单：floatItemToggle 是页面自己的 JS 函数，与 Go 常量无关。
// 数据侧不硬编码第二份枚举 —— 后端 floatItemDef() 已经把 ID 列表发给前端了。
func TestNoGoConstantNamesInJS(t *testing.T) {
	goConsts := []string{
		"floatItemInFlight", "floatItemRate", "floatItemRequests",
		"floatItemTokens", "floatItemSource", "floatItemTopmost",
		"floatItemClick",
	}

	html := string(Page())
	start := strings.Index(html, "<script>")
	if start < 0 {
		t.Fatal("index.html 里找不到 <script> 块")
	}
	body := html[start+len("<script>"):]
	if end := strings.Index(body, "</script>"); end >= 0 {
		body = body[:end]
	}

	for _, name := range goConsts {
		re := regexp.MustCompile(`\b` + name + `\b`)
		if re.MatchString(body) {
			t.Errorf("JS 里出现 Go 常量名 %q —— 它只存在于 Go 侧，运行到这行会抛 ReferenceError 让整段脚本失效；应改用后端下发的字面量 ID", name)
		}
	}
}

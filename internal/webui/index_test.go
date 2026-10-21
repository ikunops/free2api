package webui

import (
	"os"
	"os/exec"
	"path/filepath"
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

// TestInlineScriptParses 用 Node 解析内嵌脚本，拦截“HTML 仍返回 200、但 JS
// 在首行解析时就失败，导致导航和内容区一起空白”的发布事故。
//
// 这次真实根因是 window.confirm(...) 的双引号字符串里出现裸换行。浏览器会
// 在脚本解析阶段终止，renderNav()/render() 根本来不及执行；后端 healthz 却
// 完全正常，所以仅验证 HTTP 状态或字段存在抓不到。
func TestInlineScriptParses(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; cannot parse embedded JavaScript")
	}
	path := filepath.Join(t.TempDir(), "management.js")
	if err := os.WriteFile(path, []byte(scriptBody(t)), 0o600); err != nil {
		t.Fatalf("write embedded script: %v", err)
	}
	out, err := exec.Command(node, "--check", path).CombinedOutput()
	if err != nil {
		t.Fatalf("embedded JavaScript syntax error: %v\n%s", err, out)
	}
}

// Package webui 把中文控制台页面编译进二进制（go:embed）。
//
// 为什么内嵌而不是外挂静态目录：目标是「一个进程、一个端口、一个文件」，
// 整体拷到另一台机器就能跑。外挂目录会重新引入"大包小包迁移"的老问题——
// 少拷一个 index.html 就白屏。
//
// ⚠ 改了这个 index.html，必须同时重建 cmd/server 与 cmd/desktop 两个二进制。
// 它俩各自 embed 了一份（go:embed 是编译期把文件烧进 exe），所以只重建网关的话，
// 从桌面 exe 打开的管理页仍是旧版本——用户会看到"网页和 exe 不一样"。
// 已踩过：网关 19:46 的新页面，桌面 exe 还是 03:31 的旧页面，两边看着像两个项目。
// 本地部署脚本务必两个都 go build；release.yml 已经是这么做的。
package webui

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

// Page 返回内嵌的管理页 HTML（调用方直接写响应体）。
func Page() []byte { return indexHTML }

// PageBytes 页面的字节长度（供日志/自检，避免调用方再去 len）。
func PageBytes() int { return len(indexHTML) }

// Handler 返回只服务这一张页面的 handler。
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(indexHTML)
	})
}

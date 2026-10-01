//go:build !windows

// main_other.go 非 Windows 占位：桌面程序依赖 WebView2（仅 Windows）。
// 保留这个文件是为了 `go build ./...` 在 Linux/macOS 上照样能过——
// 服务器版（cmd/server）不受影响。
package main

import "fmt"

func main() {
	fmt.Println("free2api-desktop 只能在 Windows 上构建（依赖 WebView2）。无头运行请用 ./cmd/server。")
}

//go:build !windows

package source

import "errors"

// dpapiUnprotect 非 Windows 平台没有 DPAPI：Qoder 的登录态只能在本机 Windows 上读。
func dpapiUnprotect([]byte) ([]byte, error) {
	return nil, errors.New("dpapi: 仅支持 Windows（Qoder 登录态由 Windows DPAPI 保护）")
}

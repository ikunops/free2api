//go:build windows

package source

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiUnprotect 用当前 Windows 用户的 DPAPI 密钥解一段 blob。
//
// 这是 Chromium 系客户端存储 AES key 的标准做法（os_crypt.encrypted_key 去掉
// "DPAPI" 前缀后的部分）。同一台机器、同一用户下可解；换用户/换机器解不开。
func dpapiUnprotect(blob []byte) ([]byte, error) {
	if len(blob) == 0 {
		return nil, errors.New("dpapi: 空 blob")
	}
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	buf := make([]byte, out.Size)
	copy(buf, unsafe.Slice(out.Data, out.Size))
	return buf, nil
}

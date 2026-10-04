//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWantsWindowFollowsPersistedEnabled wantsWindow 必须忠实反映落盘配置里的
// enabled —— 它决定重启后要不要自动把小窗恢复出来。
//
// 踩过的坑：启动路径压根没读 enabled，于是「设置页显示已开启、但重启后小窗不见」。
// 这条断言把「读配置」这个动作钉住，避免有人再把它删掉。
func TestWantsWindowFollowsPersistedEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		want    bool
	}{
		{"上次开着", true},
		{"上次关着", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "floatwin.json")
			if err := os.WriteFile(path, []byte(`{"enabled":`+boolStr(tc.want)+`,"range":"today","theme":"dark","items":["inflight"]}`), 0o644); err != nil {
				t.Fatal(err)
			}
			f := newFloatWin(path, "127.0.0.1:1", "")
			if got := f.wantsWindow(); got != tc.want {
				t.Errorf("wantsWindow()=%v want %v（启动路径靠它决定是否恢复悬浮窗）", got, tc.want)
			}
		})
	}
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

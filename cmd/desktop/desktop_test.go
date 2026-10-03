//go:build windows

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"free2api/internal/gateway"
)

func TestHostPort(t *testing.T) {
	cases := []struct{ in, want string }{
		{"127.0.0.1:7863", "127.0.0.1:7863"},
		{":7863", "127.0.0.1:7863"},
		{"0.0.0.0:7863", "0.0.0.0:7863"},
		{"[::1]:7863", "[::1]:7863"},
		{"", "127.0.0.1:7863"},
		{"localhost:9000", "localhost:9000"},
	}
	for _, c := range cases {
		if got := hostPort(c.in); got != c.want {
			t.Errorf("hostPort(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := baseURL(":7863"); got != "http://127.0.0.1:7863" {
		t.Errorf("baseURL(:7863) = %q", got)
	}
}

func TestProbeFree2API(t *testing.T) {
	if probeFree2API("127.0.0.1:1") {
		t.Error("空端口不该被认成网关")
	}
}

// TestStartStopCycle 跑一遍真正的启停：网关起来 → /healthz 认得出 → 停掉 → 端口释放。
// 这是桌面程序最核心的那条链路（同进程托管），必须能回归。
func TestStartStopCycle(t *testing.T) {
	dir := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	cfg := fmt.Sprintf(`{"listen":%q,"api_key":"","auth_dir":"./auths","state_file":"./data/state.json","admin":{"enabled":true}}`, addr)
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)

	d := &desktop{cfgPath: cfgPath, exeDir: dir, gwAddr: addr}
	d.proxy = newProxy(addr)

	if err := d.startGateway(); err != nil {
		t.Fatalf("startGateway: %v", err)
	}
	defer d.stopGateway()

	if !probeFree2API(addr) {
		t.Fatal("启动后 /healthz 没认出 free2api")
	}
	st := d.state()
	if !st.Running || !st.Desktop || st.Error != "" {
		t.Fatalf("state = %+v", st)
	}
	if st.Base != "http://"+addr {
		t.Fatalf("Base = %q", st.Base)
	}

	if err := d.stopGateway(); err != nil {
		t.Fatalf("stopGateway: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for probeFree2API(addr) {
		if time.Now().After(deadline) {
			t.Fatal("停止后端口仍在应答")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if st := d.state(); st.Running || st.External {
		t.Fatalf("停止后 state = %+v", st)
	}
	// 再停一次：幂等，不该报错（页面上的按钮点两下很正常）。
	if err := d.stopGateway(); err != nil {
		t.Fatalf("重复 stop 报错: %v", err)
	}
}

// TestStopWhenNothingRunning：没有网关可停时应当安静地成功——用户点「停止网关」
// 而网关早就停了，不该看到红色报错。
func TestStopWhenNothingRunning(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	d := &desktop{cfgPath: filepath.Join(t.TempDir(), "config.json"), gwAddr: addr}
	if err := d.stopGateway(); err != nil {
		t.Fatalf("stopGateway = %v, want nil", err)
	}
}

// TestEnsureConfigCreatesMinimalFile：第一次运行时自动生成配置，且生成的配置必须
// 能过网关自己的归一化（否则双击后还是起不来）。
func TestEnsureConfigCreatesMinimalFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "sub", "config.json")
	if err := ensureConfig(cfgPath); err != nil {
		t.Fatalf("ensureConfig: %v", err)
	}
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("配置没生成: %v", err)
	}
	if got := resolveListen(cfgPath); got != defaultListen {
		t.Fatalf("resolveListen = %q, want %q", got, defaultListen)
	}
	// 生成的配置必须能被网关解析 + 归一化通过。
	if _, err := gateway.Load(cfgPath); err != nil {
		t.Fatalf("生成的配置过不了 gateway.Load: %v", err)
	}
	// 已存在时不覆盖。
	if err := os.WriteFile(cfgPath, []byte(`{"listen":"127.0.0.1:9999"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureConfig(cfgPath); err != nil {
		t.Fatal(err)
	}
	if got := resolveListen(cfgPath); got != "127.0.0.1:9999" {
		t.Fatalf("ensureConfig 覆盖了已有配置：listen = %q", got)
	}
}

// TestIconResourceMatchesConstant 盯住 iconResourceID 与 exe 里真实的图标组资源号一致。
//
// 为什么值得单独测：图标组资源号由 rsrc 分配（清单占 1，图标组拿 2），写错了不会
// 报任何错——只是窗口 HICON 变成 NULL，任务栏悄悄用系统默认图标。这种「静默退化」
// 靠肉眼很难第一时间发现，所以让测试来盯。
func TestIconResourceMatchesConstant(t *testing.T) {
	kernel32 := windows.NewLazySystemDLL("kernel32.dll")
	enumNames := kernel32.NewProc("EnumResourceNamesW")

	var hInst windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &hInst); err != nil {
		t.Fatal(err)
	}

	const rtGroupIcon = 14
	var ids []int64
	cb := windows.NewCallback(func(_ uintptr, _ uintptr, lpName uintptr, _ uintptr) uintptr {
		ids = append(ids, int64(lpName))
		return 1
	})
	r, _, err := enumNames.Call(uintptr(hInst), rtGroupIcon, cb, 0)
	if r == 0 {
		t.Fatalf("枚举图标组资源失败（图标资源没被链接进测试二进制？）: %v", err)
	}
	if len(ids) == 0 {
		t.Fatal("exe 里没有任何图标组资源（rsrc_windows_*.syso 是不是没进版本库？）")
	}
	found := false
	for _, id := range ids {
		if id == iconResourceID {
			found = true
		}
	}
	if !found {
		t.Fatalf("iconResourceID=%d 在真实资源 %v 里不存在——LoadImageW 会失败，窗口没图标", iconResourceID, ids)
	}
}

func TestStateViewJSON(t *testing.T) {
	d := &desktop{cfgPath: "C:/tmp/config.json", gwAddr: "127.0.0.1:7863"}
	raw, err := json.Marshal(d.state())
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["desktop"] != true {
		t.Errorf("desktop = %v", got["desktop"])
	}
	if got["listen"] != "127.0.0.1:7863" {
		t.Errorf("listen = %v", got["listen"])
	}
}

// TestDefaultWindowSizeStaysSane 锁住窗口默认尺寸的两条不变量：
// 永远不会大到超过工作区，也永远不会小到布局散架的下限以下（除非屏幕本身更小）。
// 这两个 getter 在非 Windows 上不存在，所以整个文件已带 windows build tag。
func TestDefaultWindowSizeStaysSane(t *testing.T) {
	w, h := defaultWindowSize()
	if w <= 0 || h <= 0 {
		t.Fatalf("defaultWindowSize() = %dx%d", w, h)
	}
	if wa, ok := primaryWorkArea(); ok {
		if workW, workH := int(wa.Right-wa.Left), int(wa.Bottom-wa.Top); workW > 0 && workH > 0 {
			if w > workW || h > workH {
				t.Errorf("窗口 %dx%d 超过工作区 %dx%d", w, h, workW, workH)
			}
		}
	}
	if w < 960 || h < 640 {
		t.Errorf("窗口 %dx%d 小于最小可用尺寸 960x640", w, h)
	}
}

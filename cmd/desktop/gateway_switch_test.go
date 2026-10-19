//go:build windows

package main

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestGatewaySwitchRevivesDeadHostedInstance 复现「启动网关」开关静默失效：
// 托管实例被外部停掉（另一个窗口点「停止网关」走 /admin/shutdown、或内部致命错误退出）后，
// d.inst 仍非 nil，startGateway 直接 return nil，开关再也打不开。
func TestGatewaySwitchRevivesDeadHostedInstance(t *testing.T) {
	dir := t.TempDir()
	authDir := filepath.Join(dir, "auths")
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  "listen": "127.0.0.1:17864",
  "api_key": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "admin": { "enabled": true },
  "schedule": { "checkin_enabled": false, "cat_enabled": false }
}`
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &desktop{cfgPath: cfgPath}
	d.gwAddr = resolveListen(cfgPath)
	d.proxy = newProxy(d.gwAddr)
	defer d.stopGateway()

	if err := d.startGateway(); err != nil {
		t.Fatalf("首次启动失败: %v", err)
	}
	if !probeFree2API(d.gwAddr) {
		t.Fatal("首次启动后端口没有在应答")
	}

	// 模拟「另一个窗口/外部工具」把网关停掉：只停监听，不动本进程。
	if err := shutdownViaHTTP(d.gwAddr); err != nil {
		t.Fatalf("外部停机失败: %v", err)
	}
	if probeFree2API(d.gwAddr) {
		t.Fatal("停机后端口仍应答，用例前提不成立")
	}
	// 清零探测缓存，把「缓存」这个干扰项排除掉，只看 startGateway 的短路判断。
	d.mu.Lock()
	d.probeAt, d.probeOK = time.Time{}, false
	d.mu.Unlock()

	if err := d.startGateway(); err != nil {
		t.Logf("第二次 startGateway 返回错误: %v", err)
	}
	if !probeFree2API(d.gwAddr) {
		t.Fatalf("点「启动网关」后端口仍未监听：startGateway 对已死实例短路了（d.inst != nil）")
	}
}

// TestGatewayStopDoesNotReportExternal 复现「停止网关」后响应仍报 external:true：
// 悬浮窗自愈循环每 5s 调一次 externalAlive，缓存被写成 true；stop 后不失效缓存，
// state() 就把「已停止」报成「运行中（外部进程）」，页面按钮仍停在「停止网关」。
func TestGatewayStopDoesNotReportExternal(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  "listen": "127.0.0.1:17865",
  "api_key": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "admin": { "enabled": true },
  "schedule": { "checkin_enabled": false, "cat_enabled": false }
}`
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &desktop{cfgPath: cfgPath}
	d.gwAddr = resolveListen(cfgPath)
	d.proxy = newProxy(d.gwAddr)
	if err := d.startGateway(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}

	// 模拟悬浮窗 ensureFloatLoop 的那次探测：网关活着，缓存被写成「外部可应答」。
	if !d.externalAlive(d.gwAddr) {
		t.Fatal("网关在跑时探测应命中")
	}

	if err := d.stopGateway(); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	st := d.state()
	if st.Running || st.External {
		t.Fatalf("停止后状态不诚实：running=%v external=%v（端口已释放）", st.Running, st.External)
	}
}

// TestKeepalivePauseFollowsGatewaySwitch 开关必须同时管住看门狗：
// 启动 = 删 pause（可被拉起/自启），停止 = 写 pause（谁也别再动它）。
func TestKeepalivePauseFollowsGatewaySwitch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
  "listen": "127.0.0.1:17866",
  "api_key": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "admin": { "enabled": true },
  "schedule": { "checkin_enabled": false, "cat_enabled": false }
}`
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &desktop{cfgPath: cfgPath, deployDir: dir, exeDir: dir}
	d.gwAddr = resolveListen(cfgPath)
	d.proxy = newProxy(d.gwAddr)

	// 上一次「停止」留下的 pause：启动必须把它撤掉，否则用户重新开 exe 也不会自启。
	if err := setKeepalivePaused(dir, true); err != nil {
		t.Fatal(err)
	}
	if err := d.startGateway(); err != nil {
		t.Fatalf("启动失败: %v", err)
	}
	if _, err := os.Stat(keepalivePausePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("启动后 pause 还在：%v", err)
	}

	if err := d.stopGateway(); err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if _, err := os.Stat(keepalivePausePath(dir)); err != nil {
		t.Fatalf("停止后没有 pause，看门狗会把网关拉回来：%v", err)
	}
	if probeFree2API(d.gwAddr) {
		t.Fatal("停止后端口仍应答")
	}
}

// TestProxyProbeAddr 系统代理值的两种写法都要能归一到可拨地址：
// 「127.0.0.1:7890」和「http=127.0.0.1:7890;https=127.0.0.1:7890」。
func TestProxyProbeAddr(t *testing.T) {
	cases := map[string]string{
		"":                                    "",
		"127.0.0.1:7890":                      "127.0.0.1:7890",
		"http=127.0.0.1:7890;https=1.2.3.4:1": "127.0.0.1:7890",
		"localhost":                           "localhost:80",
		"socks=127.0.0.1:1080":                "",
	}
	for in, want := range cases {
		if got := proxyProbeAddr(in); got != want {
			t.Errorf("proxyProbeAddr(%q)=%q, want %q", in, got, want)
		}
	}
}

// TestFakeIPNet 198.18.0.0/15 必须命中，真实公网地址不能命中。
func TestFakeIPNet(t *testing.T) {
	for _, s := range []string{"198.18.0.7", "198.19.255.254"} {
		if !fakeIPNet.Contains(net.ParseIP(s)) {
			t.Errorf("%s 应判为 fake-ip", s)
		}
	}
	for _, s := range []string{"43.160.158.125", "198.17.0.1", "198.20.0.1"} {
		if fakeIPNet.Contains(net.ParseIP(s)) {
			t.Errorf("%s 不应判为 fake-ip", s)
		}
	}
}

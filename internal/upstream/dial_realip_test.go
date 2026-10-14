package upstream

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNeedsRealIPDial 命中判定：workbuddy.ai 家族（精确 + 子域 + 大小写 + 尾点）命中；
// 其它上游（CN / zcode / opencode / kilo / google 等）一律不命中。
func TestNeedsRealIPDial(t *testing.T) {
	hit := []string{"workbuddy.ai", "www.workbuddy.ai", "WWW.WorkBuddy.AI", "www.workbuddy.ai.", "api.workbuddy.ai"}
	for _, h := range hit {
		if !needsRealIPDial(h) {
			t.Errorf("needsRealIPDial(%q)=false want true", h)
		}
	}
	miss := []string{"", "copilot.tencent.com", "workbuddy.ai.evil.com", "notworkbuddy.ai", "workbuddy.cn", "open.bigmodel.cn"}
	for _, h := range miss {
		if needsRealIPDial(h) {
			t.Errorf("needsRealIPDial(%q)=true want false", h)
		}
	}
}

// TestParseDoHIPv4 解析：只取 type=1 的 A 记录；CNAME/AAAA/其它忽略；
// TTL 取最小正值；坏 JSON 返回 nil。
func TestParseDoHIPv4(t *testing.T) {
	body := []byte(`{"Status":0,"Answer":[
		{"name":"www.workbuddy.ai","type":5,"TTL":50,"data":"www.workbuddy.ai.eo.dnse1.com"},
		{"name":"www.workbuddy.ai","type":1,"TTL":60,"data":"43.175.156.10"},
		{"name":"www.workbuddy.ai","type":1,"TTL":14,"data":"43.160.158.125"},
		{"name":"www.workbuddy.ai","type":28,"TTL":30,"data":"2001:db8::1"}
	]}`)
	ips, ttl := parseDoHIPv4(body)
	if len(ips) != 2 || ips[0] != "43.175.156.10" || ips[1] != "43.160.158.125" {
		t.Fatalf("ips=%v want [43.175.156.10 43.160.158.125]", ips)
	}
	if ttl != 14 {
		t.Errorf("ttl=%d want 14 (min positive)", ttl)
	}
	if ips, _ := parseDoHIPv4([]byte("not json")); ips != nil {
		t.Errorf("bad json -> %v want nil", ips)
	}
	if ips, _ := parseDoHIPv4([]byte(`{"Status":3}`)); ips != nil {
		t.Errorf("NXDOMAIN -> %v want nil", ips)
	}
}

// TestClampDoHTTL 边界：<=0 缺省 5m；<60s 夹到 60s；>30m 夹到 30m；区间内原样。
func TestClampDoHTTL(t *testing.T) {
	cases := []struct {
		in   int
		want time.Duration
	}{
		{0, dohTTLDefault}, {-1, dohTTLDefault}, {14, dohTTLMin}, {60, dohTTLMin},
		{300, 5 * time.Minute}, {int(dohTTLMax/time.Second) + 1, dohTTLMax},
	}
	for _, c := range cases {
		if got := clampDoHTTL(c.in); got != c.want {
			t.Errorf("clampDoHTTL(%d)=%v want %v", c.in, got, c.want)
		}
	}
}

// fakeDialer 记录拨号地址并按脚本返回连接/错误。
type fakeDialer struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error // addr -> err（nil = 成功）
	conn  net.Conn         // 成功时返回（可为 nil）
}

func (f *fakeDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.calls = append(f.calls, addr)
	f.mu.Unlock()
	if f.fail != nil {
		if err, ok := f.fail[addr]; ok {
			return nil, err
		}
	}
	return f.conn, nil
}

func (f *fakeDialer) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]string(nil), f.calls...)
	return cp
}

// TestRealIPDialerTargetUsesDoHIP 目标主机：DoH 结果按序试拨，第一个成功即返回，
// 不再试后续 IP，也绝不回落域名拨号。
func TestRealIPDialerTargetUsesDoHIP(t *testing.T) {
	base := &fakeDialer{}
	d := &realIPDialer{base: base, dohResolve: func(context.Context, string) []string {
		return []string{"43.175.156.10", "43.160.158.125"}
	}}
	_, err := d.DialContext(context.Background(), "tcp", "www.workbuddy.ai:443")
	if err != nil {
		t.Fatalf("dial err=%v", err)
	}
	got := base.recorded()
	if len(got) != 1 || got[0] != "43.175.156.10:443" {
		t.Fatalf("dialed=%v want [43.175.156.10:443]", got)
	}
}

// TestRealIPDialerFallsThroughToNextIP 首个 IP 失败 → 试第二个；成功即止。
func TestRealIPDialerFallsThroughToNextIP(t *testing.T) {
	base := &fakeDialer{fail: map[string]error{"43.175.156.10:443": errors.New("boom")}}
	d := &realIPDialer{base: base, dohResolve: func(context.Context, string) []string {
		return []string{"43.175.156.10", "43.160.158.125"}
	}}
	if _, err := d.DialContext(context.Background(), "tcp", "www.workbuddy.ai:443"); err != nil {
		t.Fatalf("dial err=%v", err)
	}
	got := base.recorded()
	if len(got) != 2 || got[0] != "43.175.156.10:443" || got[1] != "43.160.158.125:443" {
		t.Fatalf("dialed=%v want both IPs in order", got)
	}
}

// TestRealIPDialerDoHEmptyFallsBackToSystem DoH 无结果 → 回落原域名拨号（加固前行为）。
func TestRealIPDialerDoHEmptyFallsBackToSystem(t *testing.T) {
	base := &fakeDialer{}
	d := &realIPDialer{base: base, dohResolve: func(context.Context, string) []string { return nil }}
	if _, err := d.DialContext(context.Background(), "tcp", "www.workbuddy.ai:443"); err != nil {
		t.Fatalf("dial err=%v", err)
	}
	got := base.recorded()
	if len(got) != 1 || got[0] != "www.workbuddy.ai:443" {
		t.Fatalf("dialed=%v want [www.workbuddy.ai:443] (system fallback)", got)
	}
}

// TestRealIPDialerAllIPsFailFallsBack 全部 IP 失败 → 最后再试一次系统解析；
// 系统解析成功则连接成功（兜底可用性优先），最终错误取直拨失败。
func TestRealIPDialerAllIPsFailFallsBack(t *testing.T) {
	base := &fakeDialer{fail: map[string]error{
		"43.175.156.10:443":  errors.New("ip1 down"),
		"43.160.158.125:443": errors.New("ip2 down"),
	}}
	d := &realIPDialer{base: base, dohResolve: func(context.Context, string) []string {
		return []string{"43.175.156.10", "43.160.158.125"}
	}}
	_, err := d.DialContext(context.Background(), "tcp", "www.workbuddy.ai:443")
	if err != nil {
		t.Fatalf("system fallback should succeed, err=%v", err)
	}
	got := base.recorded()
	if len(got) != 3 || got[2] != "www.workbuddy.ai:443" {
		t.Fatalf("dialed=%v want 2 IPs + system fallback", got)
	}
}

// TestRealIPDialerNonTargetPassthrough 非目标主机与 IP 字面量直接透传，DoH 零调用。
func TestRealIPDialerNonTargetPassthrough(t *testing.T) {
	resolveCalls := 0
	base := &fakeDialer{}
	d := &realIPDialer{base: base, dohResolve: func(context.Context, string) []string {
		resolveCalls++
		return []string{"1.2.3.4"}
	}}
	for _, addr := range []string{"copilot.tencent.com:443", "open.bigmodel.cn:443", "1.2.3.4:443", "[2001:db8::1]:443"} {
		if _, err := d.DialContext(context.Background(), "tcp", addr); err != nil {
			t.Fatalf("dial %s err=%v", addr, err)
		}
	}
	got := base.recorded()
	if len(got) != 4 {
		t.Fatalf("passthrough calls=%v want 4", got)
	}
	for _, a := range got {
		if strings.Contains(a, "1.2.3.4") && !strings.HasSuffix(a, "1.2.3.4:443") {
			t.Errorf("unexpected rewrite: %s", a)
		}
	}
	if resolveCalls != 0 {
		t.Errorf("DoH called %d times for non-target hosts, want 0", resolveCalls)
	}
}

// TestRealIPDialerBadAddrPassthrough 非法 addr（缺端口）安全透传，不 panic。
func TestRealIPDialerBadAddrPassthrough(t *testing.T) {
	base := &fakeDialer{}
	d := &realIPDialer{base: base, dohResolve: func(context.Context, string) []string { return nil }}
	if _, err := d.DialContext(context.Background(), "tcp", "www.workbuddy.ai"); err != nil {
		t.Fatalf("err=%v", err)
	}
	if got := base.recorded(); len(got) != 1 || got[0] != "www.workbuddy.ai" {
		t.Fatalf("dialed=%v want raw passthrough", got)
	}
}

// TestDoHCacheHitAndNegativeCache 缓存行为：正结果在 TTL 内零重复解析；
// 负结果短缓存后允许重探（用手工过期模拟 30s 窗口）。
func TestDoHCacheHitAndNegativeCache(t *testing.T) {
	dohCache.Lock()
	dohCache.m = map[string]dohCacheEntry{}
	dohCache.Unlock()
	t.Cleanup(func() {
		dohCache.Lock()
		dohCache.m = map[string]dohCacheEntry{}
		dohCache.Unlock()
	})

	// 预置正缓存：10 分钟内不再解析（注入 resolve 不可见——直接读缓存函数）。
	dohCache.Lock()
	dohCache.m["www.workbuddy.ai"] = dohCacheEntry{ips: []string{"43.175.156.10"}, expire: time.Now().Add(10 * time.Minute)}
	dohCache.Unlock()
	if ips := resolveRealIPViaDoH(context.Background(), "www.workbuddy.ai"); len(ips) != 1 || ips[0] != "43.175.156.10" {
		t.Fatalf("cache hit ips=%v", ips)
	}

	// 负缓存：未过期 → 直接返回 nil（不可能在没有网络时也耗时 6s）。
	dohCache.Lock()
	dohCache.m["dead.example"] = dohCacheEntry{ips: nil, expire: time.Now().Add(dohNegTTL)}
	dohCache.Unlock()
	start := time.Now()
	if ips := resolveRealIPViaDoH(context.Background(), "dead.example"); ips != nil {
		t.Fatalf("negative cache ips=%v want nil", ips)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("negative cache should return instantly, took %v", elapsed)
	}
}

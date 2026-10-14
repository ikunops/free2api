// dial_realip.go 国际版（workbuddy.ai）出站 IP 直拨加固。
//
// 背景（2026-10-14 真机实测，TUN/fake-ip 环境如 FlClash）：
//   - 系统解析 www.workbuddy.ai 返回 198.18.x.x 假地址（fake-ip）；
//   - 按域名经代理节点连接时，节点侧二次解析拿到不可用目标，TLS 握手直接 EOF；
//   - 同一节点按「真实 IP 直拨」完全可用（43.175.156.10 / 43.160.158.125 均 200）。
//
// 因此 workbuddy.ai 家族出站不再依赖系统 DNS：先用 DoH 解析真实 IPv4（TTL 缓存 +
// 30s 负缓存），按 IP 逐一直拨；HTTP Host / TLS SNI 原样保留（证书仍按域名校验，
// 无安全降级）。DoH 全失败或直拨全失败 → 回落系统解析（加固前行为，兜底不变）。
// CN 域（copilot.tencent.com 等）与其它上游保持既有系统解析路径不动——实测它们在
// TUN 下走「域名路由」工作正常，不做无谓变更。
package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// realIPDialHosts 需要 DoH 真实 IP 直拨的上游主机（精确匹配或点后缀匹配）。
// 目前仅国际版 workbuddy.ai 家族——实测被节点侧域名解析破坏的唯一上游。
var realIPDialHosts = []string{"workbuddy.ai"}

// needsRealIPDial 判定主机是否需要走「DoH 解析 + IP 直拨」。
func needsRealIPDial(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if host == "" {
		return false
	}
	for _, h := range realIPDialHosts {
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// contextDialer net.Dialer 与测试桩共用的最小拨号面。
type contextDialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// dohResolveFunc DoH 解析面（测试可注入）。
type dohResolveFunc func(ctx context.Context, host string) []string

// realIPDialer 包装系统拨号器：命中 realIPDialHosts 的主机先用 DoH 拿真实 IP 再
// 逐 IP 直拨；不命中 / 解析失败 / 全部直拨失败 → 回落原系统拨号（加固前行为）。
// 返回的连接对上层透明：net/http 仍按 URL 主机发 SNI 与 Host 头。
type realIPDialer struct {
	base       contextDialer
	dohResolve dohResolveFunc
}

// newRealIPDialer 构造生产拨号器（DoH 走真实网络解析）。
func newRealIPDialer(base contextDialer) *realIPDialer {
	return &realIPDialer{base: base, dohResolve: resolveRealIPViaDoH}
}

// DialContext 实现 http.Transport.DialContext 签名。
func (d *realIPDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" || net.ParseIP(host) != nil || !needsRealIPDial(host) {
		return d.base.DialContext(ctx, network, addr)
	}
	var lastErr error
	for _, ip := range d.dohResolve(ctx, host) {
		conn, derr := d.base.DialContext(ctx, network, net.JoinHostPort(ip, port))
		if derr == nil {
			return conn, nil
		}
		lastErr = derr
	}
	// DoH 无结果或全部直拨失败：回落系统解析（加固前行为）。
	conn, ferr := d.base.DialContext(ctx, network, addr)
	if ferr == nil {
		return conn, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, ferr
}

// dohEndpoints DoH JSON API 端点（按序尝试，第一个成功即用）。doh.pub（腾讯）国内
// 外均实测可达；1.1.1.1 作海外兜底。都失败 → 返回 nil，调用方回落系统解析。
var dohEndpoints = []string{
	"https://doh.pub/dns-query",
	"https://1.1.1.1/dns-query",
}

// dohHTTPClient DoH 专用客户端：零值 Transport 的 Proxy 为 nil（不读 HTTP(S)_PROXY
// 环境变量，避免解析请求被环境代理绕进死环）；总超时 4s。
var dohHTTPClient = &http.Client{
	Timeout: 4 * time.Second,
	Transport: &http.Transport{
		TLSHandshakeTimeout: 3 * time.Second,
		MaxIdleConnsPerHost: 2,
		IdleConnTimeout:     90 * time.Second,
	},
}

// DoH 结果缓存时长：TTL 取响应值并夹在 [60s, 30m]；缺省 5m；失败负缓存 30s。
const (
	dohTTLMin     = 60 * time.Second
	dohTTLMax     = 30 * time.Minute
	dohTTLDefault = 5 * time.Minute
	dohNegTTL     = 30 * time.Second
)

type dohCacheEntry struct {
	ips    []string // nil = 负缓存（无结果）
	expire time.Time
}

var dohCache = struct {
	sync.Mutex
	m map[string]dohCacheEntry
}{m: map[string]dohCacheEntry{}}

// resolveRealIPViaDoH 查询并缓存 host 的真实 IPv4 列表；全端点失败返回 nil。
// 命中缓存（含负缓存）零网络开销。整体解析预算 6s，随拨号 ctx 取消。
func resolveRealIPViaDoH(ctx context.Context, host string) []string {
	now := time.Now()
	dohCache.Lock()
	if e, ok := dohCache.m[host]; ok && now.Before(e.expire) {
		ips := e.ips
		dohCache.Unlock()
		return ips
	}
	dohCache.Unlock()

	rctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	ips, ttl := dohQueryIPv4(rctx, host)
	life := dohNegTTL
	if len(ips) > 0 {
		life = clampDoHTTL(ttl)
	}
	dohCache.Lock()
	dohCache.m[host] = dohCacheEntry{ips: ips, expire: time.Now().Add(life)}
	dohCache.Unlock()
	return ips
}

func clampDoHTTL(ttl int) time.Duration {
	d := time.Duration(ttl) * time.Second
	switch {
	case d <= 0:
		return dohTTLDefault
	case d < dohTTLMin:
		return dohTTLMin
	case d > dohTTLMax:
		return dohTTLMax
	}
	return d
}

// dohQueryIPv4 按 dohEndpoints 顺序查询 A 记录，第一个有结果的端点胜出。
func dohQueryIPv4(ctx context.Context, host string) ([]string, int) {
	for _, ep := range dohEndpoints {
		if ips, ttl := dohQueryOne(ctx, ep, host); len(ips) > 0 {
			return ips, ttl
		}
	}
	return nil, 0
}

// dohQueryOne 单端点查询（application/dns-json 形态，兼容 doh.pub / 1.1.1.1 等）。
func dohQueryOne(ctx context.Context, endpoint, host string) ([]string, int) {
	u := endpoint + "?name=" + url.QueryEscape(host) + "&type=A"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0
	}
	req.Header.Set("Accept", "application/dns-json")
	resp, err := dohHTTPClient.Do(req)
	if err != nil {
		return nil, 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, 0
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, 0
	}
	return parseDoHIPv4(body)
}

type dohAnswer struct {
	Type int    `json:"type"`
	TTL  int    `json:"ttl"`
	Data string `json:"data"`
}

type dohResponse struct {
	Status int         `json:"Status"`
	Answer []dohAnswer `json:"Answer"`
}

// parseDoHIPv4 提取 Answer 中的 IPv4（type=1）记录；CNAME 链与 AAAA/其它类型忽略。
// 返回 (ips, 最小TTL)。
func parseDoHIPv4(body []byte) ([]string, int) {
	var r dohResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, 0
	}
	var ips []string
	ttl := 0
	for _, a := range r.Answer {
		if a.Type != 1 {
			continue
		}
		ip := net.ParseIP(a.Data)
		if ip == nil || ip.To4() == nil {
			continue
		}
		ips = append(ips, ip.String())
		if a.TTL > 0 && (ttl == 0 || a.TTL < ttl) {
			ttl = a.TTL
		}
	}
	return ips, ttl
}

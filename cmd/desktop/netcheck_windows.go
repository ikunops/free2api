//go:build windows

// netcheck_windows.go 启动时的网络自检：把「这台机器现在是不是被代理/TUN 接管了」
// 打成一行日志，排查 VPN 相关故障时不用再猜。
//
// 背景：TUN + fake-ip（FlClash/Clash 系）会同时改三样东西 —— 路由、DNS、系统代理。
// 三样里任何一样没对齐，表现都是「网关忽好忽坏、请求超时」：
//   - 系统代理指向一个没人监听的端口（关掉核心但没关系统代理，或混用两个客户端）；
//   - DNS 返回 198.18.x.x 假地址（fake-ip 生效）；
//   - 出站进 TUN 后按规则 DIRECT，而 DIRECT 自己又被 TUN 抓回来（真·回环）。
//
// 这里只做「看清现状」，不改任何系统设置：网关自己的出站不读环境代理，
// 但用户需要知道系统代理此刻指向哪里、那个端口活没活、DNS 是不是假地址。
package main

import (
	"log"
	"net"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

// fakeIPNet fake-ip 常用网段（mihomo 默认 198.18.0.1/16）。命中即说明 DNS 被接管。
var fakeIPNet = &net.IPNet{IP: net.IPv4(198, 18, 0, 0), Mask: net.CIDRMask(15, 32)}

// systemProxy 读 WinINET 的系统代理设置（HKCU，当前用户）。
func systemProxy() (enabled bool, server string) {
	k, err := registry.OpenKey(registry.CURRENT_USER,
		`Software\Microsoft\Windows\CurrentVersion\Internet Settings`, registry.QUERY_VALUE)
	if err != nil {
		return false, ""
	}
	defer k.Close()
	if v, _, err := k.GetIntegerValue("ProxyEnable"); err == nil && v == 1 {
		enabled = true
	}
	if v, _, err := k.GetStringValue("ProxyServer"); err == nil {
		server = strings.TrimSpace(v)
	}
	return enabled, server
}

// tcpAlive 端口上有没有人在听（400ms 预算，只用于自检）。
func tcpAlive(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// proxyProbeAddr 把系统代理里的 "127.0.0.1:7890" / "http=127.0.0.1:7890;https=..."
// 归一成能拨的 host:port。
func proxyProbeAddr(server string) string {
	if server == "" {
		return ""
	}
	if strings.Contains(server, "=") {
		for _, part := range strings.Split(server, ";") {
			kv := strings.SplitN(part, "=", 2)
			if len(kv) == 2 && strings.EqualFold(kv[0], "http") {
				return kv[1]
			}
		}
		return ""
	}
	if !strings.Contains(server, ":") {
		return server + ":80"
	}
	return server
}

// fakeIPActive 看系统解析结果是不是落在 fake-ip 网段。
// 用两个与业务无关的域名，避免把自检结果和 upstream 的 DNS 缓存混在一起。
func fakeIPActive() (bool, string) {
	for _, host := range []string{"www.gstatic.com", "example.com"} {
		ips, err := net.LookupHost(host)
		if err != nil {
			continue
		}
		for _, s := range ips {
			ip := net.ParseIP(s)
			if ip != nil && fakeIPNet.Contains(ip) {
				return true, host + " → " + ip.String()
			}
		}
	}
	return false, ""
}

// logNetSelfCheck 打一行自检结论。启动时调用一次即可。
//
// 典型输出：
//
//	网络自检: 系统代理=开 127.0.0.1:7890(不可达) 环境代理=空 DNS=fake-ip(198.18.0.7)
//
// 「系统代理不可达」几乎总是「VPN 客户端关了核心没关系统代理」，此时任何吃系统
// 代理的程序都会卡到超时；「DNS=fake-ip」说明 TUN 的 DNS 接管正在生效。
func logNetSelfCheck() {
	enabled, server := systemProxy()
	var parts []string
	switch {
	case enabled && server != "":
		probe := proxyProbeAddr(server)
		state := "不可达"
		if probe != "" && tcpAlive(probe) {
			state = "可达"
		}
		parts = append(parts, "系统代理=开 "+server+"("+state+")")
	case enabled:
		parts = append(parts, "系统代理=开 但没写地址")
	default:
		parts = append(parts, "系统代理=关")
	}

	envProxy := ""
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			envProxy = v
			break
		}
	}
	if envProxy == "" {
		parts = append(parts, "环境代理=空")
	} else {
		parts = append(parts, "环境代理="+envProxy+"（网关出站不读它）")
	}

	if ok, detail := fakeIPActive(); ok {
		parts = append(parts, "DNS=fake-ip("+detail+")")
	} else {
		parts = append(parts, "DNS=真实地址")
	}
	log.Printf("网络自检: %s", strings.Join(parts, " "))
}

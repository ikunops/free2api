package main

import (
	"net/http"
	"testing"
)

// TestRealmConfig 按 realm 映射上游 base 与 Origin/Referer origin：
// global → www.workbuddy.ai（base 与 origin 同域）；cn → copilot.tencent.com + codebuddy.cn。
// 非法/缺省 realm 回落 cn。这是「端点按 realm 动态切换」的映射层，生产值即此处常量。
func TestRealmConfig(t *testing.T) {
	cases := []struct {
		name       string
		realm      string
		wantBase   string
		wantOrigin string
	}{
		{name: "global", realm: realmGlobal, wantBase: "https://www.workbuddy.ai", wantOrigin: "https://www.workbuddy.ai"},
		{name: "cn", realm: realmCN, wantBase: "https://copilot.tencent.com", wantOrigin: "https://www.codebuddy.cn"},
		{name: "非法回落 cn", realm: "foo", wantBase: "https://copilot.tencent.com", wantOrigin: "https://www.codebuddy.cn"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, origin := realmConfig(c.realm)
			if base != c.wantBase {
				t.Errorf("realmConfig(%q) base=%q want %q", c.realm, base, c.wantBase)
			}
			if origin != c.wantOrigin {
				t.Errorf("realmConfig(%q) origin=%q want %q", c.realm, origin, c.wantOrigin)
			}
		})
	}
}

// TestCommonHeadersPerOrigin commonHeaders 按传入 origin 设置 Origin/Referer 头
// （Origin 不带尾斜杠、Referer 带尾斜杠，与上游 curl 抓包一致）。
func TestCommonHeadersPerOrigin(t *testing.T) {
	get := commonHeaders("https://www.workbuddy.ai")
	req, err := http.NewRequest(http.MethodGet, "https://x/", nil)
	if err != nil {
		t.Fatal(err)
	}
	get(req)
	if got := req.Header.Get("Origin"); got != "https://www.workbuddy.ai" {
		t.Errorf("Origin=%q want https://www.workbuddy.ai", got)
	}
	if got := req.Header.Get("Referer"); got != "https://www.workbuddy.ai/" {
		t.Errorf("Referer=%q want https://www.workbuddy.ai/", got)
	}
}
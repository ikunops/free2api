package upstream

import (
	"testing"

	"free2api/internal/auth"
)

// TestCommonHeadersCodeBuddyRequest 所有出站请求统一注入
// X-CodeBuddy-Request: 1（官方客户端风控闸门头，D1）。
// chat/billing/refresh 三类出站均覆盖。
func TestCommonHeadersCodeBuddyRequest(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	c := &Client{}

	// chat 路径
	req := mustRequest(t)
	c.ChatHeaders(req, a, "", ChatMeta{})
	if got := req.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("chat X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// billing 路径
	req2 := mustRequest(t)
	c.BillingHeaders(req2, a)
	if got := req2.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("billing X-CodeBuddy-Request = %q want %q", got, "1")
	}

	// refresh 路径
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, a)
	if got := req3.Header.Get("X-CodeBuddy-Request"); got != "1" {
		t.Errorf("refresh X-CodeBuddy-Request = %q want %q", got, "1")
	}
}

// TestRefreshHeadersAuthRefreshSource refresh 出站 X-Auth-Refresh-Source 对齐
// 官方客户端 refresh 渠道标识为 "plugin"（D3，原值 "workbuddy" 与官方不一致）。
func TestRefreshHeadersAuthRefreshSource(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}
	req := mustRequest(t)
	c := &Client{}
	c.RefreshHeaders(req, a)

	if got := req.Header.Get("X-Auth-Refresh-Source"); got != "plugin" {
		t.Errorf("X-Auth-Refresh-Source = %q want %q", got, "plugin")
	}
}

// TestCommonHeadersAcceptLanguageByRealm Accept-Language 按 realm 切（D5）：
// CN 账号 zh-CN，global 账号 en-US。chat/refresh 路径（走 CommonHeaders）均覆盖。
func TestCommonHeadersAcceptLanguageByRealm(t *testing.T) {
	c := &Client{}

	// CN 账号 chat
	cnA := &auth.Auth{AccessToken: "at", UID: "c1"}
	req := mustRequest(t)
	c.ChatHeaders(req, cnA, "", ChatMeta{})
	if got := req.Header.Get("Accept-Language"); got != "zh-CN" {
		t.Errorf("CN chat Accept-Language = %q want %q", got, "zh-CN")
	}

	// global 账号 chat
	glA := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	req2 := mustRequest(t)
	c.ChatHeaders(req2, glA, "", ChatMeta{})
	if got := req2.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("global chat Accept-Language = %q want %q", got, "en-US")
	}

	// CN 账号 refresh
	req3 := mustRequest(t)
	c.RefreshHeaders(req3, cnA)
	if got := req3.Header.Get("Accept-Language"); got != "zh-CN" {
		t.Errorf("CN refresh Accept-Language = %q want %q", got, "zh-CN")
	}

	// global 账号 refresh
	req4 := mustRequest(t)
	c.RefreshHeaders(req4, glA)
	if got := req4.Header.Get("Accept-Language"); got != "en-US" {
		t.Errorf("global refresh Accept-Language = %q want %q", got, "en-US")
	}
}

// TestAcceptHeaderStreamVsNonStream Accept 头分流式/非流式（D6）：
// chat 出站带 text/event-stream（流式），billing/refresh 出站仅 application/json
// （去掉宽松的 text/plain, */*）。
func TestAcceptHeaderStreamVsNonStream(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{AccessToken: "at", UID: "u1", RefreshToken: "rt"}

	// chat 流式：application/json, text/event-stream
	chatReq := mustRequest(t)
	c.ChatHeaders(chatReq, a, "", ChatMeta{})
	if got := chatReq.Header.Get("Accept"); got != "application/json, text/event-stream" {
		t.Errorf("chat Accept = %q want %q", got, "application/json, text/event-stream")
	}

	// billing 非流式：application/json
	bReq := mustRequest(t)
	c.BillingHeaders(bReq, a)
	if got := bReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("billing Accept = %q want %q", got, "application/json")
	}

	// refresh 非流式：application/json
	rReq := mustRequest(t)
	c.RefreshHeaders(rReq, a)
	if got := rReq.Header.Get("Accept"); got != "application/json" {
		t.Errorf("refresh Accept = %q want %q", got, "application/json")
	}
}

package upstream

import (
	"net/http"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// globalUAString global realm 默认出站 UA：第二段 platform 品牌为官方国际版
// `WorkBuddy AI`（intl 项目逆向证据：WorkBuddy/5.5.2 WorkBuddy AI/5.5.2 CLI/5.5.2），
// 版本段保持现有 clientVersion/cliVersion（本仓库 5.5.4/2.137.1），只切品牌段。
const globalUAString = "WorkBuddy/5.5.4 WorkBuddy AI/5.5.4 CLI/2.137.1"

// TestChatHeadersGlobalRealm global 账号的 chat 请求头对齐 intl 三件套：
//  1. UA 含 `WorkBuddy AI/<v>` 平台段（非 CN 的 `WorkBuddy/<v>`）；
//  2. X-No-Enterprise-Id: 1（个人账号无企业 ID，显式声明）；
//  3. X-Domain: www.workbuddy.ai（显式声明国际版域）。
//
// 与 CN 的差异是修复 global 403 code 11140 "request illegal" 风控的关键（
// ANALYSIS-global-chat-solutions.md）。
func TestChatHeadersGlobalRealm(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"} // global 账号
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("User-Agent"); got != globalUAString {
		t.Errorf("global UA = %q want %q", got, globalUAString)
	}
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("global X-No-Enterprise-Id = %q want %q", got, "1")
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global X-Domain = %q want %q", got, "www.workbuddy.ai")
	}
	// global 账号不落 CN 侧的 X-Enterprise-Id（企业头无意义，只发 No-* 声明）。
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("global X-Enterprise-Id = %q want empty", got)
	}
}

// TestChatHeadersCNRealm CN 账号头零回归：UA 不带 `WorkBuddy AI` 平台段、不注入
// global 专属的 X-No-Enterprise-Id / X-Domain: www.workbuddy.ai；EnterpriseID 非空
// 走原有 X-Enterprise-Id 分支，Domain 空走原有 X-No-Department-Info 分支。
func TestChatHeadersCNRealm(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "c1", EnterpriseID: "e1"} // CN 企业账号 + 无 domain
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("User-Agent"); strings.Contains(got, "WorkBuddy AI") {
		t.Errorf("CN UA = %q must NOT contain WorkBuddy AI (want %q)", got, defaultUAString)
	}
	if got := req.Header.Get("User-Agent"); got != defaultUAString {
		t.Errorf("CN UA = %q want %q", got, defaultUAString)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "e1" {
		t.Errorf("CN X-Enterprise-Id = %q want e1", got)
	}
	if got := req.Header.Get("X-No-Enterprise-Id"); got != "" {
		t.Errorf("CN X-No-Enterprise-Id = %q want empty (enterprise set)", got)
	}
	if got := req.Header.Get("X-Domain"); got != "" {
		t.Errorf("CN X-Domain = %q want empty (domain empty → X-No-Department-Info)", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "1" {
		t.Errorf("CN X-No-Department-Info = %q want 1 (zero regression)", got)
	}
}

// TestChatHeadersGlobalStrongOverride global 账号（domain 标记 global）即便带
// EnterpriseID（异常数据）也发国际客户端形态头：X-No-Enterprise-Id=1 +
// X-Domain=www.workbuddy.ai，不落 CN 侧企业头（覆盖注入顺序：global 分支先于
// 既有分支的强覆盖语义）。
func TestChatHeadersGlobalStrongOverride(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g2", EnterpriseID: "should-be-ignored", Domain: "www.workbuddy.ai"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("global(virtual enterprise) X-No-Enterprise-Id = %q want 1", got)
	}
	if got := req.Header.Get("X-Enterprise-Id"); got != "" {
		t.Errorf("global(virtual enterprise) X-Enterprise-Id = %q want empty", got)
	}
	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global(virtual enterprise) X-Domain = %q want www.workbuddy.ai", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "" {
		t.Errorf("global(virtual enterprise) X-No-Department-Info = %q want empty (X-Domain supersedes)", got)
	}
}

// TestChatHeadersGlobalWithConfiguredDomain global 账号即便显式配了登录 domain，
// chat 出站 X-Domain 也固定为 www.workbuddy.ai（国际客户端形态，不回退到会话原值）。
func TestChatHeadersGlobalWithConfiguredDomain(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "g3", Domain: "login.workbuddy.ai"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Domain"); got != "www.workbuddy.ai" {
		t.Errorf("global(with custom domain) X-Domain = %q want www.workbuddy.ai", got)
	}
}

// TestChatHeadersCNNoEnterpriseZeroRegression CN 无企业/无 domain 的老形态账号：
// X-No-Enterprise-Id=1 + X-No-Department-Info=1（各自缺省），不注入 global 的 X-Domain。
func TestChatHeadersCNNoEnterpriseZeroRegression(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "c2"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-No-Enterprise-Id"); got != "1" {
		t.Errorf("CN(no enterprise) X-No-Enterprise-Id = %q want 1", got)
	}
	if got := req.Header.Get("X-No-Department-Info"); got != "1" {
		t.Errorf("CN(no domain) X-No-Department-Info = %q want 1", got)
	}
	if got := req.Header.Get("X-Domain"); got != "" {
		t.Errorf("CN(no enterprise) X-Domain = %q want empty", got)
	}
}

// TestDefaultWorkBuddyUAForGlobal UA 品牌段按 realm 切换的单元级断言：
// global → 第二段 `WorkBuddy AI`；CN → 第二段 `WorkBuddy`。
func TestDefaultWorkBuddyUAForGlobal(t *testing.T) {
	c := &Client{}
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{}); got != defaultUAString {
		t.Errorf("defaultWorkBuddyUAFor(cn) = %q want %q", got, defaultUAString)
	}
	if got := c.defaultWorkBuddyUAFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != globalUAString {
		t.Errorf("defaultWorkBuddyUAFor(global) = %q want %q", got, globalUAString)
	}
	// version 覆盖仍生效：global 平台段跟随 clientVersion。
	c2 := &Client{ClientVersion: "6.0.0"}
	if got := c2.defaultWorkBuddyUAFor(&auth.Auth{Domain: "www.workbuddy.ai"}); got != "WorkBuddy/6.0.0 WorkBuddy AI/6.0.0 CLI/2.137.1" {
		t.Errorf("defaultWorkBuddyUAFor(global, v6) = %q", got)
	}
}

// mustRequest 构造一个无 body 的 POST 请求（测试 ChatHeaders 用）。
func mustRequest(t *testing.T) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://chat.example/console/chat/completions", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	return req
}
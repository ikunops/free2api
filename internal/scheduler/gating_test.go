package scheduler

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// TestGlobalAccountsSkipCheckinTravel gating（活动已放开，见下方两个 activity 用例）：
// 池内 global 账号在 checkin/travel 两类循环中**零上游调用**；activity 已不再跳过
// global（PR #45 实测 /v2/report 在 workbuddy.ai code=0 OK）。
//
// fake upstream 全路径统计调用数：global 账号若被误放行，会打到 fake server
// 的 /daily-checkin /buddy/info 等任意路径，调用数即 >0。
func TestGlobalAccountsSkipCheckinTravel(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for global", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	// 显式 realm=global（下行 Domain 兜底场景在 auth 包单测覆盖，这里直接构造 global）。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up})

	// 签到/旅行循环：global 账号不应发起任何上游调用。
	s.CheckinAll()
	s.RunTravelNow()

	if n := calls.Load(); n != 0 {
		t.Errorf("global account checkin/travel upstream calls=%d want 0（checkin/travel 仍跳过）", n)
	}
}

// TestGlobalAccountSkippedListedWithStatus 门控跳过的 global 账号在 CheckinAll
// 回执里以 skipped(global) 呈现——手动触发时结果可读，不再是"未覆盖"的空白。
func TestGlobalAccountSkippedListedWithStatus(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no upstream call expected for global", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	out, err := s.CheckinAll()
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(out) != 1 || out[0].UID != "g1" {
		t.Fatalf("out=%+v want 1 row for g1", out)
	}
	if out[0].Status != CheckinSkipped {
		t.Errorf("status=%q want skipped（global 门控回执）", out[0].Status)
	}
	if out[0].Detail != "global" {
		t.Errorf("detail=%q want global", out[0].Detail)
	}
	if calls.Load() != 0 {
		t.Errorf("upstream calls=%d want 0", calls.Load())
	}
}

// TestGlobalAndCNMixedPoolServed 混池：activity **两类都上报**（global 不再跳过，
// PR #45 实测 workbuddy.ai /v2/report code=0 OK）；travel 仍只服务 CN（global 跳过）。
// 一次 fake upstream 同时统计两类账号真正打到的请求——区隔"放行"不是池空。
func TestGlobalAndCNMixedPoolServed(t *testing.T) {
	fastTravel(t)
	fastActivity(t)

	stub := &reportStub{} // /v2/report 统计 + X-User-Id 判定
	growth := &travelStub{buddy: "null"}
	// 合并 growth/billing 端点：activity 的 report + travel 的 buddy 都要能被两类账号打到。
	both := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/report" {
			stub.handler().ServeHTTP(w, r)
			return
		}
		growth.handler().ServeHTTP(w, r)
	}))
	defer both.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: both.Client(), ChatBaseCN: both.URL, BillingBaseCN: both.URL,
		BillingBaseGlobal: both.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()
	// CN + global 各上报一次（global 不再跳过）。
	if n := stub.calls.Load(); n != 2 {
		t.Errorf("activity report calls=%d want 2（CN + global 各 1）", n)
	}
	// activity 阶段会顺带领猫（travelAdoptForce），把 buddy/info 计数打进快照；
	// 旅行检验只数 RunTravelNow 这段的增量（global 被 travel 跳过，只有 CN 走）。
	infoBefore := growth.infoCalls.Load()

	s.RunTravelNow()
	infoDelta := growth.infoCalls.Load() - infoBefore
	if infoDelta != 1 {
		t.Errorf("CN travel buddy-info delta=%d want 1（仅 CN 账号旅行）", infoDelta)
	}
}

// TestRunActivityNowReportsGlobalAccounts 核心验收：global 账号的 report 上报**不再跳过**
// （PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK）。fake upstream 断言
// global 账号真的发出 /v2/report 请求；且按 realm 路由到 global billing base。
func TestRunActivityNowReportsGlobalAccounts(t *testing.T) {
	fastActivity(t)

	var globalCalls, cnCalls atomic.Int32
	reportOK := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			t.Errorf("path=%s want /v2/report", r.URL.Path)
		}
		w.Write([]byte(`{"code":0,"msg":"OK"}`))
	})
	globalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		globalCalls.Add(1)
		reportOK.ServeHTTP(w, r)
	}))
	defer globalSrv.Close()
	cnSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cnCalls.Add(1)
		reportOK.ServeHTTP(w, r)
	}))
	defer cnSrv.Close()

	p := pool.New("")
	// 纯 global 池：若 report 仍被 IsGlobal() 跳过，globalSrv 计数保持 0。
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: globalSrv.Client(), ChatBaseCN: cnSrv.URL, BillingBaseCN: cnSrv.URL,
		BillingBaseGlobal: globalSrv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow()

	if n := globalCalls.Load(); n != 1 {
		t.Errorf("global account report calls=%d want 1（global 上报不再跳过）", n)
	}
	if n := cnCalls.Load(); n != 0 {
		t.Errorf("global account 不应打到 CN base: cn_calls=%d", n)
	}
}

// TestRunActivityNowGlobalReportFailWarnsButOthersProceed global report 失败（如上游
// 不认 /v2/report）→ 记 WARN 不影响其他账号：同一趟里 CN 账号照常上报。
func TestRunActivityNowGlobalReportFailWarnsButOthersProceed(t *testing.T) {
	fastActivity(t)

	var cnCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			uid := r.Header.Get("X-User-Id")
			if uid == "g1" {
				http.Error(w, "no such report endpoint on global", 404)
				return
			}
			cnCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "cn1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.RunActivityNow() // 不应 panic，g1 的 report 失败只记 WARN

	if n := cnCalls.Load(); n != 1 {
		t.Errorf("CN account report calls=%d want 1（global 失败不影响 CN）", n)
	}
}

// TestRunKeepaliveStillRefreshesGlobal keepalive 不拦 global：token refresh 端点
// 对 global 存在，刷新调用照发。
func TestRunKeepaliveStillRefreshesGlobal(t *testing.T) {
	f := &fakeUpstream{}
	srv := f.server()
	defer srv.Close()

	p := pool.New("")
	a := &auth.Auth{UID: "g1", AccessToken: "old", RefreshToken: "rt", ExpiresAt: 1,
		Domain: "www.workbuddy.ai"}
	p.Add(a)

	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})
	s.RunKeepaliveNow()
	if f.refreshCalls.Load() != 1 {
		t.Errorf("global keepalive refresh calls=%d want 1（keepalive 不拦 global）", f.refreshCalls.Load())
	}
	if a.AccessToken != "new" {
		t.Errorf("global token 未刷新: %s", a.AccessToken)
	}
}

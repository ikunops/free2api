package server

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// TestApplyErrorPolicyErrClientFeedsConsecutiveFails ErrClient（default 分支，
// 未知 4xx）喂连败计数（issue #114）：单次不罚（无冷却/熔断），达阈降权出池。
// 修复前该形态只换号不罚，坏号留在池内被反复选中——现在有连败兜底。
func TestApplyErrorPolicyErrClientFeedsConsecutiveFails(t *testing.T) {
	p := pool.New("")
	p.SetDegrade(3, 10*time.Minute, 2*time.Hour) // 阈 3 便于测试
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})

	// 前阈-1 次：只计数，不冷却不熔断。
	for n := 0; n < 2; n++ {
		ue := &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: `{"code":1,"msg":"unknown business error"}`}
		h.applyErrorPolicy("u1", upstream.ErrClient, `{"code":1,"msg":"unknown business error"}`, "", ue)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.BreakerFails != 0 {
		t.Fatalf("ErrClient 前阈-1 次不应有任何惩罚: %+v", st)
	}
	if st.ConsecutiveFails != 2 {
		t.Fatalf("ErrClient 应喂连败计数, consecutive_fails=%d want 2", st.ConsecutiveFails)
	}
	// 达阈第 3 次：降权（Cooling 呈 degrade 形态），仍不熔断不禁用。
	ue := &upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: `x`}
	h.applyErrorPolicy("u1", upstream.ErrClient, `x`, "", ue)
	st, _ = p.Status("u1")
	if !st.Cooling || st.Reason != "consecutive failures" || st.CoolKind != "degrade" {
		t.Fatalf("达阈应触发降权: %+v", st)
	}
	if st.Disabled || st.BreakerFails != 0 {
		t.Fatalf("连败降权不叠加禁用/熔断: %+v", st)
	}
}

// TestApplyErrorPolicyErrNoneNotFed ErrNone 走 default 分支（防御路径）不喂连败。
func TestApplyErrorPolicyErrNoneNotFed(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrNone, "", "", nil)
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("ErrNone 不应喂连败, got %d", st.ConsecutiveFails)
	}
}

// TestApplyErrorPolicyClassifiedErrorsNotFed 带权威分类的错误不喂连败（不重复计罚）：
// ErrServer（熔断）与 ErrSoftRate（冷却）各走各自惩罚，consecutive_fails 恒 0。
func TestApplyErrorPolicyClassifiedErrorsNotFed(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: time.Minute})

	h.applyErrorPolicy("u1", upstream.ErrServer, "boom", "", &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"})
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, "rate limit", "", &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, Msg: "rate limit"})
	st, _ := p.Status("u1")
	if st.ConsecutiveFails != 0 {
		t.Fatalf("权威分类错误不应喂连败（惩罚已存在，不重复计罚）, got %d", st.ConsecutiveFails)
	}
	if st.BreakerFails != 1 {
		t.Fatalf("ErrServer 应喂熔断, fails=%d", st.BreakerFails)
	}
}

// TestChatTransportErrorFeedsConsecutiveFailures 传输层失败（连不上上游）喂连败
// （issue #114 端到端）：N 连败后该号出池（AvailableUIDs 不再含它），仍不熔断。
// fake transport 返回 error（非 *upstream.Error）→ handler 网络抖动分支。
func TestChatTransportErrorFeedsConsecutiveFailures(t *testing.T) {
	var calls int
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return nil, errors.New("dial tcp: connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := pool.New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour) // 阈 2：两轮请求即降权
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetCredits("u1", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	for round := 0; round < 2; round++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
		if rec.Code != 503 {
			t.Fatalf("round %d: transport error 应回 503, got %d", round, rec.Code)
		}
	}
	// 两轮（每轮 1 次传输失败）后：连败=2 已达阈 → 降权出池。
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("连败达阈后账号应出池, still available: %v", uids)
	}
	st, _ := p.Status("u1")
	if st.CoolKind != "degrade" || st.Reason != "consecutive failures" {
		t.Fatalf("降权态应呈 degrade: %+v", st)
	}
	if st.BreakerFails != 0 {
		t.Fatalf("传输层错误不喂熔断（既有语义不回归）, fails=%d", st.BreakerFails)
	}
}

// TestChatErrClientDegradedNotPicked ErrClient 连败出池端到端：降权号不被
// normal 选号选中（换其他号继续服务），请求仍成功。
func TestChatErrClientDegradedNotPicked(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			// 未知业务 4xx：ErrClient（default 只换号不罚的形态）。
			return 400, `{"code":60001,"msg":"unknown business error"}`, false
		}
		return 200, sseOK, true
	})
	p := pool.New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour) // 阈 2
	p.Add(&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 第一轮：bad 号 ErrClient 一次（换 good 成功）。bad 连败=1。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("round 1: want 200, got %d %s", rec.Code, rec.Body)
	}
	// 直喂一次（bad 号选号顺序不定，直接喂满阈值确定状态）。
	h.applyErrorPolicy("bad", upstream.ErrClient, `x`, "",
		&upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: "x"})
	// bad 号已降权出池；下一轮请求只能选 good（不再浪费一次在 bad 上）。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("round 2: want 200 (good), got %d %s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("bad 号应处降权态: %+v", st)
	}
	if st.SuccessCount != 0 {
		t.Fatalf("bad 号未成功过, got %d", st.SuccessCount)
	}
}

// TestChatConsecutiveFailClearedBySuccess 成功清零端到端：flaky 号 ErrClient 失败
// 一次后**它自己**成功一次 → 连败计数归零，不出池。成功清零是「任何成功」语义
// （NoteSuccess 对该账号生效即清，无论成功来自哪一轮）。
func TestChatConsecutiveFailClearedBySuccess(t *testing.T) {
	var flakyCalls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-flaky" {
			flakyCalls++
			if flakyCalls <= 2 {
				return 400, `{"code":60001,"msg":"unknown business error"}`, false // 前两次失败
			}
			return 200, sseOK, true // 第三次起恢复
		}
		return 200, sseOK, true
	})
	p := pool.New("")
	p.SetDegrade(3, time.Hour, 2*time.Hour) // 阈 3：两次失败不出池，第三次成功清零
	// 确定性选号：rand 恒取 0 → 加权抽签必选候选头部，flaky credits 更高排前，
	// 每轮请求先选 flaky（失败后 rotate 才轮到 other）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "flaky", AccessToken: "at-flaky", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "other", AccessToken: "at-other", ExpiresAt: 9999999999})
	p.SetCredits("flaky", 2000)
	p.SetCredits("other", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 一轮真实请求：flaky 失败一次（选号顺序不定，无论先选谁，flaky 失败后换
	// other 成功，请求 200）。随后直喂第二次 ErrClient，flaky 连败=2（<阈 3 不出池）。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("round 1: want 200, got %d %s", rec.Code, rec.Body)
	}
	h.applyErrorPolicy("flaky", upstream.ErrClient, `x`, "",
		&upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: "x"})
	st, _ := p.Status("flaky")
	if st.ConsecutiveFails != 2 {
		t.Fatalf("两次 ErrClient 后计数应=2, got %d", st.ConsecutiveFails)
	}
	// 直喂 flaky 一次 ErrClient 达阈 3 → 降权出池。
	h.applyErrorPolicy("flaky", upstream.ErrClient, `x`, "",
		&upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: "x"})
	st, _ = p.Status("flaky")
	if !st.Cooling || st.CoolKind != "degrade" {
		t.Fatalf("达阈应降权: %+v", st)
	}
	// flaky 已降权出池（normal 不选它）。降权号被成功恢复的通道有二：到期自动回池、
	// 全冷却兜底选中后成功。单号池只剩 flaky → 兜底选中它，上游已恢复 200 →
	// NoteSuccess 清降权 + 计数，账号回池。
	p2 := pool.New("")
	p2.SetDegrade(2, time.Hour, 2*time.Hour)
	p2.Add(&auth.Auth{UID: "solo", AccessToken: "at-solo", ExpiresAt: 9999999999})
	p2.SetCredits("solo", 1000)
	h2 := NewHandler(Config{Pool: p2, Upstream: up})
	// 直喂两次 ErrClient 达阈降权（fake upstream 对 at-solo 恒 200）。
	for n := 0; n < 2; n++ {
		h2.applyErrorPolicy("solo", upstream.ErrClient, `x`, "",
			&upstream.Error{Kind: upstream.ErrClient, Status: 400, Msg: "x"})
	}
	st2, _ := p2.Status("solo")
	if !st2.Cooling || st2.CoolKind != "degrade" {
		t.Fatalf("solo 应已降权: %+v", st2)
	}
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("fallback recovery: want 200, got %d %s", rec2.Code, rec2.Body)
	}
	st2, _ = p2.Status("solo")
	if st2.ConsecutiveFails != 0 {
		t.Fatalf("成功后连败计数应清零, got %d", st2.ConsecutiveFails)
	}
	if st2.Cooling {
		t.Fatalf("成功后降权应撤销: %+v", st2)
	}
	if uids := p2.AvailableUIDs(); len(uids) != 1 {
		t.Fatalf("成功后账号应回池, available=%v", uids)
	}
}

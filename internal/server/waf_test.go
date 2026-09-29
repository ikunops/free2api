package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// newFakeUpstreamHeaders 同 newFakeUpstream，但可按 Authorization 决定响应头
// （Retry-After 头族测试需要）。
func newFakeUpstreamHeaders(t *testing.T, behavior func(authz string) (status int, body string, isStream bool, headers http.Header)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream, headers := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			if headers == nil {
				headers = http.Header{}
			}
			headers.Set("Content-Type", ct)
			return &http.Response{
				StatusCode: status,
				Header:     headers,
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestChatWaf403SoftCoolsWithoutDisable 端到端验收（任务书 P0-1）：
// WAF 403（空 body，非业务信封）→ 账号软冷却（CoolSoft，基数 wafCooldownBase
// 抖动后界 [45s,75s]），**不 Disable**、不喂熔断（ErrServer 才喂 NoteError）。
// 修复前该形态落 ErrClient → applyErrorPolicy default 只换号不罚。
func TestChatWaf403SoftCoolsWithoutDisable(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 403, "", false // WAF 拦截：空 body
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("WAF 403 must soft-cool: %+v", st)
	}
	if st.Disabled {
		t.Fatalf("WAF 403 must NOT disable: %+v", st)
	}
	if st.BreakerFails != 0 {
		t.Errorf("WAF 403 must not feed breaker, fails=%d", st.BreakerFails)
	}
	if d := time.Duration(st.CoolRemaining) * time.Second; d < 45*time.Second || d > 75*time.Second {
		t.Errorf("WAF cool_remaining=%v want in [45s,75s] (60s base ± 25%%)", d)
	}
	if !strings.Contains(st.Reason, "waf") {
		t.Errorf("cool reason should name waf, got %q", st.Reason)
	}
}

// TestChatWaf403HtmlBodySoftCools HTML 拦截页形态（APISIX WAF 页面）同 P0-1 分类。
func TestChatWaf403HtmlBodySoftCools(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "<html><head><title>403 Forbidden</title></head></html>", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503 (all accounts waf-blocked)", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Cooling || st.Disabled {
		t.Fatalf("HTML 403 must soft-cool without disable: %+v", st)
	}
}

// TestChat403BusinessEnvelopeNotWaf 403 带业务信封（code/msg）不走 WAF 分支
// （P0-1 约束：不劫持业务 403——11140 request illegal 维持 ErrAccountFault 禁用）。
func TestChat403BusinessEnvelopeNotWaf(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("403+request illegal must disable (ErrAccountFault path), got %+v", st)
	}
	if st.Cooling {
		t.Errorf("11140 disable must not stack cooling: %+v", st)
	}
}

// TestApplyErrorPolicyWafRetryAfter P1-2：WAF 403 带 Retry-After 头 → 冷却精确
// 对齐 now+RetryAfter（优先于固定基数），日志 reason 带 retry-after 标记。
func TestApplyErrorPolicyWafRetryAfter(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})

	reset := time.Now().Add(5 * time.Minute) // > 75s 抖动上限，验证确实取头值
	ue := &upstream.Error{Kind: upstream.ErrWafBlock, Status: 403, RetryAfter: 5 * time.Minute}
	h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "", ue)
	st, _ := p.Status("u1")
	if !st.Cooling || st.Disabled {
		t.Fatalf("WAF+retry-after must soft-cool without disable: %+v", st)
	}
	if d := st.Until.Sub(reset); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("until=%v want ~%v (retry-after priority over base)", st.Until, reset)
	}
}

// TestApplyErrorPolicySoftRateRetryAfter P1-2：429 无 body 重置文案但带
// Retry-After 头 → CooldownSoftRate 对齐 now+RetryAfter（softStreak 不堆加）。
func TestApplyErrorPolicySoftRateRetryAfter(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	ue := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, RetryAfter: 3 * time.Minute}
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", ue)
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("429+retry-after must soft-cool: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("retry-after alignment must not stack soft_streak, got %d", st.SoftStreak)
	}
	want := time.Now().Add(3 * time.Minute)
	if d := st.Until.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("until=%v want ~%v", st.Until, want)
	}
}

// TestApplyErrorPolicySoftRateBodyResetBeatsHeader P1-2 优先级回归：body 带
// 「将在 … 重置」文案时，文案墙钟优先于 Retry-After 头（文案是上游更权威口径）。
func TestApplyErrorPolicySoftRateBodyResetBeatsHeader(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	bodyReset := time.Now().Add(40 * time.Minute)
	ts := bodyReset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	body := `{"code":11140,"msg":"The model provider is rate-limiting requests. 将在 ` + ts + ` UTC+8 重置"}`
	ue := &upstream.Error{Kind: upstream.ErrSoftRate, Status: 429, RetryAfter: 1 * time.Minute} // 头值与文案明显不同
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.3", ue)
	st, _ := p.Status("u1")
	if d := st.Until.Sub(bodyReset); d < -time.Second || d > time.Second {
		t.Errorf("until=%v want ~bodyReset=%v (body reset must beat header)", st.Until, bodyReset)
	}
}

// TestChatWaf403RetryAfterHeaderEndToEnd 端到端：上游 403 空体 + Retry-After 头
// → 账号冷却对齐 now+Retry-After（头从 ChatStreamContext 信封流到 applyErrorPolicy）。
func TestChatWaf403RetryAfterHeaderEndToEnd(t *testing.T) {
	up := newFakeUpstreamHeaders(t, func(authz string) (int, string, bool, http.Header) {
		h := http.Header{}
		h.Set("Content-Type", "application/json")
		h.Set("Retry-After", "120")
		return 403, "", false, h
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("403+Retry-After must cool: %+v", st)
	}
	want := time.Now().Add(120 * time.Second)
	if d := st.Until.Sub(want); d < -2*time.Second || d > 2*time.Second {
		t.Errorf("until=%v want ~now+120s (retry-after flows to cooldown)", st.Until)
	}
}

// TestChatWafPassesThroughUpstreamBody 错误透传语义（5755fe3）回归：WAF 403 的
// message 仍透传上游原文；空体（WAF 常见形态）时保留可读分类文案兜底。
func TestChatWafPassesThroughUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "Forbidden: request blocked by WAF", false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Forbidden: request blocked by WAF") {
		t.Errorf("503 message should carry upstream原文: %s", rec.Body)
	}
}

// TestChatWafEscalatesSoftStreak WAF 冷却指数升级（复用 CooldownSoftRate 既有
// softStreak 语义）：两次独立 WAF 403（非冷却中兜底探测）→ 第二次时长翻倍基数。
func TestChatWafEscalatesSoftStreak(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})

	h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "", nil)
	st1, _ := p.Status("u1")
	if st1.SoftStreak != 1 {
		t.Fatalf("first WAF should set soft_streak=1, got %d", st1.SoftStreak)
	}
	if d := time.Duration(st1.CoolRemaining) * time.Second; d < 45*time.Second || d > 75*time.Second {
		t.Errorf("first WAF cool=%v want in [45s,75s]", d)
	}

	// 等第一次冷却到期后再触发 → streak=2，时长翻倍（120s 抖动界 [90s,150s]）。
	p.Cooldown("u1", pool.CoolSoft, 0, "force expiry")
	h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "", nil)
	st2, _ := p.Status("u1")
	if st2.SoftStreak != 2 {
		t.Fatalf("second WAF should set soft_streak=2, got %d", st2.SoftStreak)
	}
	if d := time.Duration(st2.CoolRemaining) * time.Second; d < 90*time.Second || d > 150*time.Second {
		t.Errorf("second WAF cool=%v want in [90s,150s] (doubled base)", d)
	}
}

// TestChatWafProbeNoEscalation 冷却中兜底探测不翻倍（CooldownSoftRate 既有
// 语义回归）：首次 WAF 冷却后，冷却期内再次 403 不推进 streak/不延长。
func TestChatWafProbeNoEscalation(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})

	h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "", nil)
	before, _ := p.Status("u1")
	for n := 0; n < 3; n++ {
		h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "", nil)
	}
	after, _ := p.Status("u1")
	if after.SoftStreak != before.SoftStreak {
		t.Fatalf("cooling probe must not advance streak: %d -> %d", before.SoftStreak, after.SoftStreak)
	}
	if after.CoolRemaining > before.CoolRemaining {
		t.Errorf("cooling probe must not extend: %d -> %d", before.CoolRemaining, after.CoolRemaining)
	}
}

// TestRotateBackoffRealDelay 轮转退避真实生效（任务书 P0-2 验收点）：
// 首个号 WAF 403 后，换 good 号前有可观测的退避间隔（测试态 base=0 被恢复为
// 短基数 30ms，抖动界 [22.5ms, 37.5ms]，断言总耗时 ≥ 20ms 且不超界太多）。
func TestRotateBackoffRealDelay(t *testing.T) {
	withRotateBackoff(t, 30*time.Millisecond)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 403, "", false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	elapsed := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate)", rec.Code, rec.Body)
	}
	// 一次轮转退避 ≥ 22.5ms（30ms 基数下界）；上限放宽到 1s 防环境抖动误报。
	if elapsed < 22*time.Millisecond {
		t.Fatalf("rotate must back off before retry, elapsed=%v want ≥ 22ms", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("rotate backoff runaway, elapsed=%v", elapsed)
	}
}

// TestRotateBackoffContextCancel 退避可取消：ctx 取消（客户端断连）时立即中止
// 退避退出请求（不再换号打上游）。
func TestRotateBackoffContextCancel(t *testing.T) {
	withRotateBackoff(t, 30*time.Second) // 足够长：取消路径必须立即返回
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 403, "", false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req = req.WithContext(ctx)
	go func() {
		time.Sleep(50 * time.Millisecond) // 首号 403 后、退避睡满前取消
		cancel()
	}()
	start := time.Now()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	elapsed := time.Since(start)
	// 取消后必须快速退出（远小于 30s 退避），且不再打上游（good 号零调用）。
	if elapsed > 2*time.Second {
		t.Fatalf("cancel must abort backoff immediately, elapsed=%v", elapsed)
	}
	if calls != 1 {
		t.Errorf("calls=%d want 1 (no rotate after cancel)", calls)
	}
}

// TestWafCooldownMsgFmt waf reason 文案（reason 稳定供 /status 台账识别）。
func TestWafCooldownMsgFmt(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})
	h.applyErrorPolicy("u1", upstream.ErrWafBlock, "", "", nil)
	st, _ := p.Status("u1")
	if st.Reason != "waf 403 block" {
		t.Errorf("reason=%q want %q", st.Reason, "waf 403 block")
	}
}

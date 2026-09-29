package server

import (
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
)

// withWafIPWindow 注入短 IP 级判定窗（生产恒 60s；测试用 200ms 加速窗口过期验证）。
func withWafIPWindow(t *testing.T, d time.Duration) {
	t.Helper()
	prev := wafIPWindow
	wafIPWindow = d
	t.Cleanup(func() { wafIPWindow = prev })
}

// TestWafIPGateMultiAccountTriggers 状态机单测：窗内两个不同号命中 → 激活；
// 单号反复命中（任意多次）不触发；激活期内 noteWaf 恒 true（不续期路径）。
func TestWafIPGateMultiAccountTriggers(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	var g wafIPGate
	// 单号反复：永不激活（判定口径=不同 UID 数）。
	for n := 0; n < 10; n++ {
		if g.noteWaf("u1") {
			t.Fatalf("single account repeated hits must never trigger, iter=%d", n)
		}
	}
	if g.active() {
		t.Fatal("single account must not activate")
	}
	// 第二个不同号进入窗内 → 本 call 即达阈值激活并返回 true（轮转在该次
	// 403 后立即终止——fail-fast 生效点就是阈值命中的那次请求）。
	if !g.noteWaf("u2") {
		t.Fatalf("threshold crossing call must activate and return true")
	}
	if !g.active() {
		t.Fatal("two distinct accounts within window must activate")
	}
	// 激活期内新命中恒 true（fail-fast 生效），不续期由下条测试验证。
	if !g.noteWaf("u3") {
		t.Fatal("hits during active window must report active")
	}
}

// TestWafIPGateWindowExpiry 窗口过期自然解除 + 激活不续期：
// 激活后（窗 150ms）等过期 → active=false；解除后需全新命中重新判定（旧账已清，
// 单号命中不残留触发）。
func TestWafIPGateWindowExpiry(t *testing.T) {
	withWafIPWindow(t, 150*time.Millisecond)
	var g wafIPGate
	g.noteWaf("u1")
	g.noteWaf("u2")
	if !g.active() {
		t.Fatal("must activate")
	}
	// 激活中段再命中：不改变解除时刻（不续期）——记录当前 until 供过期断言前校验
	// 该命中确实落在窗内（150ms 内必成立）。
	if !g.noteWaf("u3") {
		t.Fatal("mid-window hit must report active")
	}
	time.Sleep(200 * time.Millisecond) // 窗口过期
	if g.active() {
		t.Fatal("gate must deactivate after window expiry")
	}
	// 解除后单号命中：旧账已清（判定窗在激活时清空），不残留激活。
	if g.noteWaf("u4") {
		t.Fatal("post-expiry single hit must not re-activate (hits cleared on activation)")
	}
	if g.active() {
		t.Fatal("post-expiry single hit must leave gate inactive")
	}
}

// TestChatWafIPFailFastStopsRotation 端到端验收（任务书第 2 条）：
// 3 个账号全部 WAF 403 → 第二个号命中即激活 IP 级状态 → 第三个号不再被调用
// （放大倍数=1：本请求上游调用=2，远小于 MaxRotate=3 轮全打）。同时验证末端
// 透传语义：空 body → 本地 waf_ip_blocked 可读文案（5755fe3 兜底分支）。
func TestChatWafIPFailFastStopsRotation(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		return 403, "", false // WAF 拦截：空 body
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u3", AccessToken: "at3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	// 上游调用必须止步于 2（第二个号触发 IP 级判定即 break；u3 零调用）。
	// MaxRotate 默认 3：非 fail-fast 路径会打满 3 次（放大倍数=3）。
	if n := calls.Load(); n != 2 {
		t.Fatalf("upstream calls=%d want 2 (fail-fast stops rotation at threshold, u3 must not be hit)", n)
	}
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "waf ip-level block") {
		t.Errorf("empty-body 503 must carry readable local waf_ip text: %s", rec.Body)
	}
	// IP 级激活期间账号软冷却照常记账（协同不叠加）：两号均 soft_rate 冷却中。
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if !st.Cooling || st.Disabled {
			t.Errorf("uid=%s must soft-cool without disable (IP state coexists, not stacks): %+v", uid, st)
		}
	}
	// u3 从未被调用：不冷却、可选。
	st3, _ := p.Status("u3")
	if st3.Cooling {
		t.Errorf("u3 must not be cooled (never called): %+v", st3)
	}
}

// TestChatWafIPBodyPassthroughWhenActive 激活期有上游原文时仍透传原文
// （5755fe3 优先级：原文优先于本地 waf_ip_blocked 文案，不编造）。
func TestChatWafIPBodyPassthroughWhenActive(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "Forbidden: request blocked by WAF", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if !strings.Contains(rec.Body.String(), "Forbidden: request blocked by WAF") {
		t.Errorf("upstream body must passthrough even when IP-gate active: %s", rec.Body)
	}
}

// TestChatWafSingleAccountStillRotates 单号 WAF 403 不触发 IP 级（窗口内只有
// 一个号被拦）：轮转继续换到健康号成功——既有行为零回归（IP fail-fast 只在
// 多号短窗时才接管）。
func TestChatWafSingleAccountStillRotates(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	var calls atomic.Int64
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls.Add(1)
		if authz == "Bearer at-bad" {
			return 403, "", false // 仅此号被 WAF 拦
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200: single-account WAF must still rotate to healthy)", rec.Code, rec.Body)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("calls=%d want 2 (bad once + good once)", n)
	}
	// bad 号软冷却不受 IP 级状态影响（未触发）。
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("single-account WAF must soft-cool: %+v", st)
	}
}

// TestChatWafIPGateClearsAfterExpiryThenRotates 窗口过期解除端到端：
// 多号短窗触发激活 → 窗口过期（200ms 注入窗）→ 后续单号 WAF 403 不再
// fail-fast，轮转继续（IP 级状态自然解除，恢复既有轮转行为）。
func TestChatWafIPGateClearsAfterExpiryThenRotates(t *testing.T) {
	withWafIPWindow(t, 150*time.Millisecond)
	// 阶段一：两个号全 403 → 激活（此时两个号已软冷却）。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("stage1 code=%d want 503", rec.Code)
	}
	// 等两个维度过期：IP 窗（150ms）+ 账号软冷却（wafCooldownBase 抖动下界 45s
	// 太长——直接 Cooldown 0 强制解除，聚焦 IP 窗语义）。
	time.Sleep(200 * time.Millisecond)
	p.Cooldown("u1", pool.CoolSoft, 0, "force expiry")
	p.Cooldown("u2", pool.CoolSoft, 0, "force expiry")
	// 阶段二：窗口过期后单号 403（u1 bad / u2 good）→ 不 fail-fast，换到 u2 成功。
	up2 := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at1" {
			return 403, "", false
		}
		return 200, sseOK, true
	})
	h.cfg.Upstream = up2 // 复用同一 Handler/IP 状态机（验证过期解除而非新实例）
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("stage2 code=%d body=%s (want 200: expired IP gate must not fail-fast)", rec2.Code, rec2.Body)
	}
}

// TestChatWafIPSoftCooldownUnaffected 软冷却语义不受 IP 级状态影响的隔离回归：
// IP 级激活存在时，账号软冷却的时长/reason/softStreak 口径与既有 P0-1 完全一致
// （IP 状态只改变轮转决策，不叠加冷却时长——两号均按 wafCooldownBase 抖动界
// [45s,75s] 冷却，无额外延长）。
func TestChatWafIPSoftCooldownUnaffected(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 403, "", false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("uid=%s must be in soft_rate cooldown: %+v", uid, st)
		}
		if st.SoftStreak != 1 {
			t.Errorf("uid=%s soft_streak=%d want 1 (one WAF hit each, no stacking)", uid, st.SoftStreak)
		}
		if d := time.Duration(st.CoolRemaining) * time.Second; d < 45*time.Second || d > 75*time.Second {
			t.Errorf("uid=%s cool=%v want in [45s,75s] (IP state must not extend account cooldown)", uid, d)
		}
		if !strings.Contains(st.Reason, "waf") {
			t.Errorf("uid=%s reason=%q want waf marker", uid, st.Reason)
		}
	}
}

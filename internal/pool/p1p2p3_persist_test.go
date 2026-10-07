package pool

import (
	"os"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

// breakerRetryCount 曝露 entry.retryCount 供测试断言（包内私有 helper）。
func (p *Pool) breakerRetryCount(uid string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.retryCount, true
}

// creditsExpiringOf 曝露 entry.creditsExpiring 供测试断言（包内私有 helper）。
func (p *Pool) creditsExpiringOf(uid string) (int64, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.creditsExpiring, true
}

// ---------------------------------------------------------------------------
// P1：熔断器 breakerUntil/retryCount 持久化
// ---------------------------------------------------------------------------

// TestBreakerPersistRoundTrip 熔断器 breakerUntil + retryCount 现已持久化：
// 落盘 → 重启 → 恢复。修复重启熔断失忆：breakerUntil 在未来时，重启后熔断期保留。
func TestBreakerPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 触发两次熔断：retryCount=2，breakerUntil 在 2h 后（1h*2^1=2h，未触顶）。
	p.SetBreaker(1, time.Hour, 6*time.Hour)
	p.NoteError("u1") // 第 1 次熔断：retryCount=1，breakerUntil=+1h
	// 第 1 次熔断后 fails 清零，再 NoteError 触发第 2 次。
	p.NoteError("u1") // retryCount=2，breakerUntil=+2h
	p.Flush()

	btBefore, _ := p.breakerUntil("u1")
	rcBefore, _ := p.breakerRetryCount("u1")
	if btBefore.IsZero() || rcBefore != 2 {
		t.Fatalf("precondition: breakerUntil=%v retryCount=%d, want 非零/2", btBefore, rcBefore)
	}

	// 重启：恢复后 breakerUntil + retryCount 应保留。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	btAfter, _ := p2.breakerUntil("u1")
	rcAfter, _ := p2.breakerRetryCount("u1")
	if btAfter.IsZero() {
		t.Fatal("重启后 breakerUntil 丢失（熔断期未持久化）")
	}
	if d := btAfter.Sub(btBefore); d < -time.Second || d > time.Second {
		t.Errorf("恢复后 breakerUntil=%v want ~%v (diff %v)", btAfter, btBefore, d)
	}
	if rcAfter != rcBefore {
		t.Errorf("恢复后 retryCount=%d want %d（退避指数未持久化）", rcAfter, rcBefore)
	}

	// 重启后 healthy 判定仍阻断（breakerUntil 未过期）。
	if p2.internalHealthy("u1") {
		t.Fatal("重启后熔断中的账号应仍不可选（breakerUntil 未过期）")
	}
}

// TestBreakerPersistExpiryFilter 恢复时做过期过滤：breakerUntil 在未来才恢复，
// 过期/零值丢弃（惰性清理，防止重启后残留已过期的熔断期）。
func TestBreakerPersistExpiryFilter(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 手写一个已过期的 breakerUntil + retryCount=3，落盘应惰性过滤（不写 breaker_until）。
	p.mu.Lock()
	e := p.byUID["u1"]
	e.breakerUntil = time.Now().Add(-time.Hour)
	e.retryCount = 3
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "breaker_until") {
		t.Errorf("已过期的 breakerUntil 不应落盘:\n%s", raw)
	}
	// retryCount 仅在 breakerUntil 未过期时才有意义；过期时落盘也不写（omitempty + 配对）。
	if strings.Contains(string(raw), "retry_count") {
		t.Errorf("breakerUntil 过期时 retryCount 不应落盘:\n%s", raw)
	}

	// 重启：breakerUntil 过期 → 恢复为零值，retryCount 归零（不保留无用退避指数）。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	bt, _ := p2.breakerUntil("u1")
	rc, _ := p2.breakerRetryCount("u1")
	if !bt.IsZero() {
		t.Errorf("过期的 breakerUntil 恢复后=%v want 零值", bt)
	}
	if rc != 0 {
		t.Errorf("breakerUntil 过期时 retryCount 应归零, got %d", rc)
	}
}

// TestBreakerPersistFailsNotPersisted fails 是短期计数，不持久化：重启后归零，
// 需重新连续失败达 breakerThreshold 才熔断（不激进）。
func TestBreakerPersistFailsNotPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	// 累计 2 次失败（未达阈值 3，不熔断），fails=2。
	p.NoteError("u1")
	p.NoteError("u1")
	p.Flush()

	// 落盘 JSON 不应含 breaker_fails（运行态，不持久化）。
	// 注意用精确匹配：session_dead_fails 等字段已显式写出，"fails" 裸子串会误伤。
	raw, _ := os.ReadFile(fp)
	if strings.Contains(string(raw), `"breaker_fails"`) || strings.Contains(string(raw), `"fails"`) {
		t.Errorf("fails 不应落盘:\n%s", raw)
	}

	// 重启：fails 归零（需重新连续失败才熔断）。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	failsAfter := p2.breakerFails("u1")
	if failsAfter != 0 {
		t.Errorf("重启后 fails=%d want 0（短期计数不持久化）", failsAfter)
	}
}

// ---------------------------------------------------------------------------
// P2：creditsExpiring 持久化
// ---------------------------------------------------------------------------

// TestCreditsExpiringPersistRoundTrip creditsExpiring 现已持久化：落盘 → 重启 → 恢复。
// 修复快过期权重项（weightOf 的 expiringWeight）重启失忆：重启后到下次签到之间不应丢失快过期积分偏好。
func TestCreditsExpiringPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 500, 0) // credits=1000, creditsExpiring=500
	p.Flush()

	// 重启：creditsExpiring 应恢复。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	expiring, ok := p2.creditsExpiringOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if expiring != 500 {
		t.Errorf("恢复后 creditsExpiring=%d want 500", expiring)
	}
	// 选号第四因子用恢复的值：weightOf 应含 expiring 项。
	p2.mu.RLock()
	e := p2.byUID["u1"]
	wWith := p2.weightOf(e, 1000, time.Now())
	// 对比：把 creditsExpiring 清零后权重应更小（快过期加成消失）。
	saved := e.creditsExpiring
	e.creditsExpiring = 0
	wWithout := p2.weightOf(e, 1000, time.Now())
	e.creditsExpiring = saved
	p2.mu.RUnlock()
	if wWith <= wWithout {
		t.Errorf("恢复的 creditsExpiring 应让权重更大: wWith=%.3f wWithout=%.3f", wWith, wWithout)
	}
}

// TestCreditsExpiringPersistWritesZero creditsExpiring=0 时也显式落盘
// （运维口径：零值缺失会误解为"没记录"，实际是零值省略——stateAccount 去 omitempty）。
func TestCreditsExpiringPersistWritesZero(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 1000, 0, 0) // creditsExpiring=0
	p.Flush()

	raw, _ := os.ReadFile(fp)
	if !strings.Contains(string(raw), `"credits_expiring": 0`) {
		t.Errorf("creditsExpiring=0 时也应显式写出（运维可见）:\n%s", raw)
	}
}

// ---------------------------------------------------------------------------
// P3：statusOf reason 过期清理
// ---------------------------------------------------------------------------

// TestStatusOfClearsExpiredReason until 过期后，status 的 reason 应清空
// （与落盘清理 persist.go:281-286 同口径），不残留过期冷却的 reason。
func TestStatusOfClearsExpiredReason(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 设置一个已过期的 until + reason（冷却已到期但 e.reason 尚未落盘清理）。
	p.mu.Lock()
	e := p.byUID["u1"]
	e.until = time.Now().Add(-time.Minute)
	e.coolKind = CoolSoft
	e.reason = "429 rate limit"
	p.mu.Unlock()

	st, _ := p.Status("u1")
	if st.Reason != "" {
		t.Errorf("until 过期后 status reason=%q want \"\"（应惰性清空）", st.Reason)
	}
	if st.Cooling {
		t.Error("until 过期后 Cooling 应 false")
	}
	if st.CoolKind != "" {
		t.Errorf("until 过期后 CoolKind=%q want \"\"（应清空）", st.CoolKind)
	}
}

// TestStatusOfKeepsReasonWhenCooling until 在未来时，status 的 reason 正常透出。
func TestStatusOfKeepsReasonWhenCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")

	st, _ := p.Status("u1")
	if st.Reason != "429 rate limit" {
		t.Errorf("冷却中 status reason=%q want %q", st.Reason, "429 rate limit")
	}
	if !st.Cooling {
		t.Error("冷却中 Cooling 应 true")
	}
	if st.CoolKind != "soft_rate" {
		t.Errorf("冷却中 CoolKind=%q want soft_rate", st.CoolKind)
	}
}

// TestStatusOfKeepsDisabledReason disabled 账号的 reason 是禁用原因，保留不清空。
func TestStatusOfKeepsDisabledReason(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")

	st, _ := p.Status("u1")
	if st.Reason != "12153 session dead" && st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled 账号 reason 应保留（禁用原因），got Reason=%q DisabledReason=%q", st.Reason, st.DisabledReason)
	}
	if !st.Disabled {
		t.Error("disabled 账号 Disabled 应 true")
	}
}

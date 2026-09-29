package pool

import (
	"testing"
	"time"

	"free2api/internal/auth"
)

// ─── ResetRuntime（POST /admin/accounts/reset 的底层）──────────────────────
//
// 复位要清「冷却 + 失败计数」，同时**不能**顺手解禁或抹掉上游事实——这两条是
// 本文件的断言重点（清多了比不清更危险：坏号会被静默放回选号池）。

func resetTestEntry(t *testing.T, p *Pool, uid string) *entry {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.byUID[uid]
	if !ok {
		t.Fatalf("no entry %s", uid)
	}
	return e
}

// TestResetRuntimeClearsAllRuntimeState 冷却域 + 熔断 + 连败/12153 计数一次全清。
func TestResetRuntimeClearsAllRuntimeState(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 4242, 42)

	now := time.Now()
	e := resetTestEntry(t, p, "u1")
	p.mu.Lock()
	e.until = now.Add(time.Hour)
	e.coolKind = CoolSoft
	e.reason = "429 rate limit"
	e.softStreak = 3
	e.modelCooldowns = map[string]modelCooldown{"m1": {Until: now.Add(time.Hour), Reason: "6004"}}
	e.breakerUntil = now.Add(30 * time.Minute)
	e.fails = 2
	e.retryCount = 1
	e.sessionDeadFails = 2
	e.consecutiveFails = 4
	e.degradeUntil = now.Add(10 * time.Minute)
	p.mu.Unlock()

	if got := p.ResetRuntime(nil); got.Matched != 1 || got.Reset != 1 {
		t.Fatalf("ResetRuntime = %+v，want matched=1 reset=1", got)
	}

	e = resetTestEntry(t, p, "u1")
	p.mu.RLock()
	defer p.mu.RUnlock()
	if !e.until.IsZero() || e.coolKind != 0 || e.reason != "" || e.softStreak != 0 ||
		len(e.modelCooldowns) != 0 || !e.breakerUntil.IsZero() || e.fails != 0 ||
		e.retryCount != 0 || e.sessionDeadFails != 0 || e.consecutiveFails != 0 ||
		!e.degradeUntil.IsZero() {
		t.Fatalf("复位后仍有残留运行态：%+v", e)
	}
	// 上游事实不许动
	if e.credits != 4242 || e.creditsExpiring != 42 {
		t.Fatalf("复位不应改余额：credits=%d expiring=%d", e.credits, e.creditsExpiring)
	}
}

// TestResetRuntimeIdempotent 干净的账号再复位一次不该算「改动了」。
func TestResetRuntimeIdempotent(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if got := p.ResetRuntime(nil); got.Reset != 0 || got.Matched != 1 {
		t.Fatalf("干净账号被算作改动：%+v", got)
	}
}

// TestResetRuntimeKeepsDisabledAndManual 复位不是解禁：两种不可用态都保留，
// 且自动禁用账号的 reason（禁用原因）不能被抹掉。
func TestResetRuntimeKeepsDisabledAndManual(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "dead"})
	p.Add(&auth.Auth{UID: "paused"})
	p.Add(&auth.Auth{UID: "live"})
	p.Disable("dead", "12153 session dead")
	p.SetManualDisabled("paused", true, "观察几天")
	p.Cooldown("live", CoolSoft, time.Hour, "429")

	got := p.ResetRuntime(nil)
	if got.Matched != 3 || got.Reset != 1 {
		t.Fatalf("ResetRuntime = %+v，want matched=3 reset=1（只有 live 有运行态）", got)
	}
	if got.StillDisabled != 1 || got.StillManualDisable != 1 {
		t.Fatalf("仍不可选计数 = %+v，want still_disabled=1 still_manual_disabled=1", got)
	}
	for _, uid := range []string{"dead", "paused"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("no status %s", uid)
		}
		if uid == "dead" && (!st.Disabled || st.DisabledReason != "12153 session dead") {
			t.Fatalf("自动禁用被复位动过：%+v", st)
		}
		if uid == "paused" && (!st.ManualDisabled || st.ManualReason != "观察几天") {
			t.Fatalf("手动停用被复位动过：%+v", st)
		}
	}
	if st, _ := p.Status("live"); st.Cooling {
		t.Fatal("live 的冷却应该被清掉")
	}
}

// TestResetRuntimeEmptySliceIsNotAll 空切片 ≠ 全池（按 producer 筛不到号时必须复位 0 个）。
func TestResetRuntimeEmptySliceIsNotAll(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429")

	if got := p.ResetRuntime([]string{}); got.Matched != 0 || got.Reset != 0 {
		t.Fatalf("空切片应复位 0 个，得到 %+v", got)
	}
	if st, _ := p.Status("u1"); !st.Cooling {
		t.Fatal("空切片不该碰任何账号")
	}
}

// TestResetRuntimeByUIDList 点名复位：只动点到的，未知 uid 报 not_found。
func TestResetRuntimeByUIDList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Cooldown("u1", CoolSoft, time.Hour, "429")
	p.Cooldown("u2", CoolSoft, time.Hour, "429")

	got := p.ResetRuntime([]string{"u1", "ghost"})
	if got.Matched != 1 || got.Reset != 1 {
		t.Fatalf("ResetRuntime = %+v，want matched=1 reset=1", got)
	}
	if len(got.NotFound) != 1 || got.NotFound[0] != "ghost" {
		t.Fatalf("not_found = %v，want [ghost]", got.NotFound)
	}
	if st, _ := p.Status("u2"); !st.Cooling {
		t.Fatal("没点名的 u2 不该被动")
	}
	if st, _ := p.Status("u1"); st.Cooling {
		t.Fatal("点名的 u1 应已清掉冷却")
	}
}

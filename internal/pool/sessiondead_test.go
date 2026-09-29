package pool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// sessionDeadFailsOf 曝露 entry.sessionDeadFails 供测试断言（包内私有 helper）。
func (p *Pool) sessionDeadFailsOf(uid string) (int, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return 0, false
	}
	return e.sessionDeadFails, true
}

// ---------------------------------------------------------------------------
// sessionDeadFails 持久化（P2-12 / 审查发现 11）
// ---------------------------------------------------------------------------

// TestSessionDeadFailsPersistRoundTrip 连续 12153 计数已持久化：落盘 → 重启 → 恢复。
// 修复重启归零重学：上游持续 session dead 时不用再吃 2 次失败才禁用。
func TestSessionDeadFailsPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // sessionDeadFails=2（未达阈值 3）
	p.Flush()

	// 重启：连续计数应恢复。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	fails, ok := p2.sessionDeadFailsOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if fails != 2 {
		t.Fatalf("恢复后 sessionDeadFails=%d want 2（连续计数未持久化）", fails)
	}
	// 重启后下次 12153 从恢复的计数继续累计：第 3 次即达阈值禁用。
	if !p2.NoteSessionDead("u1") {
		t.Fatal("恢复计数=2 后第 3 次 12153 应禁用（从恢复值继续累计）")
	}
	if st, _ := p2.Status("u1"); !st.Disabled {
		t.Fatal("恢复后达阈应 disabled")
	}
}

// TestSessionDeadFailsPersistWritesZero sessionDeadFails=0 时也显式落盘
// （运维口径：零值缺失会误解为"没记录"，实际是零值省略——stateAccount 去 omitempty）。
func TestSessionDeadFailsPersistWritesZero(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// 制造一次 dirty（成功入账）让 Flush 真正写盘，sessionDeadFails 保持 0。
	p.NoteSuccess("u1")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"session_dead_fails": 0`) {
		t.Errorf("sessionDeadFails=0 时也应显式写出（运维可见）:\n%s", raw)
	}
}

// TestSessionDeadFailsClearPersists 计数清零（refresh/chat 成功）同样落盘：
// 重启后不残留旧计数。
func TestSessionDeadFailsClearPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // 模拟 refresh 成功：清计数
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if fails, _ := p2.sessionDeadFailsOf("u1"); fails != 0 {
		t.Errorf("清零后重启 sessionDeadFails=%d want 0", fails)
	}
	// 清零持久化后：重启需重新计满 3 次。
	if p2.NoteSessionDead("u1") || p2.NoteSessionDead("u1") {
		t.Fatal("清零后前 2 次不应禁用")
	}
	if !p2.NoteSessionDead("u1") {
		t.Fatal("清零后第 3 次应禁用")
	}
}

// TestNoteSessionDeadThresholdNotReached 前 2 次连续 12153 不 Disable（误判防护）。
func TestNoteSessionDeadThresholdNotReached(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	if p.NoteSessionDead("u1") {
		t.Fatal("第 1 次 12153 不应禁用")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("第 2 次 12153 不应禁用")
	}
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("连续 2 次 12153 不应禁用: %+v", st)
	}
	// 未达阈值时账号仍可选（keepalive 失败不污染选号）。
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("账号应保持可选, got %+v", got)
	}
}

// TestNoteSessionDeadDisablesAtThird 连续第 3 次 12153 → 禁用并清计数。
func TestNoteSessionDeadDisablesAtThird(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	if !p.NoteSessionDead("u1") {
		t.Fatal("第 3 次 12153 应禁用")
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Fatalf("第 3 次后应 disabled: %+v", st)
	}
	if st.Reason != "12153 session dead" {
		t.Errorf("reason=%q want 12153 session dead", st.Reason)
	}
	// 禁用后不再可选。
	if p.Pick("") != nil {
		t.Fatal("禁用账号不可被选中")
	}
}

// TestClearSessionDeadResetsCount 中间成功（refresh 成功）清计数，后续从 1 重新计。
func TestClearSessionDeadResetsCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.ClearSessionDead("u1") // 模拟 refresh 成功：清计数
	if p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 1 次不应禁用")
	}
	if p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 2 次不应禁用")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("清计数后第 3 次应禁用（从 1 重新计够 3 次）")
	}
}

// TestNoteSuccessClearsSessionDeadCount 任意成功（chat 成功）也是 session 未死的强证据，
// 同样清计数——与 refresh 成功口径一致。
func TestNoteSuccessClearsSessionDeadCount(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSuccess("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("成功清计数后第 1 次不应禁用")
	}
}

// TestReviveDisabled 复活入口：清 disabled + reason + 误判计数，账号回到池子。
func TestReviveDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1")
	p.NoteSessionDead("u1") // 触发禁用
	if st, _ := p.Status("u1"); !st.Disabled {
		t.Fatal("precondition: 应已禁用")
	}
	p.ReviveDisabled("u1")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.Disabled {
		t.Fatalf("revive 应清 disabled: %+v", st)
	}
	if st.Reason != "" {
		t.Errorf("reason=%q want 空（revive 清 reason）", st.Reason)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatalf("复活后账号应回到池子, got %+v", got)
	}
	// 误判计数一并清零：复活后重新计满 3 次才禁用。
	p.NoteSessionDead("u1")
	if p.NoteSessionDead("u1") {
		t.Fatal("复活后第 2 次不应禁用（应从新计数）")
	}
	if !p.NoteSessionDead("u1") {
		t.Fatal("复活后第 3 次应禁用（从新计数够 3 次）")
	}
}

// TestReviveDisabledPersists 复活清 disabled + reason 落盘持久化（重启后不回退）。
func TestReviveDisabledPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Disabled || st.Reason != "" {
		t.Fatalf("revive 应持久化（disabled=%v reason=%q）ok=%v", st.Disabled, st.Reason, ok)
	}
}

// TestStatusDisabledReasonDisabled when disabled, Status 透出 disabled_reason。
func TestStatusDisabledReasonDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	st, _ := p.Status("u1")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Errorf("disabled_reason=%q want 12153 session dead (disabled=%v)", st.DisabledReason, st.Disabled)
	}
}

// TestStatusDisabledReasonClearedByRevive 复活后 disabled_reason 归空。
func TestStatusDisabledReasonClearedByRevive(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.ReviveDisabled("u1")
	st, _ := p.Status("u1")
	if st.DisabledReason != "" {
		t.Errorf("revive 后 disabled_reason=%q want 空", st.DisabledReason)
	}
	if st.Reason != "" {
		t.Errorf("revive 后 reason=%q want 空", st.Reason)
	}
}

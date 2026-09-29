package pool

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

// consecutiveStateOf 曝露 entry 的连败计数与降权截止（包内私有 helper）。
func (p *Pool) consecutiveStateOf(uid string) (fails int, degradeUntil time.Time, ok bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, exists := p.byUID[uid]
	if !exists {
		return 0, time.Time{}, false
	}
	return e.consecutiveFails, e.degradeUntil, true
}

// TestNoteFailuresTriggersDegradeAtThreshold 达阈触发降权（核心验收点）：
// 前 4 次只计数不动作（账号保持可选），第 5 次触发 degradeUntil，账号出池。
func TestNoteFailuresTriggersDegradeAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for n := 1; n < 5; n++ {
		p.NoteFailures("u1")
		fails, until, _ := p.consecutiveStateOf("u1")
		if fails != n {
			t.Fatalf("第 %d 次后 consecutiveFails=%d want %d", n, fails, n)
		}
		if !until.IsZero() {
			t.Fatalf("第 %d 次不应触发降权（未达阈值 5）", n)
		}
		if got := p.Pick(""); got == nil || got.UID != "u1" {
			t.Fatalf("未达阈时账号应保持可选（第 %d 次）", n)
		}
	}
	p.NoteFailures("u1") // 第 5 次：达阈
	fails, until, _ := p.consecutiveStateOf("u1")
	if fails != 0 {
		t.Fatalf("达阈后计数应清零供下一轮累计，got %d", fails)
	}
	if until.IsZero() || !time.Now().Before(until) {
		t.Fatalf("第 5 次应触发降权，degradeUntil=%v", until)
	}
	// 降权期 normal 选号不选它（healthy 或门；单号池 Pick 走全冷却兜底另计——
	// 与 CoolSoft 同语义，用 AvailableUIDs 断言 normal 口径）。
	if uids := p.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("降权期账号不应出现在可用列表, got %v", uids)
	}
	st, _ := p.Status("u1")
	if st.Cooling != true || st.Reason != degradeReason || st.CoolKind != "degrade" {
		t.Fatalf("降权期 Status 应呈非健康+连败文案: cooling=%v reason=%q kind=%q", st.Cooling, st.Reason, st.CoolKind)
	}
	if st.ConsecutiveFails != 0 || st.DegradeUntil.IsZero() {
		t.Fatalf("Status 应透出降权态: fails=%d until=%v", st.ConsecutiveFails, st.DegradeUntil)
	}
}

// TestNoteFailuresSuccessClearsCountAndDegrade 成功清零（核心验收点）：
// 连败中途成功 → 计数归零；降权期内成功 → 降权撤销（成功即回池，同 NoteSuccess
// 清 breakerUntil 口径）。
func TestNoteFailuresSuccessClearsCountAndDegrade(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 中途成功清计数：4 次失败 + 成功 + 4 次失败 → 仍不降权（清零后重新累计）。
	for n := 0; n < 4; n++ {
		p.NoteFailures("u1")
	}
	p.NoteSuccess("u1")
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("成功应清连败计数, got %d", fails)
	}
	for n := 0; n < 4; n++ {
		p.NoteFailures("u1")
	}
	if _, until, _ := p.consecutiveStateOf("u1"); !until.IsZero() {
		t.Fatal("成功清零后重新累计的 4 次不应触发降权")
	}
	// 降权期内成功 → 撤销降权。
	for n := 0; n < 5; n++ {
		p.NoteFailures("u1") // 达阈降权
	}
	if _, until, _ := p.consecutiveStateOf("u1"); until.IsZero() {
		t.Fatal("precondition: 应已降权")
	}
	p.NoteSuccess("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if !until.IsZero() {
		t.Fatalf("成功应撤销降权（回池）, degradeUntil=%v", until)
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatal("成功后账号应立即可选")
	}
}

// TestDegradeExpiresAutoReturn 降权到期自动回池（无需显式复位）。
func TestDegradeExpiresAutoReturn(t *testing.T) {
	p := New("")
	p.SetDegrade(2, 50*time.Millisecond, 2*time.Hour) // 阈 2、50ms 便于测试
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	if _, until, _ := p.consecutiveStateOf("u1"); until.IsZero() {
		t.Fatal("precondition: 应已降权")
	}
	// 直接改内表太糙——等到期用真实时间（50ms 量级可接受）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := p.Pick(""); got != nil && got.UID == "u1" {
			return // 到期回池
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("降权到期后账号应自动回池（2s 内）")
}

// TestDegradeAndCooldownTakeLonger 与冷却「并存取更长者不叠加」（核心验收点）：
// 降权与冷却同时存在时，生效的是更远的截止；较近者先到期不提前放行。
func TestDegradeAndCooldownTakeLonger(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 冷却 1h（更远），随后触发降权 10m（较近）→ healthy 应被 1h 冷却拦住。
	p.Cooldown("u1", CoolSoft, time.Hour, "429 rate limit")
	for n := 0; n < 5; n++ {
		p.NoteFailures("u1")
	}
	_, degradeUntil, _ := p.consecutiveStateOf("u1")
	if degradeUntil.IsZero() || !time.Now().Before(degradeUntil) {
		t.Fatal("precondition: 应已降权")
	}
	// 两个截止都生效：healthy=false（或门）。冷却 1h 在降权 10m 之后 → 实际放行时间
	// 由冷却决定（取更长者）。此处验证「并存时不叠加」——降权不覆盖/缩短冷却：
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("冷却+降权并存时账号应不可选")
	}
	if st.CoolRemaining < int64((55 * time.Minute).Seconds()) {
		t.Fatalf("生效截止应是更远的 1h 冷却（取更长者），remaining=%ds", st.CoolRemaining)
	}
	// reason 呈现：有生效冷却时以冷却 reason 为准（语义更具体），降权 reason 不抢。
	if st.Reason != "429 rate limit" {
		t.Fatalf("并存时 reason 应以冷却为准, got %q", st.Reason)
	}
}

// TestDegradeNoRetryStacking 降权期内重试达阈不延长（不越堆越厚）：
// 与 CooldownSoftRate 兜底探测同哲学。
func TestDegradeNoRetryStacking(t *testing.T) {
	p := New("")
	p.SetDegrade(2, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // 触发：degradeUntil ≈ now+1h
	_, first, _ := p.consecutiveStateOf("u1")
	// 降权期内再连败 2 次（达阈）：不延长。
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	_, second, _ := p.consecutiveStateOf("u1")
	if !second.Equal(first) {
		t.Fatalf("降权期内达阈不应延长: first=%v second=%v", first, second)
	}
	// 且计数已清零：到期后需重新满 2 次才再降。
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("降权期内达阈后计数应清零, got %d", fails)
	}
}

// TestNoteFailuresSporadicNotDegraded 不误伤偶发失败（核心验收点）：
// 偶发失败被中间的成功打断 → 永远累计不满阈值，账号不出池。
func TestNoteFailuresSporadicNotDegraded(t *testing.T) {
	p := New("")
	p.SetDegrade(3, time.Hour, 2*time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	// 模式：失败失败成功 失败成功 失败……计数最多到 2，永不达阈 3。
	for round := 0; round < 20; round++ {
		p.NoteFailures("u1")
		p.NoteFailures("u1")
		p.NoteSuccess("u1")
		p.NoteFailures("u1")
		p.NoteSuccess("u1")
		p.NoteFailures("u1")
	}
	_, until, _ := p.consecutiveStateOf("u1")
	if !until.IsZero() {
		t.Fatal("偶发失败（被成功打断）不应触发降权")
	}
	if got := p.Pick(""); got == nil || got.UID != "u1" {
		t.Fatal("偶发失败账号应保持可选")
	}
	st, _ := p.Status("u1")
	if st.Cooling {
		t.Fatal("偶发失败账号不应呈冷却态")
	}
}

// TestNoteFailuresClassifiedErrorsNotFed 带权威分类的错误不喂连败（不重复计罚）：
// NoteError（5xx 熔断路径）与 Cooldown（429 冷却路径）不推进 consecutiveFails。
func TestNoteFailuresClassifiedErrorsNotFed(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 5xx 喂熔断（NoteError）：连败计数不动。
	for n := 0; n < 10; n++ {
		p.NoteError("u1")
	}
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("NoteError 不应推进连败计数, got %d", fails)
	}
	p.NoteSuccess("u1") // 清熔断
	// 429 冷却（CooldownSoftRate）：连败计数不动。
	p.CooldownSoftRate("u1", time.Minute, time.Time{}, "429 rate limit")
	if fails, _, _ := p.consecutiveStateOf("u1"); fails != 0 {
		t.Fatalf("CooldownSoftRate 不应推进连败计数, got %d", fails)
	}
}

// TestDegradePersistRoundTrip 连败降权持久化：计数 + 降权截止落盘 → 重启 → 恢复。
func TestDegradePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1") // consecutiveFails=2（未达阈 5，半开进度）
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	fails, until, ok := p2.consecutiveStateOf("u1")
	if !ok {
		t.Fatal("重启后账号缺失")
	}
	if fails != 2 {
		t.Fatalf("重启后 consecutiveFails=%d want 2（半开进度持久化）", fails)
	}
	if !until.IsZero() {
		t.Fatalf("未触发降权时重启不应有 degradeUntil, got %v", until)
	}
	// 恢复的计数继续累计：重启后再 3 次即达阈降权。
	p2.NoteFailures("u1")
	p2.NoteFailures("u1")
	p2.NoteFailures("u1")
	_, until, _ = p2.consecutiveStateOf("u1")
	if until.IsZero() || !time.Now().Before(until) {
		t.Fatal("恢复计数=2 后再 3 次应触发降权")
	}
	// 降权中重启：degradeUntil 恢复（降权期不失忆）。
	p2.Flush()
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	_, until2, _ := p3.consecutiveStateOf("u1")
	if until2.IsZero() || !time.Now().Before(until2) {
		t.Fatal("降权期内重启 degradeUntil 应恢复")
	}
	if uids := p3.AvailableUIDs(); len(uids) != 0 {
		t.Fatalf("恢复降权期的账号不应出现在可用列表, got %v", uids)
	}
}

// TestDegradeWritesZeroFails consecutiveFails=0 显式落盘（运维口径，同
// session_dead_fails：零值缺失会误解为"没记录"）。
func TestDegradeWritesZeroFails(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteSuccess("u1") // 制造 dirty 让 Flush 真正写盘，consecutiveFails 保持 0
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"consecutive_fails": 0`) {
		t.Errorf("consecutiveFails=0 时也应显式写出:\n%s", raw)
	}
}

// TestDegradeCountsAsCooling 降权计入 CountsDetailed 的 cooling 计数
// （/status 池级健康度口径覆盖降权号，不误报 healthy）。
func TestDegradeCountsAsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	for n := 0; n < 5; n++ {
		p.NoteFailures("u1")
	}
	total, healthy, cooling, disabled, _ := p.CountsDetailed()
	if total != 2 || healthy != 1 || cooling != 1 || disabled != 0 {
		t.Fatalf("counts=(%d,%d,%d,%d) want (2,1,1,0)", total, healthy, cooling, disabled)
	}
}

// TestDegradeConcurrentNoteFailures 并发喂入无竞态/无丢失超阈：
// N 个 goroutine 各喂若干次，总喂次数 ≥ 阈值时必须观察到降权发生
// （锁保护下计数串行一致；-race 下验证零竞态）。
func TestDegradeConcurrentNoteFailures(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	const goroutines = 8
	const perG = 5 // 总 40 次 ≥ 阈值 5
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < perG; n++ {
				p.NoteFailures("u1")
			}
		}()
	}
	wg.Wait()
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("并发喂入 40 次（≥阈值）应至少触发一次降权")
	}
}

// TestDegradeConcurrentNoteFailuresAndClose 并发喂入 + 停机 Close 落盘竞态
// （任务书重点：连败计数器并发更新/停机持久化竞态检查）。生产停机顺序是
// 「在途请求退出 → Close」；Close 与 NoteFailures 并发（未等到 goroutine 全退
// 就 Close）也必须不 panic/不半写——Close 内部持锁 Flush，与在途写入串行化。
func TestDegradeConcurrentNoteFailuresAndClose(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				p.NoteFailures("u1")
				p.NoteSuccess("u1")
			}
		}()
	}
	// 并发中直接 Close（乱序停机路径）：Close 持锁 Flush 与在途写入串行，不出竞态。
	p.Close()
	wg.Wait()
	// wg 全退后再补一次 Flush（模拟最后一笔入账后的停机快照）。
	p.Flush()
	// 停机后状态文件应存在且可解析（无半写/损坏）。
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("停机后 state.json 应存在: %v", err)
	}
	if !strings.Contains(string(raw), `"consecutive_fails"`) {
		t.Fatalf("停机快照应含连败计数字段:\n%s", raw)
	}
	// 重启可加载（JSON 完整性）。
	p3 := New(fp)
	p3.Add(&auth.Auth{UID: "u1"})
	if _, ok := p3.Status("u1"); !ok {
		t.Fatal("重启后账号应可恢复")
	}
}

// TestSetDegradeInjection SetDegrade 注入生效（阈值/时长/封顶），非法值保留默认。
func TestSetDegradeInjection(t *testing.T) {
	p := New("")
	p.SetDegrade(2, 100*time.Millisecond, time.Hour)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteFailures("u1")
	p.NoteFailures("u1")
	_, until, _ := p.consecutiveStateOf("u1")
	if until.IsZero() {
		t.Fatal("注入阈值 2 应在第 2 次触发")
	}
	if d := time.Until(until); d > time.Hour {
		t.Fatalf("降权时长不得超封顶, got %v", d)
	}
	// 非法注入（0/负值）保留默认：阈值仍 5。
	p2 := New("")
	p2.SetDegrade(0, 0, 0)
	p2.Add(&auth.Auth{UID: "u2"})
	for n := 0; n < 4; n++ {
		p2.NoteFailures("u2")
	}
	if _, until, _ := p2.consecutiveStateOf("u2"); !until.IsZero() {
		t.Fatal("非法注入后默认阈值 5：4 次不应触发")
	}
}

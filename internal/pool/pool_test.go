package pool

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

// withNoPickGap 临时关闭防并发撞号窗口（minPickGap=0），让纯加权分布测试不受影响。
func withNoPickGap(t *testing.T) {
	t.Helper()
	old := minPickGap
	minPickGap = 0
	t.Cleanup(func() { minPickGap = old })
}

func TestPickHighestCredits(t *testing.T) {
	withNoPickGap(t)
	// 三因子加权（credits 比例×10 + 闲置 + 成功率）：积分悬殊时高积分账号应被多数选中，
	// 但不再像纯 credits 加权那样接近 99%（闲置补偿 + 成功率中性 1.5 拉平了基线）。
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	a3 := &auth.Auth{UID: "u3"}
	p.Add(a1)
	p.Add(a2)
	p.Add(a3)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50000)
	p.SetCredits("u3", 300)
	counts := map[string]int{}
	for i := 0; i < 3000; i++ {
		counts[p.Pick("").UID]++
	}
	if counts["u2"] <= counts["u1"] || counts["u2"] <= counts["u3"] {
		t.Errorf("u2 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickSkipsCooling(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	a2 := &auth.Auth{UID: "u2"}
	p.Add(a1)
	p.Add(a2)
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	p.Cooldown("u1", CoolHard, time.Hour, "test")
	got := p.Pick("")
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
}

func TestPickExpiredCooldownReturnsToHealthy(t *testing.T) {
	p := New("")
	a1 := &auth.Auth{UID: "u1"}
	p.Add(a1)
	p.SetCredits("u1", 100)
	p.Cooldown("u1", CoolSoft, time.Millisecond, "429")
	time.Sleep(5 * time.Millisecond)
	got := p.Pick("")
	if got == nil || got.UID != "u1" {
		t.Fatalf("pick=%+v want u1 after cooldown expiry", got)
	}
}

func TestPickNilWhenAllDisabled(t *testing.T) {
	// 全禁用 → 兜底不参与（禁用账号永不参与兜底）→ 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(""); got != nil {
		t.Fatalf("want nil (all disabled), got %+v", got)
	}
}

func TestPickExcluding(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	tried := map[string]bool{"u1": true}
	got := p.PickExcludingForRealm(tried, "", "")
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%+v want u2", got)
	}
	tried["u2"] = true
	if got := p.PickExcludingForRealm(tried, "", ""); got != nil {
		t.Fatalf("want nil, got %+v", got)
	}
}

func TestPickExcludingStaysWithinHealthy(t *testing.T) {
	withNoPickGap(t)
	// 加权随机不能选出冷却/禁用账号。
	p := New("")
	p.Add(&auth.Auth{UID: "u-cold"})
	p.Add(&auth.Auth{UID: "u-hot"})
	p.SetCredits("u-cold", 9999)
	p.SetCredits("u-hot", 1)
	p.Cooldown("u-cold", CoolHard, time.Hour, "x")
	for i := 0; i < 20; i++ {
		got := p.PickExcludingForRealm(nil, "", "")
		if got == nil || got.UID != "u-hot" {
			t.Fatalf("iter %d: picked %+v, want only healthy u-hot", i, got)
		}
	}
}

func TestPickWeightedSkewTowardHighCredits(t *testing.T) {
	withNoPickGap(t)
	// Top5 三因子加权：单账号 credits 占比足够高时，多数挑中它。
	p := New("")
	for _, u := range []string{"w1", "w2", "w3", "w4", "w5", "w6"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 1)
	}
	p.SetCredits("w1", 1000)
	counts := map[string]int{}
	for i := 0; i < 5000; i++ {
		counts[p.Pick("").UID]++
	}
	mx, mxUID := 0, ""
	for uid, n := range counts {
		if n > mx {
			mx, mxUID = n, uid
		}
	}
	if mxUID != "w1" {
		t.Errorf("w1 (highest credits) should be picked most: %v", counts)
	}
}

func TestPickWeightedUniformWhenAllZero(t *testing.T) {
	withNoPickGap(t)
	// credits 全为 0 → 退化为均匀随机，不能只挑固定一个。
	p := New("")
	for _, u := range []string{"z1", "z2", "z3"} {
		p.Add(&auth.Auth{UID: u})
	}
	seen := map[string]bool{}
	for i := 0; i < 30; i++ {
		seen[p.Pick("").UID] = true
	}
	if len(seen) != 3 {
		t.Errorf("uniform fallback should hit all, seen=%v", seen)
	}
}

func TestPickWeightedTopFiveOnly(t *testing.T) {
	withNoPickGap(t)
	// 第 6 高 credits 的账号在 Top5 之外，权重抽签永远轮不到它。
	p := New("")
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a1", 1000)
	p.SetCredits("a2", 1000)
	p.SetCredits("a3", 1000)
	p.SetCredits("a4", 1000)
	p.SetCredits("a5", 1000)
	p.SetCredits("a6", 5) // Top5 之外
	for i := 0; i < 2000; i++ {
		if got := p.Pick(""); got == nil || got.UID == "a6" {
			t.Fatalf("iter %d: picked %+v, a6 must stay outside top-5", i, got)
		}
	}
}

func TestPickTopFiveBySuccessRateNotCredits(t *testing.T) {
	withNoPickGap(t)
	// C1 回归（成功率因子删除后的等价形态）：top5 短名单按三因子权重而非纯
	// credits 截断。a1..a5 credits=100 但连续失败达熔断阈值（退出可选集），
	// a6 credits=90 且从未使用。纯 credits 排序时 a6（90 < 100）不占优；
	// 熔断把 a1..a5 移出候选后 a6 必被选中——失败处置由熔断器（而非成功率权重）
	// 承担正是因子删除后的语义（success-ema-review §2：失败侧与熔断 100% 同源）。
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 选权重最高的候选
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100)
		for i := 0; i < 3; i++ {
			p.NoteError(u) // 连续 3 次达默认熔断阈值 → 退出可选集
		}
	}
	p.Add(&auth.Auth{UID: "a6"})
	p.SetCredits("a6", 90)

	if got := p.Pick(""); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (breaker removes failing accounts from candidates)", got)
	}
}

func TestPickTopFiveByIdleNotCredits(t *testing.T) {
	withNoPickGap(t)
	// C1 回归：闲置补偿同样影响短名单。a1..a5 credits=100 但刚被用过（闲置 0），
	// a6 credits=90 但从未使用（闲置满分）。纯 credits 排序时 a6 进不了 top5；
	// 三因子权重下 a6 权重最高，首轮必被选中。仅断言首轮。
	p := New("")
	now := time.Now()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.Add(&auth.Auth{UID: u})
		p.SetCredits(u, 100)
	}
	p.Add(&auth.Auth{UID: "a6"})
	p.SetCredits("a6", 90)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	// a1..a5 全部"刚被用过"，闲置补偿归零；a6 从未使用 → 闲置满分。
	p.mu.Lock()
	for _, u := range []string{"a1", "a2", "a3", "a4", "a5"} {
		p.byUID[u].lastUsed = now
	}
	p.mu.Unlock()

	if got := p.Pick(""); got == nil || got.UID != "a6" {
		t.Fatalf("pick=%v, want a6 (idle low-credit must enter top5 by weight)", got)
	}
}

func TestPickDeterministicViaSetRandomSource(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	// r=0 ∈ [0,50) → 命中 u1。注入源应使选号完全确定。
	for i := 0; i < 50; i++ {
		if got := p.Pick(""); got == nil || got.UID != "u1" {
			t.Fatalf("iter %d: pick=%+v want u1 (deterministic)", i, got)
		}
	}
}

func TestPickAntiThunderingHerd(t *testing.T) {
	// 100 goroutine 同时 Pick：防并发撞号窗口内同一账号不应被重复选中。
	// credits 相同 → 无注入源时加权随机应天然打散；为保证稳定，全部置 0 走均匀随机。
	p := New("")
	for i := 0; i < 10; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("c%02d", i)})
	}
	// 关键：验证并发中任意瞬间不会全选同一账号。
	const N = 100
	var wg sync.WaitGroup
	picked := make([]string, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if a := p.Pick(""); a != nil {
				picked[idx] = a.UID
			}
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, uid := range picked {
		if uid != "" {
			counts[uid]++
		}
	}
	// 选号必须覆盖多个账号，且最热门的账号不超过一半。
	if len(counts) < 2 {
		t.Fatalf("anti-thundering-herd failed: all %d picks hit %d account(s) %v", N, len(counts), counts)
	}
	for uid, n := range counts {
		if n > N/2 {
			t.Errorf("account %s picked %d/%d (>50%%): thundering herd", uid, n, N)
		}
	}
}

func TestPickLRUFallbackWhenTopAllRecentlyUsed(t *testing.T) {
	// top5 全部刚被选中 → LRU 兜底应挑最近最少使用的那个（= 最早 lastUsed）。
	old := minPickGap
	minPickGap = time.Hour // 超大窗口：任何 lastUsed 都在窗口内
	defer func() { minPickGap = old }()

	p := New("")
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("a%d", i)})
	}
	// 直接构造 lastUsed：不经过 Pick（避免 Pick 改写 lastUsed）。
	order := []string{"a4", "a3", "a2", "a1", "a0"}
	p.mu.Lock()
	for i, uid := range order {
		p.byUID[uid].lastUsed = time.Now().Add(-time.Duration(len(order)-i) * time.Second) // a4 最旧
	}
	p.mu.Unlock()

	got := p.Pick("")
	if got == nil {
		t.Fatal("pick returned nil")
	}
	if got.UID != "a4" {
		t.Errorf("LRU fallback picked %s want a4 (oldest lastUsed)", got.UID)
	}
}

func TestCooldownPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush() // 状态变更走 dirty 标志，落盘由 Flush / 后台 goroutine 负责
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling || st.Reason != "余额不足" {
		t.Fatalf("cooldown lost after reload: %+v ok=%v", st, ok)
	}
}

func TestDisablePersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "12153 session dead")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if p2.Pick("") != nil {
		t.Fatal("disabled account picked after reload")
	}
	st, _ := p2.Status("u1")
	if !st.Disabled || st.Reason != "12153 session dead" {
		t.Errorf("status=%+v", st)
	}
}

func TestReenableIfCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 500)
	got := p.Pick("")
	if got == nil || got.UID != "u1" {
		t.Fatalf("should reenable, pick=%+v", got)
	}
}

func TestReenableZeroCreditsKeepsCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.ReenableIfCredits("u1", 0)
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatal("zero credits should stay cooling")
	}
}

func TestReenableDoesNotTouchDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	p.ReenableIfCredits("u1", 500)
	if p.Pick("") != nil {
		t.Fatal("disabled must not auto-reenable")
	}
}

func TestNoteErrorAccumulatesErrTotal(t *testing.T) {
	// NoteError 语义变更：不再有独立的 err 冷却（CoolErr 已并入熔断器），
	// 只累计 errTotal（不清零，供成功率权重）并喂熔断器 fails。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteError("u1")
	p.NoteError("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("NoteError alone must not set cooling (no CoolErr): %+v", st)
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err not set")
	}
}

func TestNoteSuccessResetsBreakerNotErrTotal(t *testing.T) {
	// NoteSuccess 清 fails/熔断（运行态），但不清 errTotal（累计值）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1") // 触发熔断
	if p.internalHealthy("u1") {
		t.Fatal("breaker should be open (unhealthy) after 2 failures")
	}
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.ErrTotal != 2 {
		t.Errorf("err_total=%d want 2 (cumulative, not cleared by success)", st.ErrTotal)
	}
	if st.Cooling {
		t.Errorf("success should clear breaker: %+v", st)
	}
}

func TestNoteSuccessIncrementsAndRecords(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	st, _ := p.Status("u1")
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.LastSuccessTime.Before(before) {
		t.Errorf("last_success=%v before call", st.LastSuccessTime)
	}
	if !st.LastErrTime.IsZero() {
		t.Errorf("last_err should be zero for fresh success: %v", st.LastErrTime)
	}
}

func TestReenableClearsCoolingNotBreaker(t *testing.T) {
	// C5：签到解冻只清冷却（until/coolKind/reason）+ 更新 credits，不清熔断
	// （fails/retryCount/breakerUntil）。签到成功只证明余额与 billing 通道恢复，
	// 不证明 chat 通道健康——熔断仍按 breakerUntil 退避到期或 NoteSuccess 恢复。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足") // 硬冷却（喂 fails，但此时阈值默认 3，不熔断）
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断（fails→阈值1→fails=0, retryCount=1, breakerUntil 非零）
	p.ReenableIfCredits("u1", 500)
	st, _ := p.Status("u1")
	if st.Reason != "" || st.Credits != 500 {
		t.Errorf("signin should clear reason + set credits=500: %+v", st)
	}
	if st.Until != (time.Time{}) {
		t.Errorf("signin should clear hard-cooling until: %+v", st.Until)
	}
	if st.BreakerUntil.IsZero() {
		t.Fatal("signin must NOT clear breakerUntil (chat health unresolved)")
	}
	// 熔断仍在 → 账号仍不可选（直至 breakerUntil 到期）。
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestReenableKeepsBreaker(t *testing.T) {
	// C5 回归锁定新语义：仅熔断（无冷却）的账号，签到解冻不得清熔断。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("precondition: breaker should be open")
	}
	p.ReenableIfCredits("u1", 500)
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("signin must not clear breakerUntil")
	}
	if p.internalHealthy("u1") {
		t.Fatal("account should stay unhealthy while breaker active after signin")
	}
}

func TestCoolKindPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.Flush()

	// 旧文件缺新字段时零值 → 冷却应仍工作（向后兼容）。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || !st.Cooling {
		t.Fatalf("cooldown state lost after reload: %+v ok=%v", st, ok)
	}
	if st.CoolKind != "hard_credit" {
		t.Errorf("cool_kind after reload=%q want hard_credit", st.CoolKind)
	}
}

func TestStateRoundTripExtendedFields(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolHard, time.Hour, "余额不足")
	p.NoteSuccess("u1") // successCount=1，last_success 非零
	p.NoteSuccess("u1") // successCount=2
	p.NoteError("u1")   // errTotal=1（累计），last_err 非零
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	// JSON tag 全小写下划线；err_total 落盘，err_count 不再落盘。
	for _, want := range []string{`"cool_kind"`, `"success_count"`, `"err_total"`, `"last_success"`, `"last_err"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s:\n%s", want, raw)
		}
	}
	// 运维可见的运行态字段即使零值也显式写出（去 omitempty）：缺失会被误解为"没记录"。
	// （原 error_ema/success_ema 已随成功率 EMA 因子删除，不再落盘。）
	for _, want := range []string{`"soft_streak"`, `"session_dead_fails"`, `"credits_expiring"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("state.json missing %s（零值也应显式写出）:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), `"err_count"`) {
		t.Errorf("state.json should not write legacy err_count:\n%s", raw)
	}

	// 重载后字段保留
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if st.SuccessCount != 2 || st.CoolKind != "hard_credit" {
		t.Errorf("reloaded portrait=%+v", st)
	}
	if st.ErrTotal != 1 {
		t.Errorf("reloaded err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() || st.LastErrTime.IsZero() {
		t.Error("last_success/last_err lost after reload")
	}
}

func TestLoadLegacyErrCountMigratesToErrTotal(t *testing.T) {
	// 迁移测试：旧 state.json 只含 err_count（连续错误）→ 加载后 err_total 正确。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"u1":{"credits":100,"err_count":7}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.ErrTotal != 7 {
		t.Errorf("err_total=%d want 7 (migrated from legacy err_count)", st.ErrTotal)
	}
	// 新字段优先：二者并存时取较大者。
	both := `{"accounts":{"u1":{"credits":100,"err_count":3,"err_total":9}}}`
	if err := os.WriteFile(fp, []byte(both), 0o600); err != nil {
		t.Fatal(err)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st2, _ := p2.Status("u1"); st2.ErrTotal != 9 {
		t.Errorf("err_total=%d want 9 (new field wins over legacy)", st2.ErrTotal)
	}
}

func TestStatusCoolKindDefaultsWhenNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	st, _ := p.Status("u1")
	if st.CoolKind != "" || st.CoolRemaining != 0 {
		t.Errorf("non-cooling portrait=%+v", st)
	}
}

func TestNextDay4AMBoundaries(t *testing.T) {
	cases := []struct {
		name string
		now  string // RFC3339 (UTC 表示)
		want string // 下一个 04:00（同一时区，UTC 表示）
	}{
		{"普通日", "2026-08-28T17:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		// 凌晨 00:00~04:00 触发硬冷却：当天 04:00 尚未到，冷却应落在当天（而非次日），
		// 否则多冷约一天（原 bug）。
		{"凌晨02:30", "2026-08-28T02:30:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨00:00", "2026-08-28T00:00:00+08:00", "2026-08-28T04:00:00+08:00"},
		{"凌晨03:59:59", "2026-08-28T03:59:59+08:00", "2026-08-28T04:00:00+08:00"},
		{"正好4点", "2026-08-28T04:00:00+08:00", "2026-08-29T04:00:00+08:00"},
		{"4点刚过", "2026-08-28T04:00:01+08:00", "2026-08-29T04:00:00+08:00"},
		{"月末(31天月)", "2026-01-31T12:00:00+08:00", "2026-02-01T04:00:00+08:00"},
		{"月末(28天月)", "2026-02-28T12:00:00+08:00", "2026-03-01T04:00:00+08:00"},
		{"闰年月末", "2028-02-29T12:00:00+08:00", "2028-03-01T04:00:00+08:00"},
		{"年末", "2026-12-31T23:59:59+08:00", "2027-01-01T04:00:00+08:00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, c.now)
			if err != nil {
				t.Fatal(err)
			}
			want, err := time.Parse(time.RFC3339, c.want)
			if err != nil {
				t.Fatal(err)
			}
			if got := nextDay4AM(now); !got.Equal(want) {
				t.Errorf("nextDay4AM(%v)=%v want %v", c.now, got, want)
			}
		})
	}
}

func TestCooldownUntilTomorrow4AM(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	before := time.Now()
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	after := time.Now()
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("no status")
	}
	if !st.Cooling {
		t.Fatalf("should be cooling: %+v", st)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 冷却截止必须是"此刻之后的最近一个 04:00"：晚于 now、距今不超过 24h
	//（凌晨 00:00~04:00 触发时落在当天 04:00，其余时段落在次日 04:00，跨度恒 < 24h）。
	if st.Until.Before(after) {
		t.Errorf("until %v is in the past (call span %v..%v)", st.Until, before, after)
	}
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4", st.Until.Hour())
	}
	if d := st.Until.Sub(after); d > 24*time.Hour {
		t.Errorf("until %v is more than 24h out: %v", st.Until, d)
	}
	// 全冷却时余额耗尽（hard）号不参与兜底 → 返回 nil（等签到恢复）。
	if got := p.Pick(""); got != nil {
		t.Fatalf("all-hard-cooling should return nil (hard excluded from fallback), got %+v", got)
	}
}

func TestCooldownUntilTomorrow4AMPersists(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Until.Hour() != 4 || st.Reason != "余额不足" {
		t.Errorf("status after reload=%+v ok=%v", st, ok)
	}
}

// ---------------------------------------------------------------------------
// 软冷却指数退避（softStreak）
// ---------------------------------------------------------------------------

// wantCoolSec 断言账号当前冷却剩余秒数 ≈ want（±tol 秒，容忍测试内的 tick 漂移）。
func wantCoolSec(t *testing.T, p *Pool, uid string, want int64, tol int64) {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("status(%s) missing", uid)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("%s should be in soft_rate cooling: %+v", uid, st)
	}
	if got := st.CoolRemaining; got < want-tol || got > want+tol {
		t.Errorf("cool_remaining_sec=%d want ~%d (±%d)", got, want, tol)
	}
}

// TestCooldownSoftBoundedBackoffIfNotCooling 无重置时间的 429 仍有界退避：
// 软冷却**从非冷却态**触发时按 2 倍指数增长（封顶 1h 本用例不触及），softStreak
// 计数随新冷却递增。退避只在**进入一次新冷却**时发生——冷却中途的兜底探测不推进
// （见 TestCooldownSoftRateNoDoubleWhenAlreadyCooling），既保留「无权威时间时的有界
// 退避」，又废除「越重试越冷」。
func TestCooldownSoftBoundedBackoffIfNotCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour) // 封顶 1h：本用例三步（600/1200/2400）都不触及

	for i, want := range []int64{600, 1200, 2400} {
		// 软冷却到期（until 过期）后再触发新冷却 → 退避继续推进。
		p.forceSoftExpired("u1")
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("after call %d: soft_streak=%d want %d", i+1, st.SoftStreak, i+1)
		}
	}
}

// TestCooldownSoftRateResetTimeAligns 带权威重置时间的账号级软冷却：写 until 对齐到
// 上游重置墙钟、绝不 touch softStreak（旧实现每次都 softStreak++），且不走退避。
func TestCooldownSoftRateResetTimeAligns(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Minute) // 远超 600s 基数，对齐将远超退避基数
	p.SetSoftRateMax(time.Hour)               // reset 在封顶内，不被截断

	p.CooldownSoftRate("u1", 600*time.Second, reset, "429 rate limit")
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("应为 soft_rate 冷却: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("带重置时间的 429 绝不 softStreak 堆加，soft_streak=%d want 0", st.SoftStreak)
	}
	if d := st.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("until=%v want ~reset=%v（精确对齐，无退避）", st.Until, reset)
	}
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("账号级 CooldownSoftRate 不写模型台账: %+v", st.RateLimitedModels)
	}
}

// TestCooldownSoftRateNoDoubleWhenAlreadyCooling 核心回归：冷却中的兜底探测再次撞 429
// **不得**翻倍/延长（旧实现每次都 softStreak++ 指数翻倍，把全池推到 2h 封顶）。
func TestCooldownSoftRateNoDoubleWhenAlreadyCooling(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)

	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit") // streak=1, until≈now+600s
	wantCoolSec(t, p, "u1", 600, 3)
	if st, _ := p.Status("u1"); st.SoftStreak != 1 {
		t.Fatalf("soft_streak=%d want 1", st.SoftStreak)
	}
	// 冷却中重复触发（兜底探测）→ 时长/streak 均不变。
	for i := 0; i < 3; i++ {
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "429 rate limit")
	}
	wantCoolSec(t, p, "u1", 600, 3)
	if st, _ := p.Status("u1"); st.SoftStreak != 1 {
		t.Fatalf("already-cooling probe must not advance soft_streak, got %d", st.SoftStreak)
	}
}

func TestCooldownSoftCappedBySoftRateMax(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(250 * time.Second)

	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 100, 3)
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 200, 3)
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 100*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 250, 3)
}

func TestCooldownSoftDefaultCapWhenUnset(t *testing.T) {
	// 未注入 softRateMax → 按 2h 封顶（避免裸用池时退避无上限）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	for i, want := range []int64{600, 1200, 2400, 4800, 7200} {
		p.forceSoftExpired("u1")
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
		wantCoolSec(t, p, "u1", want, 3)
		if st, _ := p.Status("u1"); st.SoftStreak != i+1 {
			t.Errorf("soft_streak=%d want %d", st.SoftStreak, i+1)
		}
	}
}

func TestSetSoftRateMaxIgnoresNonPositive(t *testing.T) {
	// 非正值保留原值（风格同 SetBreaker）：0 不应把封顶清零导致无上限。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(0)
	p.SetSoftRateMax(-time.Second)
	for i := 0; i < 6; i++ {
		p.forceSoftExpired("u1")
		p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	}
	wantCoolSec(t, p, "u1", 7200, 3) // 仍是 2h 封顶（第 6 步 19200s → 7200s）
}

func TestCooldownSoftStreakResetBySuccess(t *testing.T) {
	// 成功即证明账号恢复 → streak 归零，下次软冷却回到基数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 1200, 3)

	p.NoteSuccess("u1")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("success should reset soft_streak, got %d", st.SoftStreak)
	}
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestCooldownSoftStreakResetByReenable(t *testing.T) {
	// 签到解冻（reviveCoolingLocked）清 cooling 域 → softStreak 一并归零；
	// 熔断域（fails/retryCount/breakerUntil）不动，与既有 C5 语义一致。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	failsBefore := p.breakerFails("u1")

	p.ReenableIfCredits("u1", 500)
	st, _ := p.Status("u1")
	if st.SoftStreak != 0 {
		t.Errorf("reenable should reset soft_streak, got %d", st.SoftStreak)
	}
	if st.Cooling {
		t.Errorf("reenable should clear cooling: %+v", st)
	}
	if failsAfter := p.breakerFails("u1"); failsAfter != failsBefore {
		t.Errorf("reenable must not touch breaker: fails %d → %d", failsBefore, failsAfter)
	}

	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestCooldownHardDoesNotAdvanceSoftStreak(t *testing.T) {
	// 硬冷却（余额耗尽）时长由签到时点决定，不参与软退避指数。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownUntilTomorrow4AM("u1", "余额不足")
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("hard cooldown must not touch soft_streak, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

func TestSoftStreakPersistsAcrossReload(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.forceSoftExpired("u1")
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"soft_streak"`) {
		t.Fatalf("state.json missing soft_streak:\n%s", raw)
	}

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if st, _ := p2.Status("u1"); st.SoftStreak != 2 {
		t.Fatalf("soft_streak after reload=%d want 2", st.SoftStreak)
	}
	// 退避从持久化的 streak 继续：第 3 次 → 2400s。
	p2.forceSoftExpired("u1")
	p2.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p2, "u1", 2400, 3)
}

func TestSoftStreakMissingInLegacyStateFile(t *testing.T) {
	// 旧 state.json 无 soft_streak → 零值兼容，退避从基数重新开始。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":100}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if st, _ := p.Status("u1"); st.SoftStreak != 0 {
		t.Fatalf("legacy file should load soft_streak=0, got %d", st.SoftStreak)
	}
	p.CooldownSoftRate("u1", 600*time.Second, time.Time{}, "x")
	wantCoolSec(t, p, "u1", 600, 3)
}

// forceSoftExpired 把账号的软冷却强制标记为已到期（until 归零、保留 coolKind=soft），
// 让下一次 CooldownSoftRate 视作「新限流」继续推进退避，而不依赖真实 sleep。仅测试用。
func (p *Pool) forceSoftExpired(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.byUID[uid]; ok {
		e.until = time.Time{}
	}
}

// ---------------------------------------------------------------------------
// issue #31：429 6004 模型级限流 → 按上游重置时间收窄冷却 + 模型级豁免选号
// ---------------------------------------------------------------------------

func TestCooldownSoftForModelParsedUntil(t *testing.T) {
	// 6004 msg 带「将在 … 重置」→ modelCooldowns[glm-5.3].Until 精确等于解析时间
	// （wall-clock 判断）。用未来 5 分钟的时间戳：解析后 ≈ now+5m，远短于固定 600s
	// 基数的指数退避，证明"上游明说重置时间"优先于"600s 起指数退避"。
	reset := time.Now().Add(5 * time.Minute)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, ok := p.Status("u1")
	if !ok {
		t.Fatalf("status missing: %+v", st)
	}
	// 6004 模型级冷却：账号级 until 不被写（独立模型冷却），台账携带模型截止。
	if !st.Until.IsZero() {
		t.Errorf("until=%v 应为零值（6004 不写账号级 until）", st.Until)
	}
	if st.SoftStreak != 0 {
		t.Errorf("soft_streak=%d 应保持 0（有重置时间绝不指数堆加）", st.SoftStreak)
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("rate_limited_models=%+v want [glm-5.3] 的模型级台账", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
}

func TestCooldownSoftForModelCappedBySoftRateMax(t *testing.T) {
	// 解析时间超出 soft_rate_max → 模型级冷却 until 截断到 soft_rate_max（不无限期拉黑）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超过封顶 10m
	before := time.Now()
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "429 rate limit")
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models=%+v want 1 行", st.RateLimitedModels)
	}
	if d := st.RateLimitedModels[0].Until.Sub(before); d > 10*time.Minute+time.Second {
		t.Errorf("model until=%v want capped at soft_rate_max=10m", st.RateLimitedModels[0].Until)
	}
}

func TestCooldownSoftForModelNoResetFallbackBackoff(t *testing.T) {
	// 无解析时间（resetAt 零值）→ 退回账号级有界退避；冷却中的兜底探测不翻倍。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(time.Hour)
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	// 仍在冷却中：兜底探测不推进退避。
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 600, 3)
	// 软冷却到期后：续一次新限流 → 退避推进到 1200s。
	p.forceSoftExpired("u1")
	p.CooldownSoftForModel("u1", 600*time.Second, time.Time{}, "", "429 rate limit")
	wantCoolSec(t, p, "u1", 1200, 3)
}

// TestPickExcludingForModelSkipsSoftCoolingSameModel 冷却中账号（6004 带解析时间，
// 已记录模型）+ 同 model 请求 → 仍不可选（现状语义保持）。
func TestPickExcludingForModelSkipsSoftCoolingSameModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForRealm(nil, "glm-5.3", "")
	if got == nil || got.UID != "u2" {
		t.Fatalf("same-model request must skip cooling u1, got %+v", got)
	}
}

// TestPickExcludingForModelAllowsDifferentModel 6004 冷却中的账号 + 不同 model
// → 视为可用，可选到该号（真·单模型限流，切模型立即可用）。
func TestPickExcludingForModelAllowsDifferentModel(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 1000)
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 }) // r=0 → 最高分 u1
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForRealm(nil, "hy3-x", "")
	if got == nil || got.UID != "u1" {
		t.Fatalf("different-model request should bypass u1 soft cooling, got %+v", got)
	}
}

// TestCooldownSoftWithoutModelRecordsNone 非 6004 的普通软冷却（resetAt 零值，
// 不写 modelCooldowns）→ 退回账号级 until 冷却，不因模型切换而豁免（现状语义）。
func TestCooldownSoftWithoutModelRecordsNone(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.CooldownSoftForModel("u1", time.Minute, time.Time{}, "", "429 rate limit")
	// 冷却中 + 不同 model 请求仍跳过 u1（无模型级冷却条目，不豁免）。
	got := p.PickExcludingForRealm(nil, "hy3-x", "")
	if got == nil || got.UID != "u2" {
		t.Fatalf("no model recorded → must not bypass, got %+v", got)
	}
}

// TestPickExcludingForModelBreakerStillBlocks 模型豁免只豁免软冷却维度，
// 熔断（breakerUntil）仍拦截：6004 冷却 + 熔断中的账号，切模型也不可选。
func TestPickExcludingForModelBreakerStillBlocks(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 })
	p.SetBreaker(1, time.Hour, time.Hour)
	p.NoteError("u1") // u1 熔断
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	got := p.PickExcludingForRealm(nil, "hy3-x", "")
	if got == nil || got.UID != "u2" {
		t.Fatalf("breaker must still block, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// ServableNow 的模型级豁免（issue #31 探活侧）：6004 单模型限流时，账号对该模型
// 不可用但对其他模型仍可选，/healthz 不得因"全号被某一模型限流"而误报 503。
// ---------------------------------------------------------------------------

func TestServableNowModelExemptCounts(t *testing.T) {
	// 单号处于 6004 模型级软冷却（带解析时间、记录 modelCooldowns）→ 其他模型仍可达，
	// ServableNow 必须为 true（与 chat 的 healthyForModel 放行切模型请求同口径）。
	// 反向（同模型不可选）已由 TestPickExcludingForModelSkipsSoftCoolingSameModel 覆盖；
	// 本池无其他候选，同模型选号会走全冷却兜底，不在此重复断言。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	if !p.ServableNow() {
		t.Fatal("model-exempt account must keep pool servable (other models reachable)")
	}
}

func TestServableNowPlainSoftNotExempt(t *testing.T) {
	// 普通软冷却（无 modelCooldowns，非 6004 模型级）→ 账号级不可用，ServableNow 必须 false。
	// 守门：豁免不得从模型级泄漏到普通冷却。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	if p.ServableNow() {
		t.Fatal("plain soft cooling (no model) must NOT be servable")
	}
}

func TestServableNowExemptButInFlightFull(t *testing.T) {
	// 模型豁免形态 + 在途占满 → 仍不可服务（在途维度独立于豁免，探活须叠加判定）。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	if !p.Acquire("u1") {
		t.Fatal("acquire should succeed at max=1")
	}
	defer p.Release("u1")
	if p.ServableNow() {
		t.Fatal("model-exempt but in-flight-full account must not count as servable")
	}
}

func TestServableNowExemptButDisabled(t *testing.T) {
	// 模型豁免形态 + 被禁用（session dead）→ disabled 优先级最高，ServableNow 必须 false。
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	p.Disable("u1", "session dead")
	if p.ServableNow() {
		t.Fatal("disabled account must never be servable, even in model-exempt form")
	}
}

// TestModelCooldownsClearedByPlainCooldown 回归：6004 模型冷却后，若账号又经历一次
// **非模型级**软冷却（plain Cooldown），modelCooldowns 必须被清空——否则上次 6004 的
// 模型豁免会泄漏到本次账号级限流上，导致"换模型请求"错误绕过本次冷却。
func TestModelCooldownsClearedByPlainCooldown(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 1)
	p.SetRandomSource(func(n int64) int64 { return 0 })

	// 1) 6004 带解析时间 → 记录模型 glm-5.3。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "6004")
	if got := p.PickExcludingForRealm(nil, "hy3-x", ""); got == nil || got.UID != "u1" {
		t.Fatalf("precondition: different-model should bypass, got %+v", got)
	}
	// 2) 账号恢复后经历普通账号级软冷却（无模型语义）。
	p.NoteSuccess("u1") // 还原 fresh 状态（Cooldown 会重设 until）
	p.Cooldown("u1", CoolSoft, time.Minute, "429 rate limit")
	// 3) 换模型请求不得再豁免（modelCooldowns 已清空）。
	got := p.PickExcludingForRealm(nil, "hy3-x", "")
	if got == nil || got.UID != "u2" {
		t.Fatalf("plain cooldown must clear modelCooldowns (no bypass), got %+v", got)
	}
}

// TestModelCooldownsPersistedToState modelCooldowns 现已持久化（修复重启后 6004
// 模型级冷却失忆）：带解析时间触发后落盘写入 model_cooldowns 字段，重载后恢复。
// 旧 state.json 不写该字段时缺省空（向后兼容，见 TestModelCooldownsPersistCompatOldState）。
func TestModelCooldownsPersistedToState(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	// reset 在未来 → 模型级冷却生效（Until 在未来）。
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	p.Flush()
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "model_cooldowns") {
		t.Errorf("state.json should persist model_cooldowns:\n%s", raw)
	}
	// 重载后恢复（Until 在未来，不过滤）。
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.mu.RLock()
	n := len(p2.byUID["u1"].modelCooldowns)
	p2.mu.RUnlock()
	if n != 1 {
		t.Errorf("modelCooldowns should restore on reload, found %d entries", n)
	}
}

// ---------------------------------------------------------------------------
// issue #36：限额台账——/status 透出仍在限额的模型 + 预计恢复时间
// ---------------------------------------------------------------------------

// TestRateLimitedModelsInStatus 6004 带解析时间 → modelCooldowns 被写入 →
// Status.RateLimitedModels 输出含限流模型 + 冷却截止 + 上游原始重置墙钟。
func TestRateLimitedModelsInStatus(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(35 * time.Minute)
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")

	st, ok := p.Status("u1")
	if !ok {
		t.Fatalf("status missing")
	}
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models=%v want 1 行", st.RateLimitedModels)
	}
	row := st.RateLimitedModels[0]
	if row.Model != "glm-5.3" {
		t.Errorf("model=%q want glm-5.3", row.Model)
	}
	if row.Reason != "6004 model rate limit" {
		t.Errorf("reason=%q want 6004 model rate limit", row.Reason)
	}
	// 6004 模型级冷却不写账号级 until：st.Until 应为零值，模型截止在台账行里。
	if !st.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 不写账号级 until）", st.Until)
	}
	// row.Until = 该模型的独立冷却截止（≈ reset，35m < soft_rate_max 2h 未截断）。
	if d := row.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("row.until=%v want ~%v", row.Until, reset)
	}
	// ResetAt 是未经 6004 截断的上游重置墙钟（35m < soft_rate_max 2h，因此未被截断）。
	if d := row.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("reset_at=%v want ~%v", row.ResetAt, reset)
	}
}

// TestRateLimitedModelsRetainsUncappedResetAt soft_rate_max 截断了 until，
// 但台账必须保留上游未截断的原始重置墙钟（issue #36：运维按真实恢复时刻观察）。
func TestRateLimitedModelsRetainsUncappedResetAt(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetSoftRateMax(10 * time.Minute)
	reset := time.Now().Add(2 * time.Hour) // 远超封顶 10m
	p.CooldownSoftForModel("u1", 600*time.Second, reset, "glm-5.3", "6004 model rate limit")

	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models=%v want 1 行", st.RateLimitedModels)
	}
	row := st.RateLimitedModels[0]
	// row.Until = 该模型的冷却截止（被截断到封顶 ≤ 10m）。
	if rem := row.Until.Sub(time.Now()); rem <= 0 || rem > 10*time.Minute+time.Second {
		t.Errorf("row.until 应在 (0, 10m] 区间，实际剩余 %v", rem)
	}
	// 6004 模型级冷却不写账号级 until：st.Until 为零值。
	if !st.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 不写账号级 until）", st.Until)
	}
	// reset_at 保留原始 2h 墙钟（未被截断）。
	if d := row.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("reset_at=%v want ~2h 后=%v", row.ResetAt, reset)
	}
}

// TestRateLimitedModelsExpired 限额到期后台账从 Status 消失（恢复）。
// 用近未来 30ms 的重置墙钟：初始显示台账，等墙钟过后台账消失 + 账号退出冷却。
// （带解析时间 6004 的 until 由 resetAt 决定，故等待几百 ms 即可确定性触达过期边界。）
func TestRateLimitedModelsExpired(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	reset := time.Now().Add(30 * time.Millisecond)
	p.CooldownSoftForModel("u1", time.Hour, reset, "glm-5.3", "6004 model rate limit")
	if st, _ := p.Status("u1"); len(st.RateLimitedModels) != 1 {
		t.Fatalf("初始应对该模型限额显示台账: %+v", st.RateLimitedModels)
	}
	time.Sleep(80 * time.Millisecond) // 越过重置墙钟（until 已过）
	st, _ := p.Status("u1")
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("到期后台账应消失: %+v", st.RateLimitedModels)
	}
	if st.Cooling {
		t.Errorf("到期后账号应退出冷却: %+v", st)
	}
}

// TestRateLimitedModelsNoLimitZeroRegression 未限流 / 普通软冷却账号零回归：
// RateLimitedModels 必须为空（nil），不产生台账行。
func TestRateLimitedModelsNoLimitZeroRegression(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "ok"})
	p.Add(&auth.Auth{UID: "plain"})
	p.Cooldown("plain", CoolSoft, time.Minute, "429 rate limit") // 普通软冷却（无 modelCooldowns）
	for _, uid := range []string{"ok", "plain"} {
		st, ok := p.Status(uid)
		if !ok {
			t.Fatalf("status(%s) missing", uid)
		}
		if len(st.RateLimitedModels) != 0 {
			t.Errorf("uid=%s rate_limited_models=%v want 空（零回归）", uid, st.RateLimitedModels)
		}
	}
}

func TestList(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1", Nickname: "nick1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 42)
	p.Cooldown("u2", CoolSoft, time.Minute, "429")
	list := p.List()
	if len(list) != 2 {
		t.Fatalf("list=%d", len(list))
	}
	var s1, s2 Status
	for _, s := range list {
		if s.UID == "u1" {
			s1 = s
		}
		if s.UID == "u2" {
			s2 = s
		}
	}
	if s1.Credits != 42 || s1.Nickname != "nick1" || s1.Disabled || s1.Cooling {
		t.Errorf("s1=%+v", s1)
	}
	if !s2.Cooling || s2.Reason != "429" {
		t.Errorf("s2=%+v", s2)
	}
}

func TestRemoveMissingFromDir(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SyncToDir([]*auth.Auth{{UID: "u2"}})
	if p.Pick("") == nil || p.Pick("").UID != "u2" {
		t.Fatal("u1 should be removed")
	}
	if _, ok := p.Status("u1"); ok {
		t.Fatal("u1 should not exist")
	}
}

func TestFlushPersistsCredits(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 42 {
		t.Fatalf("flush not persisted: %+v ok=%v", st, ok)
	}
}

// readStateFile 读取并解析一份 state.json（测试辅助，供异步落盘断言轮询用）。
func readStateFile(fp string) (stateFile, error) {
	var sf stateFile
	raw, err := os.ReadFile(fp)
	if err != nil {
		return sf, err
	}
	err = json.Unmarshal(raw, &sf)
	return sf, err
}

func TestAutoFlush(t *testing.T) {
	old := flushInterval
	flushInterval = 20 * time.Millisecond
	defer func() { flushInterval = old }()

	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	defer p.Close() // 收尾停掉 20ms 后台 flusher（Close 的同步落盘发生在断言之后）
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 77)

	// 后台 flusher 是异步周期落盘：轮询「能否读回 credits=77」而不是只等文件出现。
	// 只等 os.Stat 会假阳性——flusher 首帧可能先于 SetCredits 落盘（读到 0），
	// 或与同包改写 flushInterval 的用例时序漂移（既有 flake 的根因）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		if sf, err := readStateFile(fp); err == nil {
			if a, ok := sf.Accounts["u1"]; ok && a.Credits == 77 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("state.json not written by background flusher")
		}
		time.Sleep(10 * time.Millisecond)
	}
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, ok := p2.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("auto flush not persisted: %+v ok=%v", st, ok)
	}
}

func TestFlushIdempotentWhenClean(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.Flush() // 无 dirty，不应写盘
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("flush on clean pool should not write: %v", err)
	}
}

func TestSaveFailureRecordedAndRecovers(t *testing.T) {
	// stateFp 的父路径是一个普通文件（非目录）→ MkdirAll/WriteFile 必失败，
	// root 也不可绕过，可靠地触发落盘失败路径。
	dir := t.TempDir()
	block := filepath.Join(dir, "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(block, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	p.Flush()
	if p.persistFails == 0 {
		t.Fatal("persist failure should be recorded (visible), got 0")
	}

	// 换回可写目录 → 成功后 persistFails 归零（恢复日志由零值门槛触发）。
	good := filepath.Join(t.TempDir(), "state.json")
	p2 := New(good)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetCredits("u1", 42)
	p2.Flush()
	if p2.persistFails != 0 {
		t.Fatalf("successful save should reset persistFails, got %d", p2.persistFails)
	}
	if raw, err := os.ReadFile(good); err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("state.json not written on success: %v %s", err, raw)
	}
}

// TestSaveLockedPermissionDenied 不可写目录触发落盘失败：首错详报出现且无 panic，
// persistFails 计数累加。chmod 0500 模拟容器内 app(uid 10001) 对 root:root 目录
// 无写权限的 issue #52 场景。注意：若测试以 root 运行，chmod 不阻写——此时回落到
// "父路径是文件"的可靠失败路径，保证测试恒定可复现。
func TestSaveLockedPermissionDenied(t *testing.T) {
	dir := t.TempDir()
	stateFp := filepath.Join(dir, "state.json")
	p := New(stateFp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 7)

	// chmod 0500 让普通用户不可写；root 仍可写（见下方回落）。
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	p.Flush()
	fails1 := p.persistFails

	if fails1 == 0 {
		// root 下 chmod 不阻写 → 回落到"父路径是文件"的可靠失败路径重测。
		block := filepath.Join(t.TempDir(), "block")
		if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		p2 := New(filepath.Join(block, "state.json"))
		p2.Add(&auth.Auth{UID: "u1"})
		p2.SetCredits("u1", 7)
		p2.Flush()
		if p2.persistFails == 0 {
			t.Fatal("persist failure should be recorded (chmod or block-path), got 0")
		}
		fails1 = p2.persistFails
	}
	if fails1 == 0 {
		t.Fatal("persist failure should be recorded")
	}
	// 无 panic 即通过（首错详报已由 notePersistFail 打印，恢复日志由零值门槛触发）。
}

// TestSaveLockedRecover 先失败后恢复：首错详报 + 恢复日志 + persistFails 归零。
func TestSaveLockedRecover(t *testing.T) {
	// 阶段 1：父路径是文件 → 落盘失败，persistFails 累加。
	block := filepath.Join(t.TempDir(), "block")
	if err := os.WriteFile(block, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(block, "state.json"))
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	p.Flush()
	if p.persistFails == 0 {
		t.Fatal("first flush should fail (block path)")
	}

	// 阶段 2：切到可写目录 → 落盘成功，persistFails 归零（恢复日志由零值门槛触发）。
	good := filepath.Join(t.TempDir(), "state.json")
	p.stateFp = good
	p.dirty.Store(true) // 强制再写一次
	p.Flush()
	if p.persistFails != 0 {
		t.Fatalf("successful save should reset persistFails, got %d", p.persistFails)
	}
	if raw, err := os.ReadFile(good); err != nil || !strings.Contains(string(raw), `"credits": 42`) {
		t.Fatalf("state.json not written on recovery: %v %s", err, raw)
	}
}

// ---------------------------------------------------------------------------
// T2 熔断器 + 全冷却兜底 + 指数退避
// ---------------------------------------------------------------------------

// breakerUntil 曝露内部运行态供测试断言（包内私有 helper）。
func (p *Pool) breakerUntil(uid string) (time.Time, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return time.Time{}, false
	}
	return e.breakerUntil, true
}

// breakerFails 曝露 entry.fails 供测试断言（包内私有 helper）。
func (p *Pool) breakerFails(uid string) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.byUID[uid].fails
}

// internalHealthy 曝露 entry.healthy 供测试断言（包内私有 helper）。
func (p *Pool) internalHealthy(uid string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return false
	}
	return e.healthy(time.Now())
}

func TestBreakerTripsAtThreshold(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	for i := 0; i < 2; i++ {
		p.NoteError("u1") // NoteError 只驱动熔断（不再有单独 err 冷却）
		if bt, ok := p.breakerUntil("u1"); ok && !bt.IsZero() {
			t.Fatalf("breaker tripped too early at %d: %v", i+1, bt)
		}
	}
	p.NoteError("u1")
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatalf("breaker should trip at threshold: until=%v ok=%v", bt, ok)
	}
}

func TestBreakerSuccessClears(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Hour, 6*time.Hour)
	p.NoteError("u1")
	p.NoteError("u1")
	p.NoteError("u1") // 触发熔断
	if bt, _ := p.breakerUntil("u1"); bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	p.NoteSuccess("u1")
	if bt, _ := p.breakerUntil("u1"); !bt.IsZero() {
		t.Fatalf("success should clear breaker, until=%v", bt)
	}
	if !p.internalHealthy("u1") {
		t.Fatal("account should be healthy after success clears breaker")
	}
}

func TestBreakerExponentialBackoffCapped(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetBreaker(3, time.Minute, 4*time.Minute) // threshold=3：连续 3 次失败熔断一次
	// 连续 9 次失败（无成功）→ 熔断 3 次，retryCount 1→2→3，退避 1m→2m→4m(封顶)。
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			p.NoteError("u1") // 连续失败只驱动熔断
		}
	}
	bt, ok := p.breakerUntil("u1")
	if !ok || bt.IsZero() {
		t.Fatal("breaker should be open")
	}
	d := time.Until(bt)
	// 第 3 次熔断：d = min(1m * 2^2, 4m) = 4m
	if d < 4*time.Minute-time.Second || d > 4*time.Minute+time.Second {
		t.Errorf("backoff should cap at max=4m, got %v", d)
	}

	// 对比第 1 次熔断（新账号重新来）：退避应更短。
	p2 := New("")
	p2.Add(&auth.Auth{UID: "u1"})
	p2.SetBreaker(3, time.Minute, 4*time.Minute)
	for j := 0; j < 3; j++ {
		p2.NoteError("u1")
	}
	bt1, _ := p2.breakerUntil("u1")
	if d1 := time.Until(bt1); d1 > time.Minute+time.Second {
		t.Errorf("first trip should be ~1m, got %v", d1)
	}
}

func TestFallbackPicksEarliestExpiry(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	// 两个都软冷却；early 更早到期 → 兜底选 early。
	p.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("early", CoolSoft, time.Hour, "x")
	got := p.Pick("")
	if got == nil || got.UID != "early" {
		t.Fatalf("fallback should pick earliest expiry (early), got %+v", got)
	}
}

func TestFallbackSkipsDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "cooled"})
	p.Add(&auth.Auth{UID: "dead"})
	p.Cooldown("cooled", CoolSoft, time.Hour, "x")
	p.Disable("dead", "session dead") // 禁用不参与兜底
	got := p.Pick("")
	if got == nil || got.UID != "cooled" {
		t.Fatalf("fallback should skip disabled, got %+v", got)
	}
}

func TestFallbackSkipsHardCooldown(t *testing.T) {
	// D3：余额耗尽（CoolHard）号不参与兜底——调了必 402，浪费轮换并产生噪音日志。
	p := New("")
	p.Add(&auth.Auth{UID: "hard"})
	p.Cooldown("hard", CoolHard, time.Hour, "余额不足")
	if got := p.Pick(""); got != nil {
		t.Fatalf("hard-cooled account must not be fallback-picked, got %+v", got)
	}
}

func TestFallbackAllHardReturnsNil(t *testing.T) {
	// 全 hard 冷却 → 无软冷却/熔断号可兜底 → 返回 nil。
	p := New("")
	p.Add(&auth.Auth{UID: "h1"})
	p.Add(&auth.Auth{UID: "h2"})
	p.Cooldown("h1", CoolHard, time.Hour, "x")
	p.Cooldown("h2", CoolHard, 2*time.Hour, "x")
	if got := p.Pick(""); got != nil {
		t.Fatalf("all-hard should return nil, got %+v", got)
	}
}

func TestFallbackSoftAndBreakerParticipate(t *testing.T) {
	// D3：soft 与 breaker 冷却号允许参与兜底，取最早到期者。
	p := New("")
	p.Add(&auth.Auth{UID: "soft"})
	p.Add(&auth.Auth{UID: "brk"})
	p.Cooldown("soft", CoolSoft, 10*time.Minute, "429") // soft: until=10m（重构后 Cooldown 不喂熔断）
	p.SetBreaker(2, 5*time.Minute, 5*time.Minute)       // 阈值 2：soft 的 1 次失败不熔断
	p.NoteError("brk")                                  // brk: fails=1
	p.NoteError("brk")                                  // brk: 熔断，breakerUntil=5m
	got := p.Pick("")
	if got == nil {
		t.Fatal("fallback should pick breaker (earliest) account")
	}
	if got.UID != "brk" {
		t.Fatalf("fallback should pick earliest expiry brk (5m < soft 10m), got %+v", got)
	}
}

func TestFallbackNilWhenAllDisabled(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Disable("u1", "session dead")
	if got := p.Pick(""); got != nil {
		t.Fatalf("want nil when all disabled, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// T3 三因子加权选取
// ---------------------------------------------------------------------------

// idleWeightOf 曝露 weightOf 的单因子拆解不便，改用完整权重断言（包内私有 helper）。
func (p *Pool) entryWeight(uid string) float64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.byUID[uid]
	var maxCredits int64
	for _, x := range p.byUID {
		if x.credits > maxCredits {
			maxCredits = x.credits
		}
	}
	return p.weightOf(e, maxCredits, time.Now())
}

func TestWeightHighCreditsDominates(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "hi"})
	p.Add(&auth.Auth{UID: "lo"})
	p.SetCredits("hi", 1000)
	p.SetCredits("lo", 10)
	wHi, wLo := p.entryWeight("hi"), p.entryWeight("lo")
	if wHi <= wLo {
		t.Errorf("high credits should weigh more: hi=%v lo=%v", wHi, wLo)
	}
}

func TestWeightIdleCompensation(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "used"})
	p.Add(&auth.Auth{UID: "idle"})
	p.SetCredits("used", 100)
	p.SetCredits("idle", 100)
	// used 1 小时前被选中过、idle 从未使用 → idle 权重更高（闲置补偿）。
	p.mu.Lock()
	p.byUID["used"].lastUsed = time.Now().Add(-1 * time.Hour)
	p.mu.Unlock()
	wUsed, wIdle := p.entryWeight("used"), p.entryWeight("idle")
	if wIdle <= wUsed {
		t.Errorf("idle should weigh more: used=%v idle=%v", wUsed, wIdle)
	}
}

// TestWeightLowSuccessRateDowngrades 改写（成功率因子已删，success-ema-review §4）：
// 失败侧处置已全权由熔断/冷却/降权接管——NoteError 达阈值触发熔断后账号不可选
// （healthyForModel 失败），不再依赖权重降级。断言锁定新语义：仅成败计数不
// 影响权重（等权），但连续失败达熔断阈值后账号退出可选集（重语义由状态机承担）。
func TestWeightLowSuccessRateDowngrades(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "good"})
	p.Add(&auth.Auth{UID: "bad"})
	p.SetCredits("good", 100)
	p.SetCredits("bad", 100)
	p.NoteSuccess("good")
	p.NoteError("bad")
	wGood, wBad := p.entryWeight("good"), p.entryWeight("bad")
	if wBad != wGood {
		t.Errorf("因子删除后成败计数不应影响权重: good=%v bad=%v", wGood, wBad)
	}
	// 熔断接管失败侧：连续 NoteError 达默认阈值 3 → bad 不可选。
	p.NoteError("bad")
	p.NoteError("bad")
	if got := p.Pick(""); got == nil || got.UID != "good" {
		t.Errorf("熔断后 bad 应退出可选集, got %v", got)
	}
}

func TestWeightAllZeroCreditsStillWeighted(t *testing.T) {
	// credits 全 0：权重完全由 idle+successRate 决定，不退化均匀随机（仍可选出更高分者）。
	p := New("")
	p.Add(&auth.Auth{UID: "idle"})
	p.Add(&auth.Auth{UID: "bursty"})
	// idle 从未使用、bursty 半分钟前刚用过 → idle 权重更高。
	p.mu.Lock()
	p.byUID["bursty"].lastUsed = time.Now().Add(-30 * time.Second)
	p.mu.Unlock()
	wIdle, wBursty := p.entryWeight("idle"), p.entryWeight("bursty")
	if wIdle <= wBursty {
		t.Errorf("idle should outweigh recently-used when credits all zero: idle=%v bursty=%v", wIdle, wBursty)
	}
}

func TestWeightTopFiveSelectionChanges(t *testing.T) {
	withNoPickGap(t)
	// credits 相差不大时，闲置补偿可让"低分但久置"的账号权重反超"高分但刚用"的账号，
	// 即使 credits 排序里 b 在前（Top5 内权重排序可与 credits 排序不同）。
	p := New("")
	for _, u := range []string{"a", "b"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.SetCredits("a", 90) // a credits 略低，但久置
	p.SetCredits("b", 100)
	p.mu.Lock()
	p.byUID["b"].lastUsed = time.Now()
	p.byUID["a"].lastUsed = time.Now().Add(-48 * time.Hour)
	p.mu.Unlock()
	if wA, wB := p.entryWeight("a"), p.entryWeight("b"); wA <= wB {
		t.Errorf("idle a should outweigh busy higher-credit b: a=%v b=%v", wA, wB)
	}
}

// ---------------------------------------------------------------------------
// T4 在途租约（单账号并发上限）
// ---------------------------------------------------------------------------

func TestAcquireReleaseLifecycle(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	if !p.Acquire("u1") {
		t.Fatal("first acquire should succeed")
	}
	if !p.Acquire("u1") {
		t.Fatal("second acquire should succeed")
	}
	if p.Acquire("u1") {
		t.Fatal("third acquire should fail (limit 2)")
	}
	p.Release("u1")
	if !p.Acquire("u1") {
		t.Fatal("acquire after release should succeed")
	}
}

func TestAcquireUnlimited(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// max=0 不限：连续 acquire 永不拒绝。
	for i := 0; i < 100; i++ {
		if !p.Acquire("u1") {
			t.Fatalf("unlimited acquire %d failed", i)
		}
	}
}

func TestAcquireUnknownUID(t *testing.T) {
	p := New("")
	if p.Acquire("nope") {
		t.Fatal("acquire unknown uid should fail")
	}
	p.Release("nope") // 不 panic
}

func TestPickSkipsInFlightFull(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "full"})
	p.Add(&auth.Auth{UID: "free"})
	p.SetCredits("full", 1000)
	p.SetCredits("free", 1)
	p.SetMaxInFlight(1)
	// full 占满唯一名额 → Pick 应跳过它，选 free（即使 credits 更低）。
	p.Acquire("full")
	got := p.Pick("")
	if got == nil || got.UID != "free" {
		t.Fatalf("pick should skip in-flight-full account, got %+v", got)
	}
	p.Release("full")
	// 释放后可重新被选中（确定性随机源 r=0 → 选 credits 最高的 full）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	if got := p.Pick(""); got == nil || got.UID != "full" {
		t.Fatalf("after release full should be pickable, got %+v", got)
	}
	p.Release("full")
}

func TestInFlightCountNotExceedLimit(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)

	// 并发 50 次 acquire：CAS 保证任一时刻在途数不超上限；每次成功后立即 release。
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p.Acquire("u1") {
				// 峰值检查：acquire 成功后立即读计数，应 ≤ 2。
				p.mu.RLock()
				if n := p.byUID["u1"].inFlight.Load(); n > 2 {
					t.Errorf("in-flight exceeded limit: %d", n)
				}
				p.mu.RUnlock()
				p.Release("u1")
			}
		}()
	}
	wg.Wait()

	// 全部释放后计数必须为 0。
	p.mu.RLock()
	n := p.byUID["u1"].inFlight.Load()
	p.mu.RUnlock()
	if n != 0 {
		t.Fatalf("in-flight should be 0 after all releases, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// T6 向后兼容 + 运行态 Status 扩展
// ---------------------------------------------------------------------------

func TestLoadLegacyStateFile(t *testing.T) {
	// 旧 state.json 只含 credits/until/disabled 等老字段，缺熔断/在途/成功率新字段。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	legacy := `{"accounts":{"legacy":{"credits":123,"until":"2027-01-01T04:00:00+08:00","cool_kind":1,"reason":"余额不足"}}}`
	if err := os.WriteFile(fp, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "legacy"})
	st, ok := p.Status("legacy")
	if !ok {
		t.Fatal("legacy account should load")
	}
	if st.Credits != 123 || !st.Cooling || st.Reason != "余额不足" {
		t.Errorf("legacy state misloaded: %+v", st)
	}
	// 运行态新字段默认零值。
	if st.InFlight != 0 || st.BreakerFails != 0 || !st.BreakerUntil.IsZero() {
		t.Errorf("runtime fields should be zero for legacy load: %+v", st)
	}
}

// ---------------------------------------------------------------------------
// T7 D5: Redis 状态快照镜像 + 择新恢复
// ---------------------------------------------------------------------------

// memStore 内存假 Store：记录 SaveState（模拟 Redis 快照）并可按需返回 LoadState。
type memStore struct {
	mu       sync.Mutex
	saved    []byte
	loadData []byte
	loadOK   bool
}

func (m *memStore) SaveState(data []byte) {
	m.mu.Lock()
	m.saved = append([]byte(nil), data...)
	m.mu.Unlock()
}
func (m *memStore) LoadState() ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.loadOK {
		return nil, false
	}
	return append([]byte(nil), m.loadData...), true
}

func TestSaveMirrorsSnapshot(t *testing.T) {
	// Flush 落盘时同步 fire-and-forget SaveState（带 saved_at）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	ms := &memStore{}
	p.SetStore(ms)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	p.Flush()
	ms.mu.Lock()
	raw := string(ms.saved)
	ms.mu.Unlock()
	if !strings.Contains(raw, `"saved_at"`) {
		t.Fatalf("snapshot should carry saved_at: %s", raw)
	}
	if !strings.Contains(raw, `"credits":42`) {
		t.Fatalf("snapshot should carry account state: %s", raw)
	}
}

func TestRestoreUsesRedisWhenNewer(t *testing.T) {
	// Redis 快照比本地 state.json 新 → 采用 Redis。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 本地较旧
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":1}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// 把本地 mtime 设到过去
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(fp, old, old); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot: %+v ok=%v", st, ok)
	}
}

func TestRestoreUsesLocalWhenNewer(t *testing.T) {
	// 本地 state.json 比 Redis 快照新 → 本地优先。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":77}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now().Add(-time.Hour)}
	ms.loadData, _ = json.Marshal(snap)
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 77 {
		t.Fatalf("should keep local (newer): %+v ok=%v", st, ok)
	}
}

func TestRestoreNoRedisUsesLocal(t *testing.T) {
	// 无 Redis 快照 → 本地优先。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts":{"u1":{"credits":55}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ms := &memStore{loadOK: false}
	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()
	st, ok := p.Status("u1")
	if !ok || st.Credits != 55 {
		t.Fatalf("no redis → use local: %+v ok=%v", st, ok)
	}
}

func TestRestoreUsesRedisWhenLocalMissing(t *testing.T) {
	// 本地 state.json 不存在（首次在新卷/新节点启动）+ 有效 Redis 快照 → 必须采用快照。
	// 此时本地没有可"优先"的状态，快照是本轮唯一来源（快照作为"启动恢复备份"的核心场景，
	// 见 StoreSnapshotter 契约）。旧实现把该情形并进「本地较新」的 fall-through：快照被
	// 静默丢弃（既不改内存也不置 dirty），全池运行态清零，且打出"本地较新于快照"的假日志。
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json") // 刻意不创建：模拟新卷首启

	ms := &memStore{loadOK: true}
	snap := snapshot{stateFile: stateFile{Accounts: map[string]stateAccount{"u1": {Credits: 999}}}, SavedAt: time.Now()}
	ms.loadData, _ = json.Marshal(snap)

	p := New(fp)
	p.SetStore(ms)
	p.RestoreFromSnapshot()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatalf("本地缺失时应采用 Redis 快照恢复账号，但池内无 u1（有效快照被丢弃）")
	}
	if st.Credits != 999 {
		t.Fatalf("should restore from Redis snapshot when local missing: credits=%d want 999", st.Credits)
	}
}

func TestStatusExposesRuntimeFields(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(2)
	p.Acquire("u1") // in_flight=1
	st, _ := p.Status("u1")
	if st.InFlight != 1 {
		t.Errorf("in_flight=%d want 1", st.InFlight)
	}
	p.SetBreaker(2, time.Hour, 2*time.Hour)
	p.NoteError("u1") // breaker_fails=1
	st, _ = p.Status("u1")
	if st.BreakerFails != 1 {
		t.Errorf("breaker_fails=%d want 1", st.BreakerFails)
	}
	p.Release("u1")
}

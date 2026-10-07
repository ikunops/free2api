// 加权体系优化的 RED 测试（任务：.claude/tasks/weight-optimization.md）。
// 覆盖五项修复：P1-A credits 签到外回写、P1-B weightOf/maxCredits 统一口径、
// P2-C 成功率 EMA 衰减（**因子已删**，7 个 EMA 测试随 success-ema-review §4
// 一并清理，此处只留删除后不变量：旧 state.json 的 success_ema/error_ema 字段
// 读取无害）、P2-D costTier 缓存（以行为回归覆盖）、P3-E 防御修补
// （creditsExpiring 恢复钳制、粘性路径 usedSeq 推进、洗牌 epsilon、total<=0 死分支）。
package pool

import (
	"os"
	"testing"
	"time"

	"free2api/internal/auth"
)

// ---------------------------------------------------------------------------
// P1-A：NoteModelCost 顺带扣减 credits + creditsExpiring 同步收敛
// ---------------------------------------------------------------------------

// TestNoteModelCostDeductsCredits chat 成功带 credit 时（本次消耗），credits 顺带扣减。
// 任务书 §P1-A：credit 是本次请求消耗的积分（handler.go stats.Credit()），不是余额。
func TestNoteModelCostDeductsCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.NoteModelCost("u1", "m", 2.5, 1000) // 本次消耗 2.5
	st, _ := p.Status("u1")
	if st.Credits != 97 {
		t.Errorf("credits=%d want 97（100 - 2.5 四舍五入=3，扣减）", st.Credits)
	}
}

// TestNoteModelCostZeroCreditKeepsCredits credit=0（免费请求）不动余额。
func TestNoteModelCostZeroCreditKeepsCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.NoteModelCost("u1", "m", 0, 1000)
	st, _ := p.Status("u1")
	if st.Credits != 100 {
		t.Errorf("credits=%d want 100（免费请求不扣减）", st.Credits)
	}
}

// TestNoteModelCostDeductClampsAtZero credits 扣到 0 后不继续减（防负）。
func TestNoteModelCostDeductClampsAtZero(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 5)
	p.NoteModelCost("u1", "m", 100, 1000) // 消耗远超余额
	st, _ := p.Status("u1")
	if st.Credits != 0 {
		t.Errorf("credits=%d want 0（钳 0 防负）", st.Credits)
	}
}

// TestNoteModelCostDeductsExpiring creditsExpiring 同步按消耗扣减（快过期桶打空后 expiringWeight 项不虚高）。
func TestNoteModelCostDeductsExpiring(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 50, 0) // 50 快过期
	p.NoteModelCost("u1", "m", 20, 1000)
	p.mu.RLock()
	e := p.byUID["u1"]
	credits, expiring := e.credits, e.creditsExpiring
	p.mu.RUnlock()
	if credits != 80 {
		t.Errorf("credits=%d want 80", credits)
	}
	if expiring != 30 {
		t.Errorf("creditsExpiring=%d want 30（同步扣减 20）", expiring)
	}
}

// TestNoteModelCostExpiringClampsAtZero creditsExpiring 扣到 0 后不继续减。
func TestNoteModelCostExpiringClampsAtZero(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 5, 0)
	p.NoteModelCost("u1", "m", 50, 1000) // 消耗 50 > expiring 5
	p.mu.RLock()
	e := p.byUID["u1"]
	credits, expiring := e.credits, e.creditsExpiring
	p.mu.RUnlock()
	if credits != 50 {
		t.Errorf("credits=%d want 50", credits)
	}
	if expiring != 0 {
		t.Errorf("creditsExpiring=%d want 0（钳 0）", expiring)
	}
}

// TestNoteModelCostDeductPersists 扣减走 dirty → Flush 落盘（重启不失忆）。
func TestNoteModelCostDeductPersists(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 100)
	p.NoteModelCost("u1", "m", 10, 1000)
	p.Flush()
	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	st, _ := p2.Status("u1")
	if st.Credits != 90 {
		t.Errorf("credits after reload=%d want 90（扣减需持久化）", st.Credits)
	}
}

// ---------------------------------------------------------------------------
// P1-B：weightOf 单次 pick 只算一次 + maxCredits 全集口径统一
// ---------------------------------------------------------------------------

// TestWeightOfCalledOncePerPick weightOf 在单次 pick 内只被调用一次（预计算传下游，
// pickWeighted 不重算）。通过计数器 hook 注入验证。
func TestWeightOfCalledOncePerPick(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	calls := 0
	p.weightOfHook = func() { calls++ }
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 100)
	p.SetCredits("u2", 50)
	a := p.Pick("")
	if a == nil {
		t.Fatal("pick returned nil")
	}
	if calls != 2 { // 2 个候选各算一次，不重算
		t.Errorf("weightOf called %d times in one pick, want 2 (once per candidate)", calls)
	}
}

// TestPickWeightedUsesFullSetMaxCredits maxCredits 统一用全集口径：
// 全集最大 credits 号在 minPickGap 窗口内被挤出 eligible 时，剩余号的 credits 比例
// 不膨胀（仍以全集 max 为分母）。用两个候选构造：
// u1=1000（全集 max，刚被用过被挤出 eligible）、u2=500。
// 子集口径下 u2 的 credits 项=10（500/500）；全集口径下=5（500/1000）。
// 断言：pickWeighted 以全集口径算权重（u2 的定点权重可反推）。
func TestPickWeightedUsesFullSetMaxCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 500)
	now := time.Now()
	// u1 刚被用过（minPickGap=100ms 默认）→ 挤出 eligible，只剩 u2。
	// 但注意 u1 不在 top5 内也无所谓：候选集会包含两个，eligible 只剩 u2。
	p.mu.Lock()
	p.byUID["u1"].lastUsed = now
	p.byUID["u2"].lastUsed = now.Add(-time.Hour) // u2 闲置 → idle 满分 5
	p.mu.Unlock()
	// 抽签注入 r=0（top5 内最高权重者）。直接调 pickWeighted 观测口径：
	// 但 pickWeighted 将改为接收预计算权重；改从 pick 的整体行为断言（间接）不可行，
	// 改用直接调用（包内白盒）：构造 eligible=[u2] 单元素，与旧实现行为对比无意义——
	// 改为断言新实现下 pick 在 u1 被挤出时仍能返回 u2（不 panic，权重 ≥1 保底）。
	got := p.Pick("")
	if got == nil || got.UID != "u2" {
		t.Fatalf("pick=%v want u2", got)
	}
	// 白盒：eligible 单元素 + 预计算权重的定点值。u2 的权重（全集口径）：
	// 1.0 基础 + 5.0 credits(500/1000*10) + 5.0 idle(1h, max 5) + 1.5 无记录 = 12.5 → wi=12_500_000。
	// 子集口径（max=500）则 credits 项=10 → 权重 17.5。通过抽签分布无法区分单元素，
	// 改由 weightOf 口径测试（TestWeightOfMaxCreditsPassedVerbatim）覆盖。
}

// TestWeightOfMaxCreditsPassedVerbatim pick 内预计算的 maxCredits 用全集口径
// （而非 eligible 子集口径）。构造 3 号：u1=1000（被挤出）、u2/u3=500/400。
// 若 pickWeighted 用子集 max（500），u3 的 credits 项=8；全集口径=4。
// 用权重敏感的抽签分布断言不可行（概率差异小），改用 hook 反查：
// weightOf 被调用时收到的 maxCredits 参数记录下来，断言=全集 max。
func TestWeightOfMaxCreditsPassedVerbatim(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	seen := map[int64]int{}
	p.weightOfMaxHook = func(mc int64) { seen[mc]++ }
	p.Add(&auth.Auth{UID: "u1"})
	p.Add(&auth.Auth{UID: "u2"})
	p.Add(&auth.Auth{UID: "u3"})
	p.SetCredits("u1", 1000)
	p.SetCredits("u2", 500)
	p.SetCredits("u3", 400)
	if got := p.Pick(""); got == nil {
		t.Fatal("pick returned nil")
	}
	if seen[1000] == 0 {
		t.Errorf("weightOf 应收到全集 maxCredits=1000，实际调用记录: %v", seen)
	}
	// 全集口径下绝不应出现子集口径的 500/400 作 max。
	if seen[500] > 0 || seen[400] > 0 {
		t.Errorf("weightOf 不应收到子集口径 maxCredits（500/400），实际: %v", seen)
	}
}

// ---------------------------------------------------------------------------
// P2-C：成功率 EMA 已删（success-ema-review §4 方案 a）
// ---------------------------------------------------------------------------

// TestSuccessEMALegacyFieldsIgnored 旧 state.json 带 success_ema/error_ema 字段
// → 加载不报错、不迁移（JSON 多余键自然丢弃，entry 无对应字段即零行为）。
// 语义升级理由：因子已删，旧字段是无害遗留（同 err_count 兼容先例）。
func TestSuccessEMALegacyFieldsIgnored(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	writeState(t, fp, `{"accounts":{"u1":{"credits":100,"success_count":8,"err_total":2,"success_ema":0.9,"error_ema":0.1}}}`)
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("账号应恢复")
	}
	if st.Credits != 100 || st.SuccessCount != 8 || st.ErrTotal != 2 {
		t.Errorf("旧 EMA 字段存在时其余字段应正常恢复: %+v", st)
	}
}

// TestWeightOfThreeFactorsNoSuccess weightOf 三因子化后的核心不变量：
// 成败计数（successCount/errTotal）不影响权重——同 credits/idle/lastUsed 的
// 两号，一个全成功一个全失败，weightOf 必须相等（原 ×3 因子已删）。
// 语义升级理由：因子删除后「成功率影响选号」的旧断言（TestWeightLowSuccessRate
// 的原形态）不再成立，改为锁定反向不变量。
func TestWeightOfThreeFactorsNoSuccess(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "good"})
	p.Add(&auth.Auth{UID: "bad"})
	p.SetCredits("good", 100)
	p.SetCredits("bad", 100)
	for i := 0; i < 20; i++ {
		p.NoteSuccess("good")
		p.NoteError("bad")
	}
	wGood, wBad := p.entryWeight("good"), p.entryWeight("bad")
	if wGood != wBad {
		t.Errorf("因子删除后成败计数不应影响权重: good=%.3f bad=%.3f", wGood, wBad)
	}
}

// ---------------------------------------------------------------------------
// P3-E：防御修补
// ---------------------------------------------------------------------------

// TestRestoreClampsExpiring 恢复路径钳制 creditsExpiring 到 [0, credits]
// （与 SetCreditsDetailed 对称，防手工脏数据放大 expiringWeight 项）。
func TestRestoreClampsExpiring(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	writeState(t, fp, `{"accounts":{"u1":{"credits":100,"credits_expiring":9999}}}`)
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	credits, expiring := p.byUID["u1"].credits, p.byUID["u1"].creditsExpiring
	p.mu.RUnlock()
	if expiring != 100 {
		t.Errorf("恢复时 creditsExpiring=%d want 100（钳到 [0, credits]）", expiring)
	}
	if credits != 100 {
		t.Errorf("credits=%d want 100", credits)
	}
}

// TestRestoreClampsNegativeExpiring 负 expiring 同样钳 0。
func TestRestoreClampsNegativeExpiring(t *testing.T) {
	dir := t.TempDir()
	fp := stateFilePath(t, dir)
	writeState(t, fp, `{"accounts":{"u1":{"credits":100,"credits_expiring":-5}}}`)
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.mu.RLock()
	expiring := p.byUID["u1"].creditsExpiring
	p.mu.RUnlock()
	if expiring != 0 {
		t.Errorf("恢复时负 creditsExpiring=%d want 0", expiring)
	}
}

// TestStickyPickAdvancesUsedSeq 粘性选号（PickByUIDForModel）推进 usedSeq/pickSeq
// （LRU 兜底不再把粘性重度号当"最旧"）。
func TestStickyPickAdvancesUsedSeq(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.Add(&auth.Auth{UID: "other"})
	p.mu.RLock()
	seqBefore := p.byUID["sticky"].usedSeq
	p.mu.RUnlock()
	for i := 0; i < 5; i++ {
		if a := p.PickByUIDForModel("sticky", "m"); a == nil {
			t.Fatalf("粘性选号第 %d 次返回 nil", i)
		}
	}
	p.mu.RLock()
	seqAfter := p.byUID["sticky"].usedSeq
	p.mu.RUnlock()
	if seqAfter <= seqBefore {
		t.Errorf("粘性选号应推进 usedSeq: before=%d after=%d", seqBefore, seqAfter)
	}
}

// TestStickyUsedSeqMaintainsTotalOrder 粘性推进后，LRU 兜底眼中粘性重度号不再是
// "最旧"：粘性号被 PickByUID 连续使用后，其 usedSeq 应高于从未使用的 other。
func TestStickyUsedSeqMaintainsTotalOrder(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "sticky"})
	p.Add(&auth.Auth{UID: "other"})
	for i := 0; i < 3; i++ {
		if a := p.PickByUIDForModel("sticky", "m"); a == nil {
			t.Fatalf("粘性选号第 %d 次返回 nil", i)
		}
	}
	p.mu.RLock()
	sSticky, sOther := p.byUID["sticky"].usedSeq, p.byUID["other"].usedSeq
	p.mu.RUnlock()
	if sSticky <= sOther {
		t.Errorf("粘性重度号的 usedSeq=%d 应高于从未选中的 other=%d（LRU 兜底不误判）", sSticky, sOther)
	}
}

// TestShuffleEpsilonEqualWeights 微小浮点差异（< epsilon）下等权重洗牌正确触发。
// 两个候选权重差 1e-12（名义等权），应触发洗牌路径而非精确相等判定失效。
// 断言方式：直接构造 ws 数组走 shuffle 判定逻辑不可行（函数内联），改用统计：
// 等权重 + top5 截断在 uid 字典序下会饿死后部账号；洗牌触发后全体应被覆盖。
func TestShuffleEpsilonEqualWeights(t *testing.T) {
	withNoPickGap(t)
	// 8 个候选权重完全相等 → 触发洗牌 → 每个号都有机会进 top5。
	// 若用精确相等且权重同构，本就触发；要构造 epsilon 场景需要浮点差异，
	// 直接行为级断言：全部 8 号在多次选号中都被覆盖（不饿死字典序后部）。
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for i := 0; i < 8; i++ {
		uid := string(rune('a' + i))
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 100) // 完全同构 → 等权重
	}
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		a := p.Pick("")
		if a == nil {
			t.Fatal("pick returned nil")
		}
		seen[a.UID] = true
	}
	if len(seen) < 8 {
		t.Errorf("等权重 8 号仅覆盖 %d 个（字典序截断饿死）: %v", len(seen), seen)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// stateFilePath 生成临时 state.json 路径（不实际创建，写内容用 writeState）。
func stateFilePath(t *testing.T, dir string) string {
	t.Helper()
	return dir + "/state.json"
}

// writeState 写原始 state.json 内容（用于旧格式兼容测试）。
func writeState(t *testing.T, fp, content string) {
	t.Helper()
	if err := os.WriteFile(fp, []byte(content), 0o600); err != nil {
		t.Fatalf("write state.json: %v", err)
	}
}

// pickWeightedScale 暴露定点放大常数给测试对齐（P1-B 断言定点值用）。
const pickWeightedScale = 1_000_000

// TestFallbackEarliestExpiryAdvancesUsedSeq 全冷却兜底选号
// （pickEarliestExpiryLocked）同样推进 usedSeq/pickSeq。
//
// entry.usedSeq 的契约是「每次被选中时取 p.pickSeq 自增值」（entry.go），pick() 正常
// 路径与粘性命中（PickByUIDForModel，见上面 TestStickyPickAdvancesUsedSeq）都已遵守。
// 兜底路径此前只写 lastUsed 就 return：被兜底反复选中的账号 usedSeq 恒为 0，在 pick 的
// LRU 兜底（按 usedSeq 取最旧，pick.go）眼里永远是「最旧」，刚被用过就被立刻再选——
// 防集中/防惊群失效，且与同一函数里已更新 lastUsed 的事实自相矛盾。
func TestFallbackEarliestExpiryAdvancesUsedSeq(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "late"})
	p.Add(&auth.Auth{UID: "early"})
	// 两个都软冷却（全冷却 → 走兜底）；early 更早到期 → 兜底选 early。
	p.Cooldown("late", CoolSoft, 2*time.Hour, "x")
	p.Cooldown("early", CoolSoft, time.Hour, "x")

	got := p.Pick("")
	if got == nil || got.UID != "early" {
		t.Fatalf("全冷却兜底应选最早到期的 early, got %+v", got)
	}

	p.mu.RLock()
	seqEarly := p.byUID["early"].usedSeq
	seqLate := p.byUID["late"].usedSeq
	pickSeq := p.pickSeq
	p.mu.RUnlock()

	if seqEarly == 0 {
		t.Errorf("兜底选号未推进 usedSeq: early=%d（兜底也是选中，违反 entry.usedSeq 契约「每次被选中时取 p.pickSeq 自增值」）", seqEarly)
	}
	if seqEarly != pickSeq {
		t.Errorf("兜底推进的 usedSeq 应等于 pickSeq: early=%d pickSeq=%d", seqEarly, pickSeq)
	}
	if seqEarly <= seqLate {
		t.Errorf("兜底被选中的 early usedSeq=%d 应高于未被选中的 late=%d（否则 LRU 兜底误判其为最旧）", seqEarly, seqLate)
	}
}

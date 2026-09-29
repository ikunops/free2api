// cost_explore_test.go costTier 条件探索（issue #136 方案 a′）测试：
// T1 分布收敛 / T2 毕业闭环 / T3 窗口限幅 / T4 触发边界 / T5 并发单发 /
// T6 重启语义 / T7 可观测 / T8 键隔离。语义契约七条见设计报告 §2.1。
package pool

import (
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

// exploreKey 构造探索 timer 的键（realm + "\x1f" + model，语义契约第 3 条）。
func exploreKey(realm, model string) string { return realm + "\x1f" + model }

// passExploreWindow 手工把该键的上次探索时刻回拨过窗（模拟「窗口已过」）。
func passExploreWindow(p *Pool, realm, model string) {
	p.mu.Lock()
	p.exploreLast[exploreKey(realm, model)] = time.Now().Add(-time.Hour)
	p.mu.Unlock()
}

// exploreEvents 读累计探索事件数（包内私有访问）。
func exploreEvents(p *Pool) int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.costExploreEvents
}

// exploreLastLen 读 exploreLast 条目数。
func exploreLastLen(p *Pool) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.exploreLast)
}

// T1（目标① 破垄断）：1 tier0 + 2 tier1（全部实际免费）、interval 极小
// （每个窗口都探索）→ 逐号毕业，收敛后三号都被选中且 top 占比 < 70%
// （排除 100% 垄断，3 号近均匀时期望 ~33%）。
func TestCostExploreSpreadsTrafficAcrossFreeAccounts(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Nanosecond) // 窗口极小：tier 1 存在即探索
	p.Add(&auth.Auth{UID: "free1"})
	p.Add(&auth.Auth{UID: "n1"})
	p.Add(&auth.Auth{UID: "n2"})
	p.NoteModelCost("free1", "m", 0, 1000) // tier 0 垄断层

	counts := map[string]int{}
	for i := 0; i < 300; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatalf("第 %d 次 pick 返回 nil", i)
		}
		// 每次成功都写观测（credit=0 免费）：探索→NoteModelCost(0)→毕业自然发生。
		p.NoteModelCost(a.UID, "m", 0, 1000)
		counts[a.UID]++
	}
	for _, uid := range []string{"free1", "n1", "n2"} {
		if counts[uid] == 0 {
			t.Errorf("%s 从未被选中（垄断未破）: %v", uid, counts)
		}
	}
	top := counts["free1"]
	for _, uid := range []string{"n1", "n2"} {
		if c := counts[uid]; c > top {
			top = c
		}
	}
	if top >= 210 { // 300 × 70%
		t.Errorf("最大单号占比 %d/300 ≥ 70%%（垄断残留）: %v", top, counts)
	}
}

// T2（目标② 解冻结/毕业）：窗口过后首个 pick 改道给未知号（探索）；免费毕业
// （NoteModelCost 0）后两号同层均可选；收费毕业（变体）则未知号出局、垄断号恢复
// 100%（学习结果被尊重）。
func TestCostExploreProbesAndGraduates(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour) // 只探一次（后续 elapsed < 1h）
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000) // tier 0

	// 首个 pick：零值 timer = 从未探索 → 首次即探，tier 1-only 层只有 unknown。
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("首个 pick=%v want unknown（探索改道给未知号）", a)
	}
	if got := exploreEvents(p); got != 1 {
		t.Fatalf("探索事件数=%d want 1", got)
	}

	// 免费毕业：首观测写入即改变层归属（无延迟），后续 pick 两号均可选。
	p.NoteModelCost("unknown", "m", 0, 1000)
	counts := map[string]int{}
	for i := 0; i < 50; i++ {
		a := p.PickExcludingForRealm(nil, "m", "")
		if a == nil {
			t.Fatalf("第 %d 次 pick 返回 nil", i)
		}
		counts[a.UID]++
	}
	if counts["free"] == 0 || counts["unknown"] == 0 {
		t.Errorf("毕业后应两号均可选: %v", counts)
	}
	if got := exploreEvents(p); got != 1 {
		t.Errorf("毕业后探索应停止（tier 1 枯竭）: events=%d want 1", got)
	}
}

// TestCostExploreGraduatesToPaid T2 变体：探索发现未知号实际收费 → 毕业 tier 2
// 出局，垄断号恢复 100%——学费只付一次，失败即毕业。
func TestCostExploreGraduatesToPaid(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "paid-later"})
	p.NoteModelCost("free", "m", 0, 1000)

	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "paid-later" {
		t.Fatalf("首个 pick=%v want paid-later（探索改道）", a)
	}
	p.NoteModelCost("paid-later", "m", 2.9, 1000) // 收费毕业 → tier 2

	for i := 0; i < 50; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（收费毕业号出局）", i, a)
		}
	}
	if got := exploreEvents(p); got != 1 {
		t.Errorf("收费毕业后探索应停止: events=%d want 1", got)
	}
}

// T3（目标③ 成本保留）：同窗口内探索 ≤1 次（48 次/天/模型上界的微观形态），
// 窗口过后恰好再探 1 次；其余请求全部 tier 0（免费号仍占多数的确定性断言）。
func TestCostExploreBoundedPerWindow(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	// 首次 pick 探索（timer 写入）→ 同窗口内后续 50 次全部回到 tier 0。
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("首个 pick=%v want unknown（首次探索）", a)
	}
	for i := 0; i < 50; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("窗口内第 %d 次选中 %v，want free（限幅：窗口内探索 ≤1 次）", i, a)
		}
	}

	// 窗口过后 → 恰好再探 1 次，然后又回到 tier 0。
	passExploreWindow(p, "", "m")
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("过窗后首个 pick=%v want unknown（再探一次）", a)
	}
	for i := 0; i < 50; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("再探后第 %d 次选中 %v，want free", i, a)
		}
	}
	if got := exploreEvents(p); got != 2 {
		t.Errorf("探索事件数=%d want 2（每窗口至多 1 次）", got)
	}
}

// TestCostExploreOff T3 变体：interval=0 完全关停 → 50/50 全 tier 0（现状行为
// 回归锚，免费号优先语义不变）。
func TestCostExploreOff(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(0) // 0 = 关停
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	for i := 0; i < 50; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("第 %d 次选中 %v，want free（关停时现状行为：免费号 100%%）", i, a)
		}
	}
	if got := exploreEvents(p); got != 0 {
		t.Errorf("关停时不应有探索事件: events=%d", got)
	}
	if n := exploreLastLen(p); n != 0 {
		t.Errorf("关停时不应写 timer: exploreLast len=%d", n)
	}
}

// T4（触发边界）：探索只在「冻结存在」时发生——无 tier 0（bestTier=1，本就
// 无垄断）不触发；只有 tier 0 无 tier 1（无冻结）不触发；均不写 timer 不计数。
func TestCostExploreRequiresTier0AndTier1(t *testing.T) {
	withNoPickGap(t)
	// 无 tier 0：全未知 → bestTier=1，tier 1 本来就在最优层，无需探索。
	p1 := New("")
	p1.SetCostExploreInterval(time.Hour)
	p1.Add(&auth.Auth{UID: "u1"})
	p1.Add(&auth.Auth{UID: "u2"})
	for i := 0; i < 10; i++ {
		p1.PickExcludingForRealm(nil, "m", "")
	}
	if got := exploreEvents(p1); got != 0 {
		t.Errorf("无 tier 0 时不应探索: events=%d", got)
	}

	// 只有 tier 0：无 tier 1 → 无冻结，不探索。
	p2 := New("")
	p2.SetCostExploreInterval(time.Hour)
	p2.Add(&auth.Auth{UID: "u1"})
	p2.Add(&auth.Auth{UID: "u2"})
	p2.NoteModelCost("u1", "m", 0, 1000)
	p2.NoteModelCost("u2", "m", 0, 1000)
	for i := 0; i < 10; i++ {
		p2.PickExcludingForRealm(nil, "m", "")
	}
	if got := exploreEvents(p2); got != 0 {
		t.Errorf("无 tier 1 时不应探索: events=%d", got)
	}
	if n := exploreLastLen(p2); n != 0 {
		t.Errorf("无冻结时不应写 timer: exploreLast len=%d", n)
	}
}

// TestCostExploreRequiresModel T4 边界：reqModel=""（Pick 老语义）不触发探索
// （语义契约第 1 条）。
func TestCostExploreRequiresModel(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	for i := 0; i < 10; i++ {
		if a := p.Pick(""); a == nil {
			t.Fatalf("第 %d 次 Pick(\"\") 返回 nil", i)
		}
	}
	if got := exploreEvents(p); got != 0 {
		t.Errorf("reqModel 空时不应探索: events=%d", got)
	}
	if n := exploreLastLen(p); n != 0 {
		t.Errorf("reqModel 空时不应写 timer: exploreLast len=%d", n)
	}
}

// TestCostExploreSkipsWhenTier1Unavailable T4 边界：tier 1 全在冷却/在途占满
// → 不在候选内 → 不触发探索（探测号必是当前可用号，防探测风暴）。
func TestCostExploreSkipsWhenTier1Unavailable(t *testing.T) {
	// 冷却中的 tier 1 不算候选。
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)
	p.Cooldown("unknown", CoolSoft, time.Minute, "429 rate limit")
	for i := 0; i < 5; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("冷却变体第 %d 次选中 %v，want free", i, a)
		}
	}
	if got := exploreEvents(p); got != 0 {
		t.Errorf("tier 1 全在冷却时不应探索: events=%d", got)
	}

	// 在途占满的 tier 1 同样不算候选。
	p2 := New("")
	p2.SetCostExploreInterval(time.Hour)
	p2.SetMaxInFlight(1)
	p2.Add(&auth.Auth{UID: "free"})
	p2.Add(&auth.Auth{UID: "unknown"})
	p2.NoteModelCost("free", "m", 0, 1000)
	if !p2.Acquire("unknown") {
		t.Fatal("Acquire(unknown) 失败（上限 1 应可占 1 个名额）")
	}
	for i := 0; i < 5; i++ {
		if a := p2.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("在途满变体第 %d 次选中 %v，want free", i, a)
		}
	}
	if got := exploreEvents(p2); got != 0 {
		t.Errorf("tier 1 在途占满时不应探索: events=%d", got)
	}
}

// TestCostExploreFailureNoStorm T4：探索请求失败 → 既有错误策略（冷却）照常
// 作用于探测号；下一轮 pick timer 已消耗 → 回 tier 0 层，无探测风暴。
func TestCostExploreFailureNoStorm(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("首个 pick=%v want unknown（探索改道）", a)
	}
	// 探测失败走既有错误策略：软冷却出池。
	p.Cooldown("unknown", CoolSoft, time.Minute, "429 rate limit")

	for i := 0; i < 10; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "free" {
			t.Fatalf("探测失败后第 %d 次选中 %v，want free（回 tier 0 层）", i, a)
		}
	}
	if got := exploreEvents(p); got != 1 {
		t.Errorf("探测失败后不应再探（无风暴）: events=%d want 1", got)
	}
}

// T5（并发单发）：-race 下并发 pick 同模型（已过窗）→ 恰好 1 个探索
// （pick 全程写锁，窗口判定天然防并发重复探索）。
func TestCostExploreConcurrentSingleFire(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	const n = 20
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if a := p.PickExcludingForRealm(nil, "m", ""); a == nil {
				t.Error("并发 pick 返回 nil")
			}
		}()
	}
	wg.Wait()
	if got := exploreEvents(p); got != 1 {
		t.Errorf("并发探索事件数=%d want 1（写锁内窗口判定防重复）", got)
	}
}

// T6（重启语义）：探索不持久化（同 lastUsed/usedSeq 运行态）→ 重启后
// exploreLast 归零；已毕业号经 ModelCosts 恢复 tier，不重付学费。
func TestCostExploreNotPersisted(t *testing.T) {
	dir := t.TempDir()
	fp := dir + "/state.json"
	p := New(fp)
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("首个 pick=%v want unknown（探索改道）", a)
	}
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "free"})
	p2.Add(&auth.Auth{UID: "unknown"})
	if n := exploreLastLen(p2); n != 0 {
		t.Errorf("重启后 exploreLast 应归零（timer 不持久化）: len=%d", n)
	}
	// 毕业知识保留：free 的免费观测经 state.json 恢复。
	if mc, ok := modelCostSnapshot(p2, "free", "m"); !ok || mc.CostPer1k != 0 {
		t.Errorf("ModelCosts 未恢复（学费重付）: ok=%v mc=%+v", ok, mc)
	}
}

// T7（可观测性，pool 方法级）：CostExploreStatus 透出累计事件数与
// per-model 最近探索时刻（键内分隔符输出为 "|"）。
func TestCostExploreStatusExposed(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	p.Add(&auth.Auth{UID: "free"})
	p.Add(&auth.Auth{UID: "unknown"})
	p.NoteModelCost("free", "m", 0, 1000)

	events, last := p.CostExploreStatus()
	if events != 0 || len(last) != 0 {
		t.Fatalf("初始态 events=%d last=%v，want 0/空", events, last)
	}
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("首个 pick=%v want unknown（探索改道）", a)
	}
	events, last = p.CostExploreStatus()
	if events != 1 {
		t.Errorf("events=%d want 1", events)
	}
	ts, ok := last["|m"] // realm="" + model m → 键 "|m"（\x1f 输出为 "|"）
	if !ok {
		t.Fatalf("per_model 缺少键 |m: %v", last)
	}
	if ts.IsZero() {
		t.Error("per_model 时间戳为零值")
	}
}

// T8（键隔离）：跨 realm/跨模型独立 timer，探索节奏不跨域串扰；realm=""
// （Pick 老语义）单独成键。
func TestCostExploreKeyByRealmAndModel(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.SetCostExploreInterval(time.Hour)
	// CN 域：cn1 免费（tier 0）+ cn2 未知（tier 1）；global 域同构。
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "g2", Domain: "workbuddy.ai"})
	p.NoteModelCost("cn1", "m", 0, 1000)
	p.NoteModelCost("g1", "m", 0, 1000)

	// cn 域探索：只看 cn 候选（cn1 tier0 + cn2 tier1）。
	if a := p.PickExcludingForRealm(nil, "m", "cn"); a == nil || a.UID != "cn2" {
		t.Fatalf("cn 域首个 pick=%v want cn2（探索改道）", a)
	}
	// global 域同模型名：独立 timer，窗口不串扰 → 也探索。
	if a := p.PickExcludingForRealm(nil, "m", "global"); a == nil || a.UID != "g2" {
		t.Fatalf("global 域首个 pick=%v want g2（独立 timer，同模型名不串扰）", a)
	}
	// realm=""（Pick 老语义）单独成键 → 再次探索。
	if a := p.Pick("m"); a == nil {
		t.Fatal("Pick(m) 返回 nil")
	}

	if got := exploreEvents(p); got != 3 {
		t.Fatalf("探索事件数=%d want 3（cn/global/空域各一）", got)
	}
	p.mu.RLock()
	_, hasCN := p.exploreLast[exploreKey("cn", "m")]
	_, hasGlobal := p.exploreLast[exploreKey("global", "m")]
	_, hasEmpty := p.exploreLast[exploreKey("", "m")]
	p.mu.RUnlock()
	if !hasCN || !hasGlobal || !hasEmpty {
		t.Errorf("exploreLast 键不完整: cn=%v global=%v empty=%v", hasCN, hasGlobal, hasEmpty)
	}
}

package pool

import (
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestExpiringCreditBoostsWeight 快过期积分占比高的号权重大于占比低/无的号（同总量下）。
func TestExpiringCreditBoostsWeight(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "expiring-heavy"})          // 总量相同,快过期占比高
	p.Add(&auth.Auth{UID: "stable-heavy"})            // 总量相同,快过期占比低
	p.SetCreditsDetailed("expiring-heavy", 1000, 900) // 90% 快过期
	p.SetCreditsDetailed("stable-heavy", 1000, 50)    // 5% 快过期

	p.mu.RLock()
	eExp := p.byUID["expiring-heavy"]
	eSta := p.byUID["stable-heavy"]
	maxC := int64(1000)
	now := time.Now()
	wExp := p.weightOf(eExp, maxC, now)
	wSta := p.weightOf(eSta, maxC, now)
	p.mu.RUnlock()

	if wExp <= wSta {
		t.Errorf("expiring-heavy weight %.3f <= stable-heavy %.3f; 快过期积分应加权", wExp, wSta)
	}
	// 差值应约等于 (0.9-0.05)*expiringWeight = 0.85*30 = 25.5
	diff := wExp - wSta
	if diff < 25.0 || diff > 26.0 {
		t.Errorf("weight diff %.3f, want ~25.5 (0.85*expiringWeight)", diff)
	}
}

// TestFreeModelSkipsCreditWeighting 免费模型选号不按余额加权：两个号 credits 悬殊 10 倍，
// freeModel=true 时权重必须相等（只剩 idle 项），freeModel=false 时才拉开差距。
// 这是用户反馈「免费模型也按余额加权 → 流量全堆到高余额号、把它的上限白耗掉」的回归锚点。
func TestFreeModelSkipsCreditWeighting(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "rich"})
	p.Add(&auth.Auth{UID: "poor"})
	p.SetCreditsDetailed("rich", 3000, 0)
	p.SetCreditsDetailed("poor", 30, 0)
	now := time.Now()

	p.mu.RLock()
	eRich := p.byUID["rich"]
	ePoor := p.byUID["poor"]
	wFreeRich := p.weightOfFree(eRich, now)
	wFreePoor := p.weightOfFree(ePoor, now)
	wPaidRich := p.weightOf(eRich, 3000, now)
	wPaidPoor := p.weightOf(ePoor, 3000, now)
	p.mu.RUnlock()

	if wFreeRich != wFreePoor {
		t.Errorf("免费分支权重应相等（与余额无关）: rich=%.4f poor=%.4f", wFreeRich, wFreePoor)
	}
	if wPaidRich <= wPaidPoor {
		t.Errorf("收费分支仍应让高余额号权重更大: rich=%.4f poor=%.4f", wPaidRich, wPaidPoor)
	}
}

// TestExpiringHardLayer 快过期积分硬分层：只要本层有号带快过期积分，就只在带快过期的
// 号里选——即便另一个号 credits 高出一个数量级，也不得分走这一层。
// 实测反例（修复前）：A(100 积分、100 快过期) 权重 8.35 输给 B(2889 积分、0 快过期) 11.00，
// 快过期积分永远消耗不掉、静默作废。本测试用确定性随机源（恒取 0）锁死选号结果。
func TestExpiringHardLayer(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.SetRandomSource(func(n int64) int64 { return 0 }) // 恒取头名：验证「头名是谁」而非分布
	p.SetCostExploreInterval(0)                         // 关掉成本探索，避免未知层干扰
	p.Add(&auth.Auth{UID: "expiring"})
	p.Add(&auth.Auth{UID: "stable"})
	p.SetCreditsDetailed("expiring", 100, 100) // 积分少但全部快过期
	p.SetCreditsDetailed("stable", 5000, 0)    // 积分多但无快过期

	// 两个号都要有该模型观测，才不会被成本分层踢开（同层才轮得到硬分层比较）。
	p.NoteModelCost("expiring", "m", 1, 1000) // 收费观测
	p.NoteModelCost("stable", "m", 1, 1000)

	seen := map[string]int{}
	for i := 0; i < 20; i++ {
		if a := p.PickExcludingForRealm(nil, "m", ""); a != nil {
			seen[a.UID]++
		}
	}
	if seen["stable"] != 0 {
		t.Errorf("有号带快过期积分时不应选到无快过期的号: %v", seen)
	}
	if seen["expiring"] != 20 {
		t.Errorf("应 100%% 落在带快过期积分的号上: %v", seen)
	}
}

// TestExpiringClampedToCredits expiring 超过总量/负值时被钳制,不污染权重。
func TestExpiringClampedToCredits(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 100, 9999) // expiring > credits
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 100 {
		t.Errorf("creditsExpiring=%d want 100 (clamped to credits)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()

	p.SetCreditsDetailed("u1", 100, -5) // 负值
	p.mu.RLock()
	if p.byUID["u1"].creditsExpiring != 0 {
		t.Errorf("creditsExpiring=%d want 0 (negative clamped)", p.byUID["u1"].creditsExpiring)
	}
	p.mu.RUnlock()
}

// TestSetCreditsLeavesExpiringUnchanged 旧 SetCredits 只更新总量,不清 expiring(向后兼容)。
func TestSetCreditsLeavesExpiringUnchanged(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCreditsDetailed("u1", 500, 200)
	p.SetCredits("u1", 600) // 旧入口只更新总量
	p.mu.RLock()
	e := p.byUID["u1"]
	if e.credits != 600 {
		t.Errorf("credits=%d want 600", e.credits)
	}
	if e.creditsExpiring != 200 {
		t.Errorf("creditsExpiring=%d want 200 (SetCredits 不应清)", e.creditsExpiring)
	}
	p.mu.RUnlock()
}

// TestExpiringPreferredElsewhere 粘性绑定号无快过期积分、池中存在带快过期积分的可用号时，
// 报告「应让位」；绑定号自身带快过期、或候选都无快过期时不让位。
// 回归锚点：长期会话被粘性钉在无快过期号上，绕过硬分层导致快过期积分到期作废。
func TestExpiringPreferredElsewhere(t *testing.T) {
	p := New("")
	p.SetProducerOf(func(string) string { return "workbuddy" })
	p.Add(&auth.Auth{UID: "pinned"})
	p.Add(&auth.Auth{UID: "expiring"})
	p.SetCreditsDetailed("pinned", 5000, 0)    // 绑定号：积分多但无快过期
	p.SetCreditsDetailed("expiring", 100, 100) // 有快过期

	// pinned 无快过期，池中存在 expiring（带快过期）→ 应让位。
	if !p.ExpiringPreferredElsewhere("pinned", "m", "", "", false) {
		t.Fatalf("pinned(无快过期) 应让位给带快过期积分的号")
	}
	// 免费模型：不参与让位（免费调用不扣积分）。
	if p.ExpiringPreferredElsewhere("pinned", "m", "", "", true) {
		t.Fatalf("freeModel=true 时不应让位")
	}
	// 绑定号自身带快过期 → 不让位。
	if p.ExpiringPreferredElsewhere("expiring", "m", "", "", false) {
		t.Fatalf("绑定号自身带快过期积分时不应让位")
	}
	// 空 reqModel → 无法做模型级健康判定，保守不让位。
	if p.ExpiringPreferredElsewhere("pinned", "", "", "", false) {
		t.Fatalf("空模型名不应让位")
	}

	// 候选也无快过期 → 不让位。
	p.SetCreditsDetailed("expiring", 100, 0)
	if p.ExpiringPreferredElsewhere("pinned", "m", "", "", false) {
		t.Fatalf("候选都无快过期积分时不应让位")
	}
}

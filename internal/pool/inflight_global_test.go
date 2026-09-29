package pool

import (
	"testing"

	"free2api/internal/auth"
)

// TestInFlightGlobalTiering global 域在途上限分档（WAF 403 修复 P1-1）：
// maxInFlightGlobal=2 + maxInFlight=3 时，global 号第 3 次 Acquire 被拒、
// cn 号第 3 次仍成功（cn 档不受影响）。global 风控更紧压低其单号并发，
// cn 维持原上限零回归。
func TestInFlightGlobalTiering(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	// global 档 2：前两次成功、第三次被拒。
	if !p.Acquire("g1") || !p.Acquire("g1") {
		t.Fatal("global first two acquires should succeed (tier 2)")
	}
	if p.Acquire("g1") {
		t.Fatal("global third acquire should fail (tier 2 < max_in_flight 3)")
	}
	// cn 档 3（未分档回落 maxInFlight）：第三次仍成功。
	if !p.Acquire("cn1") || !p.Acquire("cn1") || !p.Acquire("cn1") {
		t.Fatal("cn three acquires should succeed (tier 3 unchanged)")
	}
	if p.Acquire("cn1") {
		t.Fatal("cn fourth acquire should fail (max_in_flight 3)")
	}
}

// TestInFlightGlobalTierUnsetFallsBack 分档未设置（0）时 global 号回落
// maxInFlight（不分档，既有部署零回归）；设置后生效。
func TestInFlightGlobalTierUnsetFallsBack(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetMaxInFlight(3)
	// 未设置 global 档：global 号按 3 放行（旧语义）。
	if !p.Acquire("g1") || !p.Acquire("g1") || !p.Acquire("g1") {
		t.Fatal("unset global tier should fall back to max_in_flight")
	}
	if p.Acquire("g1") {
		t.Fatal("unset global tier still respects max_in_flight cap")
	}
}

// TestAcquireUnknownUIDNoPanic 未知 uid 的 Acquire 安全返回 false：Acquire 在
// !ok 判空前曾先调 inFlightLimit（快照注释语义），maxInFlightGlobal>0 时对
// nil entry 解引用 e.a.Realm() 直接 panic——会话粘性路由对刚被 Remove 的账号
// （如 token 失效禁用）仍持 uid 调 Acquire 时即触发。
func TestAcquireUnknownUIDNoPanic(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	// 已入池账号正常放行（对照组，排除「号没进去」的误判）。
	if !p.Acquire("g1") {
		t.Fatal("known uid acquire should succeed")
	}
	p.Release("g1")
	// 未知 uid：不 panic，返回 false（global 分档启用 + uid 不在池内的组合）。
	if p.Acquire("no-such-uid") {
		t.Fatal("unknown uid acquire should return false")
	}
}

// TestPickSkipsGlobalTierFull 选号侧分档：global 号占满 2 档后 Pick 跳过
// （inFlightFull 走同一路径），不被 cn 档的 3 误放行。
func TestPickSkipsGlobalTierFull(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "gfull", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "gfree", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetCredits("gfull", 1000)
	p.SetCredits("gfree", 1)
	p.SetMaxInFlight(3)
	p.SetMaxInFlightGlobal(2)

	// gfull 占满 global 档 2（还差 cn 档 3 一个名额）→ Pick 必须跳过它选 gfree。
	if !p.Acquire("gfull") || !p.Acquire("gfull") {
		t.Fatal("warm up gfull to global tier limit")
	}
	got := p.Pick("")
	if got == nil || got.UID != "gfree" {
		t.Fatalf("pick should skip global-tier-full account, got %+v", got)
	}
	p.Release("gfull")
	p.Release("gfull")
}

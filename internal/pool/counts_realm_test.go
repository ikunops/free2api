package pool

import (
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestCountsDetailedForRealm 混合池（cn+global 账号，部分冷却/禁用）各域计数独立正确，
// 且全池总计数与分域之和一致（零回归锚点：CountsDetailed 口径不动）。
func TestCountsDetailedForRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	// cn 域：cn1 healthy、cn2 冷却
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "cn2", Domain: "www.codebuddy.cn", AccessToken: "at"})
	// global 域：g1 healthy、g2 disabled
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at"})

	p.Cooldown("cn2", CoolSoft, time.Hour, "429 rate limit")
	p.Disable("g2", "session dead")

	total, healthy, cooling, disabled, inFlightFull := p.CountsDetailedForRealm("cn")
	if total != 2 || healthy != 1 || cooling != 1 || disabled != 0 || inFlightFull != 0 {
		t.Errorf("CountsDetailedForRealm(cn) = (%d,%d,%d,%d,%d) want (2,1,1,0,0)",
			total, healthy, cooling, disabled, inFlightFull)
	}
	total, healthy, cooling, disabled, _ = p.CountsDetailedForRealm("global")
	if total != 2 || healthy != 1 || cooling != 0 || disabled != 1 {
		t.Errorf("CountsDetailedForRealm(global) = (%d,%d,%d,%d) want (2,1,0,1)",
			total, healthy, cooling, disabled)
	}
	// 全池汇总 = 分域之和（既让领域断言独立、又锁死 CountsDetailed 零回归）。
	gt, gh, gc, gd, _ := p.CountsDetailed()
	if gt != 4 || gh != 2 || gc != 1 || gd != 1 {
		t.Errorf("CountsDetailed() = (%d,%d,%d,%d) want (4,2,1,1)", gt, gh, gc, gd)
	}
}

// TestCountsDetailedForRealmInFlightFull 在途占满维度同样按域分桶：
// cn 占满不计入 global 的 in_flight_full。
func TestCountsDetailedForRealmInFlightFull(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.SetMaxInFlight(1)
	if !p.Acquire("cn1") {
		t.Fatal("acquire cn1")
	}
	defer p.Release("cn1")

	_, _, _, _, cnF := p.CountsDetailedForRealm("cn")
	if cnF != 1 {
		t.Errorf("CountsDetailedForRealm(cn).in_flight_full = %d want 1 (healthy 且占满)", cnF)
	}
	_, _, _, _, gF := p.CountsDetailedForRealm("global")
	if gF != 0 {
		t.Errorf("CountsDetailedForRealm(global).in_flight_full = %d want 0", gF)
	}
	// 全池口径零回归：healthy=2、in_flight_full=1（占满仍 healthy，状态机语义）。
	_, h, _, _, f := p.CountsDetailed()
	if h != 2 || f != 1 {
		t.Errorf("CountsDetailed() = healthy=%d in_flight_full=%d want 2/1", h, f)
	}
}

// TestServableForRealm 域可服务判定相互独立：global 全冷却时 cn 仍可服务，
// 且保留存在性语义（任一域可服务 → ServableNow()=true，/healthz 语义零回归）。
func TestServableForRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	// global 全部冷却（软冷却 + 余额硬冷却各一）。
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "g2", Domain: "www.workbuddy.ai", AccessToken: "at"})
	p.Cooldown("g1", CoolSoft, time.Hour, "429 rate limit")
	p.Cooldown("g2", CoolHard, time.Hour, "余额不足")

	if !p.ServableForRealm("cn") {
		t.Error("ServableForRealm(cn)=false want true (cn1 healthy)")
	}
	if p.ServableForRealm("global") {
		t.Error("ServableForRealm(global)=true want false (all global cooling)")
	}
	if !p.ServableNow() {
		t.Error("ServableNow()=false want true (cn servable keeps existence semantics)")
	}
}
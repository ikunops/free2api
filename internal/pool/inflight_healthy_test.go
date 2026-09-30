// inflight_healthy_test.go 锚定 HasHealthyInFlightFull：它区分「池里真没号」与
// 「有健康号只是被在途上限占满」两种选号失败成因，后者才值得排队等待。
package pool

import (
	"testing"

	"free2api/internal/auth"
)

func TestHasHealthyInFlightFullTrueWhenSlotBusy(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一名额

	if !p.HasHealthyInFlightFull(nil, "", "", "") {
		t.Fatal("healthy-but-slot-full account should report true (queueing is worthwhile)")
	}
	p.Release("u1")
	if p.HasHealthyInFlightFull(nil, "", "", "") {
		t.Fatal("after release the slot is free; pick can succeed, so wait helper is unnecessary")
	}
}

func TestHasHealthyInFlightFullFalseWhenNoHealthy(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.SetMaxInFlight(1)
	p.Cooldown("u1", CoolSoft, 0, "cooled")
	// 号在冷却：不是「只差名额」，排队无意义。
	if p.HasHealthyInFlightFull(nil, "", "", "") {
		t.Fatal("cooled account must not be reported as slot-waitable")
	}
}

func TestHasHealthyInFlightFullRespectsProducerAndRealm(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := New("")
	p.SetProducerOf(func(uid string) string {
		if uid == "z1" {
			return "zcode"
		}
		return "workbuddy"
	})
	p.Add(&auth.Auth{UID: "z1", Domain: "www.codebuddy.cn"})
	p.SetMaxInFlight(1)
	p.Acquire("z1")

	// 请求要 workbuddy，池里只有 zcode 号：来源不符，不是可等形态。
	if p.HasHealthyInFlightFull(nil, "", "cn", "workbuddy") {
		t.Fatal("producer mismatch must not be reported as slot-waitable")
	}
	// 请求要 zcode：命中在途占满 → 可等。
	if !p.HasHealthyInFlightFull(nil, "", "cn", "zcode") {
		t.Fatal("producer match + slot full should be reported as slot-waitable")
	}
	// tried 排除掉唯一号后无可等对象。
	if p.HasHealthyInFlightFull(map[string]bool{"z1": true}, "", "cn", "zcode") {
		t.Fatal("tried-excluded account must not be reported as slot-waitable")
	}
}

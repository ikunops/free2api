package server

import (
	"path/filepath"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/source"
)

// usableOf 从 producerTotalsView 里取某来源的 usable（测试断言用）。
func usableOf(t *testing.T, h *Handler, producer string) (total, healthy, usable int, servable bool) {
	t.Helper()
	for _, row := range h.producerTotalsView() {
		if row["producer"] != producer {
			continue
		}
		return row["total"].(int), row["healthy"].(int), row["usable"].(int), row["servable"].(bool)
	}
	t.Fatalf("producer_totals 里没有 %q 这一组", producer)
	return
}

// TestProducerUsableCounts 概览「可用」列的口径：健康 且 至少能跑一个模型。
//
// 线上症状：zcode 7 个号状态机全健康 → 旧实现显示可用 7，但套餐只给其中一个号发了
// glm-5.3-flash，真正能出流量的只有 1 个。这里锚定按来源分的判定：
//   - workbuddy：目录里有免费模型 → 健康号即可用（credits 为 0 也算）；
//   - zcode：看该号自己的套餐权益，不是号池并集；
//   - opencode / kilo：匿名免费层，健康即可用。
func TestProducerUsableCounts(t *testing.T) {
	seedFreeCatalog(t) // CN 目录含 hy3-free → workbuddy 走「有免费模型」分支
	stubZCodeEntitled([]creditRow{
		{UID: "zcode-a", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
		{UID: "zcode-b", Producer: source.ProducerZCode, OK: true}, // 读到但确实没额度
	})

	p := pool.New("")
	p.Add(&auth.Auth{UID: "wb1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.Add(&auth.Auth{UID: "zcode-a", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "k.s"})
	p.Add(&auth.Auth{UID: "zcode-b", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "k.s"})
	p.Add(&auth.Auth{UID: "oc1", Domain: "https://opencode.ai/zen/v1", AccessToken: "k.s"})
	p.SetProducerOf(func(uid string) string {
		switch uid {
		case "zcode-a", "zcode-b":
			return source.ProducerZCode
		case "oc1":
			return source.ProducerOpenCode
		default:
			return source.ProducerWorkbuddy
		}
	})
	// wb1 余额为 0：它还能用，靠的是目录里的免费模型，不是积分。
	h := NewHandler(Config{Pool: p})

	if _, _, usable, _ := usableOf(t, h, source.ProducerWorkbuddy); usable != 1 {
		t.Errorf("workbuddy 可用 = %d, want 1（有免费模型，credits=0 也算可用）", usable)
	}
	if _, _, usable, _ := usableOf(t, h, source.ProducerZCode); usable != 1 {
		t.Errorf("zcode 可用 = %d, want 1（只有 zcode-a 有套餐权益，zcode-b 没有）", usable)
	}
	if _, _, usable, _ := usableOf(t, h, source.ProducerOpenCode); usable != 1 {
		t.Errorf("opencode 可用 = %d, want 1（匿名免费层，健康即可用）", usable)
	}
	// 健康列不受影响：zcode 两个号状态机都健康。
	if _, healthy, _, _ := usableOf(t, h, source.ProducerZCode); healthy != 2 {
		t.Errorf("zcode 健康 = %d, want 2（可用口径不该动状态机健康数）", healthy)
	}
}

// TestProducerUsableZCodeUnknownNotCounted 读不到 zcode 权益（凭据解不开 / 上游抖动）
// 是「不知道」不是「没有」：不能把它当成可用，也不能因此把号判死。
func TestProducerUsableZCodeUnknownNotCounted(t *testing.T) {
	seedFreeCatalog(t)
	stubZCodeEntitled([]creditRow{
		{UID: "zcode-a", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
		{UID: "zcode-b", Producer: source.ProducerZCode, OK: false}, // 读不到
	})

	p := pool.New("")
	p.Add(&auth.Auth{UID: "zcode-a", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "k.s"})
	p.Add(&auth.Auth{UID: "zcode-b", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "k.s"})
	p.SetProducerOf(func(string) string { return source.ProducerZCode })
	h := NewHandler(Config{Pool: p})

	if _, _, usable, _ := usableOf(t, h, source.ProducerZCode); usable != 1 {
		t.Errorf("zcode 可用 = %d, want 1（读不到的那个不算可用）", usable)
	}
}

// TestZCodeByUIDPersistsAcrossRestart per-uid 权益（概览「可用」列的依据）与并集一起落盘，
// 否则每次重启后「可用」列都会先退回全量、再跳回来。
func TestZCodeByUIDPersistsAcrossRestart(t *testing.T) {
	newZCodeCatalogHandler(t)
	path := filepath.Join(t.TempDir(), "zcode_entitled.json")
	SetZCodeEntitledPath(path)
	t.Cleanup(func() { SetZCodeEntitledPath("") })

	stubZCodeEntitled([]creditRow{
		{UID: "zcode-a", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
		{UID: "zcode-b", Producer: source.ProducerZCode, OK: true},
	})

	zcodeEntitledCache.Lock()
	zcodeEntitledCache.set = nil
	zcodeEntitledCache.byUID = nil
	zcodeEntitledCache.known = false
	zcodeEntitledCache.Unlock()

	LoadZCodeEntitled()

	if ok, present := zcodeAccountUsable("zcode-a"); !present || !ok {
		t.Fatalf("重启后 zcode-a 应仍是有权益（present=%v ok=%v）", present, ok)
	}
	if ok, present := zcodeAccountUsable("zcode-b"); !present || ok {
		t.Fatalf("重启后 zcode-b 应仍是「读过、没权益」（present=%v ok=%v）", present, ok)
	}
}

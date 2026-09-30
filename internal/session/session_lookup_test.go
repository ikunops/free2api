package session

import (
	"testing"
	"time"
)

// ─── Lookup：只复用既有绑定，绝不自行分配（2026-09-30 选号策略修复）──────────
//
// 背景：handler 曾用 ResolveForModel 建会话，而该函数会**自己**从可用号里挑一个
// （优先空闲号哈希）并写绑定，完全绕过 pool.pick 的成本分层 / 快过期硬分层 /
// 免费模型不按余额加权。修复后 handler 改用 Lookup：只复用绑定，未绑定就回落
// pool.pick。这四条测试锚定 Lookup 的契约。

// TestLookupUnboundDoesNotAllocate 未绑定 key 只返回 false，**不得**新建绑定
// （否则又变成 session 自行分配、绕过选号策略）。
func TestLookupUnboundDoesNotAllocate(t *testing.T) {
	store := newCountingStore()
	r := routerWith(store, []string{"a1", "a2"}, time.Minute)

	if uid, ok := r.Lookup("fresh", "any"); ok || uid != "" {
		t.Fatalf("Lookup(未绑定) = (%q,%v), want (empty,false)", uid, ok)
	}
	if r.Count() != 0 {
		t.Errorf("Lookup 不得写绑定，Count=%d want 0", r.Count())
	}
	if n := store.setBinds; n != 0 {
		t.Errorf("Lookup 不得镜像写 SetBind，调用 %d 次 want 0", n)
	}
	// 对照：ResolveForModel 仍保留「分配」语义（供不接 handler 的调用方使用）。
	if _, ok := r.ResolveForModel("fresh", "any"); !ok {
		t.Error("ResolveForModel 应仍能分配（本测试只约束 Lookup）")
	}
}

// TestLookupReusesExistingBinding 已有绑定且在该模型上可用 → 复用并续期。
func TestLookupReusesExistingBinding(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a2")

	uid, ok := r.Lookup("c1", "any")
	if !ok || uid != "a2" {
		t.Fatalf("Lookup = (%q,%v), want (a2,true)", uid, ok)
	}
}

// TestLookupClearsStaleBinding 绑定号在目标模型上不可用 → 返回 false 且清除失效绑定，
// 让调用方回落 pool.pick 重新选择（否则陈旧绑定会把会话钉死在不可用的号上）。
func TestLookupClearsStaleBinding(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	r.cfg.AvailableForModel = func(model string) []string {
		if model == "limited" {
			return []string{"a2"} // a1 在该模型上不可用
		}
		return []string{"a1", "a2"}
	}
	r.Bind("c1", "a1")

	if uid, ok := r.Lookup("c1", "limited"); ok || uid != "" {
		t.Fatalf("Lookup(绑定号在该模型不可用) = (%q,%v), want (\"\",false)", uid, ok)
	}
	if r.Count() != 0 {
		t.Errorf("失效绑定应被清除，Count=%d want 0", r.Count())
	}
	// 同一 key 在未限额模型上：绑定已清，Lookup 仍不分配。
	if _, ok := r.Lookup("c1", "free"); ok {
		t.Error("绑定清除后 Lookup 应仍不分配，返回 false")
	}
}

// TestLookupExpiredBinding 过期绑定同样返回 false 并清除。
func TestLookupExpiredBinding(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	// 手工把 lastActive 拨回过期之前。
	r.mu.Lock()
	r.entries["c1"] = entry{uid: "a1", lastActive: time.Now().Add(-2 * time.Minute)}
	r.mu.Unlock()

	if uid, ok := r.Lookup("c1", "any"); ok || uid != "" {
		t.Fatalf("Lookup(过期绑定) = (%q,%v), want (\"\",false)", uid, ok)
	}
	if r.Count() != 0 {
		t.Errorf("过期绑定应被清除，Count=%d want 0", r.Count())
	}
}

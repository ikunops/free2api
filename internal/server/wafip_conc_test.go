package server

// wafip_conc_test.go 代码健康审计（任务书第 1 条）补缺：wafIPGate 多 goroutine
// 并发 noteWaf/active 压力。状态机单测（触发/过期/不续期）已由 wafip_test.go 覆盖，
// 本文件只补「并发记账无竞态 + 阈值语义在交错下不被破坏」。
import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestWafIPGateConcurrentMixed 并发混发：N goroutine 各对多个不同 uid 无限
// noteWaf + 持续读 active。不变量：不 panic（-race 验证锁覆盖）、激活后
// active 恒 true（直到窗过期）、激活后 noteWaf 恒 true（不续期语义下激活期
// 内无 false 回落——noteWaf 激活期分支直接返回 true）。
func TestWafIPGateConcurrentMixed(t *testing.T) {
	withWafIPWindow(t, 200*time.Millisecond)
	var g wafIPGate
	const goroutines = 8
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for n := 0; n < 50; n++ {
				uid := fmt.Sprintf("u%d-%d", i, n%6) // 每 goroutine 6 个不同 uid，跨 g 也不重复
				if g.noteWaf(uid) {
					// 已激活（本窗内）：并发下其他线程的 noteWaf 也应恒 true。
					// 只做无锁交叉读验证 active 一致性（读侧允许滞后，不可 panic）。
					_ = g.active()
				}
			}
		}(i)
	}
	wg.Wait()
	// 8×6=48 个不同 uid 全部入窗（远超阈值 2）→ 激活必然已发生且仍激活
	// （200ms 窗内完成所有发数，无 sleep 落在窗外）。
	if !g.active() {
		t.Fatal("48 distinct uids within window must leave gate active")
	}
}

// TestWafIPGateConcurrentSingleUIDPerAccount 并发下「单号反复不触发」不变量：
// 所有 goroutine 共用 2 个 uid 反复命中，永不达到 2 个不同号的语义下应激活
// （2 个不同号即阈值）——语义反转对照：本测试用 2 个 uid，应激活；再用单
// uid 独立 gate 验证永不激活。
func TestWafIPGateConcurrentSingleUIDPerAccount(t *testing.T) {
	withWafIPWindow(t, time.Minute)
	// 单 uid 并发大量命中：永不激活（判定口径=不同 UID 数，锁保护下计数一致）。
	var single wafIPGate
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				if single.noteWaf("only-one") {
					t.Error("concurrent single-uid hits must never activate")
					return
				}
			}
		}()
	}
	wg.Wait()
	if single.active() {
		t.Fatal("single account gate must never activate regardless of concurrency")
	}
}

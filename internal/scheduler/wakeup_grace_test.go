package scheduler

// wakeup_grace_test.go issue #152 迟到唤醒补跑派发前网络宽限（TDD RED，先于实现提交）。
//
// 场景：机器睡眠跨过槽位时刻，唤醒瞬间 timer 到期补跑（Run 主循环既有增值设计），
// 但 Windows Modern Standby exit 后网络栈/DNS 需 1-2s 才就绪——零宽限立即派发
// 等于把唯一一次补跑机会打在注定失败的窗口里（issue 实测 dial tcp lookup no
// such host 与 Kernel-Power 507 standby exit ≤1s 重合）。
//
// 本文件断言修复后的行为（提取的 awaitWakeupGrace 为可测面）：
//  1. 准点触发（含毫秒级 timer 抖动，<1s）零延迟放行，不被误宽限；
//  2. 迟到补跑（now 晚于槽位计划时刻 >1s）先等满宽限再放行（派发前宽限）；
//  3. 宽限等待可被 ctx 取消立即中断（优雅停机不被 5s 阻塞）；
//  4. 宽限缺省值锁 5s（生产语义防漂移）。
//
// 本提交为 RED：引用尚未实现的 awaitWakeupGrace，编译失败即 RED 证据；
// 下一提交补实现转 GREEN。wakeupGraceDelay 生产 5s、测试缩短（var 可改，
// 与 travelAccountDelay「测试可置 0」同口径）。

import (
	"context"
	"testing"
	"time"
)

// TestAwaitWakeupGraceOnTimeZeroDelay 准点触发：槽位即当前时刻（毫秒级抖动）
// 直接放行，不引入任何宽限延迟。
func TestAwaitWakeupGraceOnTimeZeroDelay(t *testing.T) {
	start := time.Now()
	if !awaitWakeupGrace(context.Background(), time.Now()) {
		t.Fatal("准点触发应放行（true）")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("准点触发不应宽限等待，耗时 %v", e)
	}
}

// TestAwaitWakeupGraceSubThresholdJitterNotLate 迟到阈值内的毫秒级抖动
// （timer 正常触发的偏移 <1s）不算迟到补跑：零延迟放行，不被误宽限。
func TestAwaitWakeupGraceSubThresholdJitterNotLate(t *testing.T) {
	// 即便宽限被放大，sub-threshold 抖动也不应等待（判定先于等待）。
	old := wakeupGraceDelay
	wakeupGraceDelay = 2 * time.Second
	t.Cleanup(func() { wakeupGraceDelay = old })

	start := time.Now()
	// 500ms 早于 1s 阈值：timer 正常触发的抖动量级。
	planned := time.Now().Add(-500 * time.Millisecond)
	if !awaitWakeupGrace(context.Background(), planned) {
		t.Fatal("阈值内抖动应放行（true）")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("阈值内抖动不应宽限等待，耗时 %v", e)
	}
}

// TestAwaitWakeupGraceLateWaitsFullGrace 迟到补跑：now 晚于槽位计划时刻 >1s
// → 派发前等满宽限（等待时长 ≥ 宽限值；断言留 20ms 测量余量防 CI 抖动误报）。
func TestAwaitWakeupGraceLateWaitsFullGrace(t *testing.T) {
	old := wakeupGraceDelay
	wakeupGraceDelay = 120 * time.Millisecond
	t.Cleanup(func() { wakeupGraceDelay = old })

	planned := time.Now().Add(-30 * time.Second) // 唤醒补跑：槽位过点 30s
	start := time.Now()
	if !awaitWakeupGrace(context.Background(), planned) {
		t.Fatal("迟到补跑等满宽限后应放行（true）")
	}
	if e := time.Since(start); e < 100*time.Millisecond {
		t.Errorf("迟到补跑应先等满宽限（~120ms）再放行，实际 %v", e)
	}
}

// TestAwaitWakeupGraceCancelledDuringGrace 宽限等待中取消 ctx：立即返回 false
// （优雅停机不等 5s 宽限睡满）。
func TestAwaitWakeupGraceCancelledDuringGrace(t *testing.T) {
	old := wakeupGraceDelay
	wakeupGraceDelay = 2 * time.Second
	t.Cleanup(func() { wakeupGraceDelay = old })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	planned := time.Now().Add(-30 * time.Second)
	start := time.Now()
	if awaitWakeupGrace(ctx, planned) {
		t.Fatal("宽限中取消 ctx 应返回 false（放弃本批）")
	}
	if e := time.Since(start); e > 1*time.Second {
		t.Errorf("取消后应快速返回（远小于 2s 宽限），实际 %v", e)
	}
}

// TestAwaitWakeupGraceDefault5s 宽限缺省值锁 5s（生产语义：覆盖 Modern Standby
// exit 后 1-2s DNS 恢复窗口；防测试改值后忘还原导致漂移）。
func TestAwaitWakeupGraceDefault5s(t *testing.T) {
	if wakeupGraceDelay != 5*time.Second {
		t.Errorf("wakeupGraceDelay=%v want 5s（生产缺省）", wakeupGraceDelay)
	}
}

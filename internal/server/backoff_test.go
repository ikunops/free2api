package server

import (
	"context"
	"testing"
	"time"
)

// withRotateBackoff 恢复退避基数并在测试结束后归零（TestMain 置 0 加速轮转测试）。
func withRotateBackoff(t *testing.T, base time.Duration) {
	t.Helper()
	rotateBackoffBase = base
	t.Cleanup(func() { rotateBackoffBase = 0 })
}

// TestBackoffAfterBounds 轮转退避的次数与抖动界（任务书 P0-2 验收点）：
// n 次失败后的退避 = base·2^n 封顶 8s，抖动 ±25%。界断言按区间而非精确值。
func TestBackoffAfterBounds(t *testing.T) {
	withRotateBackoff(t, 500*time.Millisecond)
	cases := []struct {
		n       int
		wantMin time.Duration // 基数下界·(1-0.25)
		wantMax time.Duration // 封顶值上界·(1+0.25)
	}{
		{0, 350 * time.Millisecond, 650 * time.Millisecond},
		{1, 750 * time.Millisecond, 1500 * time.Millisecond},
		{2, 1500 * time.Millisecond, 3000 * time.Millisecond},
		{3, 3000 * time.Millisecond, 6000 * time.Millisecond},   // 4s（封顶内）
		{4, 6000 * time.Millisecond, 10000 * time.Millisecond},  // 8s 封顶（含抖动上限 10s）
		{20, 6000 * time.Millisecond, 10000 * time.Millisecond}, // 极端轮转次数仍封顶
	}
	for _, c := range cases {
		for i := 0; i < 200; i++ { // 多次采样覆盖抖动区间
			got := backoffAfter(c.n)
			if got < c.wantMin || got > c.wantMax {
				t.Fatalf("backoffAfter(%d)=%v want in [%v,%v]", c.n, got, c.wantMin, c.wantMax)
			}
		}
	}
}

// TestBackoffAfterZeroBase 测试态（base=0，TestMain 置）恒零：轮转测试不等退避。
func TestBackoffAfterZeroBase(t *testing.T) {
	withRotateBackoff(t, 0)
	for n := 0; n < 5; n++ {
		if got := backoffAfter(n); got != 0 {
			t.Fatalf("backoffAfter(%d)=%v want 0 (test mode)", n, got)
		}
	}
}

// TestJitterDurZero no-op：d<=0 原样返回（零等待不抖动）。
func TestJitterDurZero(t *testing.T) {
	if got := jitterDur(0); got != 0 {
		t.Fatalf("jitterDur(0)=%v want 0", got)
	}
	if got := jitterDur(-time.Second); got != -time.Second {
		t.Fatalf("jitterDur(-1s)=%v want -1s", got)
	}
}

// TestJitterDurBounds 抖动界：jitterDur(d) ∈ [0.75d, 1.25d]。
func TestJitterDurBounds(t *testing.T) {
	d := 400 * time.Millisecond
	for i := 0; i < 500; i++ {
		got := jitterDur(d)
		if got < 300*time.Millisecond || got > 500*time.Millisecond {
			t.Fatalf("jitterDur(%v)=%v want in [300ms,500ms]", d, got)
		}
	}
}

// TestSleepCtxCancel ctx 取消立即返回 false（客户端断连/优雅停机不睡满），
// 等满返回 true，d<=0 立即放行（与 scheduler.sleepCtx 同契约的等价物）。
func TestSleepCtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepCtx(ctx, 5*time.Second) {
		t.Fatal("cancelled ctx must return false")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel must be immediate, elapsed=%v", elapsed)
	}
	if !sleepCtx(context.Background(), 0) {
		t.Fatal("d<=0 must pass immediately")
	}
	if !sleepCtx(context.Background(), 20*time.Millisecond) {
		t.Fatal("full wait must return true")
	}
}

package redisstore

import (
	"testing"
	"time"
)

// P1-4（发现 4）：Store 接口加 Close + Upstash 写并发上限。
//   - Close 关停：等所有已提交的异步写执行完毕再关底层连接（停机语义：最后一笔
//     状态镜像必须写完）；Close 后新提交的写直接丢弃；幂等。
//   - 并发上限：fire-and-forget 写共享有界写槽（cap=8 信号量），超出的排队，
//     不无限堆积在途写。
// 测试用 nil client 直接构造 Upstash——goWrite/Close 的调度语义不触网络。

var _ Store = (*Upstash)(nil)

// newTestUpstash 构造不联网的 Upstash（client=nil，Close 对 nil client 安全）。
func newTestUpstash() *Upstash {
	sem := make(chan struct{}, 1)
	done := make(chan struct{})
	return &Upstash{sem: sem, done: done}
}

// TestNoopClose Noop 的 Close 是空操作且返回 nil（纯内存降级无需关停）。
func TestNoopClose(t *testing.T) {
	var n Noop
	if err := n.Close(); err != nil {
		t.Errorf("Noop.Close()=%v want nil", err)
	}
}

// TestUpstashCloseIdempotent 多次 Close 不 panic（closeOnce 保护 done channel）。
func TestUpstashCloseIdempotent(t *testing.T) {
	u := newTestUpstash()
	if err := u.Close(); err != nil {
		t.Fatalf("Close()=%v want nil (nil client)", err)
	}
	if err := u.Close(); err != nil {
		t.Fatalf("second Close()=%v want nil", err)
	}
}

// TestUpstashWriteConcurrencyLimit 写槽占满时，后续提交的写排队不执行；
// 释放槽位后才执行（并发上限 = 排队，不是丢弃、不是无限并发）。
func TestUpstashWriteConcurrencyLimit(t *testing.T) {
	u := newTestUpstash()
	started := make(chan struct{}, 4)

	// 手动占满唯一写槽。
	u.sem <- struct{}{}
	u.goWrite(func() { started <- struct{}{} })
	select {
	case <-started:
		t.Fatal("写应在写槽占满时排队，不应执行")
	case <-time.After(50 * time.Millisecond):
	}

	// 释放槽位 → 排队的写获得槽并执行。
	<-u.sem
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("释放写槽后，排队的写应执行")
	}
}

// TestUpstashCloseDropsNewWrites Close 之后提交的写直接丢弃（不再起 goroutine）。
func TestUpstashCloseDropsNewWrites(t *testing.T) {
	u := newTestUpstash()
	if err := u.Close(); err != nil {
		t.Fatalf("Close()=%v", err)
	}
	ran := make(chan struct{}, 1)
	u.goWrite(func() { ran <- struct{}{} })
	select {
	case <-ran:
		t.Fatal("Close 后提交的写不应执行")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestUpstashCloseWaitsForInFlightClose 等在途写结束才返回：
// 在途写（已持写槽、正在执行）未完成时 Close 必须阻塞，完成后立即返回。
func TestUpstashCloseWaitsForInFlight(t *testing.T) {
	u := newTestUpstash()
	release := make(chan struct{})
	ran := make(chan struct{})
	u.goWrite(func() { close(ran); <-release }) // 在途写：执行中阻塞直到 release
	select {
	case <-ran:
	case <-time.After(time.Second):
		t.Fatal("前置条件失败：在途写未开始")
	}

	closed := make(chan struct{})
	go func() { u.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("在途写未结束时 Close 不应返回")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("在途写结束后 Close 应返回")
	}
}

// TestUpstashCloseDrainsQueuedWrites 停机关键语义：Close 前已提交的写
// （含仍在排队等写槽的）必须执行完毕，Close 才返回——pool 关停路径的最后一笔
// Redis 镜像依赖此保证。
func TestUpstashCloseDrainsQueuedWrites(t *testing.T) {
	u := newTestUpstash()
	block := make(chan struct{})
	ran1, ran2 := make(chan struct{}, 1), make(chan struct{}, 1)
	u.goWrite(func() { ran1 <- struct{}{}; <-block }) // 占住唯一写槽
	u.goWrite(func() { ran2 <- struct{}{} })          // 排队等写槽
	select {
	case <-ran1:
	case <-time.After(time.Second):
		t.Fatal("前置条件失败：第一个写未开始")
	}

	closed := make(chan struct{})
	go func() { u.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("排队写未执行完，Close 不应返回")
	case <-time.After(50 * time.Millisecond):
	}

	close(block) // 第一个写放槽 → 排队写执行
	select {
	case <-ran2:
	case <-time.After(time.Second):
		t.Fatal("Close 前提交的排队写应执行")
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("全部已提交写结束后 Close 应返回")
	}
}

package main

import (
	"syscall"
	"testing"
	"time"
)

// TestSyscallErrnoZeroIsNotAnError 锁住 blit 的判错写法。
//
// syscall.Proc.Call 的第三个返回值静态类型是 syscall.Errno（不是 error 接口），
// 所以 `err != nil` 在 Errno(0) 时同样成立 —— 也就是「只要在贴图就报错」。悬浮窗
// 一秒数帧，这个写法把 desktop.log 刷成了 25 MB/天，而且每行内容都是
// "0(The operation completed successfully.)"，看上去像成功日志，实际是无条件打印。
//
// 这里直接断言两种判法的差异：接口比较认为 Errno(0) 非 nil，errno 比较认为是成功。
func TestSyscallErrnoZeroIsNotAnError(t *testing.T) {
	var ferr error = syscall.Errno(0)
	if ferr == nil {
		t.Fatal("接口比较：Errno(0) 被判成 nil，这正是 bug 的来源")
	}
	errno, ok := ferr.(syscall.Errno)
	if !ok {
		t.Fatalf("类型断言失败：%T", ferr)
	}
	if errno != 0 {
		t.Errorf("errno=%d 期望 0", uint32(errno))
	}
}

// TestBlitErrLogIsDeduplicated 贴图失败日志必须去重：同样的错误在 30s 内只打一遍。
func TestBlitErrLogIsDeduplicated(t *testing.T) {
	f := &floatWin{}
	f.logBlitErr("boom")
	first := f.lastBlitErr
	if first != "boom" {
		t.Fatalf("lastBlitErr=%q 期望 boom", first)
	}
	// 同一错误、间隔很短 → 不更新 lastBlitErrAt（只打第一遍）
	f.logBlitErr("boom")
	if f.lastBlitErrAt.IsZero() {
		t.Fatal("lastBlitErrAt 未初始化")
	}
	at := f.lastBlitErrAt
	f.logBlitErr("boom")
	if !f.lastBlitErrAt.Equal(at) {
		t.Error("30s 内的重复错误不该刷新时间戳")
	}
	// 超过 30s → 重新提醒一次（时间戳必须被推到「现在」附近）
	f.blitLogMu.Lock()
	f.lastBlitErrAt = time.Now().Add(-31 * time.Second)
	f.blitLogMu.Unlock()
	before := time.Now()
	f.logBlitErr("boom")
	f.blitLogMu.Lock()
	got := f.lastBlitErrAt
	f.blitLogMu.Unlock()
	if got.Before(before.Add(-time.Second)) {
		t.Errorf("超过 30s 后应刷新时间戳到当前时刻，got %v（调用时刻 %v）", got, before)
	}
	// 不同错误 → 立刻记录
	f.logBlitErr("other")
	if f.lastBlitErr != "other" {
		t.Errorf("lastBlitErr=%q 期望 other", f.lastBlitErr)
	}
}
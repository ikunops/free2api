package scheduler

// parallel_test.go P1-3 调度器去串行化 + sleep 可取消化（TDD RED，先于实现提交）。
//
// 对应审查报告 comprehensive-code-review.md HIGH 发现 3：六类任务共用一个派发
// goroutine，time.Sleep 不响应 ctx 取消——活跃上报 54 号 × 5 条 ≈ 7-8 分钟纯
// 睡眠阻塞同槽其他任务族，优雅停机要等 sleep 醒来。本文件断言修复后的行为：
//  1. sleepCtx：d<=0 立即放行；等满返回 true；ctx 取消立即返回 false；
//  2. runActivity/runTravel：账号间限速等待中取消 ctx，遍历立即退出（不等
//     sleep 醒来），后续账号不再发起上游调用；
//  3. runBatch：同一唤醒时刻的多类任务并行派发——签到占用 /daily-checkin
//     窗口期间，活跃上报的 /v2/report 已能发出（串行派发时必然落在窗口之后）。
//
// 本提交为 RED：引用尚未实现的 sleepCtx / runActivity / runTravel / runBatch，
// 编译失败即 RED 证据；下一提交补实现转 GREEN。账号间延迟值不变（800ms/
// 1500ms），只换等待方式，fastActivity/fastTravel 置 0 的既有测试不受影响。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// waitCalls 轮询等待计数达到 want（deadline 内），超时返回 false。
func waitCalls(c *atomic.Int32, want int32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for c.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	return c.Load() >= want
}

// ---------------------------------------------------------------------------
// sleepCtx 可取消等待
// ---------------------------------------------------------------------------

// TestSleepCtxZeroImmediate d<=0 立即放行（兼容测试把延迟置 0 的用法）。
func TestSleepCtxZeroImmediate(t *testing.T) {
	start := time.Now()
	if !sleepCtx(context.Background(), 0) {
		t.Error("d=0 应立即返回 true")
	}
	if e := time.Since(start); e > 50*time.Millisecond {
		t.Errorf("d=0 不应等待，耗时 %v", e)
	}
}

// TestSleepCtxElapsed 等满 d 后返回 true。
func TestSleepCtxElapsed(t *testing.T) {
	start := time.Now()
	if !sleepCtx(context.Background(), 50*time.Millisecond) {
		t.Error("等满应返回 true")
	}
	if e := time.Since(start); e < 40*time.Millisecond {
		t.Errorf("应等满约 50ms，实际 %v", e)
	}
}

// TestSleepCtxCancelled 等待期间取消 ctx：立即返回 false（不等 d 醒来）。
func TestSleepCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if sleepCtx(ctx, 2*time.Second) {
		t.Error("取消后应返回 false")
	}
	if e := time.Since(start); e > 1*time.Second {
		t.Errorf("取消后应快速返回（远小于 2s），实际 %v", e)
	}
}

// ---------------------------------------------------------------------------
// 账号间限速可取消：正在 sleep 的遍历随 ctx 取消立即退出
// ---------------------------------------------------------------------------

// TestRunActivityCtxCancelsDuringAccountDelay 活跃上报账号间限速（2s）中取消
// ctx：runActivity 立即退出，第 2 号不再上报。串行 time.Sleep 版本要等满 2s。
func TestRunActivityCtxCancelsDuringAccountDelay(t *testing.T) {
	oldDelay, oldGap := activityAccountDelay, activityReportGap
	activityAccountDelay, activityReportGap = 2*time.Second, 0
	t.Cleanup(func() {
		activityAccountDelay, activityReportGap = oldDelay, oldGap
	})

	stub := &reportStub{}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "u2", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runActivity(ctx)
		close(done)
	}()

	// 等第 1 号上报完成（随后的账号间限速等待中取消）。
	if !waitCalls(&stub.calls, 1, 3*time.Second) {
		t.Fatal("3s 内未观察到第 1 号上报")
	}
	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Error("runActivity 未在取消后 1.5s 内退出（账号间限速 sleep 不可取消）")
		select { // 排干 goroutine，避免泄漏与后续断言竞争
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	if e := time.Since(start); e > 1200*time.Millisecond {
		t.Errorf("取消后退出耗时 %v（应远小于 2s 的账号间限速）", e)
	}
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("report calls=%d want 1（取消后 u2 不应上报）", n)
	}
}

// TestRunTravelCtxCancelsDuringAccountDelay 旅行账号间限速（2s）中取消 ctx：
// runTravel 立即退出，第 2 号不再巡检。
func TestRunTravelCtxCancelsDuringAccountDelay(t *testing.T) {
	old := travelAccountDelay
	travelAccountDelay = 2 * time.Second
	t.Cleanup(func() { travelAccountDelay = old })

	stub := &travelStub{buddy: "null"}
	srv := stub.server()
	defer srv.Close()

	s, _ := newTravelScheduler(t, srv, "u1", "u2")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runTravel(ctx)
		close(done)
	}()

	if !waitCalls(&stub.infoCalls, 1, 3*time.Second) {
		t.Fatal("3s 内未观察到第 1 号巡检")
	}
	cancel()
	start := time.Now()
	select {
	case <-done:
	case <-time.After(1500 * time.Millisecond):
		t.Error("runTravel 未在取消后 1.5s 内退出（账号间限速 sleep 不可取消）")
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
	if e := time.Since(start); e > 1200*time.Millisecond {
		t.Errorf("取消后退出耗时 %v（应远小于 2s 的账号间限速）", e)
	}
	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("buddy/info calls=%d want 1（取消后 u2 不应巡检）", n)
	}
}

// ---------------------------------------------------------------------------
// 同槽多类任务并行派发：慢任务族不再阻塞其他任务族
// ---------------------------------------------------------------------------

// TestRunBatchFiresKindsInParallel 同一唤醒时刻的两类任务并行执行：
// 签到 handler 在 /daily-checkin 窗口内等 /v2/report 的信号（握手）——并行派发时
// report 随时可达（checkin 不阻塞 activity）；串行派发时 report 只会在 checkin
// 结束后才发出，checkin 等信号必然超时 → overlap=false。
// 账号 token 未过期（不触发 refresh 写 AccessToken），与 activity 读并发，
// 保证 -race 干净（refresh 写 vs report 读属既有 chat/keepalive 同款并发面）。
func TestRunBatchFiresKindsInParallel(t *testing.T) {
	fastActivity(t)
	fastTravel(t)

	reportSeen := make(chan struct{})
	var reportOnce sync.Once
	var overlap atomic.Bool
	var checkinCalls, reportCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/daily-checkin"):
			checkinCalls.Add(1)
			// 窗口内等 report 信号：并行时 report 在 checkin 进行中可达；
			// 串行时 report 落在窗口之后，2s 超时 → overlap 保持 false。
			select {
			case <-reportSeen:
				overlap.Store(true)
			case <-time.After(2 * time.Second):
			}
			w.Write([]byte(`{"code":0,"msg":"ok","data":{}}`))
		case strings.HasSuffix(r.URL.Path, "/get-user-resource"):
			w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[{"CycleCapacitySize":100,"CycleCapacityRemain":500,"CycleCapacityUsed":0}]}}}}`))
		case strings.HasSuffix(r.URL.Path, "/v2/report"):
			reportCalls.Add(1)
			reportOnce.Do(func() { close(reportSeen) })
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		default:
			http.Error(w, "not found", 404) // streak 自检 / buddy-info 等，无需模拟
		}
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		s.runBatch(ctx, []taskKind{taskCheckin, taskActivity})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runBatch 未在 5s 内完成")
	}

	if checkinCalls.Load() != 1 || reportCalls.Load() != 1 {
		t.Fatalf("checkin=%d report=%d want 1/1", checkinCalls.Load(), reportCalls.Load())
	}
	if !overlap.Load() {
		t.Error("checkin 与 activity 未并行：/v2/report 未落在 /daily-checkin 窗口内（同槽串行阻塞）")
	}
}

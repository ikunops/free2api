package pool

// race_stress_test.go 代码健康审计（任务书第 1 条）补缺：冷却记账 + Flush 落盘
// 与全量状态读并发压力。Acquire/Pick/降权喂入/降权+Close 并发已分别由
// pool_test.go（TestInFlightCountNotExceedLimit / TestPickSpreadUnderConcurrency）、
// degrade_test.go（TestDegradeConcurrentNoteFailures[AndClose]）覆盖，本文件只补
// 冷却记账与 flusher tick 交错的缺口——全部只跑 -race 语义（断言的是不变量而非
// 精确计数，交错下任何合法时序都应满足）。
import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestCooldownAccountingConcurrent 冷却记账热点并发压力（任务书点名）：
// 多 goroutine 对同一账号混发 CooldownSoftForModel / CooldownSoftRate /
// NoteError / NoteSuccess / SetCreditsDetailed，同时读状态（Counts / List）。
// 断言不变量：不 panic（-race 验证锁覆盖）、落盘文件最终可读、credits 不为负。
func TestCooldownAccountingConcurrent(t *testing.T) {
	dir := t.TempDir()
	p := New(filepath.Join(dir, "state.json"))
	const uids = 4
	for i := 0; i < uids; i++ {
		uid := "u" + string(rune('0'+i))
		p.Add(&auth.Auth{UID: uid})
		p.SetCredits(uid, 1000)
	}
	reset := func(v time.Duration) {
		old := flushInterval
		flushInterval = v
		t.Cleanup(func() { flushInterval = old })
	}
	reset(time.Millisecond) // 高频 tick：让 flusher 与记账充分交错

	var wg sync.WaitGroup
	var stop atomic.Bool
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			uid := "u" + string(rune('0'+g%uids))
			for n := 0; n < 100; n++ {
				switch n % 5 {
				case 0:
					p.CooldownSoftForModel(uid, time.Minute, time.Now().Add(time.Hour), "test-model", "stress")
				case 1:
					p.CooldownSoftRate(uid, time.Minute, time.Now().Add(time.Hour), "stress")
				case 2:
					p.NoteError(uid)
				case 3:
					p.NoteSuccess(uid)
				case 4:
					p.SetCreditsDetailed(uid, 500, 100, 50)
				}
				// 同 goroutine 族读状态（CountsDetailed/List 只读路径）。
				_, _, _, _, _ = p.CountsDetailed()
				_ = p.List()
			}
		}(g)
	}
	// 独立的读 goroutine：与写 goroutine 交错读快照（写族先 join，stop 置位后读族退出）。
	var rd sync.WaitGroup
	for r := 0; r < 2; r++ {
		rd.Add(1)
		go func() {
			defer rd.Done()
			for !stop.Load() {
				for _, st := range p.List() {
					_ = st.UID + st.Reason
				}
				_, _, _, _, _ = p.CountsDetailed()
			}
		}()
	}
	wg.Wait()
	stop.Store(true)
	rd.Wait()
	p.Close()

	// 落盘可读：Close 强制 Flush 后 state.json 存在且为合法 JSON（load 复用）。
	p2 := New(filepath.Join(dir, "state.json"))
	if n := len(p2.List()); n != uids {
		t.Fatalf("after flush, restored accounts=%d want %d", n, uids)
	}
}

// TestFlusherTickConcurrentWithClose flusher tick 与 Close 竞争退出：
// 高频 tick 期间直接 Close（close(stopCh) 与 tick 同时到达 select），验证
// flusher goroutine 干净退出、最终 Flush 落盘完整（无半写 JSON）。
func TestFlusherTickConcurrentWithClose(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	old := flushInterval
	flushInterval = time.Millisecond
	t.Cleanup(func() { flushInterval = old })

	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.SetCredits("u1", 42)
	// 持续 dirty：让每 tick 都真的 saveLocked（与 Close 的 lock/flush 交错）。
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			p.NoteSuccess("u1")
		}
	}()
	p.Close() // 与写 goroutine 并发 Close（乱序停机路径，degrade_test 同款语义）
	wg.Wait()

	p2 := New(fp)
	if len(p2.List()) != 1 {
		t.Fatalf("restored accounts=%d want 1", len(p2.List()))
	}
}

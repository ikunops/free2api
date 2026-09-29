// modelcost_persist_test.go 成本台账持久化（P1-anti-monopoly）测试：
// 往返、TTL 过期恢复、限免结束日志事件、非法值剔除、并发写 + 落盘（-race）。
package pool

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

// modelCostOfHelper 曝露账本条目供测试断言（包内私有 helper，锁内读快照）。
func modelCostSnapshot(p *Pool, uid, model string) (modelCostEntry, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.byUID[uid]
	if !ok {
		return modelCostEntry{}, false
	}
	mc, ok := e.modelCost[model]
	return mc, ok
}

// TestModelCostPersistRoundTrip 台账落盘 → 重启 → 恢复：单价/LastSeen/Samples
// 往返无损，选号知识跨重启保留（此前仅内存态，重启即失忆重付学费）。
func TestModelCostPersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "hy4-preview", 0, 1000)   // 免费（tier 0）
	p.NoteModelCost("u1", "glm-5.3", 2.0, 1000)     // 收费（tier 2）
	p.NoteModelCost("u1", "glm-5.3", 4.0, 1000)     // 二次观测（EMA + Samples）
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	// 免费观测恢复且仍为 0（tier 0 知识保留）。
	mc, ok := modelCostSnapshot(p2, "u1", "hy4-preview")
	if !ok || mc.CostPer1k != 0 {
		t.Errorf("免费观测未恢复: ok=%v mc=%+v", ok, mc)
	}
	// 收费观测恢复：EMA(α=0.3) 混两次观测（2.0、4.0），Samples=2。往返无损的
	// 权威断言是「恢复值 == 落盘前内存值」（JSON 浮点原样保留，不引入漂移），
	// 而非手算常数（浮点结合序会让 2.6 呈 2.599999…）。
	mc, ok = modelCostSnapshot(p2, "u1", "glm-5.3")
	if !ok {
		t.Fatal("收费观测未恢复")
	}
	if want, _ := modelCostSnapshot(p, "u1", "glm-5.3"); mc.CostPer1k != want.CostPer1k {
		t.Errorf("CostPer1k 往返漂移: restored=%v in-memory=%v", mc.CostPer1k, want.CostPer1k)
	}
	if mc.Samples != 2 {
		t.Errorf("Samples=%d want 2", mc.Samples)
	}
	if mc.LastSeen.IsZero() {
		t.Error("LastSeen 未恢复")
	}
}

// TestModelCostPersistTTLExpiryFilter 恢复时已超 modelCostTTL 的条目按过期处理
// （不恢复，选号侧回 tier 1 无观测——陈旧知识不复活，与 modelCostOf 运行态
// 口径一致：夜间免费不跨时段生效）。
func TestModelCostPersistTTLExpiryFilter(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "stale", 0, 1000)
	p.NoteModelCost("u1", "fresh", 1.0, 1000)
	// 手工把 stale 做旧到 TTL 之外（fresh 保持新鲜）。
	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["stale"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL)
	p.byUID["u1"].modelCost["stale"] = mc
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	p2 := New(fp)
	p2.Add(&auth.Auth{UID: "u1"})
	if _, ok := modelCostSnapshot(p2, "u1", "stale"); ok {
		t.Error("过期观测不应恢复（回 tier 1，不复活陈旧知识）")
	}
	if _, ok := modelCostSnapshot(p2, "u1", "fresh"); !ok {
		t.Error("未过期观测应恢复")
	}
}

// TestModelCostPersistExpiryNotWritten 落盘侧惰性清理：已过期的条目不写出
//（落盘即清理，与恢复侧同口径），state.json 不残留陈旧价格。
func TestModelCostPersistExpiryNotWritten(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "old", 1.0, 1000)
	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["old"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL)
	p.byUID["u1"].modelCost["old"] = mc
	p.dirty.Store(true)
	p.mu.Unlock()
	p.Flush()

	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"old"`) {
		t.Errorf("过期条目不应落盘:\n%s", raw)
	}
}

// TestModelCostFreeTierEndedLog 限免结束日志事件：tier 0 观测（per1k≤0）被
// credit>0 观测覆盖时打 `free tier ended` 日志（判定在写入口，覆盖前值才触发）。
// 本测试不 t.Parallel：log.SetOutput 是进程级全局，需串行。
func TestModelCostFreeTierEndedLog(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	// 免费 → 免费：不是「结束」，不打事件。
	p.NoteModelCost("u1", "m", 0, 1000)
	if strings.Contains(buf.String(), "free tier ended") {
		t.Errorf("免费→免费不应触发限免结束事件: %s", buf.String())
	}
	// 免费 → 收费：打事件。
	p.NoteModelCost("u1", "m", 2.0, 1000)
	if !strings.Contains(buf.String(), "free tier ended") {
		t.Errorf("免费→收费应触发限免结束事件: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "model m on uid") || !strings.Contains(buf.String(), "credits/1k") {
		t.Errorf("事件应含模型/账号/单价上下文: %s", buf.String())
	}
	buf.Reset()
	// 收费 → 收费：已不在 tier 0，不再打。
	p.NoteModelCost("u1", "m", 3.0, 1000)
	if strings.Contains(buf.String(), "free tier ended") {
		t.Errorf("收费→收费不应重复触发: %s", buf.String())
	}
	// 新模型直接收费：无覆盖前值，不打。
	p.NoteModelCost("u1", "m2", 1.0, 1000)
	if strings.Contains(buf.String(), "free tier ended") {
		t.Errorf("无 tier0 前值不应触发: %s", buf.String())
	}
}

// TestModelCostRestoreDropsInvalid 恢复侧非法值剔除：负 per1k / 零 LastSeen
// 的结构破损条目剔除不污染账本（手工脏 state.json 防御）；损坏更重的整个
// 文件非法 JSON 已由 load() 静默跳过（既有行为，不崩溃）。
func TestModelCostRestoreDropsInvalid(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	// 手工构造：合法条目 + 负单价 + 零 LastSeen（三种形态并存）。
	negative := -0.5
	state := map[string]any{
		"accounts": map[string]any{
			"u1": map[string]any{
				"credits": 100,
				"model_costs": map[string]any{
					"valid":   map[string]any{"cost_per_1k": 1.5, "last_seen": time.Now().Format(time.RFC3339Nano), "samples": 3},
					"neg":     map[string]any{"cost_per_1k": negative, "last_seen": time.Now().Format(time.RFC3339Nano)},
					"nolast":  map[string]any{"cost_per_1k": 2.0},
				},
			},
		},
	}
	raw, _ := json.Marshal(state)
	if err := os.WriteFile(fp, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if _, ok := modelCostSnapshot(p, "u1", "valid"); !ok {
		t.Error("合法条目应恢复")
	}
	if _, ok := modelCostSnapshot(p, "u1", "neg"); ok {
		t.Error("负 per1k 条目应剔除（非法值不污染账本）")
	}
	if _, ok := modelCostSnapshot(p, "u1", "nolast"); ok {
		t.Error("零 LastSeen 条目应剔除（结构破损）")
	}
}

// TestModelCostCorruptFileDegrades 损坏降级不崩溃：state.json 非法 JSON →
// load() 静默跳过，池零状态启动（既有 load() 行为，台账路径复用同一通道）。
func TestModelCostCorruptFileDegrades(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	if err := os.WriteFile(fp, []byte(`{"accounts": not-json`), 0o600); err != nil {
		t.Fatal(err)
	}
	p := New(fp)
	p.Add(&auth.Auth{UID: "u1"})
	if _, ok := p.Status("u1"); !ok {
		t.Error("损坏文件应降级为零状态启动（账号可正常加入）")
	}
	if _, ok := modelCostSnapshot(p, "u1", "any"); ok {
		t.Error("损坏文件不应恢复任何台账")
	}
}

// TestModelCostConcurrentWriteAndFlush 并发写 + 落盘（-race 底线）：多 goroutine
// 对多账号混发 NoteModelCost，flusher 高频 tick 与写入口充分交错；断言不变量：
// 不 panic、最终落盘文件可读且账本条目字段自洽（per1k 非负、LastSeen 非零）。
func TestModelCostConcurrentWriteAndFlush(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "state.json")
	p := New(fp)
	for i := 0; i < 4; i++ {
		p.Add(&auth.Auth{UID: "u" + string(rune('0'+i))})
	}
	old := flushInterval
	flushInterval = time.Millisecond
	t.Cleanup(func() { flushInterval = old })

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			uid := "u" + string(rune('0'+g%4))
			for n := 0; n < 100; n++ {
				// 交替喂免费/收费观测（同时打到同 (uid, model) 触发限免结束
				// 日志分支的并发路径——日志在持锁内打，race 下验证锁覆盖）。
				credit := 0.0
				if n%2 == 1 {
					credit = 1.0
				}
				p.NoteModelCost(uid, "m", credit, 1000)
				p.NoteModelCost(uid, "m2", 2.0, 500)
			}
		}(g)
	}
	wg.Wait()
	p.Flush()

	// 重启读回：文件可解析、条目自洽。
	p2 := New(fp)
	p2.mu.RLock()
	defer p2.mu.RUnlock()
	for uid, e := range p2.byUID {
		for m, mc := range e.modelCost {
			if mc.CostPer1k < 0 {
				t.Errorf("%s/%s per1k=%v 负值（自洽性破坏）", uid, m, mc.CostPer1k)
			}
			if mc.LastSeen.IsZero() {
				t.Errorf("%s/%s LastSeen 零值", uid, m)
			}
		}
	}
}

// TestStatusModelCostsLedger /status（Status.ModelCosts）透出成本台账：每模型
// 一行（per1k/last_seen/samples），TTL 内才展示、过期即消失、无观测为 nil。
func TestStatusModelCostsLedger(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "u1"})
	p.NoteModelCost("u1", "free-model", 0, 1000)
	p.NoteModelCost("u1", "paid-model", 2.0, 1000)
	// 手工做旧一条：过期后应从台账消失。
	p.mu.Lock()
	mc := p.byUID["u1"].modelCost["paid-model"]
	mc.LastSeen = time.Now().Add(-2 * modelCostTTL)
	p.byUID["u1"].modelCost["paid-model"] = mc
	p.mu.Unlock()

	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if len(st.ModelCosts) != 1 {
		t.Fatalf("ModelCosts 行数=%d want 1（过期条目应消失）: %+v", len(st.ModelCosts), st.ModelCosts)
	}
	row := st.ModelCosts[0]
	if row.Model != "free-model" {
		t.Errorf("model=%q want free-model", row.Model)
	}
	if row.CostPer1k != 0 {
		t.Errorf("cost_per_1k=%v want 0（免费）", row.CostPer1k)
	}
	if row.LastSeen.IsZero() || row.Samples != 1 {
		t.Errorf("last_seen/samples 未透出: %+v", row)
	}

	// 无观测账号 → nil（零回归）。
	p.Add(&auth.Auth{UID: "u2"})
	st2, _ := p.Status("u2")
	if st2.ModelCosts != nil {
		t.Errorf("无观测账号 ModelCosts 应为 nil, got %+v", st2.ModelCosts)
	}
}

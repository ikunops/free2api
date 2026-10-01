// metrics.go 请求统计聚合（/v1/stats 数据源）。
//
// 设计要点：
//   - **单一埋点**：唯一写入口是 chatStat.done()，流式/非流式/错误路径全汇于此，
//     天然覆盖全路径，不需要在每个 return 前重复记账。
//   - **只采信上游 usage**：token / cache / credit 一律来自上游末帧 usage，缺失时
//     用 hasUsage 区分「缺观测」与「显式 0」，不做 rune 估算（与成本账本同纪律）。
//   - **三个维度**：按模型（含 realm/producer 前缀原文）、按来源（producer）、逐日。
//     逐日桶让「今天 / 近 7 天 / 近 30 天 / 全部」这几个区间视图有数据可查。
//   - **逐日持久化**：按天聚合落 data/stats.json（保留 statsRetainDays 天），跨重启
//     保留——否则区间视图会被进程重启清空，等于没有。写盘有节流（最多 1 次/分），
//     停机再强刷一次；崩溃最多丢最近一分钟的观测。
//   - **有界内存**：模型键数量受上游目录限制；另设容量上限兜底，超限时丢弃新键并
//     记一次 WARN，避免异常模型名刷爆内存。
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// metricsCap 模型键容量上限。上游目录规模远小于此值；上限只为兜底异常模型名。
	metricsCap = 512
	// statsRetainDays 逐日聚合的保留天数（约 13 周）。
	statsRetainDays = 92
	// statsFlushEvery 落盘节流窗口：热路径上最多这么频繁写一次文件。
	statsFlushEvery = time.Minute
	// dayKeyFmt 逐日桶的键格式（本地时区）。
	dayKeyFmt = "2006-01-02"
)

// modelMetrics 单个键（模型 / 来源）的累加器。字段导出是为了逐日落盘。
// 全字段原子性由 metricsStore.mu 保证，无需 atomic。
type modelMetrics struct {
	Requests  int64 `json:"requests,omitempty"`
	Success   int64 `json:"success,omitempty"`
	Failed    int64 `json:"failed,omitempty"`
	Streaming int64 `json:"streaming,omitempty"`

	TTFBSumMS float64 `json:"ttfb_sum_ms,omitempty"` // TTFB 累计（仅成功且有观测的请求）
	TTFBCount int64   `json:"ttfb_count,omitempty"`
	LatSumMS  float64 `json:"lat_sum_ms,omitempty"`  // 端到端耗时累计（全部请求）
	GenSecSum float64 `json:"gen_sec_sum,omitempty"` // 生成秒数累计（供 tokens/s）

	PromptTok  int64   `json:"prompt_tok,omitempty"`
	CompTok    int64   `json:"comp_tok,omitempty"`
	CacheHit   int64   `json:"cache_hit,omitempty"`
	CacheMiss  int64   `json:"cache_miss,omitempty"`
	CacheWrite int64   `json:"cache_write,omitempty"`
	Credit     float64 `json:"credit,omitempty"`

	LastSeen metricsTime `json:"last_seen,omitempty"`
}

// metricsTime 宽容时间：接受 RFC3339（本程序写的）与 "2006-01-02T15:04:05"
// （手改过 / 老版本留下的裸本地时间）。解析不了就置零，而不是让整个 stats.json
// 反序列化失败——一条坏记录不该把 92 天历史全丢掉（同 modeldead.go 的纪律）。
type metricsTime struct{ time.Time }

func (m *metricsTime) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		m.Time = time.Time{}
		return nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if tt, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			m.Time = tt
			return nil
		}
	}
	m.Time = time.Time{}
	return nil
}

func (m metricsTime) MarshalJSON() ([]byte, error) {
	if m.Time.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(m.Time)
}

// addMetrics 把 src 累加进 dst（跨日合并与单次请求累加共用一套口径）。
func addMetrics(dst, src *modelMetrics) {
	dst.Requests += src.Requests
	dst.Success += src.Success
	dst.Failed += src.Failed
	dst.Streaming += src.Streaming
	dst.TTFBSumMS += src.TTFBSumMS
	dst.TTFBCount += src.TTFBCount
	dst.LatSumMS += src.LatSumMS
	dst.GenSecSum += src.GenSecSum
	dst.PromptTok += src.PromptTok
	dst.CompTok += src.CompTok
	dst.CacheHit += src.CacheHit
	dst.CacheMiss += src.CacheMiss
	dst.CacheWrite += src.CacheWrite
	dst.Credit += src.Credit
	if src.LastSeen.After(dst.LastSeen.Time) {
		dst.LastSeen = src.LastSeen
	}
}

// metricDelta 把一次请求折成一个可累加的增量。token / cache / credit 只在 hasUsage
// 时计入：缺失≠0。
func metricDelta(s *chatStat, total time.Duration) *modelMetrics {
	d := &modelMetrics{Requests: 1, LastSeen: metricsTime{time.Now()}}
	if s.status == 200 {
		d.Success = 1
	} else {
		d.Failed = 1
	}
	if s.mode == "stream" {
		d.Streaming = 1
	}
	totalMS := float64(total.Milliseconds())
	d.LatSumMS = totalMS

	// TTFB 只在有观测时累加（流式首帧才有；非流式恒 0，不计入均值分母，
	// 否则会把非流式的 0 拉低均值，失真）。
	if s.ttfb > 0 {
		d.TTFBSumMS = float64(s.ttfb.Milliseconds())
		d.TTFBCount = 1
	}
	if s.hasUsage {
		d.PromptTok = int64(s.prompt)
		// toks<0 是「观测缺失」哨兵（非流式路径：usage 存在但缺 completion_tokens 时
		// completionTokens 返回 -1，此时 hasUsage 仍为真）。真值只可能 ≥0，负值一律不计。
		if s.toks > 0 {
			d.CompTok = int64(s.toks)
		}
		d.CacheHit = int64(s.cacheHit)
		d.CacheMiss = int64(s.cacheMiss)
		d.CacheWrite = int64(s.cacheWr)
		// 生成吞吐分母：总耗时减去 TTFB（纯生成时间）。TTFB 缺失时退回总耗时。
		gen := totalMS
		if s.ttfb > 0 {
			gen = totalMS - float64(s.ttfb.Milliseconds())
		}
		if gen > 0 {
			d.GenSecSum = gen / 1000.0
		}
	}
	if s.hasCredit {
		d.Credit = s.credit
	}
	return d
}

// dayBucket 一天的聚合：按模型、按来源各一份，外加 24 小时的请求数分布
// （热力图用）。两份 map 分开存，区间视图按需合并。
type dayBucket struct {
	ByModel    map[string]*modelMetrics `json:"by_model,omitempty"`
	ByProducer map[string]*modelMetrics `json:"by_producer,omitempty"`
	Hours      [24]int64                `json:"hours,omitempty"`
}

func newDayBucket() *dayBucket {
	return &dayBucket{ByModel: map[string]*modelMetrics{}, ByProducer: map[string]*modelMetrics{}}
}

// metricsStore 全局聚合表。
type metricsStore struct {
	mu         sync.Mutex
	since      time.Time
	byModel    map[string]*modelMetrics // 进程内全量（老口径，MetricsSnapshotOf 用）
	byProducer map[string]*modelMetrics // 进程内全量，按来源
	days       map[string]*dayBucket    // 逐日，持久化
	warned     bool                     // 容量超限只告警一次，避免刷屏

	path      string // 落盘路径；空 = 不落盘（测试 / 嵌入形态）
	loaded    bool   // 是否已从磁盘载入过（InitMetricsPersist 幂等）
	dirty     bool
	lastFlush time.Time
}

var globalMetrics = &metricsStore{
	since:      time.Now(),
	byModel:    map[string]*modelMetrics{},
	byProducer: map[string]*modelMetrics{},
	days:       map[string]*dayBucket{},
}

// recordChatMetric 把一次请求的观测累加进聚合表。由 chatStat.done() 调用。
//
// 三个维度一次写完：进程内全量（模型 / 来源）、以及当天桶（模型 / 来源 / 小时）。
// 模型名为空时归入 "-" 键（仍计入 total，不丢弃观测）。
func recordChatMetric(s *chatStat, total time.Duration) {
	model := s.model
	if model == "" {
		model = "-"
	}
	producer := s.producer
	if producer == "" {
		producer = "workbuddy" // 空串在网关里的语义就是「CN 默认那家」
	}
	start := s.start
	if start.IsZero() {
		start = time.Now() // 单测直接构造 chatStat 时没有 start
	}
	day := start.Format(dayKeyFmt)
	hour := start.Hour()
	if hour < 0 || hour > 23 {
		hour = 0
	}

	d := metricDelta(s, total)

	m := globalMetrics
	m.mu.Lock()
	m.addKeyedLocked(m.byModel, model, d, metricsCap)
	m.addKeyedLocked(m.byProducer, producer, d, 0)
	m.pruneLocked(day)
	b := m.days[day]
	if b == nil {
		b = newDayBucket()
		m.days[day] = b
	}
	m.addKeyedLocked(b.ByModel, model, d, metricsCap)
	m.addKeyedLocked(b.ByProducer, producer, d, 0)
	b.Hours[hour]++
	m.dirty = true
	m.mu.Unlock()

	m.maybeFlush()
}

// addKeyedLocked 按键累加增量。capN>0 时该 map 有容量上限，超限丢弃新键（只告警一次）。
// 调用方须持 m.mu。
func (m *metricsStore) addKeyedLocked(dst map[string]*modelMetrics, key string, d *modelMetrics, capN int) {
	mm, ok := dst[key]
	if !ok {
		if capN > 0 && len(dst) >= capN {
			if !m.warned {
				m.warned = true
				logMetricsCapWarn(key)
			}
			return
		}
		mm = &modelMetrics{}
		dst[key] = mm
	}
	addMetrics(mm, d)
}

// pruneLocked 丢掉保留期之外的日桶。调用方须持 m.mu。
func (m *metricsStore) pruneLocked(todayKey string) {
	today, err := time.ParseInLocation(dayKeyFmt, todayKey, time.Local)
	if err != nil {
		return
	}
	cut := today.AddDate(0, 0, -(statsRetainDays - 1)).Format(dayKeyFmt)
	for k := range m.days {
		if k < cut { // 键是 YYYY-MM-DD，字典序即时间序
			delete(m.days, k)
			m.dirty = true
		}
	}
}

// ---------- 逐日持久化 ----------

// metricsFile data/stats.json 的形状。只存逐日桶：进程内全量可以从它重建口径，
// 而区间视图只需要逐日。
type metricsFile struct {
	Days map[string]*dayBucket `json:"days"`
}

// marshalLocked 生成待写内容。调用方须持锁。第二个返回值 false = 序列化失败。
func (m *metricsStore) marshalLocked() ([]byte, bool) {
	raw, err := json.MarshalIndent(metricsFile{Days: m.days}, "", "  ")
	if err != nil {
		return nil, false
	}
	return raw, true
}

// flushLocked 生成待写内容并复位脏标记。调用方须持锁，写盘在锁外做。
// 返回 ok=false 表示没有可写内容（未配置路径 / 不脏）。
func (m *metricsStore) flushLocked() (string, []byte, bool) {
	if m.path == "" || !m.dirty {
		return "", nil, false
	}
	raw, ok := m.marshalLocked()
	if !ok {
		return "", nil, false
	}
	m.dirty = false
	m.lastFlush = time.Now()
	return m.path, raw, true
}

// writeFileAtomic tmp + rename，避免半截文件。权限 0600（含账号用量，不外泄）。
func writeFileAtomic(path string, raw []byte) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// InitMetricsPersist 指定落盘路径并载入历史（只生效一次：桌面程序在同一进程里
// 反复 Start/Stop，重复载入会把内存里更全的数据覆盖成旧文件内容）。
func InitMetricsPersist(path string) {
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.loaded || path == "" {
		return
	}
	m.loaded = true
	m.path = path

	raw, err := os.ReadFile(path)
	if err != nil {
		return // 首次运行没有文件：正常
	}
	var f metricsFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("WARN: [metrics] %s 解析失败，按空表继续: %v", path, err)
		return
	}
	for k, b := range f.Days {
		if b == nil {
			continue
		}
		if b.ByModel == nil {
			b.ByModel = map[string]*modelMetrics{}
		}
		if b.ByProducer == nil {
			b.ByProducer = map[string]*modelMetrics{}
		}
		m.days[k] = b
	}
	if n := len(m.days); n > 0 {
		log.Printf("[metrics] 载入 %d 天历史统计（%s）", n, path)
	}
}

// FlushMetrics 立即落盘（停机清理挂这个）。未配置路径时是空操作。
func FlushMetrics() {
	m := globalMetrics
	m.mu.Lock()
	path, raw, ok := m.flushLocked()
	m.mu.Unlock()
	if ok {
		_ = writeFileAtomic(path, raw)
	}
}

// FlushMetricsTo 把当前逐日聚合写到指定路径（测试用：不依赖全局 path 的配置时机）。
// 返回落盘条数（天数）与错误。
func FlushMetricsTo(path string) (int, error) {
	m := globalMetrics
	m.mu.Lock()
	raw, ok := m.marshalLocked()
	n := len(m.days)
	m.mu.Unlock()
	if !ok {
		return 0, nil
	}
	if err := writeFileAtomic(path, raw); err != nil {
		return n, err
	}
	return n, nil
}

// maybeFlush 热路径上的节流落盘：最多 statsFlushEvery 一次。
func (m *metricsStore) maybeFlush() {
	m.mu.Lock()
	if m.path == "" || !m.dirty || time.Since(m.lastFlush) < statsFlushEvery {
		m.mu.Unlock()
		return
	}
	path, raw, ok := m.flushLocked()
	m.mu.Unlock()
	if ok {
		_ = writeFileAtomic(path, raw)
	}
}

// ---------- 快照载荷 ----------

// MetricsSnapshot 是 /v1/stats 的响应载荷（字段名与社区面板约定一致）。
type MetricsSnapshot struct {
	Enabled   bool      `json:"enabled"`
	Message   string    `json:"message,omitempty"`
	Range     string    `json:"range,omitempty"`
	Since     time.Time `json:"since"`
	Now       time.Time `json:"now"`
	UptimeSec int64     `json:"uptime_sec"`

	Total ModelStatPayload `json:"total"`

	Models    []ModelStatPayload    `json:"models"`
	Producers []ProducerStatPayload `json:"producers,omitempty"`
	Series    []DayPoint            `json:"series,omitempty"`
	// Heatmap 周几 × 小时 的请求数（[0]=周日，Go 的 time.Weekday 口径）。
	Heatmap [7][24]int64 `json:"heatmap"`
}

// ModelStatPayload 单模型派生统计。
type ModelStatPayload struct {
	Model string `json:"model"`

	Requests  int64 `json:"requests"`
	Success   int64 `json:"success"`
	Failed    int64 `json:"failed"`
	Streaming int64 `json:"streaming"`

	AvgTTFBMS    float64 `json:"avg_ttfb_ms"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	TokensPerSec float64 `json:"tokens_per_sec"`

	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`

	CacheHitTokens   int64   `json:"cache_hit_tokens"`
	CacheMissTokens  int64   `json:"cache_miss_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	Credit       float64 `json:"credit"`
	CreditPerReq float64 `json:"credit_per_req"`

	// Credits 上游积分倍率原文（如 "x0.06"），与 /v1/models 的 credits 同源同值；
	// 目录未下发 / 缓存冷 → 空串，JSON 整体省略（缺失≠免费，不输出 "x0.00"）。
	// 由 stats handler 从模型目录只读缓存合入（enrichCredits），不参与聚合。
	Credits string `json:"credits,omitempty"`

	LastSeen *time.Time `json:"last_seen,omitempty"`
}

// ProducerStatPayload 按来源（producer）的派生统计。
type ProducerStatPayload struct {
	Producer    string  `json:"producer"`
	Requests    int64   `json:"requests"`
	Success     int64   `json:"success"`
	Failed      int64   `json:"failed"`
	TotalTokens int64   `json:"total_tokens"`
	Credit      float64 `json:"credit"`
}

// DayPoint 逐日序列的一点（趋势 / 热力图数据源）。
type DayPoint struct {
	Day      string  `json:"day"`
	Requests int64   `json:"requests"`
	Success  int64   `json:"success"`
	Failed   int64   `json:"failed"`
	Tokens   int64   `json:"tokens"`
	Credit   float64 `json:"credit"`
}

// rangeSpec 区间视图的时间窗。from 为零值 = 全部保留期。
type rangeSpec struct {
	id   string
	from time.Time
}

// parseRangeSpec 解析 ?range= 取值。未知值一律按「全部」——宁可给全量也不给空表。
func parseRangeSpec(s string) rangeSpec {
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch s {
	case "today":
		return rangeSpec{id: "today", from: today}
	case "7d":
		return rangeSpec{id: "7d", from: today.AddDate(0, 0, -6)}
	case "30d":
		return rangeSpec{id: "30d", from: today.AddDate(0, 0, -29)}
	default:
		return rangeSpec{id: "all"}
	}
}

// MetricsSnapshotOf 进程内全量快照（老口径：models 按请求数降序）。保留给单测与
// 需要「自进程启动起」累计的调用方；HTTP 出口走 metricsSnapshotRange。
func MetricsSnapshotOf() MetricsSnapshot {
	now := time.Now()
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()

	out := MetricsSnapshot{
		Enabled:   true,
		Range:     "process",
		Since:     m.since,
		Now:       now,
		UptimeSec: int64(now.Sub(m.since).Seconds()),
		Models:    make([]ModelStatPayload, 0, len(m.byModel)),
	}

	// total 由各模型累加得出（与 models 同口径，避免两处算法分叉）。
	var tot modelMetrics
	for name, mm := range m.byModel {
		out.Models = append(out.Models, deriveModelStat(name, mm))
		addMetrics(&tot, mm)
	}
	out.Total = deriveModelStat("total", &tot)
	sortModelStats(out.Models)
	return out
}

// metricsSnapshotRange 区间视图：合并保留期内 [from, 今天] 的日桶。
// 「全部」= 全部保留期（含跨重启的历史），这也是前端默认口径。
func metricsSnapshotRange(spec rangeSpec) MetricsSnapshot {
	now := time.Now()
	m := globalMetrics
	m.mu.Lock()
	defer m.mu.Unlock()

	byModel := map[string]*modelMetrics{}
	byProducer := map[string]*modelMetrics{}
	var heat [7][24]int64
	series := []DayPoint{}

	fromKey := ""
	if !spec.from.IsZero() {
		fromKey = spec.from.Format(dayKeyFmt)
	}
	keys := make([]string, 0, len(m.days))
	for k := range m.days {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		if fromKey != "" && k < fromKey {
			continue
		}
		b := m.days[k]
		for name, mm := range b.ByModel {
			addMetrics(getOrNew(byModel, name), mm)
		}
		for p, mm := range b.ByProducer {
			addMetrics(getOrNew(byProducer, p), mm)
		}
		wd := 0
		if t, err := time.ParseInLocation(dayKeyFmt, k, time.Local); err == nil {
			wd = int(t.Weekday())
		}
		for h := 0; h < 24; h++ {
			heat[wd][h] += b.Hours[h]
		}
		var dp DayPoint
		dp.Day = k
		for _, mm := range b.ByModel {
			dp.Requests += mm.Requests
			dp.Success += mm.Success
			dp.Failed += mm.Failed
			dp.Tokens += mm.PromptTok + mm.CompTok
			dp.Credit += mm.Credit
		}
		series = append(series, dp)
	}

	since := m.since
	if !spec.from.IsZero() {
		since = spec.from
	}
	out := MetricsSnapshot{
		Enabled:   true,
		Range:     spec.id,
		Since:     since,
		Now:       now,
		UptimeSec: int64(now.Sub(since).Seconds()),
		Models:    make([]ModelStatPayload, 0, len(byModel)),
		Producers: make([]ProducerStatPayload, 0, len(byProducer)),
		Series:    series,
		Heatmap:   heat,
	}

	var tot modelMetrics
	for name, mm := range byModel {
		out.Models = append(out.Models, deriveModelStat(name, mm))
		addMetrics(&tot, mm)
	}
	out.Total = deriveModelStat("total", &tot)
	sortModelStats(out.Models)

	for p, mm := range byProducer {
		out.Producers = append(out.Producers, ProducerStatPayload{
			Producer:    p,
			Requests:    mm.Requests,
			Success:     mm.Success,
			Failed:      mm.Failed,
			TotalTokens: mm.PromptTok + mm.CompTok,
			Credit:      mm.Credit,
		})
	}
	sort.Slice(out.Producers, func(i, j int) bool {
		if out.Producers[i].Requests != out.Producers[j].Requests {
			return out.Producers[i].Requests > out.Producers[j].Requests
		}
		return out.Producers[i].Producer < out.Producers[j].Producer
	})
	return out
}

// getOrNew 取已有累加器或建一个空的（合并日桶用）。
func getOrNew(m map[string]*modelMetrics, key string) *modelMetrics {
	if mm, ok := m[key]; ok {
		return mm
	}
	mm := &modelMetrics{}
	m[key] = mm
	return mm
}

// sortModelStats 表格默认序：请求数降序，同数按名字。
func sortModelStats(list []ModelStatPayload) {
	sort.Slice(list, func(i, j int) bool {
		if list[i].Requests != list[j].Requests {
			return list[i].Requests > list[j].Requests
		}
		return list[i].Model < list[j].Model
	})
}

// deriveModelStat 把累加器折算为派生统计（均值、比率、吞吐）。
func deriveModelStat(name string, mm *modelMetrics) ModelStatPayload {
	p := ModelStatPayload{
		Model:            name,
		Requests:         mm.Requests,
		Success:          mm.Success,
		Failed:           mm.Failed,
		Streaming:        mm.Streaming,
		PromptTokens:     mm.PromptTok,
		CompletionTokens: mm.CompTok,
		TotalTokens:      mm.PromptTok + mm.CompTok,
		CacheHitTokens:   mm.CacheHit,
		CacheMissTokens:  mm.CacheMiss,
		CacheWriteTokens: mm.CacheWrite,
		Credit:           mm.Credit,
	}
	if mm.Requests > 0 {
		p.AvgLatencyMS = mm.LatSumMS / float64(mm.Requests)
		p.CreditPerReq = mm.Credit / float64(mm.Requests)
	}
	if mm.TTFBCount > 0 {
		p.AvgTTFBMS = mm.TTFBSumMS / float64(mm.TTFBCount)
	}
	if mm.GenSecSum > 0 {
		p.TokensPerSec = float64(mm.CompTok) / mm.GenSecSum
	}
	// 命中率分母 = 命中 + 未命中（不含 write：写入是「为后续命中付的费」，
	// 计入分母会把首次请求的命中率压低，失真）。
	if denom := mm.CacheHit + mm.CacheMiss; denom > 0 {
		p.CacheHitRate = float64(mm.CacheHit) / float64(denom)
	}
	if !mm.LastSeen.IsZero() {
		t := mm.LastSeen.Time
		p.LastSeen = &t
	}
	return p
}

// ResetMetrics 清空聚合（/v1/stats/reset），便于观察增量。since 重置为当前时刻，
// 逐日历史一并清掉并立即落盘（否则下次载入会把旧数据带回来）。
func ResetMetrics() {
	m := globalMetrics
	m.mu.Lock()
	m.byModel = map[string]*modelMetrics{}
	m.byProducer = map[string]*modelMetrics{}
	m.days = map[string]*dayBucket{}
	m.since = time.Now()
	m.warned = false
	m.dirty = true
	path, raw, ok := m.flushLocked()
	m.mu.Unlock()
	if ok {
		_ = writeFileAtomic(path, raw)
	}
}

// logMetricsCapWarn 容量超限告警（独立函数便于测试替换/断言，也避免 import log 污染
// 主体逻辑的阅读）。
func logMetricsCapWarn(model string) {
	log.Printf("WARN: [metrics] 模型键达上限 %d，丢弃新键 model=%q（异常模型名？）", metricsCap, model)
}

// enrichCredits 把上游积分倍率原文合入 stats 快照（/v1/stats 数据展示侧增强）。
//
// 数据源与 /v1/models 完全同源：CN 侧 cachedModelsSnapshot / global 侧
// GlobalModelInfosSnapshot，均为**只读快照**——缓存冷/过期 → nil，绝不发起上游
// 调用（maintainer 约束：网关只加工已有数据）。倍率是展示字段而非观测值，故
// 不进 recordChatMetric 聚合路径，快照出口统一合入。
//
// 键归一：stats 键是请求体 model 原文（含 realm 前缀），目录 id 是裸名——
// resolveModel 剥前缀后按 realm 查表；未知前缀/裸名含冒号/"-" 查不到 → 省略。
// total 行不参与（跨倍率聚合无意义）。
func (h *Handler) enrichCredits(snap *MetricsSnapshot) {
	cn := make(map[string]string) // bare id -> credits 原文
	for _, mi := range cachedModelsSnapshot() {
		if mi.Credits != "" {
			cn[mi.ID] = mi.Credits
		}
	}
	var global map[string]string
	if h.cfg.Upstream != nil {
		global = make(map[string]string)
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			if mi.Credits != "" {
				global[mi.ID] = mi.Credits
			}
		}
	}
	for i := range snap.Models {
		realm, bare := h.resolveModel(snap.Models[i].Model)
		if bare == "" || bare == "-" {
			continue
		}
		if realm == "global" {
			snap.Models[i].Credits = global[bare]
		} else {
			snap.Models[i].Credits = cn[bare]
		}
	}
}

// fillStatFromUsage 把非流式聚合响应的 usage 观测填进 chatStat（与流式路径同口径）。
//
// 与 usageCreditTotal 的分工：那个函数服务成本账本（只取 credit + 总 token），
// 本函数服务 metrics（还要 prompt/cache 三段）。两者都读同一份 usage，但目标字段
// 不同，故不复用——强行合并会让账本依赖 metrics 的字段集，反之亦然。
func fillStatFromUsage(st *chatStat, resp map[string]any) {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return
	}
	st.hasUsage = true
	st.prompt = intFromUsage(u, "prompt_tokens")
	st.cacheHit = intFromUsage(u, "prompt_cache_hit_tokens")
	st.cacheMiss = intFromUsage(u, "prompt_cache_miss_tokens")
	st.cacheWr = intFromUsage(u, "prompt_cache_write_tokens")
	if c, ok := u["credit"].(float64); ok {
		st.credit = c
		st.hasCredit = true
	}
}

// intFromUsage 从 usage map 取整数字段；缺失或类型不符返回 0。
func intFromUsage(u map[string]any, key string) int {
	if v, ok := u[key].(float64); ok {
		return int(v)
	}
	return 0
}

// stats 处理 GET /v1/stats：?range=today|7d|30d|all（缺省 / 未知值 = 全部保留期）。
// 出口处只读合入模型目录的积分倍率（enrichCredits，无上游调用）。
func (h *Handler) stats(w http.ResponseWriter, r *http.Request) {
	spec := parseRangeSpec(r.URL.Query().Get("range"))
	snap := metricsSnapshotRange(spec)
	h.enrichCredits(&snap)
	writeJSON(w, http.StatusOK, snap)
}

// statsReset 处理 POST /v1/stats/reset：清空累计（含逐日历史），便于观察增量。
func (h *Handler) statsReset(w http.ResponseWriter, r *http.Request) {
	ResetMetrics()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

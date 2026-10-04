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
//   - **从历史日志回填**：stats.json 是随本特性一起引入的，之前的请求只留在网关
//     自己的流水日志里（stdout.log / server.out.log，含轮转的 .prevN）。启动时扫一遍
//     这些日志把可解析的行补进逐日桶（BackfillFromLogs），否则「近 7 天 / 全部」只剩
//     启用之后的那几天。回填只补 stats.json 里没有的日子，幂等，不会重复计数。
//   - **有界内存**：模型键数量受上游目录限制；另设容量上限兜底，超限时丢弃新键并
//     记一次 WARN，避免异常模型名刷爆内存。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
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

// ---------- 短窗口速率（实时） ----------
//
// 为什么需要它：/v1/stats 里的成功率 / 缓存命中率 / 平均延迟 / tokens_per_sec
// 全都是「区间均值」——它们天生迟钝，区间越长越迟钝（实测一次 51s 的请求里
// total_tokens 51 秒只跳了 4 次）。这不是 bug，是均值该有的样子。
//
// 但「现在跑多快」这个问题，均值答不了。答它的是短窗口：最近 60 秒里
// 实际发生了什么。窗口是滚动的（tumbling，按到达顺序切），不是全局累计，
// 所以数字会随每个请求落地而变化——这才是页面该拿来表达「实时」的那类量。
//
// 内存有界：只保留最近 liveWindowMax 个观测点，超出就丢最旧的
// （而不是无限增长）。60s 内最多能记多少请求取决于实际负载，
// 128 个点足够覆盖「一秒多请求」的极端情况。

const (
	// liveWindowSec 速率窗口长度（秒）。
	liveWindowSec = 60
	// liveWindowMax 窗口内最多保留的观测点数。有界内存：超出丢最旧。
	liveWindowMax = 128
)

// livePoint 窗口里的一个观测点（一次请求结束时记一笔）。
type livePoint struct {
	at       time.Time
	compTok  int64
	genSec   float64 // 生成阶段秒数；tokens/s = compTok / ΣgenSec
	ok       bool
	latencyMS float64
}

// liveWindow 固定长度的滚动窗口。零值不可用，必须 newLiveWindow。
type liveWindow struct {
	pts []livePoint // 环形缓冲，按 at 升序（旧 → 新）
}

func newLiveWindow() *liveWindow { return &liveWindow{pts: make([]livePoint, 0, liveWindowMax)} }

// add 记一笔并淘汰过期点。调用方须持 m.mu。
func (w *liveWindow) add(p livePoint) {
	w.pts = append(w.pts, p)
	if len(w.pts) > liveWindowMax {
		w.pts = w.pts[len(w.pts)-liveWindowMax:]
	}
}

// prune 淘汰早于 cutoff 的点（每次取快照时调一次；add 时也顺手做）。
func (w *liveWindow) prune(cutoff time.Time) {
	i := 0
	for i < len(w.pts) && w.pts[i].at.Before(cutoff) {
		i++
	}
	if i > 0 {
		w.pts = append(w.pts[:0], w.pts[i:]...)
	}
}

// liveRate 把窗口折算成速率快照。
type liveRate struct {
	WindowSec    int     `json:"window_sec"`
	Samples      int     `json:"samples"`
	Requests     int64   `json:"requests"`
	Success      int64   `json:"success"`
	Failed       int64   `json:"failed"`
	CompTok      int64   `json:"completion_tokens"`
	TokensPerSec float64 `json:"tokens_per_sec"`
	AvgLatencyMS float64 `json:"avg_latency_ms"`
	SuccessRate  float64 `json:"success_rate"`
	LastAt       *time.Time `json:"last_at,omitempty"`
}

// snapshot 折算当前窗口。cutoff 之前的不算。
func (w *liveWindow) snapshot(now time.Time) liveRate {
	if w == nil {
		return liveRate{WindowSec: liveWindowSec} // 零值 store：空窗口，不是崩溃
	}
	cutoff := now.Add(-liveWindowSec * time.Second)
	w.prune(cutoff)
	out := liveRate{WindowSec: liveWindowSec}
	var genSec, latSum float64
	var last *time.Time
	for i := range w.pts {
		p := w.pts[i]
		out.Samples++
		out.Requests++
		if p.ok {
			out.Success++
		} else {
			out.Failed++
		}
		out.CompTok += p.compTok
		genSec += p.genSec
		latSum += p.latencyMS
		t := p.at
		last = &t
	}
	if genSec > 0 {
		out.TokensPerSec = float64(out.CompTok) / genSec
	}
	if out.Requests > 0 {
		out.AvgLatencyMS = latSum / float64(out.Requests)
		out.SuccessRate = float64(out.Success) / float64(out.Requests)
	}
	out.LastAt = last
	return out
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
	// Partial 标记这天的输入侧统计不完整（回填日志时该日的旧格式行
	// 没有 ptok，输入 token 永远是 0）。前端据此把那些天的 token
	// 标为「下限」，而不是把它当成完整口径。
	Partial    bool                     `json:"partial,omitempty"`
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

	// live 是「最近 liveWindowSec 秒」的滚动窗口，供实时速率用。
	// 与 byModel/days 那些「区间累计」并存：前者答「现在多快」，后者答「一共多少」。
	live *liveWindow

	// changed 是 SSE 的信号量（容量 1），不是事件队列：
	// 有请求落地就敲一下，正在推的连接会被唤醒；没人听时信号直接丢掉，
	// 下一个连上来的连接会先收到一帧全量快照，所以不丢信息。
	// 容量 1 的非阻塞发送保证高频请求不会把信号堆成内存泄漏。
	changed chan struct{}
}

var globalMetrics = &metricsStore{
	since:      time.Now(),
	byModel:    map[string]*modelMetrics{},
	byProducer: map[string]*modelMetrics{},
	days:       map[string]*dayBucket{},
	live:       newLiveWindow(),
	changed:    make(chan struct{}, 1),
}

// notifyChanged 通知所有 SSE 订阅者「有新的观测落地了」。非阻塞。
// 调用方不必持 m.mu（在解锁之后调），所以不会被慢订阅者拖住写路径。
func (m *metricsStore) notifyChanged() {
	select {
	case m.changed <- struct{}{}:
	default: // 已有一个待处理信号，下游会读到更新后的快照，无需再敲
	}
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
	// 短窗口速率：同一次观测再记一份到滚动窗口，回答「现在多快」。
	// 与上面的区间累计是同一笔数据、两种口径——不是重复统计。
	if m.live == nil {
		m.live = newLiveWindow() // 零值 metricsStore（部分测试直接构造）自愈
	}
	m.live.add(livePoint{
		at:        start,
		compTok:   d.CompTok,
		genSec:    d.GenSecSum,
		ok:        s.status >= 200 && s.status < 300,
		latencyMS: float64(total.Milliseconds()),
	})
	m.mu.Unlock()

	// 通知放在解锁之后：SSE 订阅者读快照要抢同一把锁，锁内通知等于把
	// 每个请求的收尾都绑在订阅者的 marshal 速度上。
	m.notifyChanged()
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

// ---------- 历史日志回填 ----------
//
// 背景：/v1/stats 的逐日桶是后加的，上线之前的请求只存在于网关自己的流水日志里
// （logChatRow 输出的 "| #NNN | HH:MM:SS | model | mode | status | acct | TTFB=… |
// tok=… | …tok/s | total=…s |"）。这些行含日期以外的全部所需字段，足以重建「每天
// 每模型多少请求、成功/失败、token、耗时」。不读它们的话，用户会看到「明明跑了
// 好几天，近 7 天/全部却只有今天」。
//
// 纪律：
//   - 只补 stats.json 里**还没有的日桶**（已有那天不碰），所以幂等，重启多少次都一样；
//   - 日志只有时间没有日期：按文件 mtime 反推日期，逐行往前遇到「时间倒退」就跨天；
//   - 只解析得出请求数 / 成功失败 / 流式 / 耗时 / token；缓存与积分日志里没有，留空
//     （缺失≠0，不编造）；
//   - 上限 backfillMaxLines 行，避免超大日志拖慢启动；解析失败的行静默跳过。
const (
	// backfillMaxLines 单次回填最多读多少行（含所有候选文件），兜底异常大日志。
	backfillMaxLines = 200000
	// backfillMaxFiles 单目录最多扫多少个候选日志文件。
	backfillMaxFiles = 32
)

// chatRowRe 匹配 logChatRow 的输出行。列宽是显示宽度补齐的，故一律 \s* 宽松匹配；
// 模型名不含 '|'，账号标签不含 '|'，用它做分隔安全。
var chatRowRe = regexp.MustCompile(
	`^\|\s*#\d+\s*\|\s*(\d{1,2}):(\d{2}):(\d{2})\s*\|\s*([^|]*?)\s*\|\s*(stream|sync)\s*\|\s*(\d{3})\s*\|\s*[^|]*\|\s*TTFB=([^|]*?)\s*\|\s*tok=([^|]*?)\s*\|\s*(?:ptok=([^|]*?)\s*\|\s*)?([^|]*?)\s*\|\s*total=([0-9.]+)s\s*\|`)

// backfillLogNames 候选日志文件名（按此顺序扫）。stdout.log 是主流水，
// server.out.log 是重定向形态；.prevN 是轮转备份。
//
// dir 可以是多个目录：流水日志的落点在不同形态下不一样——命令行形态重定向到
// ./data/server.out.log，桌面程序则写 ./data/desktop.log，而宿主控制台的 stdout
// 落在应用根目录的 stdout.log。都扫一遍才不会漏。重复路径由调用方去重。
func backfillLogNames(dirs []string) []string {
	base := []string{"stdout.log", "server.out.log", "desktop.log"}
	out := make([]string, 0, len(base)*4*len(dirs))
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		for _, b := range base {
			out = append(out, filepath.Join(dir, b))
			out = append(out, filepath.Join(dir, b+".prev"))
			for i := 1; i <= 3; i++ {
				out = append(out, filepath.Join(dir, fmt.Sprintf("%s.prev%d", b, i)))
			}
		}
	}
	return out
}

// parseChatRow 解析一行流水日志。ok=false 表示这行不是请求行 / 解析失败。
// 返回值：当天内的时刻、模型、模式、状态码、输出 token（<0 = 缺失）、
// 输入 token（<0 = 缺失；旧格式行没有 ptok 列）、TTFB、总耗时。
func parseChatRow(line string) (clock time.Time, model, mode string, status, toks, ptoks int, ttfb, total time.Duration, ok bool) {
	m := chatRowRe.FindStringSubmatch(line)
	if m == nil {
		return
	}
	h, err1 := strconv.Atoi(m[1])
	mi, err2 := strconv.Atoi(m[2])
	se, err3 := strconv.Atoi(m[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return
	}
	if h > 23 || mi > 59 || se > 59 {
		return
	}
	st, err := strconv.Atoi(m[6])
	if err != nil {
		return
	}
	tk := -1
	if v, err := strconv.Atoi(strings.TrimSpace(m[8])); err == nil {
		tk = v
	}
	// ptok 是后加列：旧格式行没有这个分组，m[9] 为空 → 保持 -1。
	ptk := -1
	if v, err := strconv.Atoi(strings.TrimSpace(m[9])); err == nil {
		ptk = v
	}
	var ttfbD time.Duration
	if s := strings.TrimSpace(m[7]); s != "" && s != "-" {
		if d, err := time.ParseDuration(s); err == nil {
			ttfbD = d
		}
	}
	fs, err := strconv.ParseFloat(m[11], 64)
	if err != nil {
		return
	}
	clock = time.Date(2000, 1, 1, h, mi, se, 0, time.Local)
	model = strings.TrimSpace(m[4])
	mode = m[5]
	status = st
	toks = tk
	ptoks = ptk
	ttfb = ttfbD
	total = time.Duration(fs * float64(time.Second))
	ok = true
	return
}

// modelProducerFromName 从模型名推断 producer（回填日志里没记来源，只有模型名）。
// 显式前缀优先（zcode:… / opencode:… 等）；裸名回落到 workbuddy（历史日志绝大多数是
// 它，且这是网关的默认来源口径）。
func modelProducerFromName(model string) string {
	_, producer, _ := resolveModelRoute(model)
	if producer != "" {
		return producer
	}
	return "workbuddy"
}

// logTimeToDay 把「文件 mtime 的日期 + 行内时刻」折成该行所属日期。
// 逐行调用时用 prev 时刻检测跨天：行内时刻比上一行（更晚的行）还大 → 往前退一天。
func logTimeToDay(day time.Time, clock time.Time) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), clock.Hour(), clock.Minute(), clock.Second(), 0, time.Local)
}

// BackfillFromLogs 扫 dirs 下的网关流水日志，把 stats.json 里**还没有的日桶**补上。
// 返回补进去的天数与解析到的行数。幂等：同一天只要已有桶就整日跳过。
//
// 为什么整日跳过而不是逐条去重：日志行没有唯一 id 可与 stats.json 对齐，逐条比对
// 不可行；而「这天已经统计过」是可靠信号（那天的请求在发生时就已经记账了）。
func BackfillFromLogs(dirs ...string) (days, rows int) {
	if len(dirs) == 0 {
		return 0, 0
	}
	type dayAgg struct {
		byModel    map[string]*modelMetrics
		byProducer map[string]*modelMetrics
		hours      [24]int64
		partial    bool // 至少一行缺输入 token 观测 → 这天输入侧不完整
	}
	found := map[string]*dayAgg{}

	seen := map[string]bool{}
	files := 0
	for _, p := range backfillLogNames(dirs) {
		if files >= backfillMaxFiles || rows >= backfillMaxLines {
			break
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		fi, err := os.Stat(p)
		if err != nil || fi.IsDir() {
			continue
		}
		files++
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		var lines []string
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			l := sc.Text()
			if len(l) > 0 && l[0] == '|' {
				lines = append(lines, l)
			}
		}
		f.Close()
		if len(lines) == 0 {
			continue
		}
		// 反推日期：最后一行落在文件 mtime 那天，逐行往前，时刻变大即跨天。
		day := fi.ModTime()
		type stamped struct {
			at     time.Time
			model  string
			mode   string
			stat   int
			toks   int
			ptoks  int // 输入 token；<0 = 旧格式行，无观测
			ttfb   time.Duration
			total  time.Duration
		}
		stampedRows := make([]stamped, 0, len(lines))
		var prevClock time.Time
		first := true
		for i := len(lines) - 1; i >= 0; i-- {
			clock, model, mode, st, tk, ptk, ttfb, total, ok := parseChatRow(lines[i])
			if !ok {
				continue
			}
			if !first && clock.After(prevClock) {
				day = day.AddDate(0, 0, -1)
			}
			first = false
			prevClock = clock
			stampedRows = append(stampedRows, stamped{
				at: logTimeToDay(day, clock), model: model, mode: mode,
				stat: st, toks: tk, ptoks: ptk, ttfb: ttfb, total: total,
			})
			rows++
			if rows >= backfillMaxLines {
				break
			}
		}
		// stampedRows 是从后往前收集的，累加顺序无所谓（纯求和），直接遍历。
		for _, r := range stampedRows {
			k := r.at.Format(dayKeyFmt)
			a := found[k]
			if a == nil {
				a = &dayAgg{byModel: map[string]*modelMetrics{}, byProducer: map[string]*modelMetrics{}}
				found[k] = a
			}
			mm := r.model
			if mm == "" {
				mm = "-"
			}
			d := &modelMetrics{Requests: 1, LatSumMS: float64(r.total.Milliseconds()),
				LastSeen: metricsTime{r.at}}
			if r.stat == 200 {
				d.Success = 1
			} else {
				d.Failed = 1
			}
			if r.mode == "stream" {
				d.Streaming = 1
			}
			if r.ttfb > 0 {
				d.TTFBSumMS = float64(r.ttfb.Milliseconds())
				d.TTFBCount = 1
			}
			if r.toks >= 0 {
				// 日志的 tok= 是完成 token（不含 prompt），按同一口径记入 CompTok；
				// 生成秒数用「总耗时 - TTFB」近似，好让 tokens/s 有个分母。
				d.CompTok = int64(r.toks)
				gen := r.total
				if r.ttfb > 0 {
					gen = r.total - r.ttfb
				}
				if gen > 0 {
					d.GenSecSum = gen.Seconds()
				}
			}
			// ptok= 后加列：有观测就记入输入侧，没有则标记这天不完整。
			if r.ptoks >= 0 {
				d.PromptTok = int64(r.ptoks)
			} else {
				a.partial = true
			}
			addMetrics(getOrNew(a.byModel, mm), d)
			addMetrics(getOrNew(a.byProducer, modelProducerFromName(r.model)), d)
			a.hours[r.at.Hour()]++
		}
	}

	if len(found) == 0 {
		return 0, rows
	}

	m := globalMetrics
	m.mu.Lock()
	// 只补还没有的日：已有那天说明统计已经覆盖，不重复计。
	added := 0
	for k, a := range found {
		if _, exists := m.days[k]; exists {
			continue
		}
		b := newDayBucket()
		for mk, mm := range a.byModel {
			b.ByModel[mk] = mm
		}
		for pk, pm := range a.byProducer {
			b.ByProducer[pk] = pm
		}
		b.Hours = a.hours
		b.Partial = a.partial
		m.days[k] = b
		added++
	}
	if added > 0 {
		m.pruneLocked(time.Now().Format(dayKeyFmt))
		m.dirty = true
	}
	path, raw, flush := m.flushLocked()
	m.mu.Unlock()
	if flush {
		_ = writeFileAtomic(path, raw)
	}
	return added, rows
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
	// PartialDays 区间内「缺输入 token 观测」的天数：>0 时总 token
	// 只是下限（那几天的输入侧无法回填，日志里就没记）。
	PartialDays int `json:"partial_days"`

	// Live 最近 liveWindowSec 秒的滚动速率（不随区间变）。区间均值天生迟钝，
	// 「现在跑多快」只能由短窗口回答；页面拿它做实时展示。
	Live liveRate `json:"live"`

	// LiveModels 此刻在跑的模型 -> 在飞请求数（与 /status.live_models 同源）。
	// 放进快照的理由：桌面悬浮窗只连一条 SSE 就能拿到「在飞 + 速率 + 累计」三样，
	// 不必为了「正在跑什么」再开一路 2s 轮询 /status（那是几百 KB 的账号全量）。
	// 零值时省略，避免每帧多带一个空对象。
	LiveModels map[string]int `json:"live_models,omitempty"`
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
	// Partial 同 dayBucket.Partial：这天的 token 只含输出侧，不是完整口径。
	Partial  bool    `json:"partial,omitempty"`
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
		Live:      m.live.snapshot(now),
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
	partialDays := 0
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
		var dayPrompt, dayComp int64
		for _, mm := range b.ByModel {
			dp.Requests += mm.Requests
			dp.Success += mm.Success
			dp.Failed += mm.Failed
			dp.Tokens += mm.PromptTok + mm.CompTok
			dp.Credit += mm.Credit
			dayPrompt += mm.PromptTok
			dayComp += mm.CompTok
		}
		// 这天标不完整的两种来源：
		//   - b.Partial：本次回填时就发现有行缺 ptok；
		//   - 有输出但输入为 0：旧版本已经写进 stats.json 的回填日，
		//     没有 Partial 标记但同样缺输入侧（真实请求不可能 0 输入）。
		dp.Partial = b.Partial || (dayComp > 0 && dayPrompt == 0)
		if dp.Partial {
			partialDays++
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
		// PartialDays 数出区间内输入侧缺失的天数，前端据此把
		// 总量标为下限而不是完整口径。
		PartialDays: partialDays,
		// Live 与 range 无关：它永远是「最近 60 秒」，不因为切区间而变。
		Live: m.live.snapshot(now),
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
	m.live = newLiveWindow()
	m.since = time.Now()
	m.warned = false
	m.dirty = true
	path, raw, ok := m.flushLocked()
	m.mu.Unlock()
	m.notifyChanged()
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
	snap.LiveModels = globalLiveModels.snapshot()
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

// sseKeepaliveSec 心跳间隔。纯保活用：中间没有代理时会因为空闲超时掐断长连，
// 浏览器 EventSource 断开后会自动重连，但前端会看到一次「已暂停」闪烁。
const sseKeepaliveSec = 20

// statsStream 处理 GET /v1/stats/stream?range=...：把 /v1/stats 变成 SSE 推送。
//
// 为什么要有这条线：统计是「请求落地那一刻就已经算好了」的即时数据，
// 但纯轮询天生带一个「最多迟一个间隔」的延迟，还每次都把整份快照重新
// JSON 序列化一遍（85 个模型时不小）。改成推送后：
//   - 延迟从「≤ 轮询间隔」变成「≈0」；
//   - 没有新观测时一条字节都不发，比轮询省。
//
// 载荷与 /v1/stats 完全一致（同一个 metricsSnapshotRange + enrichCredits），
// 所以前端可以无脑用同一段渲染逻辑；现有接口不动，纯增量。
func (h *Handler) statsStream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		// 没 Flusher 说明中间件链里套了不支持流式的 writer，
		// 此时降级成一次性快照而不是 500：至少页面上还有数据。
		spec := parseRangeSpec(r.URL.Query().Get("range"))
		snap := metricsSnapshotRange(spec)
		h.enrichCredits(&snap)
		writeJSON(w, http.StatusOK, snap)
		return
	}
	// range 只认「今天 / 7 天 / 30 天 / 全部」，但 from 必须**每帧重算**。
	// 建连时算一次是不够的：SSE 是长连接，跨零点它不会断，于是 from 永远停在
	// 建连那天的零点 —— 过了 0 点还在推送昨天的桶，而且那个桶仍在被写入，
	// 表现为「今天请求 1694 而且还在涨」，可实际那是昨天的数据在涨。
	// 每天零点这一行自然就切到新的一天，不用重连、不用刷新页面。
	rangeArg := r.URL.Query().Get("range")
	sig := globalMetrics.changed

	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	// nginx 之类的反代会缓冲响应，不关掉的话事件全攒在代理里一次性吐给前端，
	// 那就退化成轮询了。
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// 心跳：既保活，也顺带给前端一个「连接还活着」的信号。
	// 用注释帧（以 ':' 开头，SSE 规范里被客户端忽略），不污染事件流。
	ping := func() {
		fmt.Fprintf(w, ": ping\n\n")
		fl.Flush()
	}

	// 首帧立即推全量：连上来的瞬间就有东西显示，不用等下一次请求落地。
	send := func() bool {
		snap := metricsSnapshotRange(parseRangeSpec(rangeArg))
		h.enrichCredits(&snap)
		raw, err := json.Marshal(snap)
		if err != nil {
			return false
		}
		if _, err := fmt.Fprintf(w, "event: stats\ndata: %s\n\n", raw); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	// 建连前先排空信号槽：里面可能压着「上一批观测」留下的信号，而下面的
	// 首帧快照已经把它们全算进去了。不排空的话，循环第一次 select 就会
	// 立刻醒来再推一帧内容完全相同的快照（客户端表现为收到两次一样的数据）。
	//
	// 顺序很讲究：先排空、后取快照。反过来（先快照后排空）会吞掉
	// 「落地在两步之间的那次请求」的通知，那条数据就永远推不出去了。
	// 现在的顺序最坏情况只是多推一帧相同内容，不会漏——宁可重复不可丢。
	for {
		select {
		case <-sig:
			// 继续排空，直到空。
		default:
			goto drained
		}
	}
drained:
	if !send() {
		return
	}
	ping()

	tick := time.NewTicker(sseKeepaliveSec * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			// 客户端关页面 / 断开：这是正常路径，静默收尾。
			return
		case <-sig:
			if !send() {
				return
			}
		case <-tick.C:
			ping()
		}
	}
}

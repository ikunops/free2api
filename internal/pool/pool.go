// Pool 账号池核心：结构定义、构造（New/Set* 注入）、在途租约（Acquire/Release）
// 与账号增删（Add/SyncToDir/upsertLocked）。选号/冷却/状态/持久化见同包其他文件。
package pool

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"free2api/internal/auth"
)

type Pool struct {
	mu      sync.RWMutex
	byUID   map[string]*entry
	stateFp string
	dirty   atomic.Bool // 内存有变更待落盘
	// store 池状态快照镜像（redisstore.Store）；nil = 无需镜像（未配置 Redis / Noop 之外也可能 nil）。
	// SaveState/LoadState 经它接线，与本地 state.json 并存作启动恢复备份。
	store StoreSnapshotter
	// 熔断器调优（SetBreaker 注入；默认值见 defaultBreaker*）。
	breakerThreshold   int
	breakerCooldown    time.Duration
	breakerCooldownMax time.Duration
	// softRateMax 软冷却指数退避的封顶（SetSoftRateMax 注入；默认 defaultSoftRateMax）。
	softRateMax time.Duration
	// costExploreInterval costTier 条件探索窗口（issue #136 方案 a′，SetCostExploreInterval
	// 注入；默认 defaultCostExploreInterval 30m）。tier 0 垄断 + tier 1 存在且距上次
	// 探索 ≥ 窗口时，本次 pick 生效层切 tier 1-only（探索=搭车改道，零新增上游请求）。
	// 0 = 关停（完全回到现状行为）。
	costExploreInterval time.Duration
	// exploreLast 各 (realm, 模型) 的上次探索时刻，键 = realm + "\x1f" + model。
	// 运行态（不持久化，同 lastUsed/usedSeq 口径）：重启归零 → 每个仍冻结的
	// (域, 模型) 多至 1 次即时重探；已毕业号经 ModelCosts 恢复 tier，学费不重付。
	// 只在探索事件时写入（tier 1 枯竭期间停走，陈旧无害）；不做对称清理。
	exploreLast map[string]time.Time
	// costExploreEvents 累计探索事件数（/status 透出；pick 写锁内 ++，无需 atomic）。
	costExploreEvents int64
	// degradeThreshold / degradeCooldown / degradeCooldownMax 连败降权参数
	// （SetDegrade 注入；默认值见 defaultDegrade*，issue #114）。
	degradeThreshold   int
	degradeCooldown    time.Duration
	degradeCooldownMax time.Duration
	// 三因子加权调优（SetWeights 注入；默认值见 defaultIdle*）。
	idleWeightPerHour float64
	idleWeightMax     float64
	// maxInFlight 单账号最大在途请求数；0 = 不限（租约关闭）。
	maxInFlight int
	// maxInFlightGlobal global 域单账号在途上限分档（WAF 403 修复 P1-1：global 域
	// WAF 风控更紧，压低并发）；0 = 未设置，回落 maxInFlight（不分档，零回归）。
	maxInFlightGlobal int
	// producerOf 解析「uid → 生产者（哪个 AI 客户端）」：由 main 从来源台账
	// （auths/.origins.json）注入，/status 据此给每个账号标出归属客户端。
	// 读侧恒持 p.mu（statusOf 在 List 的 RLock 内被调用），写侧 SetProducerOf
	// 持写锁——同一把锁，无数据竞争。nil 或查不到 → 回落 workbuddy（历史凭证
	// 零回归：workbuddy 是唯一走通端到端反代的生产者）。
	producerOf func(uid string) string
	// randInt64N 仅供测试注入确定性随机源；nil 时用 math/rand/v2 全局源。
	// 生产代码不应设置此字段。
	randInt64N func(n int64) int64
	// persistFails 本地 state.json 连续落盘失败计数（仅 saveLocked 在持锁下读写，无需 atomic）。
	// 用于落盘失败的日志节流：首败/每 N 次提醒/恢复各打一条，避免磁盘满时刷屏。
	persistFails int
	// weightOfHook / weightOfMaxHook 仅供测试观测（DeptestOnly）：分别统计 weightOf
	// 被调次数与收到的 maxCredits 口径，验证「单次 pick 只算一次 + 全集口径」的重构
	// 契约（TestWeightOfCalledOncePerPick / TestWeightOfMaxCreditsPassedVerbatim）。
	// 生产恒 nil，零开销（nil 函数调用分支预测友好）。
	weightOfHook    func()
	weightOfMaxHook func(maxCredits int64)
	// pickSeq 选号单调序号源：仅 pick 在持 p.mu 写锁时自增并赋给 entry.usedSeq，
	// 无需 atomic。见 entry.usedSeq 注释（解决 Windows 时钟精度导致的 LRU 失效）。
	pickSeq uint64
	// stopCh 关闭信号：Close 关闭它使 startFlusher 的后台 goroutine 退出。
	// nil = 未启动 flusher（stateFp 为空时 New 不起 flusher）。
	stopCh chan struct{}
	// closeOnce 保证 Close 幂等（多次调用不重复 close channel）。
	closeOnce sync.Once
}

// New 构建池；stateFp 非空时尝试加载旧状态，并启动后台周期性落盘 goroutine。
func New(stateFp string) *Pool {
	p := &Pool{
		byUID:              map[string]*entry{},
		stateFp:            stateFp,
		breakerThreshold:   defaultBreakerThreshold,
		breakerCooldown:    defaultBreakerCooldown,
		breakerCooldownMax: defaultBreakerCooldownMax,
		idleWeightPerHour:  defaultIdleWeightPerHour,
		idleWeightMax:      defaultIdleWeightMax,
		degradeThreshold:   defaultDegradeThreshold,
		degradeCooldown:    defaultDegradeCooldown,
		degradeCooldownMax: defaultDegradeCooldownMax,
		// 探索缺省 30m：tier 0 垄断下的 tier 1 探索窗口（issue #136）。用户经
		// config 显式 "0" 关停（SetCostExploreInterval(0)）。
		costExploreInterval: defaultCostExploreInterval,
		exploreLast:         map[string]time.Time{},
	}
	if stateFp != "" {
		p.load()
		p.startFlusher()
	}
	return p
}

// SetBreaker 注入熔断器参数（main 从 config 解析后调用）。非正值保留原值（用默认）。
func (p *Pool) SetBreaker(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.breakerThreshold = threshold
	}
	if cooldown > 0 {
		p.breakerCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.breakerCooldownMax = cooldownMax
	}
}

// SetSoftRateMax 注入软冷却指数退避的封顶时长（main 从 config 解析后调用）。
// 非正值保留原值（用默认 2h），风格同 SetBreaker。
func (p *Pool) SetSoftRateMax(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d > 0 {
		p.softRateMax = d
	}
}

// SetCostExploreInterval 注入 costTier 条件探索窗口（main 从 config 解析后调用，
// issue #136）。0 = 关停（完全回到现状行为）；正值覆盖默认 30m。
// 注意：与 SetSoftRateMax「非正值保留默认」不同，0 在这里是**合法值**（关停开关，
// 与 config 的 "0" 关停语义对齐）——不设 0 语义就无法关停探索。
func (p *Pool) SetCostExploreInterval(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if d < 0 {
		return // 负值非法，保留现值
	}
	p.costExploreInterval = d
}

// SetProducerOf 注入「uid → 生产者」解析器（来源台账；main 从 auths/.origins.json 构造）。
// 未注入时 /status 一律按 workbuddy 标注——早期凭证没有台账标注，零回归。
func (p *Pool) SetProducerOf(fn func(uid string) string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.producerOf = fn
}

// CostExploreStatus 透出探索台账（/status 用）：累计探索事件数 + 各 (域, 模型)
// 的最近探索时刻（键内 \x1f 分隔符输出为 "|"，与 model_costs 行对照即可读出
// 「探索→毕业」全链路）。RLock 只读遍历；map 大小受「服务过的 (域, 模型)」集合
// 约束（与 modelCost 同界，天然有界）。
func (p *Pool) CostExploreStatus() (events int64, last map[string]time.Time) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	last = make(map[string]time.Time, len(p.exploreLast))
	for k, ts := range p.exploreLast {
		// 键 realm+"\x1f"+model → 输出 "|"（JSON 安全可读；\x1f 不可打印）。
		last[strings.ReplaceAll(k, "\x1f", "|")] = ts
	}
	return p.costExploreEvents, last
}

// SetDegrade 注入连败降权参数（main 从 config 解析后调用，issue #114）。
// 非正值保留原值（用默认，见 defaultDegrade*），风格同 SetBreaker/SetSoftRateMax。
func (p *Pool) SetDegrade(threshold int, cooldown, cooldownMax time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if threshold > 0 {
		p.degradeThreshold = threshold
	}
	if cooldown > 0 {
		p.degradeCooldown = cooldown
	}
	if cooldownMax > 0 {
		p.degradeCooldownMax = cooldownMax
	}
}

// SetWeights 注入三因子加权的闲置补偿参数。非正值保留原值（用默认）。
func (p *Pool) SetWeights(idlePerHour, idleMax float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idlePerHour > 0 {
		p.idleWeightPerHour = idlePerHour
	}
	if idleMax > 0 {
		p.idleWeightMax = idleMax
	}
}

// SetMaxInFlight 注入单账号最大在途请求数；0 = 不限。负值保留原值。
func (p *Pool) SetMaxInFlight(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlight = n
	}
}

// SetMaxInFlightGlobal 注入 global 域单账号在途上限（WAF 403 修复 P1-1 分档）；
// 0 = 未设置，global 账号回落 maxInFlight（不分档）。负值保留原值。
func (p *Pool) SetMaxInFlightGlobal(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n >= 0 {
		p.maxInFlightGlobal = n
	}
}

// inFlightLimit 报告账号的生效在途上限（global 分档优先，回落 maxInFlight）；
// 0 = 不限。调用方需已持 p.mu（或快照过 limit，见 Acquire 注释）。
func (p *Pool) inFlightLimit(e *entry) int {
	if p.maxInFlightGlobal > 0 && e.a.Realm() == "global" {
		return p.maxInFlightGlobal
	}
	return p.maxInFlight
}

// SetStore 注入池状态快照镜像（redisstore.Store）。nil 表示不镜像（纯本地恢复）。
// 必须在 SyncToDir 之前调用，使"择新恢复"发生在账号对齐之前。
func (p *Pool) SetStore(s StoreSnapshotter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.store = s
}

// Acquire 为 uid 占一个在途名额（会话粘性命中后调用）；池上限内返回 true。
// 名额用 entry.inFlight 原子自增，满额返回 false。上限按账号 realm 分档
// （global 档 maxInFlightGlobal，P1-1；未设置回落 maxInFlight）。
func (p *Pool) Acquire(uid string) bool {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	if !ok {
		p.mu.RUnlock()
		return false
	}
	limit := p.inFlightLimit(e)
	p.mu.RUnlock()
	if limit <= 0 {
		// 不限：计数仍累加（供状态观测），但永不拒绝。
		e.inFlight.Add(1)
		return true
	}
	for {
		cur := e.inFlight.Load()
		if cur >= int64(limit) {
			return false
		}
		if e.inFlight.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// Release 释放一个在途名额。幂等减到 0 为止（防重复释放扣成负数）。
func (p *Pool) Release(uid string) {
	p.mu.RLock()
	e, ok := p.byUID[uid]
	p.mu.RUnlock()
	if !ok {
		return
	}
	for {
		cur := e.inFlight.Load()
		if cur <= 0 {
			return
		}
		if e.inFlight.CompareAndSwap(cur, cur-1) {
			return
		}
	}
}

// SetRandomSource 仅供测试注入确定性随机源；生产代码不应调用。
// 注入源取 n∈[0,n) 后，pickWeighted 的抽签结果完全可预测。
func (p *Pool) SetRandomSource(fn func(n int64) int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.randInt64N = fn
}

// Add 加入账号；已存在则保留原状态、更新凭证（upsert 单账号）。
func (p *Pool) Add(a *auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.upsertLocked(a)
}

// SyncToDir 用最新扫描结果对齐池：新账号加入、消失的账号剔除（状态保留）。
// 剔除结果持久化回 state.json，避免已删账号在下次启动时被 load() 复活。
func (p *Pool) SyncToDir(auths []*auth.Auth) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := make(map[string]bool, len(auths))
	for _, a := range auths {
		seen[a.UID] = true
		p.upsertLocked(a)
	}
	changed := false
	for uid := range p.byUID {
		if !seen[uid] {
			delete(p.byUID, uid)
			changed = true
		}
	}
	if changed {
		p.saveLocked()
	}
}

// upsertLocked 更新或插入单个账号；已存在则只换凭证、保留 credits/cooling 状态。
// 调用方必须已持有 p.mu；Add 与 SyncToDir 共用此 upsert 逻辑。
func (p *Pool) upsertLocked(a *auth.Auth) {
	if e, ok := p.byUID[a.UID]; ok {
		e.a = a // 保留 credits/cooling 状态
		return
	}
	p.byUID[a.UID] = &entry{a: a}
}

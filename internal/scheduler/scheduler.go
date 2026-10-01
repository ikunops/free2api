// Package scheduler 定时任务：签到 / 活跃上报 / 猫猫旅行 / token keepalive / 开学季 / 夜猫子 六类独立排程。
// 签到成功后重新查余额，余额 > 0 的冷却账号自动解冻。
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// Config 调度器依赖。
//
// 任务开关用「禁用」命名而非「启用」：零值 Config 即六类任务都启用（hours 回落默认），
// 与引入开关前的行为逐字一致（老调用方/老测试无需改动）。
type Config struct {
	Pool           *pool.Pool
	Upstream       *upstream.Client
	CheckinHours   []int // 默认 [9, 21]
	TravelHours    []int // 默认 [9,21]：一趟派出 + 一趟领奖闭环
	ActivityHours  []int // 默认 [10]
	KeepaliveHours []int // 默认 [22]
	SchoolHours    []int // 默认 [12]：开学季任务（迁移自系统 crontab）
	CatHours       []int // 默认 [1]：夜猫子任务（迁移自系统 crontab）
	// ActivityReportCount 每号每次活跃上报的条数：领猫前置需 5 次对话，
	// 默认 5 条同一 conversationId 内多轮上报把 chat_5 刷满；0/缺省=1 兼容旧行为。
	ActivityReportCount int

	// ExpiringSoonWindow 快过期积分窗口：签到查余额时，把到期时间 <= now+window 的
	// 套餐余额标记为"快过期"（pool 据此优先消耗，见 entry.creditsExpiring）。
	// <=0 时禁用分桶（全部归长期，行为与引入前一致）。默认建议 7*24h。
	ExpiringSoonWindow time.Duration

	// CheckinDisabled 显式关闭签到排程（对应 config 的 schedule.checkin_enabled=false）。
	// 禁用后不再有任何签到时点。旅行不再搭签到便车（已剥离为独立排程）。
	CheckinDisabled bool
	// TravelDisabled 显式关闭猫猫旅行排程（schedule.travel_enabled=false）。
	TravelDisabled bool
	// ActivityDisabled 显式关闭活跃上报排程（schedule.activity_enabled=false）。
	ActivityDisabled bool
	// KeepaliveDisabled 显式关闭 token 保活排程（schedule.keepalive_enabled=false）。
	KeepaliveDisabled bool
	// SchoolDisabled 显式关闭开学季任务排程（schedule.school_enabled=false）。
	SchoolDisabled bool
	// CatDisabled 显式关闭夜猫子任务排程（schedule.cat_enabled=false）。
	CatDisabled bool
}

// Scheduler 调度器。
type Scheduler struct {
	cfg Config

	// mu/adoptTried 领养当日失败记录：uid → 自然日（CST）。门槛未达的账号当日不再重试，
	// 避免同日多趟对上游重试轰炸；进程重启即清零（无需持久化）。
	mu         sync.Mutex
	adoptTried map[string]string

	// rewardClaimed 连登奖励按自然日（CST）领取标记：uid → 当日日期。已领/已尝试的账号
	// 当日不再重打 redeem（领取类写操作按天幂等，避免对上游重复写请求）；自然日 00:00
	// CST 重置（上游增长体系按 CST 自然日刷新，见 travelDay/cstZone）。进程重启即清零
	// （服务端幂等兜底：重启后当日重复 redeem 会拿 409 正常态，无副作用）。
	rewardClaimed map[string]string

	// checkinMu 串行化签到：定时入口与手动触发互斥，避免同一时刻重复打上游签到接口。
	checkinMu sync.Mutex
}

// New 构建。
func New(cfg Config) *Scheduler {
	if len(cfg.CheckinHours) == 0 {
		cfg.CheckinHours = []int{9, 21}
	}
	if len(cfg.TravelHours) == 0 {
		cfg.TravelHours = []int{9, 21}
	}
	if len(cfg.ActivityHours) == 0 {
		cfg.ActivityHours = []int{10}
	}
	if len(cfg.KeepaliveHours) == 0 {
		cfg.KeepaliveHours = []int{22}
	}
	if len(cfg.SchoolHours) == 0 {
		cfg.SchoolHours = []int{12}
	}
	if len(cfg.CatHours) == 0 {
		cfg.CatHours = []int{1}
	}
	// 0/缺省 = 1 条（兼容旧行为：每号每天 1 条上报点亮连登）。
	if cfg.ActivityReportCount <= 0 {
		cfg.ActivityReportCount = 1
	}
	return &Scheduler{cfg: cfg, adoptTried: make(map[string]string), rewardClaimed: make(map[string]string)}
}

// checkinRefreshSkew 签到前判定"token 是否临近过期"的时间窗口（10 分钟）。
// 长时间停机/容器长期停跑后 access token 往往已过期，不先刷新则签到必然 401 白跑。
const checkinRefreshSkew = 10 * time.Minute

// CheckinStatus 单账号签到结果状态。
type CheckinStatus string

const (
	CheckinOK      CheckinStatus = "ok"      // 签到成功
	CheckinAlready CheckinStatus = "already" // 上游判定今天已签到（幂等重复，视为正常）
	CheckinFail    CheckinStatus = "fail"    // 刷新 token / 签到 / 余额查询失败
	CheckinSkipped CheckinStatus = "skipped" // 禁用账号或无有效凭证，未参与
)

// CheckinOutcome 单账号签到结果（供手动签到回执与日志汇总）。
type CheckinOutcome struct {
	UID      string        `json:"uid"`
	Nickname string        `json:"nickname,omitempty"`
	Status   CheckinStatus `json:"status"`
	Credits  *int64        `json:"credits,omitempty"` // 签到后余额（余额查询成功才有值）
	Detail   string        `json:"detail,omitempty"`  // 失败/跳过原因（"已签到"不填）
}

// ErrBusy 已有一次签到正在执行（手动入口与定时撞车）。
var ErrBusy = errors.New("checkin already running")

// nextFire 返回 now 之后最近的一个整点触发时间；hours 为本地小时（0-23）。
func nextFire(now time.Time, hours []int) time.Time {
	var earliest time.Time
	for _, h := range hours {
		t := time.Date(now.Year(), now.Month(), now.Day(), h, 0, 0, 0, now.Location())
		if !t.After(now) {
			t = t.Add(24 * time.Hour)
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
	}
	return earliest
}

// taskKind 调度任务类型。
type taskKind int

const (
	taskCheckin taskKind = iota
	taskTravel
	taskActivity
	taskKeepalive
	taskSchool
	taskCat
)

// nextWake 返回 now 之后最近的唤醒时刻，以及该时刻需要执行的全部任务。
// 多类任务若配到同一小时（如签到与旅行都含 9），该时刻多类任务需一并执行。
// 已显式禁用的任务不进候选（nextFire 对其零值返回零时间，nextWake 再跳过零时点）。
func (s *Scheduler) nextWake(now time.Time) (time.Time, []taskKind) {
	type slot struct {
		at   time.Time
		kind taskKind
	}
	var slots []slot
	if !s.cfg.CheckinDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CheckinHours), taskCheckin})
	}
	if !s.cfg.TravelDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.TravelHours), taskTravel})
	}
	if !s.cfg.ActivityDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.ActivityHours), taskActivity})
	}
	if !s.cfg.KeepaliveDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.KeepaliveHours), taskKeepalive})
	}
	if !s.cfg.SchoolDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.SchoolHours), taskSchool})
	}
	if !s.cfg.CatDisabled {
		slots = append(slots, slot{nextFire(now, s.cfg.CatHours), taskCat})
	}
	var earliest time.Time
	for _, sl := range slots {
		if sl.at.IsZero() {
			continue
		}
		if earliest.IsZero() || sl.at.Before(earliest) {
			earliest = sl.at
		}
	}
	if earliest.IsZero() {
		return time.Time{}, nil
	}
	var kinds []taskKind
	for _, sl := range slots {
		if !sl.at.IsZero() && sl.at.Equal(earliest) {
			kinds = append(kinds, sl.kind)
		}
	}
	return earliest, kinds
}

// wakeupGraceDelay 迟到唤醒补跑的派发前网络宽限：Windows Modern Standby exit 后
// 网络栈/DNS 1-2s 才恢复（issue #152 实测 dial tcp lookup no such host 与
// Kernel-Power 507 standby exit ≤1s 重合），宽限 5s 覆盖 90%+ 唤醒场景。
// 只对迟到补跑生效（准点触发零延迟），零配置（分析报告裁定全套配置不成比例）。
// 测试可缩短（与 travelAccountDelay「测试可置 0」同口径）。
var wakeupGraceDelay = 5 * time.Second

// wakeupLateThreshold 迟到判定阈值：now 晚于槽位计划时刻超过 1s 才算迟到补跑。
// 毫秒级抖动（timer 正常触发的偏移量级）不算，避免准点触发被误宽限。
const wakeupLateThreshold = 1 * time.Second

// awaitWakeupGrace 迟到唤醒补跑派发前的网络宽限：槽位时刻已过点超过阈值
// （机器刚从睡眠唤醒）时先等满 wakeupGraceDelay 让网络栈/DNS 就绪再派发。
// 准点/阈值内抖动零延迟直接放行。ctx 取消立即返回 false（优雅停机不等宽限睡满，
// 本批放弃，下轮 nextWake 照旧从"现在"起算）。返回是否继续派发。
func awaitWakeupGrace(ctx context.Context, planned time.Time) bool {
	if late := time.Since(planned); late <= wakeupLateThreshold {
		return ctx.Err() == nil // 准点触发：零延迟放行
	}
	log.Printf("wakeup grace %s: late catch-up for slot %s", wakeupGraceDelay, planned.Format("15:04"))
	return sleepCtx(ctx, wakeupGraceDelay)
}

// Run 主循环，阻塞直到 ctx 取消。
func (s *Scheduler) Run(ctx context.Context) {
	for {
		next, kinds := s.nextWake(time.Now())
		if next.IsZero() {
			// 六类任务全部禁用：不空转，只等退出信号。
			<-ctx.Done()
			return
		}
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// 到点任务在排程时确定（不依赖唤醒时刻的小时数），迟到唤醒也不会漏跑。
			// 迟到唤醒（睡眠跨过槽位时刻，timer 在唤醒瞬间才到期）先等网络宽限：
			// 唤醒瞬间 DNS 未就绪，零宽限派发等于把唯一一次补跑机会打在注定失败
			// 的窗口里（issue #152）；准点触发零延迟不受影响。
			if !awaitWakeupGrace(ctx, next) {
				return // ctx 取消：放弃本批，优雅退出
			}
			// 唤醒时全部并行派发：每类一个 goroutine，慢任务族（如活跃上报
			// 54 号 × 5 条 ≈ 7-8 分钟睡眠）不再阻塞同槽其他任务族；返回前
			// 等全部任务收尾（下一轮 nextWake 照旧从"现在"起算，多轮重叠
			// 的风险与串行版相同——nextWake 只挑现在之后的时点）。
			s.runBatch(ctx, kinds)
		}
	}
}

// runBatch 并行派发一批任务（同一唤醒时刻的多类任务），等全部完成返回。
// 供 Run 主循环与测试使用；ctx 取消时由各任务内部的 sleepCtx 快速收尾。
func (s *Scheduler) runBatch(ctx context.Context, kinds []taskKind) {
	var wg sync.WaitGroup
	for _, k := range kinds {
		wg.Add(1)
		go func(k taskKind) {
			defer wg.Done()
			s.dispatch(ctx, k)
		}(k)
	}
	wg.Wait()
}

// dispatch 按任务类型分发到对应执行函数。脚本类（school/cat）失败只记 WARN、
// 不影响其余任务继续执行（与现有各任务"单账号失败不阻断遍历"同口径）。
// ctx 传导给带账号间限速的遍历（取消时立即放弃剩余账号），纯脚本类任务不感知。
func (s *Scheduler) dispatch(ctx context.Context, k taskKind) {
	switch k {
	case taskCheckin:
		s.RunCheckinNow()
	case taskTravel:
		s.runTravel(ctx)
	case taskActivity:
		s.runActivity(ctx)
	case taskKeepalive:
		s.RunKeepaliveNow()
	case taskSchool:
		s.RunSchoolNow()
	case taskCat:
		s.RunCatNow()
	}
}

// RunCheckinNow 定时触发的立即签到：逐账号结果由 CheckinAll 记日志，此处只兜住"撞车跳过"。
func (s *Scheduler) RunCheckinNow() {
	if _, err := s.CheckinAll(); err != nil {
		log.Printf("scheduled checkin skipped: %v", err)
	}
}

// CheckinAll 全量签到：按需刷新 token → daily-checkin → 查余额 → 解冻冷却账号。
// 冷却中的账号也参与（签到就是为了解冻它们）；禁用的跳过。
// 同一时刻只允许一次签到在跑，重复调用返回 ErrBusy（防止手动触发与定时撞车重复打上游）。
//
// session dead 走 Pool.NoteSessionDead 的**连续计数**语义（与 keepalive 一致）：
// 一次刷新失败不再立即杀号，连续 sessionDeadThreshold 次才禁用，刷新成功清计数。
func (s *Scheduler) CheckinAll() ([]CheckinOutcome, error) {
	if !s.checkinMu.TryLock() {
		return nil, ErrBusy
	}
	defer s.checkinMu.Unlock()

	statuses := s.cfg.Pool.List()
	out := make([]CheckinOutcome, 0, len(statuses))
	var okN, alreadyN, failN, skipN int
	for _, st := range statuses {
		oc := CheckinOutcome{UID: st.UID, Nickname: st.Nickname}
		if st.Disabled {
			oc.Status, oc.Detail = CheckinSkipped, "disabled"
			skipN++
			out = append(out, oc)
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			oc.Status, oc.Detail = CheckinSkipped, "no credentials"
			skipN++
			out = append(out, oc)
			continue
		}
		// D4 门控：realm=global 账号无签到体系/任务中心，直接跳过（不发起任何上游调用，避免风控）。
		// 经 auth.Realm() 统一判定：逃生门（global.enabled=false）下 global 账号被降级为 cn、
		// 按 CN 处理——这是 D5 逃生门的刻意语义（纯 CN 部署锁死一切 global），与引用处一致。
		if a.IsGlobal() {
			oc.Status, oc.Detail = CheckinSkipped, "global"
			skipN++
			out = append(out, oc)
			continue
		}
		// 停机跨过 token 有效期（关机过夜/容器长期停跑）时先补一次刷新，否则签到必然 401 白跑。
		if a.NeedsRefresh(checkinRefreshSkew) {
			if err := s.cfg.Upstream.RefreshToken(a); err != nil {
				log.Printf("checkin %s refresh: %v", logfmt.Label(st.UID, st.Nickname), err)
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					if s.cfg.Pool.NoteSessionDead(st.UID) {
						log.Printf("WARN: checkin %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
					}
				}
				// 刷新只是"提前补票"：token 若仍有效，继续照常签到（否则刷新接口抖动
				// 会让本可成功的签到被白白跳过）；真正过期才判定失败。
				if a.NeedsRefresh(0) {
					oc.Status, oc.Detail = CheckinFail, "refresh: "+err.Error()
					failN++
					out = append(out, oc)
					continue
				}
			} else {
				a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
				if err := a.SaveAtomic(); err != nil {
					// 刷新成功但落盘失败：重启会用旧 token，必须暴露。
					log.Printf("checkin %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
				}
			}
		}
		// 签到返回错误（含"今天已签到"）也继续查余额：余额恢复即可解冻账号。
		if err := s.cfg.Upstream.DailyCheckin(a); err != nil {
			if upstream.IsAlreadyCheckin(err) {
				// "今天已签到"是幂等成功，不是错误：不填 detail，免得回执里
				// 出现一整段 400 报文、被误读成签到失败。
				oc.Status = CheckinAlready
			} else {
				oc.Status = CheckinFail
				oc.Detail = err.Error()
				log.Printf("checkin %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			}
		} else {
			oc.Status = CheckinOK
		}
		// 分桶查余额：快过期窗口内的积分单独标记，pool 优先消耗（issue:积分过期）。
		// ExpiringSoonWindow<=0 时退化为纯总量（与引入前一致）。
		remain, buckets, err := s.cfg.Upstream.UserResourceDetailed(a, s.cfg.ExpiringSoonWindow)
		if err != nil {
			log.Printf("user-resource %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			oc.Status = CheckinFail
			oc.Detail = joinDetail(oc.Detail, "resource: "+err.Error())
			failN++
			out = append(out, oc)
			continue
		}
		s.cfg.Pool.ReenableIfCredits(st.UID, remain)
		s.cfg.Pool.SetCreditsDetailed(st.UID, remain, buckets.Expiring)
		oc.Credits = &remain
		switch oc.Status {
		case CheckinOK:
			okN++
		case CheckinAlready:
			alreadyN++
		default:
			failN++
		}
		out = append(out, oc)
	}
	log.Printf("checkin done: total=%d ok=%d already=%d fail=%d skipped=%d",
		len(statuses), okN, alreadyN, failN, skipN)
	return out, nil
}

// joinDetail 拼接多段原因，避免后一段覆盖前一段的失败信息。
func joinDetail(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}

// RunActivityNow 立即对池内所有可用账号执行对话活跃上报。
// 禁用账号跳过；无 AccessToken 的跳过；账号间限速 activityAccountDelay。
// CN 与 global 账号**都上报**（PR #45 实测国际版 /v2/report 可用）；单账号失败
// 只记 WARN 不影响遍历。
//
// 每号上报 N 条（ActivityReportCount，默认 5）：N 条共用同一 conversationId
// （free2api-<ms>），模拟同一会话内 N 轮对话——这是领养猫（buddy/first）对话量
// 门槛的实测刷法（chat_5 前置需 5 次对话）。requestId 各条独立（同会话多轮）。
// 账号内 N 条之间间隔 activityReportGap（1.5s）避免秒发触发风控。
//
// 0/缺省 ActivityReportCount = 1 条，兼容旧行为（仅点亮连登 + 解锁 first_buddy）。
//
// 上报成功后：① streak 自检（回读连登，发现「200 但静默丢弃」）；
// ② 无猫账号立即重试领养（travelAdoptForce）——对话量刚补满的新状态，不算重试，
// 豁免 adoptTriedToday 当日防抖（旅行排程 09 点已领养过且 skip，10 点上报补满后
// 不能依赖下一轮旅行领养，就地闭环）。
// RunActivityNow 立即对池内所有可用账号执行对话活跃上报（无 ctx 的外部入口：
// cmd/activity 一次性触发、测试）。内部走 runActivity，取背景 ctx（不可取消，
// 语义与引入前 time.Sleep 版一致）。
func (s *Scheduler) RunActivityNow() {
	s.runActivity(context.Background())
}

// runActivity 活跃上报遍历，随 ctx 取消立即退出。
func (s *Scheduler) runActivity(ctx context.Context) {
	count := s.cfg.ActivityReportCount
	first := true
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.AccessTokenValue() == "" {
			continue
		}
		// global 账号同样上报（PR #45 实测国际版 /v2/report 在 workbuddy.ai 上 code=0 OK，
		// 点亮连登）；realmBase 路由/头由 upstream.billingJSON/BillingHeaders 按 realm 切。
		// 单账号失败只记 WARN 不影响遍历（下方 report err → break 该号 → continue 下号）。
		if !first {
			if !sleepCtx(ctx, activityAccountDelay) {
				return // 优雅停机：不等限速睡满，剩余账号下轮再报
			}
		}
		first = false
		// N 条共用同一 conversationId（同会话），requestId 各自独立（每条一个）。
		cid := fmt.Sprintf("free2api-%d", time.Now().UnixMilli())
		ok := 0
		for i := 1; i <= count; i++ {
			rid := fmt.Sprintf("%s-r%d", cid, i)
			if err := s.cfg.Upstream.ReportChatActivity(a, cid, rid); err != nil {
				log.Printf("activity %s: report %d/%d: %v", logfmt.Label(a.UID, a.Nickname), i, count, err)
				break // 本号上报失败：不再续发，streak 自检无意义
			}
			log.Printf("activity %s: report %d/%d ok", logfmt.Label(a.UID, a.Nickname), i, count)
			ok++
			if i < count {
				// 账号内 5 条之间间隔，避免秒发风控；取消时立即放弃本号剩余条数。
				if !sleepCtx(ctx, activityReportGap) {
					return
				}
			}
		}
		if ok < count {
			continue // N 条未发满：streak 自检与领养均无意义，下个账号
		}
		s.checkActivityStreak(a) // N 条全发满 → 回读 streak 自检（只留结论行）
		s.travelAdoptForce(a)    // 无猫账号对话量刚补满 → 立即重试领养（豁免防抖）
		s.claimGrowthRewards(a)  // 连登奖励 + 抽奖：点亮连登后按天领取（finally 语义：失败不拖累上报）
	}
}

// checkActivityStreak 上报成功后回读连登天数（只读 oracle，发现静默失败）。
// 背景：REPORT-active-map.md §2 实测「上报 200 但静默丢弃」（缺 userId 时 progress 不动），
// 上报 200 ≠ streak 计分——需要回读验证闭环。
// 异常检测口径：days==0 → warn（report OK but streak.days=0 (silent drop?)）；
// GET 失败 → warn 但不影响主流程（上报本身已成功，按天幂等，不做重试）。
// 日志每号一行、一眼可 grep：`activity %s: streak days=%d`（成功也打，方便对账）。
// 返回 true 表示「上报 OK 但 streak 可疑」（days==0 或回读失败），供测试断言。
func (s *Scheduler) checkActivityStreak(a *auth.Auth) bool {
	days, err := s.cfg.Upstream.GrowthStreak(a)
	if err != nil {
		log.Printf("WARN: activity %s: streak check failed (report OK): %v", logfmt.Label(a.UID, a.Nickname), err)
		return true
	}
	if days == 0 {
		log.Printf("WARN: activity %s: report OK but streak.days=0 (silent drop?)", logfmt.Label(a.UID, a.Nickname))
		return true
	}
	log.Printf("activity %s: streak days=%d", logfmt.Label(a.UID, a.Nickname), days)
	return false
}

// claimGrowthRewards 领取连登奖励（里程碑兑换）+ 执行连登抽奖。
// 在 runActivity 上报成功 + streak 自检之后调用：连登达标（days>=某档）才能领奖，
// 领奖送的 chances 才是抽奖次数来源，故先领奖后抽奖。
//
// 全链（panel 连登管家吸收版）：礼包/补偿领取 → 读 reward-state → 补签保连登
// （补签成功重读 state 吃恢复后的天数）→ 挑档 redeem → chances → draw。
//
// 幂等/风控语义（与现有 travel/travel 同口径：单号失败只该号 WARN，不影响其他账号）：
//   - 按天幂等：每日每号最多领一轮（rewardClaimedToday 闸；自然日 CST 重置，进程重启清零——
//     重启后当日重复 redeem 由上游 409 duplicate 正常态兜底，不刷 WARN）。
//   - 服务端正常态识别为静默跳过：redeem 409 duplicate/403 天数不足；lottery 400 无次数/未开启——
//     这些不是失败，不刷 WARN（见 upstream.IsRedeemAlreadyClaimed 等）。
//   - 领奖只领「本次新达标」的档位：状态非 claimed 且 days>=档位天数。跨档连领（14d 未领而
//     days 已到 28）是官方正常态（spa 按 byTier 逐档可兑），但每日一轮限一档，避免同日多写。
//   - 礼包/补偿是「有则领」的幂等写，业务错误静默；补签只在「昨日漏签且有卡」时触发，
//     无卡/无漏签不写。
//
// 日志每号一行可 grep：`activity %s: redeem tier=%s ...` / `activity %s: lottery ...`。
func (s *Scheduler) claimGrowthRewards(a *auth.Auth) {
	if a == nil || a.AccessTokenValue() == "" {
		return
	}
	// global 门控：连登奖励/抽奖链只服务 CN。国际版 /activity/growth/* 端点虽同构存在
	// （/tmp/analysis-global-credit.md §1.1：lottery/streak/redeem 在国际版上线），但真实
	// global 新账号 GET /activity/growth/streak 返回 500（实测）——链上第一步就
	// 拿不到 days，无法挑档；且 streak 500 会每趟刷 WARN 污染日志。结论：证据不足，跳过
	// global（不发起任何领取类调用）。CN 账号无此问题（CN streak 200 days=N）。
	if a.IsGlobal() {
		return
	}
	if s.rewardClaimedToday(a.UID) {
		return // 当日已领过一轮，跳过（按天幂等）
	}
	// 0. 礼包/补偿领取（panel 连登管家口径）：每号一次 / 有则领的幂等写，
	// 业务错误静默跳过（不刷 WARN），先于 redeem——到账积分不依赖连登状态。
	s.claimGrowthBonus(a)
	state, err := s.cfg.Upstream.GrowthRewardState(a)
	if err != nil {
		log.Printf("WARN: activity %s: reward-state: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	// 0.5 补签保连登（panel makeupYesterday 口径）：昨日漏签（heatmap score==0）且
	// 有补签卡 → 补昨日。放在 reward-state 之后：若补签把 streak 恢复到新档位，
	// 重读 state 让本日 redeem 直接吃到恢复后的天数（补签是保 7d/14d/28d 里程碑的关键）。
	if s.makeupYesterday(a) {
		if st2, err2 := s.cfg.Upstream.GrowthRewardState(a); err2 == nil {
			state = st2 // 补签成功 → 用恢复后的天数挑档
		}
	}
	days := state.Days()
	tier := growthEligibleTier(days, &state.Redemption)
	if tier == "" {
		// 无新达标档位：不动写接口（不刷 WARN，这是正常态——很多天没到 7d）。
		return
	}
	res, err := s.cfg.Upstream.GrowthRedeem(a, tier, "")
	switch {
	case err == nil:
		log.Printf("activity %s: redeem tier=%s ok (+%d credit, +%d energy, +%d chances)",
			logfmt.Label(a.UID, a.Nickname), tier, res.CreditGranted, res.EnergyGranted, res.ChancesGranted)
	case upstream.IsRedeemAlreadyClaimed(err) || upstream.IsRedeemNotEnoughDays(err):
		log.Printf("activity %s: redeem tier=%s skip (already claimed or days not enough)", logfmt.Label(a.UID, a.Nickname), tier)
	default:
		log.Printf("activity %s: redeem tier=%s: %v", logfmt.Label(a.UID, a.Nickname), tier, err)
	}
	// 标记当日已处理（无论 redeem 是否成功都记一次：领取类各状态当日不再重试，
	// 避免对上游重复写；成功→无需再领，失败→当日不轰炸，次日自然日重置/上游幂等兜底）。
	s.markRewardClaimed(a.UID)
	s.claimGrowthLottery(a)
}

// growthEligibleTier 按当前连登天数挑选「尚未领取且达标」的最高档位。
// 返回 "" 表示无可领档（未达标或全部已领），调用方据此跳过 redeem（正常态）。
func growthEligibleTier(days int, rs *upstream.GrowthRedemptionStatus) string {
	if rs == nil {
		return ""
	}
	// 档位按 days 升序，从高到低挑最高的已达标未领档（一次领一份，每日一轮）。
	for i := len(rs.Tiers) - 1; i >= 0; i-- {
		sp := rs.Tiers[i]
		if days >= sp.Days && !rs.Claimed(sp.Tier) {
			return sp.Tier
		}
	}
	return ""
}

// claimGrowthBonus 新手礼包 + 活动补偿领取（panel 连登管家口径）。
// 两者都是幂等写：礼包每号一次（已领业务错误静默）、补偿有则领（无则业务错误静默）。
// 只在成功到账时打日志（每号一生一次的事件，不值得每日刷行）；失败静默——
// 业务错误是常态（绝大多数号早已领过），无法与真错误可靠区分，不刷 WARN。
func (s *Scheduler) claimGrowthBonus(a *auth.Auth) {
	if credit, err := s.cfg.Upstream.ClaimGift(a); err == nil && credit > 0 {
		log.Printf("activity %s: gift ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
	if credit, err := s.cfg.Upstream.ClaimCompensation(a); err == nil && credit > 0 {
		log.Printf("activity %s: compensation ok (+%d credit)", logfmt.Label(a.UID, a.Nickname), credit)
	}
}

// makeupYesterday 昨日漏签且有补签卡时自动补签（保住连登连续天数，panel 口径）。
// 连续天数一断就要重攒 7 天，一张卡代价远小——有漏签 + 有卡即补。
// 判据链：heatmap 昨日格 score==0（漏签）→ streak.makeup_cards.balance>0（有卡）
// → POST makeup-cards/use {"target_date":昨日}。
// 无卡 / 无漏签 / 无该日格 / 查询失败均静默返回 false（不影响主流程）；
// 补签成功打一行日志并返回 true（调用方重读 streak 天数挑档）。
func (s *Scheduler) makeupYesterday(a *auth.Auth) bool {
	cells, err := s.cfg.Upstream.GrowthHeatmap(a)
	if err != nil {
		return false // 只读判据失败：静默（每日重试，无写风险）
	}
	yesterday := upstream.GrowthYesterdayDate(time.Now())
	score, ok := upstream.HeatmapDayScore(cells, yesterday)
	if !ok || score != 0 {
		return false // 昨日有分或无判据：无需补签
	}
	// 有漏签 → 查补签卡余额（streak 端点同一响应体）。
	st, err := s.cfg.Upstream.GrowthStreakWithCards(a)
	if err != nil || st.MakeupCards.Balance <= 0 {
		return false // 无卡或查询失败：静默（次日再判）
	}
	if err := s.cfg.Upstream.UseMakeupCard(a, yesterday); err != nil {
		log.Printf("activity %s: makeup %s: %v", logfmt.Label(a.UID, a.Nickname), yesterday, err)
		return false
	}
	log.Printf("activity %s: makeup ok %s (+streak kept)", logfmt.Label(a.UID, a.Nickname), yesterday)
	return true
}

// claimGrowthLottery 消耗连登奖励赠与的抽奖次数。仅抽 balance>0 的次数；无次数跳过
// （400 insufficient 正常态静默）；抽奖未开启（400 lottery disabled）静默。
// client_token 每次 draw 必须新键（security-relevant，见 upstream.GrowthLotteryDraw）。
func (s *Scheduler) claimGrowthLottery(a *auth.Auth) {
	chances, err := s.cfg.Upstream.GrowthLotteryChances(a)
	if err != nil {
		log.Printf("WARN: activity %s: lottery-chances: %v", logfmt.Label(a.UID, a.Nickname), err)
		return
	}
	if chances <= 0 {
		log.Printf("activity %s: lottery skip (no chances)", logfmt.Label(a.UID, a.Nickname))
		return
	}
	res, err := s.cfg.Upstream.GrowthLotteryDraw(a, "") // 每次自动新 client_token
	switch {
	case err == nil:
		log.Printf("activity %s: lottery drawn prize=%s (%s)", logfmt.Label(a.UID, a.Nickname), res.PrizeName, res.PrizeType)
	case upstream.IsLotteryNoChance(err) || upstream.IsLotteryDisabled(err):
		log.Printf("activity %s: lottery skip (no chances or disabled)", logfmt.Label(a.UID, a.Nickname))
	default:
		log.Printf("activity %s: lottery draw: %v", logfmt.Label(a.UID, a.Nickname), err)
	}
}

// rewardClaimedToday 该账号当日是否已处理过连登奖励领取（自然日 CST）。
func (s *Scheduler) rewardClaimedToday(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rewardClaimed[uid] == travelDay(time.Now())
}

// markRewardClaimed 记录该账号当日已处理连登奖励领取。
func (s *Scheduler) markRewardClaimed(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rewardClaimed[uid] = travelDay(time.Now())
}

// RunKeepaliveNow 立即对所有账号刷新 token；session 死亡的自动禁用。
// 12153 禁用走 Pool.NoteSessionDead 的**连续计数**语义：一次刷新失败不再立即杀号，
// 连续 sessionDeadThreshold 次（3 次）才禁用（P0-1：13 个 disabled 号全是历史误判）。
// 刷新成功 → ClearSessionDead 清计数（错误判定的账号有复活路径）。
func (s *Scheduler) RunKeepaliveNow() {
	for _, st := range s.cfg.Pool.List() {
		if st.Disabled {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.RefreshTokenValue() == "" {
			continue
		}
		if err := s.cfg.Upstream.RefreshToken(a); err != nil {
			log.Printf("keepalive %s: %v", logfmt.Label(st.UID, st.Nickname), err)
			var ue *upstream.Error
			if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
				if s.cfg.Pool.NoteSessionDead(st.UID) {
					log.Printf("WARN: keepalive %s: 连续 %d 次 12153 session dead — 禁用", logfmt.Label(st.UID, st.Nickname), pool.SessionDeadThreshold())
				}
			}
			continue
		}
		s.cfg.Pool.ClearSessionDead(st.UID) // 刷新成功清误判计数，失败不该累计
		a.BackfillRealm()                   // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
		if err := a.SaveAtomic(); err != nil {
			log.Printf("keepalive %s save: %v", logfmt.Label(st.UID, st.Nickname), err)
		}
	}
}

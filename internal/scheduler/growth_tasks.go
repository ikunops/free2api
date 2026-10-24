// growth_tasks.go 通用成长任务扫描与领取。
//
// 边界：本文件只做“读状态 + 领取已经 completed 的任务”。可点亮任务的事件构造在
// growth_executors.go，必须按白名单逐类放行；PC 限定任务不在这里伪造。
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

var (
	errGrowthAccountNotFound = errors.New("成长任务账号不存在")
	errGrowthAccountDisabled = errors.New("成长任务账号已停用")
	errGrowthAccountGlobal   = errors.New("国际版账号不参与成长任务")
)

// IsGrowthAccountNotFound / Disabled / Global 供 server 映射成稳定的 HTTP 语义。
func IsGrowthAccountNotFound(err error) bool { return errors.Is(err, errGrowthAccountNotFound) }
func IsGrowthAccountDisabled(err error) bool { return errors.Is(err, errGrowthAccountDisabled) }
func IsGrowthAccountGlobal(err error) bool   { return errors.Is(err, errGrowthAccountGlobal) }

// GrowthTaskStatus 单账号任务扫描结果。
type GrowthTaskStatus struct {
	UID      string                `json:"uid"`
	Nickname string                `json:"nickname"`
	Realm    string                `json:"realm"`
	Platform string                `json:"platform"`
	OK       bool                  `json:"ok"`
	Error    string                `json:"error,omitempty"`
	Tasks    []upstream.GrowthTask `json:"tasks,omitempty"`
	// LocalPending / LocalCapable 标记本网关是否有低风险执行器可补进度。
	// 不是“已完成”承诺，只是给面板区分“能自动做”和“必须真实客户端”。
	LocalPending int `json:"local_pending,omitempty"`
	LocalCapable int `json:"local_capable,omitempty"`
	// 账号资产与消耗画像：来自号池本地状态，不额外请求上游。
	Credits         int64     `json:"credits"`
	CreditsExpiring int64     `json:"credits_expiring,omitempty"`
	CreditsUrgent   int64     `json:"credits_urgent,omitempty"`
	SuccessCount    int64     `json:"success_count,omitempty"`
	LastSuccessTime time.Time `json:"last_success,omitempty"`
	ScannedAt       time.Time `json:"scanned_at"`
}

// GrowthScanResult 全池扫描结果。
type GrowthScanResult struct {
	ScannedAt time.Time          `json:"scanned_at"`
	Accounts  []GrowthTaskStatus `json:"accounts"`
	Error     string             `json:"error,omitempty"`
}

// GrowthClaimResult 已完成任务领取结果。
type GrowthClaimResult struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname"`
	Code     string `json:"code"`
	TaskName string `json:"task_name,omitempty"`
	Platform string `json:"platform"`
	Status   string `json:"status"` // claimed / already / skipped / failed
	Credit   int64  `json:"credit,omitempty"`
	Energy   int64  `json:"energy,omitempty"`
	Error    string `json:"error,omitempty"`
}

// GrowthClaimReport 全池领取结果。
type GrowthClaimReport struct {
	StartedAt  time.Time           `json:"started_at"`
	FinishedAt time.Time           `json:"finished_at"`
	DryRun     bool                `json:"dry_run"`
	Results    []GrowthClaimResult `json:"results"`
}

// GrowthAccountRunReport 是单账号“帮我做”操作的返回。
// 管理面只展示结果数量，不暴露内部任务白名单或平台口径。
type GrowthAccountRunReport struct {
	UID      string    `json:"uid"`
	Nickname string    `json:"nickname,omitempty"`
	Started  time.Time `json:"started_at"`
	Finished time.Time `json:"finished_at"`
	Reported int       `json:"reported"`
	Claimed  int       `json:"claimed"`
	Already  int       `json:"already"`
	Failed   int       `json:"failed"`
	Errors   []string  `json:"errors,omitempty"`
	Credit   int64     `json:"credit,omitempty"`
	Energy   int64     `json:"energy,omitempty"`
}

// GrowthTaskExecutor 可点亮任务的执行器。返回 nil 表示本轮已上报，仍需回读确认；
// 不代表任务已完成。实现必须幂等、可取消，并且不得伪造 PC 限定任务。
type GrowthTaskExecutor interface {
	Code() string
	Run(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) error
}

// growthClaimDelay 两次领取之间的节流；测试可置 0。
var growthClaimDelay = 800 * time.Millisecond

// growthTaskMu 串行化手动与定时成长任务扫描，避免同一账号同时被扫两遍。
var growthTaskMu sync.Mutex

// ScanGrowthTasks 扫描全部 CN WorkBuddy 账号的普通任务与小程序任务。
// 只读，不写上游。global 账号不参与 CN 任务中心，直接跳过。
func (s *Scheduler) ScanGrowthTasks(ctx context.Context) GrowthScanResult {
	growthTaskMu.Lock()
	defer growthTaskMu.Unlock()
	return s.scanGrowthTasksLocked(ctx)
}

func (s *Scheduler) scanGrowthTasksLocked(ctx context.Context) GrowthScanResult {
	res := GrowthScanResult{ScannedAt: time.Now()}
	statuses := s.cfg.Pool.List()
	for _, st := range statuses {
		if ctx.Err() != nil {
			res.Error = ctx.Err().Error()
			break
		}
		if st.Disabled {
			continue
		}
		if strings.TrimSpace(st.Producer) != "" && st.Producer != "workbuddy" {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		if a.IsGlobal() {
			continue
		}
		for _, platform := range []upstream.GrowthTaskPlatform{upstream.GrowthPlatformDefault, upstream.GrowthPlatformMiniprogram} {
			row := GrowthTaskStatus{
				UID: st.UID, Nickname: st.Nickname, Realm: a.Realm(), Platform: platformLabel(platform), ScannedAt: time.Now(),
				Credits: st.Credits, CreditsExpiring: st.CreditsExpiring, CreditsUrgent: st.CreditsUrgent,
				SuccessCount: st.SuccessCount, LastSuccessTime: st.LastSuccessTime,
			}
			tasks, err := s.cfg.Upstream.ListGrowthTasks(a, platform)
			if err != nil {
				row.Error = err.Error()
			} else {
				row.OK = true
				row.Tasks = tasks
				for _, t := range tasks {
					ex, ok := growthTaskExecutors[t.TaskCode]
					if !ok || ex == nil || t.AcceptStatus == "claimed" {
						continue
					}
					if !growthExecutorMatchesPlatform(ex, platform) {
						continue
					}
					row.LocalCapable++
					if t.AcceptStatus != "completed" {
						row.LocalPending++
					}
				}
			}
			res.Accounts = append(res.Accounts, row)
		}
	}
	return res
}

func platformLabel(p upstream.GrowthTaskPlatform) string {
	if p == upstream.GrowthPlatformMiniprogram {
		return "miniprogram"
	}
	return "default"
}

// ClaimCompletedGrowthTasks 只领取 completed/claimed 状态的任务。dryRun=true 只返回计划。
// 这是最低风险的写操作：不点亮任何未完成任务，不构造事件，不碰 PC 限定任务。
func (s *Scheduler) ClaimCompletedGrowthTasks(ctx context.Context, dryRun bool) GrowthClaimReport {
	growthTaskMu.Lock()
	defer growthTaskMu.Unlock()
	return s.claimCompletedGrowthTasksLocked(ctx, dryRun, "")
}

func (s *Scheduler) claimCompletedGrowthTasksLocked(ctx context.Context, dryRun bool, onlyUID string) GrowthClaimReport {
	rep := GrowthClaimReport{StartedAt: time.Now(), DryRun: dryRun}
	scan := s.scanGrowthTasksLocked(ctx)
	for _, acct := range scan.Accounts {
		if ctx.Err() != nil {
			break
		}
		if onlyUID != "" && acct.UID != onlyUID {
			continue
		}
		if !acct.OK || len(acct.Tasks) == 0 {
			continue
		}
		a := s.cfg.Pool.AuthByUID(acct.UID)
		if a == nil {
			continue
		}
		platform := upstream.GrowthPlatformDefault
		if acct.Platform == "miniprogram" {
			platform = upstream.GrowthPlatformMiniprogram
		}
		for _, t := range acct.Tasks {
			if ctx.Err() != nil {
				break
			}
			if t.AcceptStatus != "completed" {
				continue
			}
			r := GrowthClaimResult{UID: acct.UID, Nickname: acct.Nickname, Code: t.TaskCode, Platform: acct.Platform, TaskName: t.TaskName}
			if dryRun {
				r.Status = "skipped"
				rep.Results = append(rep.Results, r)
				continue
			}
			outcome, err := s.cfg.Upstream.ClaimGrowthTask(a, t.TaskCode, platform)
			switch {
			case err == nil && outcome.AlreadyClaimed:
				r.Status = "already"
				r.Credit, r.Energy = outcome.Credit, outcome.Energy
			case err == nil:
				r.Status = "claimed"
				r.Credit, r.Energy = outcome.Credit, outcome.Energy
			case upstream.IsGrowthAlreadyClaimed(err):
				r.Status = "already"
			default:
				r.Status = "failed"
				r.Error = err.Error()
			}
			rep.Results = append(rep.Results, r)
			if !dryRun {
				// 单账号串行，账号之间也不需要并发；成长任务写操作刻意慢一点。
				sleepCtx(ctx, growthClaimDelay)
			}
		}
	}
	rep.FinishedAt = time.Now()
	return rep
}

// RunGrowthNow 定时任务入口：白名单任务补进度 + 回读 + 领取已完成任务。
// 不在白名单的任务不会被伪造；PC/真实客户端限定任务留给后续 CDP 适配器。
func (s *Scheduler) RunGrowthNow() {
	s.RunGrowthNowReport(context.Background())
}

// growthClaimRetryDelay 让执行完任务后的二次回读等上游完成状态落库。
// 测试可置 0；生产保持短延迟，仍不依赖一次 HTTP 200 就当完成。
var growthClaimRetryDelay = 8 * time.Second

// RunGrowthNowReport 与 RunGrowthNow 同语义，但返回领取明细，供管理面手动触发。
func (s *Scheduler) RunGrowthNowReport(ctx context.Context) GrowthClaimReport {
	growthTaskMu.Lock()
	defer growthTaskMu.Unlock()
	reported, execErrors := s.runGrowthExecutorsLocked(ctx, "", "")
	rep := s.claimCompletedGrowthTasksLocked(ctx, false, "")
	if len(rep.Results) == 0 && reported > 0 && growthClaimRetryDelay > 0 {
		select {
		case <-time.After(growthClaimRetryDelay):
		case <-ctx.Done():
			rep.FinishedAt = time.Now()
			return rep
		}
		// WorkBuddy 的完成状态是异步落库的。事件上报成功后立即回读仍可能是
		// not_accepted/completed=0；等一个短窗口再领取，避免把已完成的专家任务
		// 留到下一次排程才领。
		rep = s.claimCompletedGrowthTasksLocked(ctx, false, "")
	}
	claimed, already, failed := 0, 0, 0
	for _, r := range rep.Results {
		switch r.Status {
		case "claimed":
			claimed++
		case "already":
			already++
		case "failed":
			failed++
			log.Printf("WARN: growth claim %s %s: %s", logfmt.Label(r.UID, r.Nickname), r.Code, r.Error)
		}
	}
	log.Printf("growth scan+claim: reported=%d claimed=%d already=%d failed=%d", reported, claimed, already, failed)
	for _, msg := range execErrors {
		log.Printf("WARN: growth run: %s", msg)
	}
	return rep
}

// RunGrowthAccountReport 只处理指定 CN WorkBuddy 账号：补白名单任务进度，
// 回读后领取该账号已经 completed 的任务。global/不存在/非 workbuddy 均拒绝。
func (s *Scheduler) RunGrowthAccountReport(ctx context.Context, uid, taskCode string) (GrowthAccountRunReport, error) {
	uid = strings.TrimSpace(uid)
	taskCode = strings.TrimSpace(taskCode)
	rep := GrowthAccountRunReport{UID: uid, Started: time.Now()}
	if uid == "" {
		return rep, errGrowthAccountNotFound
	}
	growthTaskMu.Lock()
	defer growthTaskMu.Unlock()

	st, _, err := s.growthAccountLocked(uid)
	if err != nil {
		return rep, err
	}
	rep.Nickname = st.Nickname
	reported, execErrors := s.runGrowthExecutorsLocked(ctx, uid, taskCode)
	claim := s.claimCompletedGrowthTasksLocked(ctx, false, uid)
	// WorkBuddy 的完成状态异步落库：事件刚上报时立即回读可能仍是
	// not_accepted。单项操作也等一个短窗口再回读，避免用户看到“做了但没完成”。
	if len(claim.Results) == 0 && reported > 0 && growthClaimRetryDelay > 0 {
		select {
		case <-time.After(growthClaimRetryDelay):
		case <-ctx.Done():
			rep.Reported = reported
			rep.Errors = append(rep.Errors, execErrors...)
			rep.Finished = time.Now()
			return rep, nil
		}
		claim = s.claimCompletedGrowthTasksLocked(ctx, false, uid)
	}
	rep.Reported = reported
	rep.Errors = append(rep.Errors, execErrors...)
	for _, r := range claim.Results {
		switch r.Status {
		case "claimed":
			rep.Claimed++
			rep.Credit += r.Credit
			rep.Energy += r.Energy
		case "already":
			rep.Already++
		case "failed":
			rep.Failed++
			rep.Errors = append(rep.Errors, r.Code+": "+r.Error)
		}
	}
	rep.Finished = time.Now()
	return rep, nil
}

func (s *Scheduler) growthAccountLocked(uid string) (pool.Status, *auth.Auth, error) {
	for _, st := range s.cfg.Pool.List() {
		if st.UID != uid {
			continue
		}
		if st.Disabled {
			return st, nil, errGrowthAccountDisabled
		}
		if strings.TrimSpace(st.Producer) != "" && st.Producer != "workbuddy" {
			return st, nil, errGrowthAccountNotFound
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			return st, nil, errGrowthAccountNotFound
		}
		if a.IsGlobal() {
			return st, nil, errGrowthAccountGlobal
		}
		return st, a, nil
	}
	return pool.Status{}, nil, errGrowthAccountNotFound
}

type growthSummaryTask struct {
	TaskCode     string                  `json:"task_code"`
	TaskName     string                  `json:"task_name"`
	TaskDesc     string                  `json:"task_desc,omitempty"`
	Platform     string                  `json:"platform,omitempty"`
	Progress     upstream.GrowthProgress `json:"progress"`
	Status       string                  `json:"status"`
	Action       string                  `json:"action"` // run / claim / manual
	Reward       json.RawMessage         `json:"reward,omitempty"`
	RewardCredit *int64                  `json:"reward_credit,omitempty"`
	RewardEnergy *int64                  `json:"reward_energy,omitempty"`
	HasReward    bool                    `json:"has_reward,omitempty"`
}

type growthSummaryAccount struct {
	UID             string              `json:"uid"`
	Nickname        string              `json:"nickname"`
	Realm           string              `json:"realm"`
	OK              bool                `json:"ok"`
	Error           string              `json:"error,omitempty"`
	Done            int                 `json:"done"`
	Pending         int                 `json:"pending"`
	CanHelp         int                 `json:"can_help"`
	Manual          int                 `json:"manual"`
	Claimable       int                 `json:"claimable"`
	Credits         int64               `json:"credits"`
	CreditsExpiring int64               `json:"credits_expiring,omitempty"`
	CreditsUrgent   int64               `json:"credits_urgent,omitempty"`
	SuccessCount    int64               `json:"success_count,omitempty"`
	LastSuccessTime time.Time           `json:"last_success,omitempty"`
	Tasks           []growthSummaryTask `json:"tasks"`
}

// GrowthTaskSummary 生成账号视角摘要：每个账号还剩哪些任务、能否帮做/领取。
// 普通任务与小程序任务按 UID 合并，已领取任务不展示，global 不进入面板。
func GrowthTaskSummary(res GrowthScanResult) map[string]any {
	type accState struct {
		meta      growthSummaryAccount
		all       map[string]growthSummaryTask
		platforms map[string]string
		done      map[string]bool
	}
	byUID := map[string]*accState{}
	for _, acct := range res.Accounts {
		if acct.Realm == "global" {
			continue
		}
		st := byUID[acct.UID]
		if st == nil {
			st = &accState{
				meta: growthSummaryAccount{
					UID: acct.UID, Nickname: acct.Nickname, Realm: acct.Realm, OK: acct.OK, Error: acct.Error,
					Credits: acct.Credits, CreditsExpiring: acct.CreditsExpiring, CreditsUrgent: acct.CreditsUrgent,
					SuccessCount: acct.SuccessCount, LastSuccessTime: acct.LastSuccessTime,
				},
				all:       map[string]growthSummaryTask{},
				platforms: map[string]string{},
				done:      map[string]bool{},
			}
			byUID[acct.UID] = st
		}
		if acct.OK {
			st.meta.OK = true
			st.meta.Error = ""
			if acct.Credits > st.meta.Credits {
				st.meta.Credits = acct.Credits
			}
			if acct.CreditsExpiring > st.meta.CreditsExpiring {
				st.meta.CreditsExpiring = acct.CreditsExpiring
			}
			if acct.CreditsUrgent > st.meta.CreditsUrgent {
				st.meta.CreditsUrgent = acct.CreditsUrgent
			}
			if acct.SuccessCount > st.meta.SuccessCount {
				st.meta.SuccessCount = acct.SuccessCount
			}
			if acct.LastSuccessTime.After(st.meta.LastSuccessTime) {
				st.meta.LastSuccessTime = acct.LastSuccessTime
			}
		} else if st.meta.Error == "" {
			st.meta.Error = acct.Error
		}
		for _, t := range acct.Tasks {
			if t.AcceptStatus == "claimed" {
				st.done[t.TaskCode] = true
				continue
			}
			if t.Done() {
				st.done[t.TaskCode] = true
			}
			status := t.AcceptStatus
			if status == "" {
				status = "not_accepted"
			}
			action := "manual"
			switch {
			case status == "completed":
				action = "claim"
			case growthTaskExecutors[t.TaskCode] != nil:
				action = "run"
			}
			item := growthSummaryTask{
				TaskCode: t.TaskCode, TaskName: t.TaskName, TaskDesc: t.TaskDesc,
				Progress: t.Progress, Status: status, Action: action, Reward: t.Reward,
				RewardCredit: t.RewardCredit, RewardEnergy: t.RewardEnergy, HasReward: t.HasReward,
			}
			if old, ok := st.all[t.TaskCode]; ok && growthTaskRank(old.Status) >= growthTaskRank(status) {
				continue
			}
			st.all[t.TaskCode] = item
			st.platforms[t.TaskCode] = acct.Platform
		}
	}

	accounts := make([]growthSummaryAccount, 0, len(byUID))
	var pending, canHelp, manual, claimable int
	for _, st := range byUID {
		acc := st.meta
		acc.Done = len(st.done)
		for _, t := range st.all {
			t.Platform = st.platforms[t.TaskCode]
			if t.Status != "claimed" {
				acc.Pending++
			}
			if t.Status == "claimed" {
				continue
			}
			acc.Tasks = append(acc.Tasks, t)
			switch t.Action {
			case "claim":
				acc.Claimable++
				claimable++
			case "run":
				acc.CanHelp++
				canHelp++
			default:
				acc.Manual++
				manual++
			}
		}
		pending += acc.Pending
		if acc.Done == 0 && acc.Pending == 0 && acc.Error == "" {
			continue
		}
		sort.Slice(acc.Tasks, func(i, j int) bool {
			if acc.Tasks[i].Action != acc.Tasks[j].Action {
				return growthTaskActionRank(acc.Tasks[i].Action) < growthTaskActionRank(acc.Tasks[j].Action)
			}
			return acc.Tasks[i].TaskName < acc.Tasks[j].TaskName
		})
		accounts = append(accounts, acc)
	}
	sort.Slice(accounts, func(i, j int) bool {
		if accounts[i].Pending != accounts[j].Pending {
			return accounts[i].Pending > accounts[j].Pending
		}
		return accounts[i].Nickname < accounts[j].Nickname
	})
	done := 0
	for _, acc := range accounts {
		done += acc.Done
	}
	return map[string]any{
		"scanned_at": res.ScannedAt, "done": done, "pending": pending, "can_help": canHelp,
		"manual": manual, "claimable": claimable, "accounts": accounts, "error": res.Error,
		"executor_codes": GrowthExecutorCodes(),
	}
}

func growthTaskRank(status string) int {
	switch status {
	case "claimed":
		return 4
	case "completed":
		return 3
	case "accepted":
		return 2
	case "not_accepted":
		return 1
	default:
		return 0
	}
}

func growthTaskActionRank(action string) int {
	switch action {
	case "claim":
		return 0
	case "run":
		return 1
	default:
		return 2
	}
}

// GrowthExecutorCodes 返回当前构建声明可自动补进度的任务码。
// 管理页用它显示“这一项具体怎么自动做”，也便于诊断“为什么某些项还是手动”。
func GrowthExecutorCodes() []string {
	out := make([]string, 0, len(growthTaskExecutors))
	for code := range growthTaskExecutors {
		out = append(out, code)
	}
	sort.Strings(out)
	return out
}

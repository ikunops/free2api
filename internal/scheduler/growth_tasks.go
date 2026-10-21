// growth_tasks.go 通用成长任务扫描与领取。
//
// 边界：本文件只做“读状态 + 领取已经 completed 的任务”。可点亮任务的事件构造在
// growth_executors.go，必须按白名单逐类放行；PC 限定任务不在这里伪造。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/upstream"
)

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
	LocalPending int       `json:"local_pending,omitempty"`
	LocalCapable int       `json:"local_capable,omitempty"`
	ScannedAt    time.Time `json:"scanned_at"`
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

// GrowthTaskExecutor 可点亮任务的执行器。返回 nil 表示本轮已上报，仍需回读确认；
// 不代表任务已完成。实现必须幂等、可取消，并且不得伪造 PC 限定任务。
type GrowthTaskExecutor interface {
	Code() string
	Run(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) error
}

// growthTaskExecutors 可点亮任务白名单。这里只注册“接口型、低风险、无需真实客户端
// 操作”的任务；桌面/小程序必须真实交互的任务（canvas、template、skill、school、
// RichMeow、Library_read、Expert_Philanthropy 等）刻意不注册，留给后续 CDP 适配器。
var growthTaskExecutors = map[string]GrowthTaskExecutor{
	"chat_5":             growthChatExecutor{code: "chat_5", target: 5},
	"Model_chat_GLM5.2":  growthChatExecutor{code: "Model_chat_GLM5.2", target: 1, modelID: "glm-5.2", modelName: "GLM-5.2"},
	"Sequential_Tasks_1": growthChatExecutor{code: "Sequential_Tasks_1", target: 1, mini: true},
}

// RegisterGrowthTaskExecutor 注册可点亮任务执行器（测试和后续模块使用）。
func RegisterGrowthTaskExecutor(e GrowthTaskExecutor) {
	if e == nil || strings.TrimSpace(e.Code()) == "" {
		return
	}
	growthTaskExecutors[e.Code()] = e
}

// growthClaimDelay 两次领取之间的节流；测试可置 0。
var growthClaimDelay = 800 * time.Millisecond

// growthTaskMu 串行化手动与定时成长任务扫描，避免同一账号同时被扫两遍。
var growthTaskMu sync.Mutex

// ScanGrowthTasks 扫描全部 CN WorkBuddy 账号的普通任务与小程序任务。
// 只读，不写上游。global 账号不参与 CN 任务中心，明确跳过并记录原因。
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
			res.Accounts = append(res.Accounts, GrowthTaskStatus{
				UID: st.UID, Nickname: st.Nickname, Realm: "global", OK: false,
				Error: "global realm 不适用 CN 成长任务", ScannedAt: time.Now(),
			})
			continue
		}
		for _, platform := range []upstream.GrowthTaskPlatform{upstream.GrowthPlatformDefault, upstream.GrowthPlatformMiniprogram} {
			row := GrowthTaskStatus{UID: st.UID, Nickname: st.Nickname, Realm: a.Realm(), Platform: platformLabel(platform), ScannedAt: time.Now()}
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
	return s.claimCompletedGrowthTasksLocked(ctx, dryRun)
}

func (s *Scheduler) claimCompletedGrowthTasksLocked(ctx context.Context, dryRun bool) GrowthClaimReport {
	rep := GrowthClaimReport{StartedAt: time.Now(), DryRun: dryRun}
	scan := s.scanGrowthTasksLocked(ctx)
	for _, acct := range scan.Accounts {
		if ctx.Err() != nil {
			break
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

// RunGrowthNow 定时任务入口：白名单任务补进度 + 回读 + 领取已完成任务。\n// 不在白名单的任务不会被伪造；PC/真实客户端限定任务留给后续 CDP 适配器。
func (s *Scheduler) RunGrowthNow() {
	s.RunGrowthNowReport(context.Background())
}

// RunGrowthNowReport 与 RunGrowthNow 同语义，但返回领取明细，供管理面手动触发。
func (s *Scheduler) RunGrowthNowReport(ctx context.Context) GrowthClaimReport {
	growthTaskMu.Lock()
	defer growthTaskMu.Unlock()
	reported := s.runGrowthExecutorsLocked(ctx)
	rep := s.claimCompletedGrowthTasksLocked(ctx, false)
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
	return rep
}

// GrowthTaskSummary 生成面板摘要：每个账号缺什么、哪些可领。
func GrowthTaskSummary(res GrowthScanResult) map[string]any {
	var total, completed, claimable, pending, localPending, localCapable int
	var credit, energy int64
	byAccount := make([]map[string]any, 0, len(res.Accounts))
	for _, acct := range res.Accounts {
		var acctTotal, acctDone, acctClaim, acctPending int
		for _, t := range acct.Tasks {
			acctTotal++
			total++
			switch {
			case t.AcceptStatus == "claimed":
				acctDone++
				completed++
			case t.AcceptStatus == "completed":
				acctClaim++
				claimable++
				var rw struct {
					Credit int64 `json:"credit"`
					Energy int64 `json:"energy"`
				}
				if len(t.Reward) > 0 {
					_ = json.Unmarshal(t.Reward, &rw)
				}
				credit += rw.Credit
				energy += rw.Energy
			default:
				acctPending++
				pending++
			}
		}
		localPending += acct.LocalPending
		localCapable += acct.LocalCapable
		byAccount = append(byAccount, map[string]any{
			"uid": acct.UID, "nickname": acct.Nickname, "realm": acct.Realm,
			"platform": acct.Platform, "ok": acct.OK, "error": acct.Error,
			"total": acctTotal, "claimed": acctDone, "claimable": acctClaim, "pending": acctPending,
			"local_pending": acct.LocalPending, "local_capable": acct.LocalCapable,
			"tasks": acct.Tasks,
		})
	}
	sort.Slice(byAccount, func(i, j int) bool {
		return fmt.Sprint(byAccount[i]["uid"]) < fmt.Sprint(byAccount[j]["uid"])
	})
	return map[string]any{
		"scanned_at": res.ScannedAt, "total": total, "claimed": completed,
		"claimable": claimable, "pending": pending, "claimable_credit": credit, "claimable_energy": energy,
		"local_pending": localPending, "local_capable": localCapable,
		"accounts": byAccount, "error": res.Error,
	}
}

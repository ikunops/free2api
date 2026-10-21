// growth_executors.go 低风险“接口型”成长任务执行器。
//
// 边界：只做官方客户端会做的上报，不伪造需要真实 UI/业务副作用的动作。
// 当前注册 chat_5 / Model_chat_GLM5.2 / Sequential_Tasks_1；其余任务只扫描、
// 等待用户真实完成或后续 CDP 适配器，不在 Go 侧坐标点击。
package scheduler

import (
	"context"
	"log"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/upstream"
)

// 以下节流值生产环境保持官方客户端节奏；测试可置 0。
var (
	growthAcceptDelay = 600 * time.Millisecond
	growthEventGap    = 1050 * time.Millisecond
	growthExecutorGap = 1200 * time.Millisecond
)

// growthChatExecutor 通过 chat_request_send 事件点亮“真实对话 N 次”类任务。
// mini=true 走小程序口径（任务列表/accept/claim 带 X-Client-Platform），事件形状
// 使用 source=mini_program。事件只上报，不直接 claim；完成后由 RunGrowthNow
// 统一回读 accept_status，只有 completed 才领取。
type growthChatExecutor struct {
	code      string
	target    int
	modelID   string
	modelName string
	mini      bool
}

func (e growthChatExecutor) Code() string { return e.code }

func (e growthChatExecutor) Run(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) error {
	need := e.target
	if task.Progress.Target > 0 {
		need = task.Progress.Target
	}
	remain := need - task.Progress.Current
	if remain <= 0 {
		return nil
	}
	// 防御：官方任务 target 不会大；异常下发（例如 1000）不把号打成刷子。
	if remain > 20 {
		remain = 20
	}
	modelID, modelName := e.modelID, e.modelName
	if modelID == "" {
		modelID, modelName = "deepseek-v4-flash", "DeepSeek V4 Flash"
	}
	for i := 0; i < remain; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		cid := upstream.NewGrowthConversationID("growth")
		ev := growthChatEvent(a, cid, modelID, modelName, e.mini)
		if err := c.ReportGrowthEvents(a, []map[string]any{ev}); err != nil {
			return err
		}
		if i < remain-1 {
			if !sleepCtx(ctx, growthEventGap) {
				return ctx.Err()
			}
		}
	}
	return nil
}

// runGrowthExecutors 扫描任务并在白名单内补未完成进度；只对 CN workbuddy 账号。
// 返回本轮实际上报的任务数。不在此处 claim：调用方随后 ClaimCompletedGrowthTasks
// 会重新回读，避免“上报成功但服务端尚未刷新”被误判为可领。
func (s *Scheduler) runGrowthExecutorsLocked(ctx context.Context) int {
	reported := 0
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return reported
		}
		if st.Disabled {
			continue
		}
		if strings.TrimSpace(st.Producer) != "" && st.Producer != "workbuddy" {
			continue
		}
		a := s.cfg.Pool.AuthByUID(st.UID)
		if a == nil || a.IsGlobal() {
			continue
		}
		for _, platform := range []upstream.GrowthTaskPlatform{upstream.GrowthPlatformDefault, upstream.GrowthPlatformMiniprogram} {
			tasks, err := s.cfg.Upstream.ListGrowthTasks(a, platform)
			if err != nil {
				log.Printf("WARN: growth executor list %s %s: %v", logfmt.Label(st.UID, st.Nickname), platformLabel(platform), err)
				continue
			}
			for _, t := range tasks {
				if t.AcceptStatus == "completed" || t.AcceptStatus == "claimed" {
					continue
				}
				ex, ok := growthTaskExecutors[t.TaskCode]
				if !ok || ex == nil {
					continue
				}
				// 小程序任务必须从小程序口径列表里触发，避免普通口径误报。
				if !growthExecutorMatchesPlatform(ex, platform) {
					continue
				}
				if t.AcceptStatus == "not_accepted" {
					if _, err := s.cfg.Upstream.AcceptGrowthTasks(a, []string{t.TaskCode}, platform); err != nil {
						log.Printf("WARN: growth accept %s %s: %v", logfmt.Label(st.UID, st.Nickname), t.TaskCode, err)
						continue
					}
					if !sleepCtx(ctx, growthAcceptDelay) {
						return reported
					}
				}
				if err := ex.Run(ctx, s.cfg.Upstream, a, t); err != nil {
					log.Printf("WARN: growth executor %s %s: %v", logfmt.Label(st.UID, st.Nickname), t.TaskCode, err)
					continue
				}
				reported++
				log.Printf("growth executor %s %s: reported %s", logfmt.Label(st.UID, st.Nickname), t.TaskCode, platformLabel(platform))
				if !sleepCtx(ctx, growthExecutorGap) {
					return reported
				}
			}
		}
	}
	return reported
}

func growthExecutorMatchesPlatform(e GrowthTaskExecutor, platform upstream.GrowthTaskPlatform) bool {
	mini := isMiniGrowthExecutor(e)
	if platform == upstream.GrowthPlatformMiniprogram {
		return mini
	}
	return !mini
}
func isMiniGrowthExecutor(e GrowthTaskExecutor) bool {
	x, ok := e.(growthChatExecutor)
	return ok && x.mini
}

func growthChatEvent(a *auth.Auth, cid, modelID, modelName string, mini bool) map[string]any {
	now := time.Now().UnixMilli()
	ev := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "chat", "conversationId": cid, "requestId": cid,
		"inputLength": 12, "requestModelId": modelID, "requestModelName": modelName,
		"isPlan": false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false, "maxToken": 0, "maxSteps": 0, "temperature": 0,
		"maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
		"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
		"command": "", "expertId": "", "recommendId": "", "skillId": "",
		"skillCount": 0, "totalCount": 0, "fileUri": "", "presentAt": now,
		"traceId": "", "rootRequestId": cid, "parentConversationId": cid,
		"agentName": "default", "agentType": "conversation", "userId": a.UID,
	}
	if mini {
		ev["source"] = "mini_program"
		ev["ideName"] = "wx_app_cloud"
		ev["ideType"] = "WorkBuddy_MP"
		ev["extName"] = "workbuddy-mp"
		ev["extVersion"] = "2.4.0"
	}
	return ev
}

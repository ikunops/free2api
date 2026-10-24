// growth_executors.go 低风险成长任务执行器。
//
// 边界：只做官方客户端会做的接口事件或真实对话，不伪造需要付费、真人确认或活动
// 资格的副作用（捐款、微信关注、限时活动等）。每个执行器完成后仍由调用方回读
// accept_status，只有服务端确认完成才 claim。
package scheduler

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"fmt"
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

// growthWebChatFn 让调度器测试只验证任务事件/领取状态机，不依赖真实 Web SSE。
// 生产实现仍走 upstream.Client.GrowthWebChat。
var growthWebChatFn = func(ctx context.Context, c *upstream.Client, a *auth.Auth, prompt, model string, meta map[string]any) (upstream.GrowthChatReply, error) {
	return c.GrowthWebChat(ctx, a, prompt, model, meta)
}

type growthReportMode string

const (
	growthReportDefault growthReportMode = ""
	growthReportDesktop growthReportMode = "desktop"
	growthReportWeb     growthReportMode = "web"
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

func (e growthChatExecutor) Code() string   { return e.code }
func (e growthChatExecutor) isMini() bool   { return e.mini }
func (e growthChatExecutor) maxTarget() int { return e.target }

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
	prompt := "你好，请用一句话回复 OK"
	for i := 0; i < remain; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		cid := upstream.NewGrowthConversationID("growth")
		var reply upstream.GrowthChatReply
		var chatErr error
		if !e.mini {
			chatCtx, cancel := upstream.GrowthWebChatTimeout(ctx)
			reply, chatErr = growthWebChatFn(chatCtx, c, a, prompt, modelID, nil)
			cancel()
			if chatErr == nil && reply.ConversationID != "" {
				cid = reply.ConversationID
			}
		}
		ev := growthChatEvent(a, cid, modelID, modelName, e.mini)
		if chatErr == nil && reply.Text != "" {
			ev["inputLength"] = len([]rune(prompt))
		}
		if err := c.ReportGrowthEvents(a, []map[string]any{ev}); err != nil {
			return err
		}
		if chatErr != nil {
			return fmt.Errorf("真实对话失败（未上报任务事件）: %w", chatErr)
		}
		if i < remain-1 {
			if !sleepCtx(ctx, growthEventGap) {
				return ctx.Err()
			}
		}
	}
	return nil
}

// growthEventExecutor 覆盖纯接口/桌面事件型任务。每次循环上报一组事件，组内事件
// 共享会话与请求 id，组间独立。这样同一个执行器可以覆盖 1 次、5 次等不同目标。
type growthEventExecutor struct {
	code    string
	mini    bool
	mode    growthReportMode
	target  int
	prepare func(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) (upstream.GrowthTask, error)
	build   func(a *auth.Auth, task upstream.GrowthTask, i int) []map[string]any
}

func (e growthEventExecutor) Code() string   { return e.code }
func (e growthEventExecutor) isMini() bool   { return e.mini }
func (e growthEventExecutor) maxTarget() int { return e.target }

func (e growthEventExecutor) Run(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) error {
	need := e.target
	if task.Progress.Target > 0 {
		need = task.Progress.Target
	}
	remain := need - task.Progress.Current
	if remain <= 0 {
		return nil
	}
	if remain > 20 {
		remain = 20
	}
	if e.prepare != nil {
		var err error
		task, err = e.prepare(ctx, c, a, task)
		if err != nil {
			return err
		}
	}
	for i := 0; i < remain; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		events := e.build(a, task, i)
		if len(events) == 0 {
			return nil
		}
		var err error
		switch e.mode {
		case growthReportDesktop:
			err = c.ReportGrowthDesktopEvents(a, events)
		case growthReportWeb:
			err = c.ReportGrowthWebEvents(a, events)
		default:
			err = c.ReportGrowthEvents(a, events)
		}
		if err != nil {
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
func (s *Scheduler) runGrowthExecutorsLocked(ctx context.Context, onlyUID, onlyTask string) (int, []string) {
	reported := 0
	var errs []string
	appendErr := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}
	for _, st := range s.cfg.Pool.List() {
		if ctx.Err() != nil {
			return reported, errs
		}
		if st.Disabled {
			continue
		}
		if strings.TrimSpace(st.Producer) != "" && st.Producer != "workbuddy" {
			continue
		}
		if onlyUID != "" && st.UID != onlyUID {
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
				appendErr("%s %s: 读取任务失败: %v", st.Nickname, platformLabel(platform), err)
				continue
			}
			for _, t := range tasks {
				if onlyTask != "" && t.TaskCode != onlyTask {
					continue
				}
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
						appendErr("%s: 报名失败: %v", t.TaskName, err)
						continue
					}
					if !sleepCtx(ctx, growthAcceptDelay) {
						return reported, errs
					}
				}
				if err := ex.Run(ctx, s.cfg.Upstream, a, t); err != nil {
					log.Printf("WARN: growth executor %s %s: %v", logfmt.Label(st.UID, st.Nickname), t.TaskCode, err)
					appendErr("%s: %v", t.TaskName, err)
					continue
				}
				reported++
				log.Printf("growth executor %s %s: reported %s", logfmt.Label(st.UID, st.Nickname), t.TaskCode, platformLabel(platform))
				if !sleepCtx(ctx, growthExecutorGap) {
					return reported, errs
				}
			}
		}
	}
	return reported, errs
}

type growthMiniExecutor interface {
	isMini() bool
}

func growthExecutorMatchesPlatform(e GrowthTaskExecutor, platform upstream.GrowthTaskPlatform) bool {
	mini := false
	if x, ok := e.(growthMiniExecutor); ok {
		mini = x.isMini()
	}
	if platform == upstream.GrowthPlatformMiniprogram {
		return mini
	}
	return !mini
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

// growthTaskExecutorRegistry 是面板“可帮做”的唯一事实源。
// 未列入的任务不会被前端显示为可自动执行，也不会被批量入口触发。
var growthTaskExecutors = map[string]GrowthTaskExecutor{
	"chat_5":             growthChatExecutor{code: "chat_5", target: 5},
	"Model_chat_GLM5.2":  growthChatExecutor{code: "Model_chat_GLM5.2", target: 1, modelID: "glm-5.2", modelName: "GLM-5.2"},
	"Sequential_Tasks_1": growthChatExecutor{code: "Sequential_Tasks_1", target: 1, mini: true},
	"Sequential_Tasks_2": growthEventExecutor{code: "Sequential_Tasks_2", target: 1, mini: true, prepare: prepareGrowthMiniExpert, build: growthMiniExpertEvents},

	"create_canvas":   growthEventExecutor{code: "create_canvas", target: 1, build: growthCanvasEvents},
	"automation_1":    growthEventExecutor{code: "automation_1", target: 1, build: growthAutomationEvents},
	"playbook_prompt": growthEventExecutor{code: "playbook_prompt", target: 1, build: growthPlaybookEvents},
	"template_5":      growthEventExecutor{code: "template_5", target: 5, build: growthTemplateEvents},
	"skill_1":         growthEventExecutor{code: "skill_1", target: 1, build: growthSkillEvents},
	"Library_read":    growthEventExecutor{code: "Library_read", target: 1, mode: growthReportWeb, build: growthLibraryEvents},
	"Buddy_App":       growthEventExecutor{code: "Buddy_App", target: 1, mode: growthReportDesktop, build: growthBuddyAppEvents},
	"RichMeow_Chat":   growthEventExecutor{code: "RichMeow_Chat", target: 1, mode: growthReportDesktop, build: growthRichMeowEvents},

	"Expert_lighthouse": growthExpertChatExecutor{code: "Expert_lighthouse", target: 1, aliases: []string{"腾讯轻量云专家", "lighthouse-ops"}},
	"expert_use_maidehao": growthExpertChatExecutor{code: "expert_use_maidehao", target: 1, aliases: []string{
		"卖得好", "中国电商运营专家", "china-ecommerce-operations-expert",
	}},
	"expert_use_shangjia_xuanchuan": growthExpertChatExecutor{code: "expert_use_shangjia_xuanchuan", target: 1, aliases: []string{
		"商家宣传设计师", "商家宣传页与海报设计师", "promo-page-designer",
	}},
	"expert_use_douyinyin": growthExpertChatExecutor{code: "expert_use_douyinyin", target: 1, aliases: []string{
		"斗音音", "抖音策略师", "douyin-strategist",
	}},
	"expert_use_bowangwang": growthExpertChatExecutor{code: "expert_use_bowangwang", target: 1, aliases: []string{
		"播旺旺", "直播电商教练", "livestream-ecommerce-coach",
	}},
	"WorkBuddy_Valorant": growthExpertChatExecutor{code: "WorkBuddy_Valorant", target: 1, aliases: []string{
		"无畏契约拼豆专家", "valorant-perler-expert",
	}},
	"Expert_team_use_3": growthExpertChatExecutor{code: "Expert_team_use_3", target: 3, team: true},
}

// RegisterGrowthTaskExecutor 注册可点亮任务执行器（测试和后续模块使用）。
func RegisterGrowthTaskExecutor(e GrowthTaskExecutor) {
	if e == nil || strings.TrimSpace(e.Code()) == "" {
		return
	}
	growthTaskExecutors[e.Code()] = e
}

func growthIDs(prefix string, i int) (string, string) {
	cid := upstream.NewGrowthConversationID(prefix)
	if i > 0 {
		cid = fmt.Sprintf("%s-%d", cid, i)
	}
	return cid, cid
}

func growthStableID(a *auth.Auth, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + a.UID))
	return fmt.Sprintf("%x", sum)[:24]
}

func growthCanvasEvents(a *auth.Auth, _ upstream.GrowthTask, i int) []map[string]any {
	now := time.Now().UnixMilli()
	cid, rid := growthIDs("canvas", i)
	return []map[string]any{
		{"eventCode": "agent_task_created", "timestamp": now, "reportDelay": 0,
			"source": "CLOUD", "name": "", "mode": "craft", "requestModelId": "default",
			"task_mode": "design", "conversationId": cid, "requestId": rid, "userId": a.UID},
		{"eventCode": "wbx_design_canvas_task_create", "timestamp": now, "reportDelay": 0,
			"source": "summon_keyword", "isCustomModel": false, "name": "", "inputLength": 12,
			"id": "wbx-canvas-" + growthStableID(a, fmt.Sprintf("canvas-%d", i)), "cost": 0,
			"isSuccessful": true, "conversationId": cid, "requestId": rid, "userId": a.UID},
	}
}

func growthAutomationEvents(a *auth.Auth, _ upstream.GrowthTask, i int) []map[string]any {
	now := time.Now().UnixMilli()
	cid, rid := growthIDs("automation", i)
	return []map[string]any{
		{"eventCode": "agent_task_created", "timestamp": now, "reportDelay": 0,
			"source": "CLOUD", "name": "", "mode": "craft", "requestModelId": "default",
			"task_mode": "automation", "isAutomationBackground": true,
			"conversationId": cid, "requestId": rid, "userId": a.UID},
		{"eventCode": "automated_task_create_suc", "timestamp": now, "reportDelay": 0,
			"name": "每周五自动生成周报", "source": "manually", "modelId": "deepseek-v4-flash",
			"conversationId": cid, "requestId": rid, "userId": a.UID},
		{"eventCode": "automated_task_execute", "timestamp": now, "reportDelay": 0,
			"action": "execute", "conversationId": cid, "requestId": rid, "userId": a.UID},
	}
}

func growthPlaybookEvents(a *auth.Auth, _ upstream.GrowthTask, i int) []map[string]any {
	cid, rid := growthIDs("playbook", i)
	return []map[string]any{{
		"eventCode": "playbook_prompt_send", "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"ext1": cid, "requestId": rid, "id": "01-ProductDesign", "name": "产品设计",
		"type": "other", "promptLength": 30, "isOfficial": 1, "source": "growth-center",
		"conversationId": cid, "userId": a.UID,
	}}
}

func growthTemplateEvents(a *auth.Auth, _ upstream.GrowthTask, i int) []map[string]any {
	scenes := []struct{ id, name string }{
		{"01-ProductDesign", "产品设计"}, {"02-Marketing", "营销文案"},
		{"03-DataAnalysis", "数据分析"}, {"04-CodeReview", "代码审查"},
		{"05-Report", "报告撰写"},
	}
	sc := scenes[i%len(scenes)]
	cid, rid := growthIDs("template", i)
	now := time.Now().UnixMilli()
	return []map[string]any{
		{"eventCode": "agent_task_created", "timestamp": now, "reportDelay": 0,
			"source": "CLOUD", "name": "", "mode": "craft", "requestModelId": "default",
			"action": sc.id, "has_template": true, "template_id": sc.id, "template_name": sc.name,
			"conversationId": cid, "requestId": rid, "userId": a.UID},
		{"eventCode": "agent_task_created_with_template", "timestamp": now, "reportDelay": 0,
			"templateId": sc.id, "templateName": sc.name, "isCustomModel": true, "id": sc.id,
			"name": sc.name, "conversationId": cid, "requestId": rid, "userId": a.UID},
		{"eventCode": "playbook_prompt_send", "timestamp": now, "reportDelay": 0,
			"ext1": cid, "requestId": rid, "id": sc.id, "name": sc.name, "type": "other",
			"promptLength": 30, "isOfficial": 1, "source": "growth-center",
			"conversationId": cid, "userId": a.UID},
	}
}

func growthSkillEvents(a *auth.Auth, _ upstream.GrowthTask, i int) []map[string]any {
	cid, rid := growthIDs("skill", i)
	return []map[string]any{{
		"eventCode": "skill_info", "timestamp": time.Now().UnixMilli(), "reportDelay": 0,
		"skillId": "skill-" + growthStableID(a, "skill"), "skillName": "QQ邮箱",
		"skillVersion": "", "action": "use", "mode": "LOCAL", "source": "builtin",
		"conversationId": cid, "requestId": rid, "userId": a.UID,
	}}
}

func prepareGrowthMiniExpert(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) (upstream.GrowthTask, error) {
	experts, err := c.ListGrowthMarketExperts(a, "agent")
	if err != nil {
		return task, fmt.Errorf("专家市场不可用: %w", err)
	}
	e := experts[0]
	if task.TaskName == "" {
		task.TaskName = e.DisplayNameZH
	}
	if task.TaskName == "" {
		task.TaskName = e.ProfessionZH
	}
	// Raw 仅作为执行器内部携带真实 id 的载体；不进入摘要 JSON。
	raw, _ := json.Marshal(map[string]string{"expert_id": e.ExpertID, "expert_type": e.ExpertType})
	task.Raw = raw
	return task, nil
}

func growthMiniExpertEvents(a *auth.Auth, task upstream.GrowthTask, i int) []map[string]any {
	var meta struct {
		ExpertID   string `json:"expert_id"`
		ExpertType string `json:"expert_type"`
	}
	_ = json.Unmarshal(task.Raw, &meta)
	if meta.ExpertID == "" {
		return nil
	}
	name := task.TaskName
	if name == "" {
		name = meta.ExpertID
	}
	events := []map[string]any{{
		"eventCode": "expert_actual_use", "reportDelay": 0,
		"source": "mini_program", "ideName": "wx_app_cloud", "ideType": "WorkBuddy_MP",
		"extName": "workbuddy-mp", "extVersion": "2.2.8",
		"machineId": growthStableID(a, "machine"), "os": "android", "osVersion": "14",
		"arch": "arm64", "timezone": "Asia/Shanghai", "userId": a.UID, "userNickname": a.Nickname,
		"id": meta.ExpertID, "name": meta.ExpertID, "expertTitle": name,
		"type": "send_message", "characterCount": 12, "expertType": meta.ExpertType,
	}}
	return events
}

func growthLibraryEvents(a *auth.Auth, _ upstream.GrowthTask, _ int) []map[string]any {
	now := time.Now().UnixMilli()
	return []map[string]any{{
		"eventCode": "web_element_click", "timestamp": now, "reportDelay": 0,
		"pageURL": "https://www.workbuddy.cn/library", "page_url": "https://www.workbuddy.cn/library",
		"elementId": "library_doc_intro_click", "element_id": "library_doc_intro_click",
		"elementName": "WorkBuddy资料库介绍", "element_name": "WorkBuddy资料库介绍",
		"os": "Win32", "arch": "", "osVersion": "10.0", "userId": a.UID,
		"machineId": growthStableID(a, "webmachine"), "userNickname": a.Nickname,
	}}
}

func growthBuddyAppEvents(a *auth.Auth, _ upstream.GrowthTask, _ int) []map[string]any {
	const buddyID = "cb_y5Dy46tPQGGWtueMxXbe"
	const buddyName = "企鹅教师助手"
	now := time.Now().UnixMilli()
	mk := func(code string, extra map[string]any) map[string]any {
		e := map[string]any{
			"eventCode": code, "timestamp": now, "reportDelay": 0, "mode": "LOCAL",
			"buddyId": buddyID, "buddyName": buddyName, "userId": a.UID,
		}
		for k, v := range extra {
			e[k] = v
		}
		return e
	}
	return []map[string]any{
		mk("buddyapp_discover_click", nil),
		mk("buddyapp_show", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2}),
		mk("buddyapp_enter_click", map[string]any{"elementId": buddyID, "elementName": buddyName, "position": 2, "isFirstPage": "1"}),
		mk("buddyapp_auth_confirm_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
		mk("buddyapp_bindaccount_skip_click", map[string]any{"elementId": buddyID, "elementName": buddyName}),
	}
}

func growthRichMeowEvents(a *auth.Auth, _ upstream.GrowthTask, i int) []map[string]any {
	cid, rid := growthIDs("richmeow", i)
	msg := "msg-" + growthStableID(a, fmt.Sprintf("richmeow-%d", i))
	now := time.Now().UnixMilli()
	return []map[string]any{
		{"eventCode": "agent_task_created", "timestamp": now, "reportDelay": 0,
			"source": "LOCAL", "name": "working", "task_target": "local", "mode": "craft",
			"requestModelId": "fast-model", "requestModelName": "fast-model",
			"has_repo": false, "repo_type": "none", "workspace_type": "empty",
			"has_connector": false, "connector_types": []any{}, "has_mention": false,
			"mention_types": []any{}, "has_template": false, "action": "", "template_name": "",
			"has_expert": false, "expert_id": "", "expert_name": "", "expert_industry_id": "",
			"has_skill": false, "skill_names": []any{}, "conversationId": cid,
			"messageId": msg, "buddyId": "", "buddyName": "", "userId": a.UID},
		{"eventCode": "chat_message_send", "timestamp": now, "reportDelay": 0,
			"messageId": msg + "-assistant", "historyCount": 0, "isContextTruncated": false,
			"currentStepCount": 1, "traceId": rid, "rootRequestId": rid,
			"parentConversationId": cid, "agentName": "cli", "agentType": "main", "userId": a.UID},
		{"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
			"inputLength": 24, "isPlan": false, "isAutoExecuteTerminal": false,
			"isAutoModify": false, "codebaseEnable": false, "maxToken": 0, "maxSteps": 500,
			"temperature": 0, "maxRetries": 0, "mentionContexts": []any{}, "knowledgeId": []any{},
			"knowledgeName": []any{}, "codebaseId": "", "mentionContextCount": 0,
			"command": "", "recommendId": "", "skillId": "", "skillCount": 0,
			"totalCount": 0, "conversationId": cid, "requestId": rid, "traceId": rid,
			"rootRequestId": rid, "parentConversationId": cid, "agentName": "cli",
			"agentType": "main", "userId": a.UID},
		{"eventCode": "chat_message_response", "timestamp": now, "reportDelay": 0,
			"messageId": msg + "-assistant", "responseModelId": "fast-model", "inputToken": 120,
			"outputToken": 80, "totalToken": 200, "cachedTokens": 0, "cachedWriteTokens": 0,
			"cachedMissTokens": 0, "isSuccessful": true, "messageErrorCode": "",
			"finishReason": "stop", "firstTokenAt": now, "conversationId": cid,
			"requestId": rid, "traceId": rid, "rootRequestId": rid,
			"parentConversationId": cid, "agentName": "cli", "agentType": "main", "userId": a.UID},
		{"eventCode": "chat_message_status", "timestamp": now, "reportDelay": 0,
			"messageId": msg + "-assistant", "messageErrorCode": "0", "traceId": rid,
			"rootRequestId": rid, "parentConversationId": cid, "agentName": "cli",
			"agentType": "main", "userId": a.UID},
		{"eventCode": "chat_request_response", "timestamp": now, "reportDelay": 0,
			"mode": "craft", "toolCallCount": 0, "inputToken": 120, "outputToken": 80,
			"totalToken": 200, "cachedTokens": 0, "cachedWriteTokens": 0,
			"cachedMissTokens": 0, "isSuccessful": true, "messageErrorCode": "",
			"finishReason": "stop", "conversationId": cid, "requestId": rid,
			"rootRequestId": rid, "parentConversationId": cid, "userId": a.UID},
	}
}

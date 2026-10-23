// growth_expert_executor.go 真实专家任务执行器。
//
// 官方客户端链路：真实专家元数据注入会话 _meta → 真实 Web SSE 成功 → 上报
// expert_actual_use / agent_task_completed / agent_task_created。这里不做“只报事件”
// 的伪完成；找不到真实专家或对话失败都不会发送任务事件。
package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// growthLocalExpertFn 让调度器测试注入本地专家解析，不依赖运行机缓存。
var growthLocalExpertFn = func(alias string) (upstream.GrowthMarketExpert, bool) {
	return upstream.FindLocalGrowthExpert(alias)
}

// 官方桌面客户端把模型分成两层：链路里的 modelId 是 fast-model，
// 但 /console/chat/completions 的真实模型是 cn:glm-5.3-flash-x0.08；事件上报时
// requestModelId 写 fast-model。两者不能混成一个字段。
const (
	growthExpertWebModelID    = "cn:glm-5.3-flash-x0.08"
	growthExpertClientModelID = "fast-model"
)

type growthExpertChatExecutor struct {
	code    string
	target  int
	aliases []string
	team    bool
	modelID string
}

func (e growthExpertChatExecutor) Code() string   { return e.code }
func (e growthExpertChatExecutor) isMini() bool   { return false }
func (e growthExpertChatExecutor) maxTarget() int { return e.target }

func (e growthExpertChatExecutor) Run(ctx context.Context, c *upstream.Client, a *auth.Auth, task upstream.GrowthTask) error {
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
	expert, err := resolveGrowthExpert(c, a, task, e.aliases, e.team)
	if err != nil {
		return err
	}
	// e.modelID 为空时保留客户端展示名给事件，实际请求模型由 GrowthWebChat
	// 依据网关/上游的真实目录解析；不要用硬编码的展示名冒充上游 service id。
	modelID := strings.TrimSpace(e.modelID)
	if modelID == "" {
		modelID = growthExpertWebModelID
	}
	modelName := growthExpertModelName(modelID)
	// 任务事件是客户端归因：requestModelId 保持官方 Web 客户端的 fast-model；
	// 真实 /console/chat/completions 请求则使用 modelID，两者不能混写。
	eventModelID := growthExpertClientModelID
	eventModelName := growthExpertModelName(eventModelID)
	prompt := growthExpertPrompt(task, expert)
	for i := 0; i < remain; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		cid, rid := growthIDs("expert", i)
		events := growthRealExpertEvents(a, task, expert, cid, rid, eventModelID, eventModelName, len([]rune(prompt)))
		meta, err := upstream.GrowthExpertChatMeta(expert, cid, rid, modelID, modelName, events)
		if err != nil {
			return fmt.Errorf("真实专家会话参数失败: %w", err)
		}
		chatCtx, cancel := upstream.GrowthWebChatTimeout(ctx)
		reply, chatErr := growthWebChatFn(chatCtx, c, a, prompt, modelID, meta)
		cancel()
		if chatErr != nil {
			return fmt.Errorf("真实专家对话失败（未上报任务事件）: %w", chatErr)
		}
		if reply.ConversationID != "" {
			cid = reply.ConversationID
			events = growthRealExpertEvents(a, task, expert, cid, rid, eventModelID, eventModelName, len([]rune(prompt)))
		}
		if err := c.ReportGrowthEvents(a, events); err != nil {
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

func resolveGrowthExpert(c *upstream.Client, a *auth.Auth, task upstream.GrowthTask, aliases []string, team bool) (upstream.GrowthMarketExpert, error) {
	expertType := "agent"
	if team {
		expertType = "team"
	}
	terms := growthExpertSearchTerms(task, aliases)
	market, marketErr := c.ListGrowthMarketExperts(a, expertType)
	if marketErr == nil {
		for _, term := range terms {
			if e, ok := upstream.FindGrowthMarketExpert(market, term); ok {
				return mergeLocalGrowthExpert(e), nil
			}
		}
	}
	for _, term := range terms {
		if e, ok := growthLocalExpertFn(term); ok {
			return e, nil
		}
	}
	if marketErr != nil {
		return upstream.GrowthMarketExpert{}, fmt.Errorf("找不到任务对应的真实专家（市场: %v；本机缓存也无匹配）", marketErr)
	}
	return upstream.GrowthMarketExpert{}, fmt.Errorf("找不到任务对应的真实专家: %s", strings.Join(terms, " / "))
}

func growthExpertSearchTerms(task upstream.GrowthTask, aliases []string) []string {
	out := make([]string, 0, len(aliases)+4)
	for _, s := range aliases {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	for _, s := range []string{task.TaskCode, task.TaskName, task.Title, task.TaskDesc} {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func mergeLocalGrowthExpert(e upstream.GrowthMarketExpert) upstream.GrowthMarketExpert {
	local, ok := upstream.LocalGrowthExpertByID(e.ExpertID)
	if !ok {
		local, ok = upstream.LocalGrowthExpertByID(e.SourceID)
	}
	if !ok {
		return e
	}
	if e.DisplayNameZH == "" {
		e.DisplayNameZH = local.DisplayNameZH
	}
	if e.ProfessionZH == "" {
		e.ProfessionZH = local.ProfessionZH
	}
	if e.Version == "" {
		e.Version = local.Version
	}
	if e.PluginName == "" {
		e.PluginName = local.PluginName
	}
	if e.AgentName == "" {
		e.AgentName = local.AgentName
	}
	if e.LocalPath == "" {
		e.LocalPath = local.LocalPath
	}
	if e.Prompt == "" {
		e.Prompt = local.Prompt
	}
	return e
}

func growthExpertPrompt(task upstream.GrowthTask, e upstream.GrowthMarketExpert) string {
	// 任务本身只需要完成一次专家使用。提示词保持真实、可执行，不诱导专家做高风险副作用。
	if strings.Contains(task.TaskCode, "lighthouse") || strings.Contains(e.PluginName, "lighthouse") {
		return "请帮我查看轻量应用服务器实例列表，并简要说明当前实例状态。只做只读查询。"
	}
	if name := strings.TrimSpace(e.DisplayNameZH); name != "" {
		return "你好，我想体验" + name + "的能力，请用你的专业方法回复一段简短建议。"
	}
	return "你好，请用你的专业方法回复一段简短建议。"
}

func growthExpertModelName(modelID string) string {
	switch strings.ToLower(strings.TrimSpace(modelID)) {
	case "fast-model":
		return "fast-model"
	case "glm-5.2":
		return "GLM-5.2"
	case "deepseek-v4-flash":
		return "DeepSeek V4 Flash"
	default:
		return modelID
	}
}

func growthRealExpertEvents(a *auth.Auth, task upstream.GrowthTask, e upstream.GrowthMarketExpert, cid, rid, modelID, modelName string, characterCount int) []map[string]any {
	now := time.Now().UnixMilli()
	expertID := strings.TrimSpace(e.ExpertID)
	name := strings.TrimSpace(e.DisplayNameZH)
	if name == "" {
		name = strings.TrimSpace(task.TaskName)
	}
	expertType := strings.TrimSpace(e.ExpertType)
	if expertType == "" {
		expertType = "agent"
	}
	messageID := rid + "-assistant"
	return []map[string]any{
		{"eventCode": "expert_actual_use", "timestamp": now, "reportDelay": 0,
			"mode": "CLOUD", "id": expertID, "name": name, "expertTitle": name, "type": "send_message",
			"expertType": expertType, "source": "builtin", "version": e.Version, "cost": 0,
			"characterCount": characterCount, "conversationId": cid, "requestId": rid,
			"messageId": messageID, "requestModelId": modelID, "requestModelName": modelName, "userId": a.UID},
		{"eventCode": "agent_task_completed", "timestamp": now, "reportDelay": 0,
			"source": "CLOUD", "name": "completed", "mode": "craft", "conversationId": cid,
			"requestId": rid, "messageId": messageID, "expertId": expertID, "expertName": name,
			"userId": a.UID},
		{"eventCode": "agent_task_created", "timestamp": now, "reportDelay": 0,
			"source": "CLOUD", "name": "completed", "mode": "craft", "conversationId": cid,
			"requestId": rid, "messageId": messageID, "expertId": expertID, "expertName": name,
			"has_expert": true, "userId": a.UID},
	}
}

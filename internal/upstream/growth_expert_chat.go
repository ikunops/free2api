// growth_expert_chat.go 真实专家会话所需的上游元数据。
//
// 专家任务不能只上报 expert_actual_use：官方客户端会先创建会话，在请求的
// _meta["codebuddy.ai"] 中携带 expertId/plugin/agent/prompt/growthEvent，SSE 成功后才
// 上报任务事件。这里负责把真实专家元数据转成该请求的 _meta，不做任何本地伪造。
package upstream

import (
	"encoding/json"
	"errors"
	"strings"
)

var errGrowthExpertMissingID = errors.New("growth expert id missing")

// GrowthExpertChatMeta 构造官方客户端同形的专家会话 _meta。
// growthEvent 必须是 JSON 字符串而不是数组；这是客户端 appendGrowthEvent 的协议形状。
func GrowthExpertChatMeta(expert GrowthMarketExpert, conversationID, requestID, modelID, modelName string, growthEvents []map[string]any) (map[string]any, error) {
	conversationID = strings.TrimSpace(conversationID)
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = conversationID
	}
	expertID := strings.TrimSpace(expert.ExpertID)
	if expertID == "" {
		return nil, errGrowthExpertMissingID
	}
	expertType := strings.TrimSpace(expert.ExpertType)
	if expertType == "" {
		expertType = "agent"
	}
	expertMeta := map[string]any{
		"id": expertID, "name": expert.DisplayNameZH, "profession": expert.ProfessionZH,
		"pluginName": expert.PluginName, "agentName": expert.AgentName,
		"expertType": expertType, "source": "builtin", "version": expert.Version,
	}
	if strings.TrimSpace(expert.Prompt) != "" {
		expertMeta["prompt"] = expert.Prompt
	}
	ai := map[string]any{
		"mode": "craft", "expertId": expertID, "expert": expertMeta,
		"conversationId": conversationID,
	}
	if len(growthEvents) > 0 {
		raw, err := json.Marshal(growthEvents)
		if err != nil {
			return nil, err
		}
		ai["growthEvent"] = string(raw)
	}
	return map[string]any{"codebuddy.ai": ai}, nil
}

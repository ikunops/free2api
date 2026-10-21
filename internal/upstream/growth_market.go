// growth_market.go 成长任务需要真实对象 id 时的只读市场查询。
//
// 专家任务的判据要求 id 来自官方市场；本地编造 id 可能被服务端忽略。这里仅拉取
// 公共市场元数据，不执行任务，也不改变账号状态。
package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"free2api/internal/auth"
)

const growthMarketExpertPath = "/v2/operation-platform/market/expert/list"

// GrowthMarketExpert 是 expert/list 里任务事件需要的字段。
type GrowthMarketExpert struct {
	ExpertID      string
	SourceID      string
	ExpertType    string
	DisplayNameZH string
	ProfessionZH  string
	Version       string
}

// ListGrowthMarketExperts 拉取真实专家列表。expertType 为空表示全部。
func (c *Client) ListGrowthMarketExperts(a *auth.Auth, expertType string) ([]GrowthMarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 50, "sort_by": "use_count", "sort_order": "desc"}
	if strings.TrimSpace(expertType) != "" {
		body["expert_type"] = strings.TrimSpace(expertType)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, c.chatBase(a)+growthMarketExpertPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Experts []struct {
			ExpertID      string `json:"expert_id"`
			SourceID      string `json:"source_id"`
			ExpertType    string `json:"expert_type"`
			DisplayNameZH string `json:"display_name_zh"`
			ProfessionZH  string `json:"profession_zh"`
			Version       string `json:"version"`
		} `json:"experts"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	out := make([]GrowthMarketExpert, 0, len(resp.Experts))
	seen := map[string]bool{}
	for _, e := range resp.Experts {
		id := strings.TrimSpace(e.ExpertID)
		if id == "" {
			id = strings.TrimSpace(e.SourceID)
		}
		if id == "" || seen[id] {
			continue
		}
		typ := strings.TrimSpace(e.ExpertType)
		if typ == "" {
			typ = "agent"
		}
		out = append(out, GrowthMarketExpert{
			ExpertID: id, SourceID: e.SourceID, ExpertType: typ,
			DisplayNameZH: e.DisplayNameZH, ProfessionZH: e.ProfessionZH, Version: e.Version,
		})
		seen[id] = true
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("expert market returned no %s experts", expertType)
	}
	return out, nil
}

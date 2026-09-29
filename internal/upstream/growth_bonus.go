// growth_bonus.go growth 域「补签卡」+ billing 域「新手礼包 / 活动补偿」接口。
// 逆向来源：下游 panel（/tmp/wb2a-panel internal/upstream/blackcat.go + scheduler/streak.go）
// 的连登管家闭环——补签保连登 + 礼包/补偿领取，同属 growth 任务体系（活跃地图家族）。
//
//		GET  /activity/growth/streak          → data.makeup_cards{balance,max}（补签卡余额，
//		                                        streak 同一响应体内，复用 streakPath 零加请求）
//		GET  /activity/growth/heatmap        → data.cells[]{date,score}（活跃地图热力格，
//	                                       score==0 判漏签）
//		POST /activity/growth/makeup-cards/use → {"target_date":"2006-01-02"}（对指定日期补签，
//	                                       保住连登连续天数；无卡/无漏签返回业务错误）
//		POST /billing/meter/claim-gift        → 新手礼包（每号一次，重复领返回业务错误）
//		POST /billing/meter/claim-compensation → 活动补偿（有则领，无则业务错误）
//
// 补签语义：昨日漏签（heatmap score==0）且有卡（balance>0）才补——连续天数一断
// 就要重攒 7 天，代价远大于一张卡；无卡/无漏签/查询失败均静默（不影响主流程）。
// 礼包/补偿语义：幂等写（每号一次/有则领），业务错误静默跳过不刷 WARN。
package upstream

import (
	"encoding/json"
	"net/http"
	"time"

	"free2api/internal/auth"
)

// 补签/礼包路径（panel 实测口径）。
const (
	heatmapPath           = "/activity/growth/heatmap"
	makeupCardUsePath     = "/activity/growth/makeup-cards/use"
	claimGiftPath         = "/billing/meter/claim-gift"
	claimCompensationPath = "/billing/meter/claim-compensation"
)

// GrowthMakeupCards 补签卡余额（streak 响应的 makeup_cards 段）。
type GrowthMakeupCards struct {
	Balance int `json:"balance"` // 可用补签卡数
	Max     int `json:"max"`     // 持有上限
}

// GrowthStreakWithCards 连登天数 + 补签卡余额（一次 GET /activity/growth/streak 读完；
// 与 GrowthRewardState 同响应体，只多解析 makeup_cards 段）。
type GrowthStreakWithCards struct {
	Streak struct {
		Days int `json:"days"`
	} `json:"streak"`
	MakeupCards GrowthMakeupCards `json:"makeup_cards"`
}

// GrowthStreakWithCards 读取连登天数 + 补签卡余额（GET /activity/growth/streak，
// 与 GrowthRewardState/GrowthStreak 同端点不同切片：一次请求同时供两者解析）。
func (c *Client) GrowthStreakWithCards(a *auth.Auth) (*GrowthStreakWithCards, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	var st GrowthStreakWithCards
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// HeatmapCell 活跃地图热力格（一日一格）。
type HeatmapCell struct {
	Date  string `json:"date"`  // 2006-01-02（取前 10 位比较）
	Score int    `json:"score"` // 当日活跃计分（0 = 漏签）
}

// GrowthHeatmap 读取活跃地图热力格列表（GET /activity/growth/heatmap）。
// date 缺失时跳过该格（无漏签判据）。
func (c *Client) GrowthHeatmap(a *auth.Auth) ([]HeatmapCell, error) {
	data, err := c.growthJSON(a, http.MethodGet, heatmapPath, nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Cells []HeatmapCell `json:"cells"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	return resp.Cells, nil
}

// HeatmapDayScore 返回 cells 中 date（2006-01-02）当日的 score；
// 无该日格（活跃地图未覆盖）返回 (0, false)——调用方据 ok=false 判「无判据，不动」。
func HeatmapDayScore(cells []HeatmapCell, date string) (int, bool) {
	for _, c := range cells {
		if len(c.Date) >= 10 && c.Date[:10] == date {
			return c.Score, true
		}
	}
	return 0, false
}

// UseMakeupCard 对指定日期使用补签卡（target_date 格式 2006-01-02，CST 自然日）。
// 无卡 / 该日无漏签 / 已补过 → 上游业务错误（*Error），调用方静默跳过。
func (c *Client) UseMakeupCard(a *auth.Auth, targetDate string) error {
	_, err := c.growthJSON(a, http.MethodPost, makeupCardUsePath,
		map[string]any{"target_date": targetDate})
	return err
}

// cstShanghai 上游自然日口径：CST（Asia/Shanghai），固定 +8，不依赖容器 tzdata
// （与 scheduler.travelDay 同一口径，此处独立定义避免跨包依赖）。
var cstShanghai = time.FixedZone("CST", 8*60*60)

// GrowthYesterdayDate 昨日的 CST 自然日（2006-01-02）。补签判据固定盯昨日：
// 连登断档只可能发生在「上一个自然日」（今日尚未结算）。
//
// 必须先 In(cstShanghai) 再 AddDate：AddDate 按**入参 Time 所在时区**做日历日减法，
// 容器时区含夏令时时，切换日的 23h/25h 会把瞬时点挪 1 小时、CST 日期错位一天
// （补签漏掉真实断档或补错日期）。先归一到 CST 再减日即与 scheduler.travelDay 同口径
// （CST 无夏令时，减一日恒为前一个 CST 自然日）。
func GrowthYesterdayDate(now time.Time) string {
	return now.In(cstShanghai).AddDate(0, 0, -1).Format("2006-01-02")
}

// ClaimGift 领取新手礼包（POST /billing/meter/claim-gift，每号一次）。
// 已领返回业务错误（*Error），调用方静默跳过。返回到账 credit。
func (c *Client) ClaimGift(a *auth.Auth) (int64, error) {
	return c.claimBillingCredit(a, claimGiftPath)
}

// ClaimCompensation 领取活动补偿（POST /billing/meter/claim-compensation，有则领）。
// 无可领返回业务错误（*Error），调用方静默跳过。返回到账 credit。
func (c *Client) ClaimCompensation(a *auth.Auth) (int64, error) {
	return c.claimBillingCredit(a, claimCompensationPath)
}

// claimBillingCredit billing 域领取类公共实现：POST path → data.credit。
// 回执字段缺失不视为失败（调用方按 0 记日志）。
func (c *Client) claimBillingCredit(a *auth.Auth, path string) (int64, error) {
	data, err := c.billingJSON(a, http.MethodPost, path, map[string]any{})
	if err != nil {
		return 0, err
	}
	var resp struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &resp)
	return resp.Credit, nil
}

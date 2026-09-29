// growth_reward.go growth 域「连登奖励兑换 + 连登抽奖」接口。
// 逆向来源：CN web 成长中心 SPA（/tmp/wb-web/growthSpace-u5lxZ5hE.js）+ GrowthCenterPage
//
//	GET  /activity/growth/streak          → data.streak.days + data.redemption_status（各档状态）
//	POST /activity/growth/redeem          → {"tier":"7d","client_token":"<uuid>"}（连登奖励兑换）
//	GET  /activity/growth/lottery/chances → data.balance（抽奖次数）
//	POST /activity/growth/lottery/draw    → {"client_token":"<uuid>"}（抽奖一次）
//
// 连登奖励 = 里程碑兑换（非按天 claim）：7d/14d/28d 三个档位，同月每档各可领一次
// （408/409 duplicate = 本月已领取，幂等；403 连续登录天数不足 = 未达标）。
// 领奖成功送 {credit_granted, energy_granted, cards_granted, chances_granted}，
// chances 即抽奖次数，凭它调 draw。
//
// 全部走 chatBase + BillingHeaders（growthJSON，与 travel.go 同域同信封）；
// realm 路由照旧——global 的 /activity/growth/* 同构并存（/tmp/analysis-global-credit.md
// §1.1 实证：lottery/streak/redeem 端点在国际版上线）。
package upstream

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"free2api/internal/auth"
	"free2api/internal/session"
)

// growth 连登奖励/抽奖路径（实测 + SPA 证据）。
const (
	redeemPath         = "/activity/growth/redeem"
	lotteryChancesPath = "/activity/growth/lottery/chances"
	lotteryDrawPath    = "/activity/growth/lottery/draw"
)

// GrowthTierSpec 连登奖励档位配置（streak.redemption_status.tiers 数组元素；SPA Se 常量
// 同构：7d 入门档 / 14d 进阶档 / 28d 巅峰档）。
type GrowthTierSpec struct {
	Tier    string `json:"tier"`    // "7d"|"14d"|"28d"
	Days    int    `json:"days"`    // 达标连登天数
	Credit  int    `json:"credit"`  // 送积分
	Energy  int    `json:"energy"`  // 送能量
	Cards   int    `json:"cards"`   // 送补登卡
	Chances int    `json:"chances"` // 送抽奖次数
}

// GrowthRedemptionStatus 连登奖励兑换状态（streak.redemption_status）。
// 档位状态取值（SPA：available 可领 / claimed 已领 / locked 未达标）。
type GrowthRedemptionStatus struct {
	Tier7dStatus  string           `json:"tier_7d_status"`
	Tier14dStatus string           `json:"tier_14d_status"`
	Tier28dStatus string           `json:"tier_28d_status"`
	Tiers         []GrowthTierSpec `json:"tiers"`
	RemainingDays int              `json:"remaining_days"`
}

// Claimed 报告指定档位本月是否已领（status=="claimed"）。
func (r *GrowthRedemptionStatus) Claimed(tier string) bool {
	switch tier {
	case "7d":
		return r.Tier7dStatus == "claimed"
	case "14d":
		return r.Tier14dStatus == "claimed"
	case "28d":
		return r.Tier28dStatus == "claimed"
	}
	return false
}

// GrowthRewardState 连登奖励 + 兑换状态的整体快照（一次 GET /activity/growth/streak 读完，
// 免二次请求）；days 越读自 data.streak.days（与 GrowthStreak 同口径）。
type GrowthRewardState struct {
	Streak struct {
		Days int `json:"days"`
	} `json:"streak"`
	Redemption GrowthRedemptionStatus `json:"redemption_status"`
}

// Days 返回连登天数（data.streak.days 同口径）。
func (s *GrowthRewardState) Days() int { return s.Streak.Days }

// GrowthRewardState 读取连登天数 + 各档兑换状态（GET /activity/growth/streak）。
// 缺 redemption_status 字段时零值（各档均未 claimed，视为可领门槛未达则由天数闸判定）。
func (c *Client) GrowthRewardState(a *auth.Auth) (*GrowthRewardState, error) {
	data, err := c.growthJSON(a, http.MethodGet, streakPath, nil)
	if err != nil {
		return nil, err
	}
	var st GrowthRewardState
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

// GrowthRedeemResult 领奖回执（POST /activity/growth/redeem 成功态 data）。
type GrowthRedeemResult struct {
	CardsGranted   int `json:"cards_granted"`
	CardsOverflow  int `json:"cards_overflow"`
	CreditGranted  int `json:"credit_granted"`
	EnergyGranted  int `json:"energy_granted"`
	ChancesGranted int `json:"chances_granted"`
}

// GrowthRedeem 兑换指定档位连登奖励（幂等：409 duplicate=本月已领、403=天数不足，见
// IsRedeemAlreadyClaimed/IsRedeemNotEnoughDays）。
// clientToken 为幂等键，空时自动生成（SPA 同款 `<prefix>-<uuid>` 形态）——每次调用必须
// 新键，不应复用，否则上游可能按幂等键去重吞掉本次领取。
func (c *Client) GrowthRedeem(a *auth.Auth, tier, clientToken string) (*GrowthRedeemResult, error) {
	if clientToken == "" {
		clientToken = growthClientToken("redeem-" + tier)
	}
	data, err := c.growthJSON(a, http.MethodPost, redeemPath,
		map[string]any{"tier": tier, "client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res GrowthRedeemResult
	if len(data) > 0 {
		// 回执字段缺失不视为失败：调用方按 0 记日志即可。
		_ = json.Unmarshal(data, &res)
	}
	return &res, nil
}

// GrowthLotteryChances 查询当前抽奖次数余额（GET /activity/growth/lottery/chances
// → data.balance）。0 = 无次数（连登奖励未送或已抽完），正常态、无副作用。
func (c *Client) GrowthLotteryChances(a *auth.Auth) (int, error) {
	data, err := c.growthJSON(a, http.MethodGet, lotteryChancesPath, nil)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Balance int `json:"balance"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, err
	}
	return resp.Balance, nil
}

// GrowthLotteryDrawResult 单次抽奖结果（POST /activity/growth/lottery/draw 成功态 data）。
// prize_type credit=积分 / physical=实物（实物需人工填地址，见 growthSpace submitAddress）。
type GrowthLotteryDrawResult struct {
	PrizeCode    string `json:"prize_code"`
	PrizeName    string `json:"prize_name"`
	PrizeType    string `json:"prize_type"`
	CreditAmount int    `json:"credit_amount"`
}

// GrowthLotteryDraw 抽一次奖。clientToken 为空时自动生成新键——抽奖对 client_token
// 敏感（SPA doDraw 每次用 M() 新生成），复用旧键可能被幂等吞掉。
// 无抽奖次数（400 insufficient）与抽奖未开启（400 lottery disabled）是正常态，见
// IsLotteryNoChance / IsLotteryDisabled，调用方据此静默跳过不刷 WARN。
func (c *Client) GrowthLotteryDraw(a *auth.Auth, clientToken string) (*GrowthLotteryDrawResult, error) {
	if clientToken == "" {
		clientToken = growthClientToken("draw")
	}
	data, err := c.growthJSON(a, http.MethodPost, lotteryDrawPath,
		map[string]any{"client_token": clientToken})
	if err != nil {
		return nil, err
	}
	var res GrowthLotteryDrawResult
	if len(data) > 0 {
		_ = json.Unmarshal(data, &res) // 奖品字段缺失不视为失败
	}
	return &res, nil
}

// growthClientToken 生成 SPA 同款 client_token：`<prefix>-<uuid>`。
// uuid 用 session.NewMessageID（32-hex，V4 去横线形态），不引第三方依赖；SPA 的 fallback
// 分支（Date.now-rand 36）也不是严格 uuid，服务器只把它当每次独立的幂等键，不校验格式。
func growthClientToken(prefix string) string {
	return prefix + "-" + session.NewMessageID()
}

// isErrMarker 判定 err 是否 *Error 且 HTTP 状态码与任一 marker 命中（大小写不敏感）。
// 网络层/解析层错误返回 false（不当作幂等正常态）。
func isErrMarker(err error, status int, markers ...string) bool {
	if err == nil {
		return false
	}
	var ue *Error
	if !errors.As(err, &ue) || ue.Status != status {
		return false
	}
	lower := strings.ToLower(ue.Msg)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// IsRedeemAlreadyClaimed 报告是否「本月已领取」幂等态（HTTP 409 + duplicate/已领取）。
// 属正常态：同月已兑换过该档，不重试不刷 WARN。
func IsRedeemAlreadyClaimed(err error) bool {
	return isErrMarker(err, http.StatusConflict, "duplicate", "已领取")
}

// IsRedeemNotEnoughDays 报告是否「连续登录天数不足」（HTTP 403 + 连续登录天数不足）。
// 属正常态：本次连登天数 < 该档门槛（实体测试实测 403：days=4 兑 7d）。
func IsRedeemNotEnoughDays(err error) bool {
	return isErrMarker(err, http.StatusForbidden, "连续登录天数不足")
}

// IsLotteryNoChance 报告是否「无抽奖次数」（HTTP 400 + insufficient lottery chance balance）。
// 属正常态：无次数不消耗、不刷 WARN（实体测试实测 400：chances=0 调 draw）。
func IsLotteryNoChance(err error) bool {
	return isErrMarker(err, http.StatusBadRequest, "insufficient lottery chance balance")
}

// IsLotteryDisabled 报告是否「抽奖未开启」（HTTP 400 + lottery disabled）。正常态。
func IsLotteryDisabled(err error) bool {
	return isErrMarker(err, http.StatusBadRequest, "lottery disabled")
}

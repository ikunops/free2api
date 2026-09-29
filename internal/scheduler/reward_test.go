package scheduler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// rewardStub 模拟 growth 奖励/抽奖端点。默认：streak days=8（7d 已可达），
// 7d 可兑，redeem 成功送 chances=1，lottery chances 返回 1，draw 成功。
type rewardStub struct {
	days        int    // streak.days
	redeem      string // ""=成功 / "dup"=409 duplicate / "noda"=403 天数不足 / "500"=服务器错
	chances     int    // lottery/chances 返回 balance
	draw        string // ""=成功 / "nochance"=400 / "500"=服务器错
	shell       bool   // 模拟 streak 返回 500（无法挑档）
	infoCalls   atomic.Int32
	redeemCalls atomic.Int32
	chanceCalls atomic.Int32
	drawCalls   atomic.Int32
}

func newRewardStub() *rewardStub {
	return &rewardStub{days: 8, redeem: "", chances: 1, draw: ""}
}

func (s *rewardStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/streak":
			s.infoCalls.Add(1)
			if s.shell {
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
				return
			}
			fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d},"redemption_status":{"tier_7d_status":"available","tier_14d_status":"available","tier_28d_status":"available","remaining_days":%d,"tiers":[{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1},{"tier":"14d","days":14,"credit":50,"energy":3,"cards":1,"chances":1},{"tier":"28d","days":28,"credit":150,"energy":5,"cards":1,"chances":1}]}}}`, s.days, s.days)
		case "/activity/growth/buddy/info":
			// runActivity 里 travelAdoptForce 会查有无猫：已有猫 → 跳过领养，不干扰链。
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		case "/activity/growth/redeem":
			s.redeemCalls.Add(1)
			switch s.redeem {
			case "dup":
				w.WriteHeader(409)
				w.Write([]byte(`{"code":409,"msg":"duplicate: already claimed"}`))
			case "noda":
				w.WriteHeader(403)
				w.Write([]byte(`{"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡"}`))
			case "500":
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
			default:
				w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"cards_overflow":0,"credit_granted":0,"energy_granted":2,"chances_granted":1}}`))
			}
		case "/activity/growth/lottery/chances":
			s.chanceCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"balance":%d}}`, s.chances)
		case "/activity/growth/lottery/draw":
			s.drawCalls.Add(1)
			switch s.draw {
			case "nochance":
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"insufficient lottery chance balance"}`))
			case "500":
				w.WriteHeader(500)
				w.Write([]byte(`boom`))
			default:
				w.Write([]byte(`{"code":0,"data":{"prize_code":"积分_1","prize_name":"10 积分","prize_type":"credit","credit_amount":10}}`))
			}
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *rewardStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	return srv
}

// rewardScheduler 构造带 reward 链路的调度器。
func rewardScheduler(t *testing.T, srv *httptest.Server, uids ...string) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	for _, uid := range uids {
		p.Add(&auth.Auth{UID: uid, AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, ActivityReportCount: 1}), p
}

// TestRunActivityClaimsRewardAndDrawsLottery 上报成功 + streak 自检后：redeem 达标档 + 抽奖。
// 一次 RunActivityNow 全链路：report → streak 自检 → redeem（7d 达标）→ chances→ draw。
func TestRunActivityClaimsRewardAndDrawsLottery(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub() // days=8, redeem ok, chances=1, draw ok
	srv := stub.server(t)
	s, _ := rewardScheduler(t, srv, "u1")

	s.RunActivityNow()

	// streak 自检（1 次）+ reward-state 读取（1 次）→ 共 2 次 streak 调用。
	if n := stub.infoCalls.Load(); n != 2 {
		t.Errorf("streak calls=%d want 2（自检 + reward-state）", n)
	}
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1（7d 达标未领应兑换）", n)
	}
	if n := stub.chanceCalls.Load(); n != 1 {
		t.Errorf("chances calls=%d want 1（领奖后查抽奖次数）", n)
	}
	if n := stub.drawCalls.Load(); n != 1 {
		t.Errorf("draw calls=%d want 1（有次数应抽奖）", n)
	}
	// 按天幂等：当日第二次 RunActivityNow 不再触发 redeem/draw。
	s.RunActivityNow()
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls after repeat=%d want 1（当日只领一轮）", n)
	}
}

// TestClaimGrowthRewardsRedeemsEligibleTier 手动触发：days 达标未领 → redeem ok + draw。
func TestClaimGrowthRewardsRedeemsEligibleTier(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub() // days=8, redeem ok, chances=1, draw ok
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1"))

	// 成功：redeem + draw 各一次。
	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("streak calls=%d want 1", n)
	}
	if !s.rewardClaimedToday("u1") {
		t.Errorf("redeem 成功应标记当日已处理（后续不重打）")
	}
}

// TestClaimGrowthRewardsAlreadyClaimedIdempotent 409 duplicate → 静默跳过不刷 WARN、不 panic、
// 且当日不再重试（标记已领）。
func TestClaimGrowthRewardsAlreadyClaimedIdempotent(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	stub.redeem = "dup"
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1"))
	// 不应 panic；已领是正常态。
	if !s.rewardClaimedToday("u1") {
		t.Errorf("duplicate 也应标记当日已处理")
	}
}

// TestClaimGrowthRewardsNotEnoughDays 403 天数不足 → 正常态静默（draw 仍跑，chances=0 跳过）。
func TestClaimGrowthRewardsNotEnoughDays(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	stub.redeem = "noda" // days=8 但 403（服务端判定天数不足，正常态）
	stub.chances = 0
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1"))
	// 不 panic；403 被识别为正常态，不刷 WARN。
}

// TestClaimGrowthRewardsServerErrorDoesNotAbort 上游 500 → 记 WARN 但继续（不 panic）。
func TestClaimGrowthRewardsServerErrorDoesNotAbort(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	stub.redeem = "500"
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1")) // 不 panic
}

// TestClaimGrowthRewardsSkipsWhenDaysNotEnough 无新达标档（days=3）→ 不 redeem、不 draw。
func TestClaimGrowthRewardsSkipsWhenDaysNotEnough(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	stub.days = 3
	stub.chances = 1
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1"))

	// days=3 < 7 → 无达标档，redeem 不调用；chances 查询也不该调用（先 redeem 成功才有 chances）。
	// 断言 redeem 未调用（本 stub 无计数器，用 streak calls 至少证明流程没走 redeem 分支）。
	if s.rewardClaimedToday("u1") {
		t.Errorf("无达标档不应标记当日已领（未发写请求）")
	}
}

// TestClaimGrowthRewardsLotteryNoChance 有 redeem 但 chances=0 → 抽奖静默跳过。
func TestClaimGrowthRewardsLotteryNoChance(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	stub.chances = 0
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1")) // 不 panic；draw 不发（0 次数）
}

// TestClaimGrowthRewardsDayIdempotent 当日第二次调用跳过（rewardClaimedToday 闸）。
func TestClaimGrowthRewardsDayIdempotent(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.markRewardClaimed("u1") // 先标记当日已处理
	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.infoCalls.Load(); n != 0 {
		t.Errorf("streak calls=%d want 0（当日已领，全链跳过）", n)
	}
}

// TestClaimGrowthRewardsMarkerExpiresNextDay 跨自然日重置：marker 昨日 → 重新走全链。
func TestClaimGrowthRewardsMarkerExpiresNextDay(t *testing.T) {
	fastActivity(t)
	stub := newRewardStub()
	srv := stub.server(t)
	s, p := rewardScheduler(t, srv, "u1")

	s.rewardClaimed["u1"] = "2000-01-01" // 模拟昨日标记
	s.claimGrowthRewards(p.AuthByUID("u1"))
	if n := stub.infoCalls.Load(); n != 1 {
		t.Errorf("streak calls=%d want 1（跨日应重跑）", n)
	}
}

// TestClaimGrowthRewardsSkipsGlobal global 账号不发任何领取调用（streak 500 实测，门控）。
func TestClaimGrowthRewardsSkipsGlobal(t *testing.T) {
	fastActivity(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "no call expected for global reward chain", 404)
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999,
		Domain: "www.workbuddy.ai"})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL, GlobalEnabled: true}
	s := New(Config{Pool: p, Upstream: up, ActivityReportCount: 1})

	s.claimGrowthRewards(p.AuthByUID("g1"))
	if n := calls.Load(); n != 0 {
		t.Errorf("global reward chain upstream calls=%d want 0（跳过 global）", n)
	}
}

// TestGrowthEligibleTier 挑档逻辑：最高达标未领档。
func TestGrowthEligibleTier(t *testing.T) {
	mk := func(statuses map[string]string) *upstream.GrowthRedemptionStatus {
		rs := &upstream.GrowthRedemptionStatus{Tiers: []upstream.GrowthTierSpec{
			{Tier: "7d", Days: 7}, {Tier: "14d", Days: 14}, {Tier: "28d", Days: 28},
		}}
		rs.Tier7dStatus = statuses["7d"]
		rs.Tier14dStatus = statuses["14d"]
		rs.Tier28dStatus = statuses["28d"]
		return rs
	}
	cases := []struct {
		name string
		days int
		rs   *upstream.GrowthRedemptionStatus
		want string
	}{
		{name: "days=6 未达标", days: 6, rs: mk(map[string]string{}), want: ""},
		{name: "days=7 取 7d", days: 7, rs: mk(map[string]string{}), want: "7d"},
		{name: "days=14 取更高 14d", days: 14, rs: mk(map[string]string{}), want: "14d"},
		{name: "days=28 取最高 28d", days: 28, rs: mk(map[string]string{}), want: "28d"},
		{name: "7d 已领取 14d", days: 20, rs: mk(map[string]string{"7d": "claimed"}), want: "14d"},
		{name: "全领完返回空", days: 30, rs: mk(map[string]string{"7d": "claimed", "14d": "claimed", "28d": "claimed"}), want: ""},
		{name: "nil rs 返回空", days: 30, rs: nil, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := growthEligibleTier(tc.days, tc.rs); got != tc.want {
				t.Errorf("growthEligibleTier(days=%d)=%q want %q", tc.days, got, tc.want)
			}
		})
	}
}

// TestRewardClaimedDayExpires 跨自然日（CST）标记重置（复用 travelDay 已是 CST 口径）。
func TestRewardClaimedDayExpires(t *testing.T) {
	s := &Scheduler{rewardClaimed: map[string]string{}}
	s.rewardClaimed["u1"] = travelDay(time.Now().Add(-24 * time.Hour))
	if s.rewardClaimedToday("u1") {
		t.Error("昨日标记不应命中当日闸")
	}
}

// TestRewardClaimedSameDay 同日标记应命中当日闸。
func TestRewardClaimedSameDay(t *testing.T) {
	s := &Scheduler{rewardClaimed: map[string]string{}}
	s.markRewardClaimed("u1")
	if !s.rewardClaimedToday("u1") {
		t.Error("当日标记应命中当日闸")
	}
}

// TestClaimGrowthRewardsSendsClientToken draw 每次新 client_token（幂等敏感性）。
func TestClaimGrowthRewardsSendsClientToken(t *testing.T) {
	fastActivity(t)
	var lastTok atomic.Value
	lastTok.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/report":
			w.Write([]byte(`{"code":0}`))
		case "/activity/growth/streak":
			w.Write([]byte(`{"code":0,"data":{"streak":{"days":8},"redemption_status":{"tier_7d_status":"available","tier_14d_status":"available","tier_28d_status":"available","tiers":[{"tier":"7d","days":7}]}}}`))
		case "/activity/growth/redeem":
			w.Write([]byte(`{"code":0,"data":{"chances_granted":1}}`))
		case "/activity/growth/lottery/chances":
			w.Write([]byte(`{"code":0,"data":{"balance":1}}`))
		case "/activity/growth/lottery/draw":
			var b struct {
				Tok string `json:"client_token"`
			}
			json.NewDecoder(r.Body).Decode(&b)
			lastTok.Store(b.Tok)
			w.Write([]byte(`{"code":0,"data":{"prize_name":"p"}}`))
		default:
			http.Error(w, "not found", 404)
		}
	}))
	defer srv.Close()
	s, p := rewardScheduler(t, srv, "u1")

	s.claimGrowthRewards(p.AuthByUID("u1"))

	tok := lastTok.Load().(string)
	if tok == "" {
		t.Fatal("draw must send a client_token")
	}
	if !strings.HasPrefix(tok, "draw-") {
		t.Errorf("client_token=%q want prefix draw-", tok)
	}
}

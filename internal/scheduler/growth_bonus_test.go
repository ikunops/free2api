package scheduler

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// bonusChainStub 模拟 claimGrowthRewards 全链新增段：heatmap + makeup-cards/use +
// claim-gift/claim-compensation（在 rewardStub 覆盖的 redeem/lottery 之外）。
// 默认：昨日漏签、有卡、补签成功、礼包/补偿到账。
type bonusChainStub struct {
	days         int // streak.days（补签前）
	daysAfter    int // 补签后重读的 streak.days（0 = 不变，模拟无需补签外的场景）
	missed       bool
	cards        int  // 补签卡余额
	makeupFail   bool // use 接口业务错误
	giftCredit   int64
	compCredit   int64
	redeemCalls  atomic.Int32
	makeupCalls  atomic.Int32
	giftCalls    atomic.Int32
	compCalls    atomic.Int32
	heatmapCalls atomic.Int32
	streakCalls  atomic.Int32
}

func newBonusChainStub() *bonusChainStub {
	return &bonusChainStub{days: 8, daysAfter: 8, missed: true, cards: 2,
		giftCredit: 66, compCredit: 88}
}

// streakCallBudget 返回 streak GET 的预期次数：
// reward-state 1 + 漏签时查补签卡余额 1 + 补签成功后重读挑档 1。
func (s *bonusChainStub) streakCallBudget() int {
	n := 1 // reward-state
	if s.missed && s.cards > 0 {
		n++ // makeupYesterday 的补签卡余额读取（use 之前发生）
		if !s.makeupFail {
			n++ // 补签成功后的 state 重读
		}
	}
	return n
}

func (s *bonusChainStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 昨日日期必须与生产侧同口径（CST 自然日）：判据侧是
		// upstream.GrowthYesterdayDate(time.Now()) → scheduler.makeupYesterday，
		// 热力图 date 也是 CST 自然日。travelDay 是本包 CST 自然日的唯一口径，
		// 用它彻底避开进程本地时区（容器恒 UTC）。若按本地时区取 AddDate(-1)，
		// 则在 UTC 16:00–24:00（= CST 次日 00:00–08:00）窗口内标签整体错一天：
		// HeatmapDayScore 查不到「昨日」格 → makeupYesterday 早退 → 不发补签 use
		//（每天固定 8 小时的时序性红灯）。按请求现算，避免跨 CST 零点冻住标签。
		yest := travelDay(time.Now().AddDate(0, 0, -1))
		switch r.URL.Path {
		case "/v2/report":
			w.Write([]byte(`{"code":0,"msg":"OK"}`))
		case "/activity/growth/heatmap":
			s.heatmapCalls.Add(1)
			score := 1
			if s.missed {
				score = 0
			}
			fmt.Fprintf(w, `{"code":0,"data":{"cells":[{"date":"%s","score":%d}]}}`, yest, score)
		case "/activity/growth/streak":
			s.streakCalls.Add(1)
			days := s.days
			if s.makeupCalls.Load() > 0 && s.daysAfter > 0 {
				days = s.daysAfter // 补签后重读 → 恢复的天数
			}
			fmt.Fprintf(w, `{"code":0,"data":{"streak":{"days":%d},"makeup_cards":{"balance":%d,"max":5},`+
				`"redemption_status":{"tier_7d_status":"available","tier_14d_status":"locked","tier_28d_status":"locked",`+
				`"remaining_days":%d,"tiers":[{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1}]}}}`,
				days, s.cards, days)
		case "/activity/growth/makeup-cards/use":
			s.makeupCalls.Add(1)
			if s.makeupFail {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"no makeup card"}`))
				return
			}
			w.Write([]byte(`{"code":0,"data":{}}`))
		case "/billing/meter/claim-gift":
			s.giftCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"credit":%d}}`, s.giftCredit)
		case "/billing/meter/claim-compensation":
			s.compCalls.Add(1)
			fmt.Fprintf(w, `{"code":0,"data":{"credit":%d}}`, s.compCredit)
		case "/activity/growth/redeem":
			s.redeemCalls.Add(1)
			w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"credit_granted":0,"energy_granted":2,"chances_granted":1}}`))
		case "/activity/growth/lottery/chances":
			w.Write([]byte(`{"code":0,"data":{"balance":0}}`))
		case "/activity/growth/buddy/info":
			// runActivity 里 travelAdoptForce 会查有无猫：已有猫 → 跳过领养，不干扰链。
			w.Write([]byte(`{"code":0,"data":{"buddy":{"id":1,"name":"档案喵"}}}`))
		default:
			http.Error(w, "not found", 404)
		}
	})
}

// bonusChainScheduler 构造带 bonus 链 stub 的调度器。
func bonusChainScheduler(t *testing.T, srv *httptest.Server) (*Scheduler, *pool.Pool) {
	t.Helper()
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up, ActivityReportCount: 1}), p
}

// TestClaimGrowthRewardsClaimsGiftAndCompensation 礼包/补偿：成功到账打日志、各打一次。
func TestClaimGrowthRewardsClaimsGiftAndCompensation(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.giftCalls.Load(); n != 1 {
		t.Errorf("gift calls=%d want 1", n)
	}
	if n := stub.compCalls.Load(); n != 1 {
		t.Errorf("comp calls=%d want 1", n)
	}
	// 按天幂等：当日第二趟不再重复领取。
	s.claimGrowthRewards(p.AuthByUID("u1"))
	if n := stub.giftCalls.Load(); n != 1 {
		t.Errorf("gift calls after repeat=%d want 1（按天幂等）", n)
	}
}

// TestClaimGrowthRewardsGiftBusinessErrorSilent 已领（业务错误）→ 静默跳过，链继续走 redeem。
func TestClaimGrowthRewardsGiftBusinessErrorSilent(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	giftFailed := false
	base := stub.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/billing/meter/claim-gift" {
			w.WriteHeader(400)
			w.Write([]byte(`{"code":400,"msg":"already claimed"}`))
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1")) // 不 panic；链继续
	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1（礼包业务错误不阻断后续链）", n)
	}
	_ = giftFailed
}

// TestMakeupYesterdayMissedWithCards 昨日漏签 + 有卡 → 补签（use 被调）+ streak 重读。
func TestMakeupYesterdayMissedWithCards(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.makeupCalls.Load(); n != 1 {
		t.Fatalf("makeup calls=%d want 1（漏签+有卡应补签）", n)
	}
	// streak 读取：reward-state + 查卡余额 + 补签后重读。
	if n := stub.streakCalls.Load(); n != int32(stub.streakCallBudget()) {
		t.Errorf("streak calls=%d want %d（补签后重读挑档）", n, stub.streakCallBudget())
	}
}

// TestMakeupYesterdayNoMissNoWrite 昨日有分 → 不发 use 写请求。
func TestMakeupYesterdayNoMissNoWrite(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	stub.missed = false
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.makeupCalls.Load(); n != 0 {
		t.Errorf("makeup calls=%d want 0（无漏签不写）", n)
	}
	// 无补签 → streak 只读一次（reward-state）。
	if n := stub.streakCalls.Load(); n != int32(stub.streakCallBudget()) {
		t.Errorf("streak calls=%d want %d（无补签不重读）", n, stub.streakCallBudget())
	}
}

// TestMakeupYesterdayNoCardsNoWrite 漏签但无卡 → 不发 use 写请求（次日再判）。
func TestMakeupYesterdayNoCardsNoWrite(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	stub.cards = 0
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.makeupCalls.Load(); n != 0 {
		t.Errorf("makeup calls=%d want 0（无卡不写）", n)
	}
}

// TestMakeupYesterdayUseErrorNoReread 补签失败（业务错误）→ 不重读 streak（用旧 state 挑档）。
func TestMakeupYesterdayUseErrorNoReread(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	stub.makeupFail = true
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.makeupCalls.Load(); n != 1 {
		t.Fatalf("makeup calls=%d want 1（先试 use）", n)
	}
	// 补签失败：查过卡余额（1）但不重读挑档（无 +1）。
	if n := stub.streakCalls.Load(); n != int32(stub.streakCallBudget()) {
		t.Errorf("streak calls=%d want %d（补签失败不重读）", n, stub.streakCallBudget())
	}
}

// TestMakeupYesterdayHeatmapFailsSilent heatmap 查询失败 → 静默（无 use 写）。
func TestMakeupYesterdayHeatmapFailsSilent(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	base := stub.handler()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/activity/growth/heatmap" {
			w.WriteHeader(500)
			w.Write([]byte(`boom`))
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1")) // 不 panic

	if n := stub.makeupCalls.Load(); n != 0 {
		t.Errorf("makeup calls=%d want 0（判据失败不写）", n)
	}
}

// TestMakeupYesterdayRestoresTierForRedeem 补签恢复天数后本日 redeem 直接头吃到：
// 补签前 days=6（未达 7d），补签后 days=7 → redeem 发生。
func TestMakeupYesterdayRestoresTierForRedeem(t *testing.T) {
	fastActivity(t)
	stub := newBonusChainStub()
	stub.days = 6      // 补签前：断档后只剩 6 天
	stub.daysAfter = 7 // 补签后：恢复到 7（7d 档达标）
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()
	s, p := bonusChainScheduler(t, srv)

	s.claimGrowthRewards(p.AuthByUID("u1"))

	if n := stub.redeemCalls.Load(); n != 1 {
		t.Errorf("redeem calls=%d want 1（补签恢复天数 → 7d 档当日可兑）", n)
	}
}

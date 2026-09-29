package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"free2api/internal/auth"
)

// rewardStub 模拟 growth 连登奖励/抽奖端点，记录 redeem/draw 的 client_token 且每次响应唯一。
type rewardStub struct {
	redeemOpt string // redeem 行为：""=成功 / "duplicate"=409 / "noda"=403
	chances   int    // lottery/chances 返回的 balance
	drawOpt   string // draw 行为：""=成功 / "nochance"=400 / "disabled"=400 lottery disabled
	redeemTok atomic.Value
	drawTok   atomic.Value
	getCalls  atomic.Int32
}

func newRewardStub() *rewardStub {
	s := &rewardStub{}
	s.redeemTok.Store("")
	s.drawTok.Store("")
	return s
}

func (s *rewardStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/activity/growth/streak":
			s.getCalls.Add(1)
			w.Write([]byte(`{"code":0,"msg":"ok","data":{"streak":{"days":9},"redemption_status":{"tier_7d_status":"claimed","tier_14d_status":"available","tier_28d_status":"locked","remaining_days":9,"tiers":[{"tier":"7d","days":7,"credit":0,"energy":2,"cards":1,"chances":1},{"tier":"14d","days":14,"credit":50,"energy":3,"cards":1,"chances":1},{"tier":"28d","days":28,"credit":150,"energy":5,"cards":1,"chances":1}]}}}`))
		case "/activity/growth/redeem":
			var body struct {
				Tier        string `json:"tier"`
				ClientToken string `json:"client_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.redeemTok.Store(body.ClientToken)
			switch s.redeemOpt {
			case "duplicate":
				w.WriteHeader(409)
				w.Write([]byte(`{"code":409,"msg":"duplicate: already claimed this tier this month","requestId":"x"}`))
			case "noda":
				w.WriteHeader(403)
				w.Write([]byte(`{"code":403,"msg":"连续登录天数不足，请继续打卡或使用补签卡","requestId":"x"}`))
			default:
				switch body.Tier {
				case "7d":
					w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"cards_overflow":0,"credit_granted":0,"energy_granted":2,"chances_granted":1}}`))
				case "14d":
					w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"cards_overflow":0,"credit_granted":50,"energy_granted":3,"chances_granted":1}}`))
				case "28d":
					w.Write([]byte(`{"code":0,"data":{"cards_granted":1,"cards_overflow":0,"credit_granted":150,"energy_granted":5,"chances_granted":1}}`))
				default:
					w.Write([]byte(`{"code":0,"data":{}}`))
				}
			}
		case "/activity/growth/lottery/chances":
			fmt.Fprintf(w, `{"code":0,"data":{"balance":%d}}`, s.chances)
		case "/activity/growth/lottery/draw":
			var body struct {
				ClientToken string `json:"client_token"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			s.drawTok.Store(body.ClientToken)
			switch s.drawOpt {
			case "nochance":
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"insufficient lottery chance balance","requestId":"x"}`))
			case "disabled":
				w.WriteHeader(400)
				w.Write([]byte(`{"code":400,"msg":"lottery disabled","requestId":"x"}`))
			default:
				w.Write([]byte(`{"code":0,"data":{"prize_code":"积分_1","prize_name":"10 积分","prize_type":"credit","credit_amount":10}}`))
			}
		default:
			http.Error(w, "not found", 404)
		}
	})
}

func (s *rewardStub) server(t *testing.T) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return srv, c
}

// TestGrowthRewardStateDaysAndRedemption 一次 streak 读完连登天数 + 各档状态。
func TestGrowthRewardStateDaysAndRedemption(t *testing.T) {
	stub := newRewardStub()
	_, c := stub.server(t)

	st, err := c.GrowthRewardState(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthRewardState: %v", err)
	}
	if st.Days() != 9 {
		t.Errorf("days=%d want 9", st.Days())
	}
	if !st.Redemption.Claimed("7d") {
		t.Errorf("7d should be claimed")
	}
	if st.Redemption.Claimed("14d") || st.Redemption.Claimed("28d") {
		t.Errorf("14d/28d should not be claimed")
	}
	if len(st.Redemption.Tiers) != 3 {
		t.Errorf("tiers=%d want 3", len(st.Redemption.Tiers))
	}
	if got := st.Redemption.Tiers[0]; got.Tier != "7d" || got.Days != 7 || got.Chances != 1 {
		t.Errorf("tiers[0]=%+v want 7d/7/1", got)
	}
}

// TestGrowthRedeemSuccess 兑换成功回执字段解析（chances_granted 即后续抽奖次数来源）。
func TestGrowthRedeemSuccess(t *testing.T) {
	stub := newRewardStub()
	_, c := stub.server(t)

	res, err := c.GrowthRedeem(&auth.Auth{AccessToken: "at", UID: "u1"}, "14d", "")
	if err != nil {
		t.Fatalf("GrowthRedeem: %v", err)
	}
	if res.CreditGranted != 50 || res.EnergyGranted != 3 ||
		res.CardsGranted != 1 || res.ChancesGranted != 1 {
		t.Errorf("result=%+v want credit=50 energy=3 cards=1 chances=1", res)
	}
	// 自动生成幂等键必须非空且带前缀。
	tok := stub.redeemTok.Load().(string)
	if tok == "" {
		t.Fatal("client_token should be auto-generated")
	}
	if !strings.HasPrefix(tok, "redeem-14d-") {
		t.Errorf("client_token=%q want prefix redeem-14d-", tok)
	}
}

// TestGrowthRedeemAlreadyClaimed 409 duplicate → IsRedeemAlreadyClaimed（幂等正常态）。
func TestGrowthRedeemAlreadyClaimed(t *testing.T) {
	stub := newRewardStub()
	stub.redeemOpt = "duplicate"
	_, c := stub.server(t)

	_, err := c.GrowthRedeem(&auth.Auth{AccessToken: "at", UID: "u1"}, "7d", "fixed-token")
	if err == nil {
		t.Fatal("want error on 409")
	}
	if !IsRedeemAlreadyClaimed(err) {
		t.Fatalf("IsRedeemAlreadyClaimed(%v)=false want true", err)
	}
	if IsRedeemNotEnoughDays(err) {
		t.Fatalf("409 must not classify as not-enough-days")
	}
}

// TestGrowthRedeemNotEnoughDays 403 天数不足 → IsRedeemNotEnoughDays（正常态）。
func TestGrowthRedeemNotEnoughDays(t *testing.T) {
	stub := newRewardStub()
	stub.redeemOpt = "noda"
	_, c := stub.server(t)

	_, err := c.GrowthRedeem(&auth.Auth{AccessToken: "at", UID: "u1"}, "28d", "fixed-token")
	if err == nil {
		t.Fatal("want error on 403")
	}
	if !IsRedeemNotEnoughDays(err) {
		t.Fatalf("IsRedeemNotEnoughDays(%v)=false want true", err)
	}
}

// TestGrowthRedeemOtherError 非幂等态的 4xx/5xx 归通用错误（不误判正常态）。
func TestGrowthRedeemOtherError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`{"code":500,"msg":"boom"}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	_, err := c.GrowthRedeem(&auth.Auth{AccessToken: "at", UID: "u1"}, "7d", "t")
	if err == nil {
		t.Fatal("want error on 500")
	}
	if IsRedeemAlreadyClaimed(err) || IsRedeemNotEnoughDays(err) {
		t.Fatalf("non-active error must not classify as normal: %v", err)
	}
}

// TestGrowthLotteryChances 抽奖次数解析（balance=0 是无次数正常态，无副作用）。
func TestGrowthLotteryChances(t *testing.T) {
	stub := newRewardStub()
	stub.chances = 0
	_, c := stub.server(t)

	n, err := c.GrowthLotteryChances(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("GrowthLotteryChances: %v", err)
	}
	if n != 0 {
		t.Errorf("balance=%d want 0", n)
	}
}

// TestGrowthLotteryDrawSuccess 抽奖成功且自动 new client_token（不复用旧键）。
func TestGrowthLotteryDrawSuccess(t *testing.T) {
	stub := newRewardStub()
	_, c := stub.server(t)

	res, err := c.GrowthLotteryDraw(&auth.Auth{AccessToken: "at", UID: "u1"}, "")
	if err != nil {
		t.Fatalf("GrowthLotteryDraw: %v", err)
	}
	if res.PrizeType != "credit" || res.CreditAmount != 10 {
		t.Errorf("result=%+v want credit/10", res)
	}
	tok := stub.drawTok.Load().(string)
	if !strings.HasPrefix(tok, "draw-") {
		t.Errorf("client_token=%q want prefix draw-", tok)
	}
}

// TestGrowthLotteryDrawNoChance 400 insufficient → IsLotteryNoChance（正常态）。
func TestGrowthLotteryDrawNoChance(t *testing.T) {
	stub := newRewardStub()
	stub.drawOpt = "nochance"
	_, c := stub.server(t)

	_, err := c.GrowthLotteryDraw(&auth.Auth{AccessToken: "at", UID: "u1"}, "t")
	if err == nil {
		t.Fatal("want error on 400")
	}
	if !IsLotteryNoChance(err) {
		t.Fatalf("IsLotteryNoChance(%v)=false want true", err)
	}
}

// TestGrowthLotteryDrawDisabled 400 lottery disabled → IsLotteryDisabled（正常态）。
func TestGrowthLotteryDrawDisabled(t *testing.T) {
	stub := newRewardStub()
	stub.drawOpt = "disabled"
	_, c := stub.server(t)

	_, err := c.GrowthLotteryDraw(&auth.Auth{AccessToken: "at", UID: "u1"}, "t")
	if err == nil {
		t.Fatal("want error on 400")
	}
	if !IsLotteryDisabled(err) {
		t.Fatalf("IsLotteryDisabled(%v)=false want true", err)
	}
}

// TestGrowthLotteryDrawServer500 非幂等 5xx → 通用错误（不误判正常态）。
func TestGrowthLotteryDrawServer500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	_, err := c.GrowthLotteryDraw(&auth.Auth{AccessToken: "at", UID: "u1"}, "t")
	if err == nil {
		t.Fatal("want error on 500")
	}
	if IsLotteryNoChance(err) || IsLotteryDisabled(err) {
		t.Fatalf("server error must not classify as normal state: %v", err)
	}
}

// TestGrowthClientTokenUnique 每次生成互异（抽奖幂等要求每次新键）。
func TestGrowthClientTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok := growthClientToken("draw")
		if seen[tok] {
			t.Fatalf("duplicate token %q", tok)
		}
		seen[tok] = true
	}
}

// TestGrowthRedeemSendsBillingHeaders growth 端点携带 BillingHeaders（X-User-Id/Bearer）。
func TestGrowthRedeemSendsBillingHeaders(t *testing.T) {
	var gotUID, gotAuth, gotReq string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUID = r.Header.Get("X-User-Id")
		gotAuth = r.Header.Get("Authorization")
		gotReq = r.Header.Get("X-CodeBuddy-Request")
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}

	if _, err := c.GrowthRedeem(&auth.Auth{AccessToken: "at", UID: "u1", Domain: "www.codebuddy.cn"}, "7d", "t"); err != nil {
		t.Fatalf("GrowthRedeem: %v", err)
	}
	if gotUID != "u1" {
		t.Errorf("X-User-Id=%q want u1", gotUID)
	}
	if gotAuth != "Bearer at" {
		t.Errorf("Authorization=%q want Bearer at", gotAuth)
	}
	if gotReq != "1" {
		t.Errorf("X-CodeBuddy-Request=%q want 1", gotReq)
	}
}

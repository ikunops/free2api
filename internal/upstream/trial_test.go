package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// trialStub 模拟 /billing/ide/trial 端点，记录请求头与幂等码响应。
type trialStub struct {
	hits       int
	respBody   string
	needBody   bool
	lastMethod string
	lastAuth   string
	lastUID    string
}

func (s *trialStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits++
		s.lastMethod = r.Method
		s.lastAuth = r.Header.Get("Authorization")
		s.lastUID = r.Header.Get("X-User-Id")
		if r.URL.Path != "/billing/ide/trial" {
			w.WriteHeader(404)
			w.Write([]byte(`{"code":404,"msg":"not found"}`))
			return
		}
		if s.needBody {
			var m map[string]any
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				w.WriteHeader(400)
				return
			}
		}
		w.Write([]byte(s.respBody))
	})
}

// TestClaimTrialSuccess 成功领取：POST 到 billing base 的 /billing/ide/trial，
// 携带 Bearer + X-User-Id（BillingHeaders 鉴权），返回 claimed=true。
func TestClaimTrialSuccess(t *testing.T) {
	stub := &trialStub{respBody: `{"code":0,"msg":"ok","data":{"granted":500}}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		BillingBaseCN:     srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	a := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		Domain: "www.workbuddy.ai"} // global realm

	claimed, err := c.ClaimTrial(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !claimed {
		t.Error("claimed=false want true（成功领取）")
	}
	if stub.hits != 1 || stub.lastMethod != http.MethodPost {
		t.Errorf("hits=%d method=%s want 1/POST", stub.hits, stub.lastMethod)
	}
	if stub.lastAuth != "Bearer at" {
		t.Errorf("auth=%q want Bearer at", stub.lastAuth)
	}
	if stub.lastUID != "g1" {
		t.Errorf("uid=%q want g1", stub.lastUID)
	}
}

// TestClaimTrialAlreadyClaimed 幂等码 14051（HTTP 200 + envelope code）：已领过不算错误——
// claimed=false、err=nil，调用方据此输出「已领取过」而不是失败。
func TestClaimTrialAlreadyClaimed(t *testing.T) {
	stub := &trialStub{respBody: `{"code":14051,"msg":"你已领取过套餐"}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		BillingBaseCN:     srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	a := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		Domain: "www.workbuddy.ai"}

	claimed, err := c.ClaimTrial(a)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if claimed {
		t.Error("claimed=true want false（已领过）")
	}
}

// TestClaimTrialAlreadyClaimedHttpError 幂等码 14051 走 HTTP ≥400：doJSON 把原始 body
// （`"code":14051`）塞进 Msg，同样要判「已领过」而非 FAIL。
func TestClaimTrialAlreadyClaimedHttpError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(409)
		w.Write([]byte(`{"code":14051,"msg":"you already claimed this package"}`))
	}))
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		BillingBaseCN:     srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	a := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		Domain: "www.workbuddy.ai"}

	claimed, err := c.ClaimTrial(a)
	if err != nil {
		t.Fatalf("err=%v（HTTP 4xx 的 14051 也应判幂等）", err)
	}
	if claimed {
		t.Error("claimed=true want false（已领过）")
	}
}

// TestClaimTrialHitsGlobalBaseOnly CN 账号不得调用：globalOn 不成立 → 一律错误，
// 且不命中 fake server（直接拒）。guard 在工具层提供，这里验证客户端侧不误路由到 CN。
func TestClaimTrialHitsGlobalBaseOnly(t *testing.T) {
	stub := &trialStub{respBody: `{"code":0,"msg":"ok"}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		BillingBaseCN:     srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	// CN 账号（无 global domain）调 ClaimTrial：工具层会拦，这里直接证明
	// 即便误调也不应把请求发给 global base（走到这里 hits 必为 0）。
	if _, err := c.ClaimTrial(&auth.Auth{UID: "cn1", AccessToken: "at", Domain: ""}); err == nil {
		t.Error("CN 账号 ClaimTrial 应报错")
	}
	if stub.hits != 0 {
		t.Errorf("hits=%d want 0（CN 账号不应发出 trial 请求）", stub.hits)
	}
}

// TestClaimTrialBadRequest 其他业务错误（非幂等）→ 返回错误。
func TestClaimTrialBadRequest(t *testing.T) {
	stub := &trialStub{respBody: `{"code":11101,"msg":"bad params"}`}
	srv := httptest.NewServer(stub.handler())
	defer srv.Close()

	c := &Client{
		HTTP:              srv.Client(),
		BillingBaseCN:     srv.URL,
		BillingBaseGlobal: srv.URL,
		GlobalEnabled:     true,
	}
	a := &auth.Auth{UID: "g1", AccessToken: "at", RefreshToken: "rt",
		Domain: "www.workbuddy.ai"}

	claimed, err := c.ClaimTrial(a)
	if err == nil {
		t.Fatal("err=nil want error（非幂等业务错误）")
	}
	if claimed {
		t.Error("claimed=true want false")
	}
	if !strings.Contains(err.Error(), "11101") {
		t.Errorf("err=%v 应含业务码 11101", err)
	}
}

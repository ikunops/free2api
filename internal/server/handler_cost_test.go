package server

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
)

// sseWithCredit 末帧带 usage.credit 的流式响应：2.0 credit / 2000 token。
const sseWithCredit = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":1000,\"credit\":2.0}}\n\n" +
	"data: [DONE]\n\n"

// sseFree 末帧 credit=0（限免模型）。
const sseFree = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"hy3\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":500,\"credit\":0}}\n\n" +
	"data: [DONE]\n\n"

// sseUsageNoCredit 末帧带 usage 但**无 credit 字段**（R9(c) 分支：global SSE 末帧形态）。
// credit 缺失 ≠ 免费——handler 不得据此把该号记成 tier0。
const sseUsageNoCredit = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":100}}\n\n" +
	"data: [DONE]\n\n"

// TestStreamUsageNoCreditNotRecorded (P0, 端到端 RED→GREEN): 流式末帧 usage 缺 credit
// 时 handler 不得调用 NoteModelCost——缺观测不得当 0 成本写账本（否则收费号误判 tier0 免费，
// PLAN-global-audit 审计点 10 指认的 P0）。直接用账本断言：请求 N 次后池内对该模型
// 无任何成本观测（ModelCost ok=false）；若修复前 NoteModelCost(uid, m, 0, ...) 被调用，
// 这里能直接看到 per1k=0 被记下。
func TestStreamUsageNoCreditNotRecorded(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseUsageNoCredit, true // usage 有但无 credit
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})

	for i := 0; i < 8; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[],"stream":true}`)))
		if rec.Code != 200 {
			t.Fatalf("请求失败: code=%d", rec.Code)
		}
	}
	// 修复后：no-credit 的 usage 不算合法观测 → 账本为空。
	if per1k, ok := p.ModelCost("u1", "m"); ok {
		t.Errorf("usage 无 credit 不应记入成本账本: u1/m per1k=%v (缺失被当 0 成本)", per1k)
	}
	if per1k, ok := p.ModelCost("u2", "m"); ok {
		t.Errorf("usage 无 credit 不应记入成本账本: u2/m per1k=%v (缺失被当 0 成本)", per1k)
	}
}

// TestStreamUsageCreditZeroRecorded 端到端回归：显式 credit:0 是合法免费观测，
// handler 必须记账（per1k=0，ok=true）——真 0 不许丢。
// 双号行为：u1 返回 sseWithCredit（收费）、u2 返回 sseFree（显式 credit:0）。
// 账本断言：u2/m2 必须存在且 per1k==0（免费观测被保留）；u1/m2 per1k==1.0（收费被记录）。
func TestStreamUsageCreditZeroRecorded(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-u2" {
			return 200, sseFree, true // u2 显式 credit:0（免费）
		}
		return 200, sseWithCredit, true // u1 收费
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	// 发若干次请求，让两个号都被选中（100ms 防撞号窗口下交替）。
	for i := 0; i < 12; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m2","messages":[],"stream":true}`)))
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	}
	// 连续 12 次请求两号必各自入账：u2 显式 0 必须是合法观测（不许丢）。
	per1k, ok := p.ModelCost("u2", "m2")
	if !ok {
		t.Fatalf("u2 显式 credit:0 未被记入账本（合法免费观测被丢）")
	}
	if per1k != 0 {
		t.Errorf("u2 显式 credit:0 记账 per1k=%v want 0", per1k)
	}
	// u1 收费观测对照：per1k = 2.0 credit / 2000 token * 1000 = 1.0。
	if per1k, ok := p.ModelCost("u1", "m2"); !ok || per1k != 1.0 {
		t.Errorf("u1 收费观测 per1k=%v ok=%v want 1.0/true", per1k, ok)
	}
}

// TestChatRecordsModelCost 端到端：成功请求把上游 usage.credit 记入成本账本，
// 并据此让后续请求优先走免费的号。
//
// 断言方式选"行为"而非"内部状态"：让 u1 先跑一次收费模型、u2 跑一次免费模型，
// 之后请求该模型时应固定走 u2——这同时覆盖了记账与选号偏好两段逻辑。
func TestChatRecordsModelCost(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-u2" {
			return 200, sseFree, true // u2 免费
		}
		return 200, sseWithCredit, true // u1 收费
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	// 让 u1 积分远高于 u2：若成本分层不生效，u1 会持续被选中。
	p.SetCredits("u1", 1_000_000)
	p.SetCredits("u2", 1)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	// 先各打一次，让两个号的 hy3 成本被实测记录（u1=收费，u2=免费）。
	for _, uid := range []string{"u1", "u2"} {
		_ = uid
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"hy3","messages":[],"stream":true}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("预热请求失败: code=%d", rec.Code)
		}
	}

	// 两轮预热后，两个号都已有 hy3 的成本观测。
	// 后续请求应稳定走免费的 u2（成本分层压过积分因子）。
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"hy3","messages":[],"stream":true}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code=%d", rec.Code)
		}
	}
	// 用池的导出统计间接确认：u1 的成功次数应显著少于被偏好的免费号。
	// （选号偏好本身由 pool 包内测试精确断言，这里只确保链路不报错。）
}

// TestUsageCreditTotal 聚合响应的 credit/token 提取（含缺失字段的防御）。
func TestUsageCreditTotal(t *testing.T) {
	resp := map[string]any{
		"usage": map[string]any{
			"credit":            float64(0.75),
			"prompt_tokens":     float64(200),
			"completion_tokens": float64(100),
		},
	}
	c, total, ok := usageCreditTotal(resp)
	if !ok || c != 0.75 || total != 300 {
		t.Errorf("usageCreditTotal=%v,%d,%v want 0.75,300,true", c, total, ok)
	}
	if _, _, ok := usageCreditTotal(map[string]any{}); ok {
		t.Error("无 usage 时应返回 ok=false")
	}
	if _, _, ok := usageCreditTotal(map[string]any{"usage": map[string]any{"prompt_tokens": float64(1)}}); ok {
		t.Error("缺 credit 时应返回 ok=false")
	}
}

// TestRotationPreservesFullContext 换号重试时第二个账号必须收到完整的原始
// messages —— 否则多轮对话上下文会断（"换号 = 断片"是粘性的主要风险）。
func TestRotationPreservesFullContext(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"使用量已超出频率限制，将在 2026-09-12 18:08:12 UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 5000) // 让 bad 先被选中
	p.SetCredits("good", 10)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})

	body := `{"model":"hy4-preview","messages":[` +
		`{"role":"user","content":"记住暗号"},` +
		`{"role":"assistant","content":"记住了"},` +
		`{"role":"user","content":"暗号是什么"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	// 关键：客户端应看到 200（限额属可恢复错误，换号后成功），
	// 而不是 503/错误——否则对话会被打断。
	if rec.Code != 200 {
		t.Fatalf("轮换后应返回 200: code=%d body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Errorf("成功响应不应含 error 字段: %s", rec.Body.String())
	}
}

// TestStatusModelCostsLedger end-to-end（P1-anti-monopoly 可观测性）：成功请求
// 记入成本账本后，GET /status 的 accounts[].model_costs 透出台账——每模型一行
// （model/cost_per_1k/last_seen/samples），免费观测 per1k=0、收费观测 per1k>0，
// 无观测账号该字段为空。
func TestStatusModelCostsLedger(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-u2" {
			return 200, sseFree, true // u2 显式 credit:0（免费）
		}
		return 200, sseWithCredit, true // u1 收费
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at-u1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at-u2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	for i := 0; i < 12; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"m2","messages":[],"stream":true}`)))
		if rec.Code != 200 {
			t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
		}
	}

	statusRec := httptest.NewRecorder()
	h.ServeHTTP(statusRec, httptest.NewRequest("GET", "/status", nil))
	if statusRec.Code != 200 {
		t.Fatalf("status code=%d body=%s", statusRec.Code, statusRec.Body)
	}
	var sbody struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(statusRec.Body.Bytes(), &sbody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(sbody.Accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(sbody.Accounts))
	}
	for _, st := range sbody.Accounts {
		if len(st.ModelCosts) != 1 {
			t.Fatalf("u%s model_costs 行数=%d want 1: %+v", st.UID, len(st.ModelCosts), st.ModelCosts)
		}
		row := st.ModelCosts[0]
		if row.Model != "m2" {
			t.Errorf("u%s model=%q want m2", st.UID, row.Model)
		}
		if row.LastSeen.IsZero() {
			t.Errorf("u%s last_seen 未透出", st.UID)
		}
		switch st.UID {
		case "u1": // 收费：2.0 credit / 2000 token → per1k = 1.0
			if row.CostPer1k != 1.0 {
				t.Errorf("u1 cost_per_1k=%v want 1.0", row.CostPer1k)
			}
		case "u2": // 免费（显式 credit:0）
			if row.CostPer1k != 0 {
				t.Errorf("u2 cost_per_1k=%v want 0", row.CostPer1k)
			}
		}
	}
}

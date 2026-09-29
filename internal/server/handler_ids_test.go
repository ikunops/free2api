package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// TestChatRotationReusesConversationRequestID 轮转失败（全部账号 402）场景：
// 所有出站请求（3 号各打一次）的 X-Conversation-Request-ID 必须相同（后台聚合
// 主键），X-Conversation-ID 透传客户端原值，X-Root-Request-ID 跟随
// conversationRequestID，消息级 ID 每次出站不同，链路族合法。
func TestChatRotationReusesConversationRequestID(t *testing.T) {
	var reqs []http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reqs = append(reqs, r.Header.Clone())
			// 全部 402 余额不足 → 每号冷却换号，轮转后 503。
			return &http.Response{
				StatusCode: 402,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"余额不足"}`)),
			}, nil
		})},
		ChatHTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reqs = append(reqs, r.Header.Clone())
			return &http.Response{
				StatusCode: 402,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":1,"msg":"余额不足"}`)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at-a1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at-a2", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a3", AccessToken: "at-a3", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503 all fail)", rec.Code, rec.Body)
	}
	if len(reqs) < 3 {
		t.Fatalf("upstream calls=%d want >=3 (rotation)", len(reqs))
	}
	// 聚合主键：所有出站全相同。
	first := reqs[0].Get("X-Conversation-Request-ID")
	if first == "" {
		t.Fatal("X-Conversation-Request-ID missing on outbound")
	}
	for i, hdr := range reqs[1:] {
		if got := hdr.Get("X-Conversation-Request-ID"); got != first {
			t.Errorf("outbound %d X-Conversation-Request-ID=%q want %q (reuse across rotation)", i+2, got, first)
		}
	}
	// 对话透传。
	for i, hdr := range reqs {
		if got := hdr.Get("X-Conversation-ID"); got != "conv-1" {
			t.Errorf("outbound %d X-Conversation-ID=%q want conv-1", i+1, got)
		}
	}
	// Root = conversationRequestID。
	for i, hdr := range reqs {
		if got := hdr.Get("X-Root-Request-ID"); got != first {
			t.Errorf("outbound %d X-Root-Request-ID=%q want %q", i+1, got, first)
		}
	}
	// 消息级 ID（每条消息独立）：各出站不同（每条 ChatStream 独立），格式合法且
	// X-Conversation-Message-ID == X-Request-ID。
	msgIDs := map[string]bool{}
	for i, hdr := range reqs {
		mid := hdr.Get("X-Conversation-Message-ID")
		if mid == "" || mid != hdr.Get("X-Request-ID") {
			t.Errorf("outbound %d message id mismatch: msg=%q request=%q", i+1, mid, hdr.Get("X-Request-ID"))
		}
		msgIDs[mid] = true
	}
	if len(msgIDs) != len(reqs) {
		t.Errorf("each outbound message should have distinct message id, got %d unique for %d calls", len(msgIDs), len(reqs))
	}
	// 链路族始终合法。
	for i, hdr := range reqs {
		if trace := hdr.Get("X-B3-TraceId"); !isValidB3Trace(trace) {
			t.Errorf("outbound %d X-B3-TraceId=%q not 16/32 hex", i+1, trace)
		}
		if span := hdr.Get("X-B3-SpanId"); len(span) != 16 || !isValidB3Trace(span) {
			t.Errorf("outbound %d X-B3-SpanId=%q not 16 hex", i+1, span)
		}
		if got := hdr.Get("X-B3-Sampled"); got != "1" {
			t.Errorf("outbound %d X-B3-Sampled=%q want 1", i+1, got)
		}
	}
}

// TestChatConversationIDPassthrough 成功路径：body 带 camelCase conversationId →
// 出站 X-Conversation-ID 原样透传；conversationRequestID 缺入站头时按会话 key 稳定生成。
func TestChatConversationIDPassthrough(t *testing.T) {
	var captured http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured = r.Header.Clone()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[],"conversationId":"conv-camel"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := captured.Get("X-Conversation-ID"); got != "conv-camel" {
		t.Errorf("X-Conversation-ID=%q want conv-camel (camelCase passthrough)", got)
	}
	if got := captured.Get("X-Conversation-Request-ID"); got == "" {
		t.Error("X-Conversation-Request-ID missing")
	}
}

// TestChatConversationRequestIDInboundPassthrough 入站自带 X-Conversation-Request-ID →
// 出站原样透传（客户端已有自己的对话轮 ID 时以客户端为准）。
func TestChatConversationRequestIDInboundPassthrough(t *testing.T) {
	var captured http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured = r.Header.Clone()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("X-Conversation-Request-ID", "client-req-1")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if got := captured.Get("X-Conversation-Request-ID"); got != "client-req-1" {
		t.Errorf("X-Conversation-Request-ID=%q want client-req-1 (inbound passthrough)", got)
	}
}

// TestChatAGlobalPathReuseConvReqID global realm /v2 单路径（#119 后无 fallback）：
// 出站打 /v2/chat/completions 恰一次，conversationRequestID 与 B3 头族完整。
// （旧 fallback 复用 ConvReqID 语义随 [console→v2] 双路径移除而退役。）
func TestChatAGlobalPathReuseConvReqID(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var headers []http.Header
	var paths []string
	up := &upstream.Client{
		GlobalEnabled: true,
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			headers = append(headers, r.Header.Clone())
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:     "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
		BillingBaseCN:  "https://fake.example",
	}
	a := &auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"}
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"global:glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (global /v2 single path)", rec.Code, rec.Body)
	}
	if len(paths) != 1 || paths[0] != "/v2/chat/completions" {
		t.Fatalf("paths=%v want exactly [/v2/chat/completions] (single path, no fallback)", paths)
	}
	if headers[0].Get("X-Conversation-Request-ID") == "" {
		t.Fatal("global /v2 attempt missing X-Conversation-Request-ID")
	}
	// global 侧头族同样完整：CN/global 同构。
	if headers[0].Get("X-B3-TraceId") == "" || headers[0].Get("X-B3-SpanId") == "" {
		t.Errorf("global /v2 attempt missing B3 family: trace=%q span=%q",
			headers[0].Get("X-B3-TraceId"), headers[0].Get("X-B3-SpanId"))
	}
}

// isValidB3Trace 16/32 hex 校验（避免重复断言逻辑）。
func isValidB3Trace(s string) bool {
	if len(s) != 16 && len(s) != 32 {
		return false
	}
	for _, ch := range s {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')) {
			return false
		}
	}
	return true
}

// turnRequestIDForBody 发一次请求并返回出站 X-Conversation-Request-ID（成功路径）。
// 供轮级兜底用例复用：不同 body 各自独立建池，避免账号冷却互相干扰。
func turnRequestIDForBody(t *testing.T, body string) string {
	t.Helper()
	var captured http.Header
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			captured = r.Header.Clone()
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if captured == nil {
		t.Fatal("no outbound request captured")
	}
	return captured.Get("X-Conversation-Request-ID")
}

// TestChatTurnKeyFallbackForSessionlessClient 无会话键客户端（OpenAI 兼容协议——
// dsh / Codex / Cherry Studio 等不带 conversationId/metadata）：粘性 key 为空时按
// 「末条 user 消息」派生轮级聚合键。同一轮同键，换 user 消息换键。
func TestChatTurnKeyFallbackForSessionlessClient(t *testing.T) {
	first := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"核查部署文档"}]}`)
	second := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"核查部署文档"}]}`)
	if first == "" {
		t.Fatal("X-Conversation-Request-ID missing on sessionless request")
	}
	if first != second {
		t.Errorf("同一轮（末条 user 消息相同）应复用聚合键: %q vs %q", first, second)
	}
	if next := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"继续"}]}`); next == first {
		t.Errorf("换 user 消息应换聚合键（跨轮不混并）: %q", next)
	}
	if len(first) != 32 || !isValidB3Trace(first) {
		t.Errorf("turn-level id %q want 32 hex", first)
	}
}

// TestChatTurnKeyStableAcrossAgentSteps agent 多步（tool call 多轮）：轮内 messages
// 不断追加 assistant/tool 消息，末条 user 消息不变 → 全部复用同一聚合键。
func TestChatTurnKeyStableAcrossAgentSteps(t *testing.T) {
	step1 := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"跑一下"}]}`)
	step2 := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[`+
		`{"role":"user","content":"跑一下"},`+
		`{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"pwsh"}}]},`+
		`{"role":"tool","tool_call_id":"c1","content":"结果"}]}`)
	step3 := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[`+
		`{"role":"user","content":"跑一下"},`+
		`{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"pwsh"}}]},`+
		`{"role":"tool","tool_call_id":"c1","content":"结果"},`+
		`{"role":"assistant","tool_calls":[{"id":"c2","function":{"name":"read"}}]},`+
		`{"role":"tool","tool_call_id":"c2","content":"文件内容"}]}`)
	if step1 == "" {
		t.Fatal("X-Conversation-Request-ID missing on sessionless request")
	}
	if step1 != step2 || step2 != step3 {
		t.Errorf("轮内追加消息不应改变聚合键: step1=%q step2=%q step3=%q", step1, step2, step3)
	}
}

// TestChatSessionKeyBeatsTurnKey 带 conversationId 时聚合键随末条 user 消息变化
// （#170 统一轮级，取代旧「会话键跨轮稳定」契约——对齐官方 CLI）。同轮同会话
// 仍同键（与 TestChatSessionKeyTurnStableWithinTurn 互补：这里覆盖顶层
// conversationId 形态 + 重复文本轮不并轮）。
func TestChatSessionKeyBeatsTurnKey(t *testing.T) {
	a := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"conversationId":"conv-x","messages":[{"role":"user","content":"第一问"}]}`)
	b := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"conversationId":"conv-x","messages":[{"role":"user","content":"第二问"}]}`)
	if a == "" {
		t.Fatal("X-Conversation-Request-ID missing")
	}
	if a == b {
		t.Errorf("会话键路径应轮级换键（随末条 user 变化，#170 对齐官方 CLI）: %q", a)
	}
	// 同轮同会话（重复发同一问）：turnKey 含 user 序号 + 内容签名 → 同键。
	c := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"conversationId":"conv-x","messages":[{"role":"user","content":"第一问"}]}`)
	if a != c {
		t.Errorf("同会话同轮文本应同聚合键: a=%q c=%q", a, c)
	}
}

// TestChatSessionKeyTurnScoped issue #170 RED 锚（R1）：带会话键客户端
// （metadata.conversation_id）同会话两轮（末条 user 不同）→ 出站
// X-Conversation-Request-ID 必须不同——对齐官方桌面 CLI 的轮级语义
// （TraceStartHook 每次 USER_PROMPT_SUBMIT 清空重生成）。旧行为
// RequestIDForKey(sessKey) 会话级跨轮同值 → RED。
func TestChatSessionKeyTurnScoped(t *testing.T) {
	a := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[{"role":"user","content":"第一问"}]}`)
	b := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[`+
		`{"role":"user","content":"第一问"},{"role":"assistant","content":"答"},`+
		`{"role":"user","content":"第二问"}]}`)
	if a == "" || b == "" {
		t.Fatal("X-Conversation-Request-ID missing on session-keyed request")
	}
	if a == b {
		t.Errorf("同会话跨轮应换聚合 ID（轮级，对齐官方 CLI）: a=%q b=%q", a, b)
	}
}

// TestChatSessionKeyTurnStableWithinTurn R2：带会话键客户端轮内 tool-call
// 多步（追加 assistant/tool，末条 user 不变）→ 同 ID（#35 轮内聚合核心语义保留）。
func TestChatSessionKeyTurnStableWithinTurn(t *testing.T) {
	step1 := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[{"role":"user","content":"跑一下"}]}`)
	step2 := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-t"},"messages":[`+
		`{"role":"user","content":"跑一下"},`+
		`{"role":"assistant","tool_calls":[{"id":"c1","function":{"name":"pwsh"}}]},`+
		`{"role":"tool","tool_call_id":"c1","content":"结果"}]}`)
	if step1 == "" {
		t.Fatal("X-Conversation-Request-ID missing")
	}
	if step1 != step2 {
		t.Errorf("轮内追加消息不应改变聚合键（会话键客户端）: step1=%q step2=%q", step1, step2)
	}
}

// TestChatSessionKeyCrossSessionSameTurnText R4：不同会话同轮文本 → 不同 ID
// （sessKey 入复合键防跨会话互撞）。
func TestChatSessionKeyCrossSessionSameTurnText(t *testing.T) {
	a := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-a"},"messages":[{"role":"user","content":"同样的问题"}]}`)
	b := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-b"},"messages":[{"role":"user","content":"同样的问题"}]}`)
	if a == "" || b == "" {
		t.Fatal("X-Conversation-Request-ID missing")
	}
	if a == b {
		t.Errorf("不同会话同轮文本不应共用聚合 ID（会话段入键防撞）: %q", a)
	}
}

// TestChatSessionKeyEmptyTurnKeyFallback R5：turnKey 空态（无 user 消息）
// + sessKey 非空 → 会话级兜底同值（残留空态仍聚合，好于请求级随机）。
func TestChatSessionKeyEmptyTurnKeyFallback(t *testing.T) {
	a := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-e"},"messages":[{"role":"assistant","content":"续"}]}`)
	b := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-e"},"messages":[{"role":"assistant","content":"又续"}]}`)
	if a == "" || b == "" {
		t.Fatal("X-Conversation-Request-ID missing")
	}
	if a != b {
		t.Errorf("turnKey 空态应回落会话级兜底（同 sessKey 同值）: a=%q b=%q", a, b)
	}
	// 且是会话派生（非请求级随机）：同 body 重放同值。
	c := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"metadata":{"conversation_id":"conv-e"},"messages":[{"role":"assistant","content":"续"}]}`)
	if a != c {
		t.Errorf("会话级兜底应为纯派生（同 body 同值）: a=%q c=%q", a, c)
	}
}

// TestChatImageTurnAggregation 纯图 body 两次经 /v1/chat/completions：出站
// X-Conversation-Request-ID 同值（contentSignature 修复 G1——原为请求级随机
// 碎片化；审计 §3.1 探针用例转正）。带文本的下一轮换键（跨轮不混并）。
func TestChatImageTurnAggregation(t *testing.T) {
	imgBody := `{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`
	first := turnRequestIDForBody(t, imgBody)
	second := turnRequestIDForBody(t, imgBody)
	if first == "" {
		t.Fatal("纯图请求应派生轮级聚合 ID（G1：原请求级随机）")
	}
	if first != second {
		t.Errorf("纯图同 body 两次出站应同聚合 ID: %q vs %q", first, second)
	}
	if !isValidB3Trace(first) {
		t.Errorf("轮级 id %q want 32 hex", first)
	}
	// 跨轮：末条 user 换成文本 → 换键。
	next := turnRequestIDForBody(t, `{"model":"glm-5.2","stream":true,"messages":[`+
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]},`+
		`{"role":"assistant","content":"答"},{"role":"user","content":"继续"}]}`)
	if next == "" || next == first {
		t.Errorf("下一轮（末条 user 换文本）应换聚合键: first=%q next=%q", first, next)
	}
}

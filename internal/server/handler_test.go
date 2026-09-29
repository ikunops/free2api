package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/prompt"
	"free2api/internal/redisstore"
	"free2api/internal/session"
	"free2api/internal/upstream"
)

// TestMain 默认关闭聊天表格日志（chatLogEnabled=false），消除 go test 期间的 stdout 噪音。
// 断言表格行输出的测试（logging_test.go 中的 ChatLogs/LogChatRow 系列）用 withChatLog 临时开启。
// 同时把轮转退避基数置 0（backoff.go：测试不等退避；退避界断言测试临时恢复）。
// 另：/v1/models 四级查找链（upstream.model_catalog/modelsdev）是包级单例——
// 复位入口与 dynamicModelsCache 同理，避免跨测试缓存污染与测试末尾异步 goroutine
// 对 models.dev 发起真实网络请求。
func TestMain(m *testing.M) {
	chatLogEnabled = false
	rotateBackoffBase = 0
	upstream.ResetLookupChainForTest()
	os.Exit(m.Run())
}

// resetModelsCache 清空 package 级动态模型缓存（测试隔离：fetchDynamicModels 是全包共享
// 状态，不复位会导致 /v1/models 断言被先前测试的缓存污染——shuffle 下偶发失败）。
func resetModelsCache() {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
	// zcode（智谱）模型目录缓存同步清空：包级共享变量，不清会跨测试相互污染
	// （上一条用 fake 上游的清单残留到下一条不同 fake 的测试）。
	zcodeCatalogCache.Lock()
	zcodeCatalogCache.infos = nil
	zcodeCatalogCache.fetched = time.Time{}
	zcodeCatalogCache.lastFail = time.Time{}
	zcodeCatalogCache.Unlock()
}

const sseOK = "data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"你好\"}}]}\n\n" +
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"created\":1753600000,\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

// newFakeUpstream 返回一个 ChatStream 走 fake 的 upstream.Client。
// fake 依据 Authorization 头决定行为。
func newFakeUpstream(t *testing.T, behavior func(auth string) (status int, body string, isStream bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			authz := r.Header.Get("Authorization")
			status, body, isStream := behavior(authz)
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// bindStore 记录粘性绑定镜像调用（不联网），供 D4 端到端断言绑定收敛到最终成功号。
type bindStore struct {
	redisstore.Noop
	mu     sync.Mutex
	binds  map[string]string
	delCnt int
}

func newBindStore() *bindStore { return &bindStore{binds: map[string]string{}} }

func (b *bindStore) SetBind(key, uid string, ttl time.Duration) {
	b.mu.Lock()
	b.binds[key] = uid
	b.mu.Unlock()
}
func (b *bindStore) DelBind(key string) {
	b.mu.Lock()
	b.delCnt++
	delete(b.binds, key)
	b.mu.Unlock()
}
func (b *bindStore) LoadBinds() map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := map[string]string{}
	for k, v := range b.binds {
		out[k] = v
	}
	return out
}
func (b *bindStore) lastUID(key string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	u, ok := b.binds[key]
	return u, ok
}

// testPoolWith 构建一个所有账号 credits=1000 的池，并注入确定性随机源：
// randInt64N 恒返回 0 → pickWeighted 必选候选集中积分最高者（第一个）。
// 这让依赖"bad 先被选中"的轮转测试（如 TestChatRotatesOnHardCredit）完全确定，
// 不再受加权随机影响而 flake。
func testPoolWith(auths ...*auth.Auth) *pool.Pool {
	p := pool.New("")
	p.SetRandomSource(func(n int64) int64 { return 0 })
	for _, a := range auths {
		p.Add(a)
		p.SetCredits(a.UID, 1000)
	}
	return p
}

// errReader 固定返回 err 的 io.Reader：模拟客户端发送 body 中途断流。
type errReader struct{ err error }

func (r errReader) Read([]byte) (int, error) { return 0, r.err }

// assertJSONErrorCode 断言响应体是 OpenAI 风格 error 信封且 code 精确等于 want。
func assertJSONErrorCode(t *testing.T, body, want string) bool {
	t.Helper()
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, body)
		return false
	}
	if envelope.Error.Code != want {
		t.Errorf("error code=%q want %q body=%s", envelope.Error.Code, want, body)
		return false
	}
	return true
}

// TestChatLargeBodyProceeds 无请求体大小上限（max_body_mb 已移除）：任意大 body 完整
// 读入并正常进入后续处理（打到上游），网关侧不再 413 预拦截。超限类问题交由上游
// 自然返回错误（错误响应经既有分类链路透出，信息量更大），网关不挡上游真实行为。
func TestChatLargeBodyProceeds(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	// 远超旧 8MB 默认上限的合法 JSON body（50MB+）：必须完整读入、照常进上游。
	pad := strings.Repeat("a", 50<<20)
	var b bytes.Buffer
	b.WriteString(`{"model":"glm-5.2","messages":[],"pad":"`)
	b.WriteString(pad)
	b.WriteString(`"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(b.Bytes())))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (oversized body must proceed to upstream, no 413)", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1 (large body must reach upstream)", calls)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Errorf("large body must not penalize account: %+v", st)
	}
}

// TestChatReadBodyErrorReturns400 客户端断流（读 body 出错）→ 400 invalid_request：
// 这是 #41 截断防御语义的保留形态——移除预拦截后，截断只可能来自客户端自己断流，
// 读错误在网关侧就地 400，不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 罚号。
func TestChatReadBodyErrorReturns400(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", errReader{errors.New("client reset")}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400 (read error)", rec.Code)
	}
	assertJSONErrorCode(t, rec.Body.String(), "invalid_request")
	if calls != 0 {
		t.Errorf("upstream must not be called on read error, got %d", calls)
	}
}

// TestChatBadParamsRotatesWithoutPenalty 上游 400 + Unmarshal chat params failed（11101）
// → 该类归 ErrBadParams：不罚账号（无冷却/无禁用/无熔断计数/无 errTotal），但**仍然轮转**
// （换号重试可能命中不同权限的账号）。端到端断言 bad 失败、good 成功、账号完好。
func TestChatBadParamsRotatesWithoutPenalty(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // 确定性源 r=0 → 先选 bad
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	// 账号完好：无冷却、无禁用、无熔断计数、无 errTotal。
	st, _ := p.Status("bad")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 || st.BreakerFails != 0 {
		t.Errorf("ErrBadParams must not penalize account: %+v", st)
	}
}

// TestChatAllBadParams503CarriesUpstreamBody 全部账号都 11101 时 503 文案必须包含
// 上游原始 11101 信息（不再是空洞的 no_healthy_account）。
// 现状即透传 lastErr.Error()（含上游 body），本测试把它锁定为回归。
func TestChatAllBadParams503CarriesUpstreamBody(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF","requestId":"req-xyz-777"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "11101") || !strings.Contains(body, "Unmarshal chat params failed") {
		t.Errorf("503 message should carry upstream 11101 info: %s", body)
	}
	if !strings.Contains(body, "req-xyz-777") {
		t.Errorf("503 message should carry upstream requestId: %s", body)
	}
}

// TestChatPassesThroughUpstreamErrorWithCodeMsgRequestID 验收（error-passthrough）：
// 构造上游错误响应（含 code/msg/requestId，非 429 状态码）经 handler 后，客户端可见的
// error.message 必须等于上游 body 原文（code/msg/requestId 原样保留），而非网关固定文案
// （任务书验收 2：透视可见真实上游错误）。
func TestChatPassesThroughUpstreamErrorWithCodeMsgRequestID(t *testing.T) {
	const raw = `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF","requestId":"req-xyz-777"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, raw, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s (want 503)", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	// error.message 必须等于上游原文（原样，非固定文案/非重建 JSON）。
	if e.Error.Message != raw {
		t.Errorf("message=%q want raw upstream body passthrough %q", e.Error.Message, raw)
	}
	if !strings.Contains(e.Error.Message, "11101") ||
		!strings.Contains(e.Error.Message, "Unmarshal chat params failed") ||
		!strings.Contains(e.Error.Message, "req-xyz-777") {
		t.Errorf("message must preserve code/msg/requestId: %s", e.Error.Message)
	}
	if strings.Contains(e.Error.Message, "no_healthy_account") || strings.Contains(e.Error.Message, "all accounts are temporarily unavailable") {
		t.Errorf("message must NOT be the fixed local scheduling text: %s", e.Error.Message)
	}
}

// TestChatLocalNoAccountKeepsOwnMessage 验收（本地调度类错误）：池中无可用账号（本地
// 调度失败，非上游返回）时，错误保留网关自有文案 no_healthy_account（任务书：内部调度
// 类错误保留自有文案——本地没有上游原文可透传，不编造）。
func TestChatLocalNoAccountKeepsOwnMessage(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		t.Fatal("no upstream call expected for empty pool")
		return 200, "", false
	})
	// 空池：Pick 返回 nil → 本地调度失败，无上游错误可透传。
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "no_healthy_account" {
		t.Errorf("code=%q want no_healthy_account (local scheduling error)", e.Error.Code)
	}
	if !strings.Contains(e.Error.Message, "all accounts are temporarily unavailable") {
		t.Errorf("message=%q want fixed local scheduling message (no upstream to passthrough)", e.Error.Message)
	}
}

func TestChatNonStreamAggregates(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz != "Bearer at1" {
			t.Errorf("auth=%q", authz)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	msg := resp["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "你好" {
		t.Errorf("content=%q", msg["content"])
	}
}

func TestChatStreamPassthrough(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("ct=%q", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "你好") || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("body=%q", body)
	}
}

func TestChatRotatesOnHardCredit(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.Reason == "" {
		t.Errorf("bad account should be cooling: %+v", st)
	}
}

// TestChatSoftCoolsOnRateLimitBody 端到端回归 issue #28：上游用非 429 状态码
// （400 + 限流文案）表达模型侧限流时，该账号必须进入 CoolSoft 冷却，而不是只换号。
// 修复前 Classify 归 ErrClient → applyErrorPolicy 走 default 分支只换号不罚，
// 账号留在可用池里，下一个请求仍会被选中。
func TestChatSoftCoolsOnRateLimitBody(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 400, `{"code":1,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	const soft = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	// 冷却时长取自注入的 SoftCooldown，不依赖真实等待。
	if max := int64(soft / time.Second); st.CoolRemaining <= 0 || st.CoolRemaining > max {
		t.Errorf("cool_remaining_sec=%d want in (0,%d]", st.CoolRemaining, max)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

// TestChatAccountFault11140Disables 账号级授权封禁（11140 request illegal，实测为
// global 账号 auth_forbidden 风控）：软冷却到期也不会自动恢复（需重新 OAuth 登录），
// 到期后重新选号只会再撞 403 浪费轮换——故**硬禁用**（不在池中参与选号），同一请求
// 轮换到下一个号、后续请求直接跳过。
// 修复前 Classify 对 403+request illegal 归 ErrClient → applyErrorPolicy 走 default
// 只换号不罚，坏号留在可用池反复被选中刷风控；软冷却列后来只是临时止血，到期复发。
func TestChatAccountFault11140Disables(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 让 bad 积分更高被先选中（与 TestChatRotatesOnHardCredit 同一确定性手法）。
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	const cool = 90 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: cool})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 坏号只打一次即被禁用并轮换到 good：无无限重试。
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}
	st, _ := p.Status("bad")
	if !st.Disabled {
		t.Fatalf("bad 应被硬禁用（11140 封禁需重登，不可自愈）: %+v", st)
	}
	wantReason := "account banned by upstream (11140 request illegal), re-login required"
	if st.DisabledReason != wantReason {
		t.Errorf("disabled_reason=%q want %q", st.DisabledReason, wantReason)
	}
	if st.Cooling {
		t.Errorf("硬禁用由 disabled 表达，不应叠加冷却状态: %+v", st)
	}

	// 禁用生效：后续请求（冷却早过期）也不再选中 bad（不再刷上游风控）。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("禁用的账号不应被选中: calls=%v", calls)
	}
}

// TestChatAccountFault14017Rotates 配额未激活（14017 trial not activated，实测 global
// 新账号 register 未完成）同样纳入轮换：坏号冷却、轮换到下一号、不再被重复选中。
// 与 11140（硬禁用）区分的关键：14017 属于 register 未完成，完善 register 后可能
// 自动恢复——所以**保持软冷却**，不禁用（禁用会让用户补完 register 后仍无法用）。
func TestChatAccountFault14017Rotates(t *testing.T) {
	calls := map[string]int{}
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls[authz]++
		if authz == "Bearer at-bad" {
			return 429, `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated. Please log out of your current account and log in again to activate it immediately and start your free trial."}}}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	const cool = 45 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: cool})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if calls["Bearer at-bad"] != 1 || calls["Bearer at-good"] != 1 {
		t.Errorf("calls=%v want bad/good 各 1 次", calls)
	}

	// 14017 保持软冷却（软 45s），不得硬禁用——register 完善后自动恢复（区别于 11140）。
	st, _ := p.Status("bad")
	if st.Disabled {
		t.Fatalf("bad 不应被禁用（14017 trial 未激活可能自愈）: %+v", st)
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Errorf("bad 应进入 soft_rate 冷却（14017 软冷却，非禁用）: %+v", st)
	}

	// 冷却生效：同一账号在冷却期内不得再被选中。
	before := calls["Bearer at-bad"]
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec2.Code != 200 {
		t.Fatalf("second code=%d body=%s", rec2.Code, rec2.Body)
	}
	if calls["Bearer at-bad"] != before {
		t.Errorf("冷却中的账号不应再次被选中: calls=%v", calls)
	}
}

// TestApplyErrorPolicyAccountFaultSplit applyErrorPolicy 层直接回归：同一 ErrAccountFault
// 分类下按 msg 分野——"request illegal"(11140) → 硬禁用；"trial"(14017) → 软冷却不禁用。
// 端到端接线由上方 TestChatAccountFault11140Disables / TestChatAccountFault14017Rotates 覆盖。
func TestApplyErrorPolicyAccountFaultSplit(t *testing.T) {
	const why11140 = "account banned by upstream (11140 request illegal), re-login required"

	t.Run("11140 request illegal disables", func(t *testing.T) {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1"})
		h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

		h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, "glm-5.2", nil)
		st, _ := p.Status("u1")
		if !st.Disabled {
			t.Fatalf("11140 应硬禁用: %+v", st)
		}
		if st.DisabledReason != why11140 {
			t.Errorf("disabled_reason=%q want %q", st.DisabledReason, why11140)
		}
		if st.Cooling {
			t.Errorf("11140 禁用不应叠加冷却: %+v", st)
		}
	})

	t.Run("14017 trial stays soft cool", func(t *testing.T) {
		p := pool.New("")
		p.Add(&auth.Auth{UID: "u1"})
		h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

		h.applyErrorPolicy("u1", upstream.ErrAccountFault, `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated"}}}`, "glm-5.2", nil)
		st, _ := p.Status("u1")
		if st.Disabled {
			t.Fatalf("14017 不应禁用: %+v", st)
		}
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Errorf("14017 应 soft_rate 软冷却: %+v", st)
		}
		if st.Reason == "" {
			t.Errorf("14017 冷却 reason 不应为空: %+v", st)
		}
	})
}

// TestApplyErrorPolicySoftRateNoDoubleWhenCooling handler 层回归：无重置时间的 429
// 保留有界冷却，但**冷却中的兜底探测不得翻倍**（这正是旧实现「越重试越冷」的根因，
// 全池被推到 2h 封顶的元凶）。时长断言全部取自注入值，不依赖真实等待。
// Classify→applyErrorPolicy 的接线由 TestChatSoftCoolsOnRateLimitBody 端到端覆盖。
func TestApplyErrorPolicySoftRateNoDoubleWhenCooling(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	// 第 1 次：进入冷却，streak=1，600s（固定基数，无重置时间）。
	h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
	st, _ := p.Status("u1")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("call 1: 应为 soft_rate 冷却: %+v", st)
	}
	if st.SoftStreak != 1 {
		t.Errorf("call 1: soft_streak=%d want 1", st.SoftStreak)
	}
	if st.CoolRemaining < 597 || st.CoolRemaining > 600 {
		t.Errorf("call 1: cool_remaining_sec=%d want ~600", st.CoolRemaining)
	}

	// 冷却中重复触发（兜底探测）→ 不翻倍、不推进 streak。
	before := st.CoolRemaining
	for n := 0; n < 3; n++ {
		h.applyErrorPolicy("u1", upstream.ErrSoftRate, "", "", nil)
	}
	st, _ = p.Status("u1")
	if st.SoftStreak != 1 {
		t.Fatalf("already-cooling probe must not advance soft_streak, got %d", st.SoftStreak)
	}
	if got := st.CoolRemaining; got < before-3 || got > before {
		t.Errorf("already-cooling probe must not extend, cool_remaining_sec=%d want ~%d", got, before)
	}
}

// TestApplyErrorPolicySoftRateResetTime11140 handler 层回归：非 6004 形态的限流
// （code 11140 "The model provider is rate-limiting requests." +「将在 … 重置」）同样
// 必须**精确对齐**到上游重置墙钟，而不是走 600s 基数/有界退避，更不得 softStreak
// 指数堆加。与 6004 的分野：11140 走账号级 CooldownSoftRate（写 until、不计模型、
// 不产生切模型豁免），6004 走模型级 CooldownSoftForModel（写 modelCooldowns）。
func TestApplyErrorPolicySoftRateResetTime11140(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second})

	reset := time.Now().Add(35 * time.Minute) // 远超 soft_rate 基数，验证确实对齐穷钟而非固定 600s
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	body := `{"code":11140,"msg":"The model provider is rate-limiting requests. 将在 ` + ts + ` UTC+8 重置"}`

	h.applyErrorPolicy("u1", upstream.ErrSoftRate, body, "glm-5.3", nil)
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("应为账号级 soft_rate 冷却: %+v", st)
	}
	if st.SoftStreak != 0 {
		t.Errorf("带重置时间的 429 绝不 softStreak 堆加，soft_streak=%d want 0", st.SoftStreak)
	}
	if d := st.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("账号级 until=%v want ~reset=%v（精确对齐，无退避）", st.Until, reset)
	}
	// 非 6004 走账号级，不写模型级台账（不存在切模型豁免）。
	if len(st.RateLimitedModels) != 0 {
		t.Errorf("11140 账号级限流不应产生模型级台账: %+v", st.RateLimitedModels)
	}
}

// TestApplyErrorPolicyNotFoundUsesFixedBase 404 分流：偶发上游 404 的冷却基数固定 60s
// （notFoundCooldown），不取 soft_rate 的 600s 基数，也不受其配置值影响，且不参与
// softStreak 指数退避（404 是偶发路径缺失，不是限流信号，不该因 404 升级惩罚，
// 也不该因 404 与 429 共用 softStreak 导致催促升级）。
// （重构后 404 仍走 Cooldown(CoolSoft) 固定时长分支，softStreak 不再被 404 推进。）
func TestApplyErrorPolicyNotFoundUsesFixedBase(t *testing.T) {
	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p, SoftCooldown: 20 * time.Minute}) // soft_rate 配得很大，验证 404 不受其影响

	notFoundSec := int64(notFoundCooldown / time.Second)
	for i := 0; i < 3; i++ {
		h.applyErrorPolicy("u1", upstream.ErrNotFound, "", "", nil)
		st, _ := p.Status("u1")
		if !st.Cooling || st.CoolKind != "soft_rate" {
			t.Fatalf("call %d: 应为 soft 冷却: %+v", i+1, st)
		}
		if st.SoftStreak != 0 {
			t.Errorf("call %d: 404 must not advance soft_streak, got %d", i+1, st.SoftStreak)
		}
		// 基数取自 notFoundCooldown（60s）而非注入的 soft_rate（20m），且固定不翻倍
		// （冷却中重复 404 是兜底探测，不得把 404 冷却也越堆越厚）。
		if st.CoolRemaining < notFoundSec-3 || st.CoolRemaining > notFoundSec {
			t.Errorf("call %d: 404 cool_remaining_sec=%d want ~%d（固定基数，非 soft_rate，不翻倍）",
				i+1, st.CoolRemaining, notFoundSec)
		}
	}
}

// TestNewHandlerSoftCooldownDefault 端到端：未注入 SoftCooldown 时基数回落到 600s（原为 60s）。
func TestNewHandlerSoftCooldownDefault(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":1,"msg":"rate limit"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 SoftCooldown

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad 应进入 soft_rate 冷却: %+v", st)
	}
	if st.CoolRemaining <= 599 || st.CoolRemaining > 600 {
		t.Errorf("default soft cooldown cool_remaining_sec=%d want 600", st.CoolRemaining)
	}
}

// TestChatStickyFollowsFinalSuccess 端到端验证 D4：粘性号失败换号成功后，会话绑定收敛到成功号。
func TestChatStickyFollowsFinalSuccess(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// 先把会话预绑定到 bad（模拟历史粘性），bad 失败、good 成功 → 绑定应切到 good。
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 500, `{"code":500}`, false
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	// 预绑定：sess.Bind("conv-1", "bad")，然后请求体带同 conversation_id。
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 绑定必须收敛到最终成功的 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should follow final success to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
}

// TestChatStickySuccessKeepsBinding 粘性号直接成功 → 绑定不变（仍为该号）。
func TestChatStickySuccessKeepsBinding(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"good"} },
	})
	p := testPoolWith(&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess, SoftCooldown: time.Minute})
	sess.Bind("conv-1", "good")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("binding should stay good after success, got %s ok=%v", uid, ok)
	}
}

// TestChatStickyFullFallsBackToRotation 端到端验证 C3 语义：粘性号满载不可用时，
// 请求在同一轮内解绑并回落普通轮换选中健康账号，绑定收敛到最终成功号——而非空耗一轮。
func TestChatStickyFullFallsBackToRotation(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:       time.Minute,
		Store:     st,
		Available: func() []string { return []string{"bad", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// bad 占满唯一在途名额：粘性命中校验将返回 nil（healthy 但 inFlight 满）→ 解绑 + 回落轮换。
	p.SetMaxInFlight(1)
	p.Acquire("bad")

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "bad")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 满载粘性号被解绑，绑定收敛到最终成功号 good。
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("sticky binding should fall back to good, got %s ok=%v (binds=%v)", uid, ok, st.binds)
	}
	p.Release("bad")
}

func TestChatHardCreditCooldownUntilNextDay4AM(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 402, `{"code":1,"msg":"余额不足"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000) // bad 积分高，确定性源 → 先被选中
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("bad should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" {
		t.Errorf("reason=%q", st.Reason)
	}
	// 硬信贷冷却必须是次日 04:00，而不是固定 12h/配置时长。
	if st.Until.Hour() != 4 {
		t.Errorf("until hour=%d want 4 (next-day 04:00)", st.Until.Hour())
	}
	// 距次日 04:00 最长 28h（凌晨 00:00~04:00 间运行时 now→次日 04:00 跨度 > 24h，属正常）。
	if d := time.Until(st.Until); d <= 0 || d > 28*time.Hour {
		t.Errorf("until %v not within (0,28h]: %v", st.Until, d)
	}
	// 立即换号成功：good 被选中。
	stGood, _ := p.Status("good")
	if stGood.Cooling || stGood.Disabled {
		t.Errorf("good should stay healthy: %+v", stGood)
	}
}

func TestChat429Code14018UsesHardCreditCooldown(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-bad" {
			return 429, `{"code":14018,"msg":"Credits exhausted"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	st, ok := p.Status("bad")
	if !ok || !st.Cooling {
		t.Fatalf("14018 account should be cooling: %+v ok=%v", st, ok)
	}
	if st.Reason != "余额不足" || st.Until.Hour() != 4 {
		t.Fatalf("14018 should use hard-credit cooldown, got reason=%q until=%v", st.Reason, st.Until)
	}
	if got := p.Pick(""); got == nil || got.UID != "good" {
		t.Fatalf("hard-cooled 14018 account must not be fallback-picked, got %+v", got)
	}
}

// TestChat6004ModelResetCoolsToParsedTime 端到端回归 issue #31：上游 429 + code 6004
// +「将在 … 重置」→ 冷却 until 精确等于解析时间（而非 600s 固定基数/指数退避），
// 且记录触发模型 → 同模型请求仍被冷却、切模型请求按豁免可选。
func TestChat6004ModelResetCoolsToParsedTime(t *testing.T) {
	// 用未来 5 分钟的重置时间（wall-clock）构造上游响应。
	reset := time.Now().Add(5 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	// 隔离对 breaker 的干扰：熔断阈值默认 3，一次失败不触发。
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after rotate to good)", rec.Code, rec.Body)
	}
	// bad 已进入 6004 模型级冷却：模型级台账含 glm-5.3 + 该模型独立截止 ≈ reset。
	st, _ := p.Status("bad")
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.3" {
		t.Fatalf("bad should have glm-5.3 model limit ledger: %+v", st)
	}
	if d := st.RateLimitedModels[0].Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("model until=%v want ~reset=%v (diff %v)", st.RateLimitedModels[0].Until, reset, d)
	}
	// 6004 不写账号级 until（模型级独立冷却）。
	if !st.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 不写账号级 until）", st.Until)
	}
	// 记录触发模型（bad 池内 private 字段需经 Status 不可见，改用行为断言）：
	// 同模型 glm-5.3 的请求不应选中 bad（仍冷却）；
	// 不同模型 hy3-x 的请求应豁免冷却选中 bad（最高分）。
	p.SetRandomSource(func(n int64) int64 { return 0 })
	same := p.PickExcludingForRealm(nil, "glm-5.3", "")
	if same == nil || same.UID != "good" {
		t.Fatalf("same-model pick should skip bad (still cooling), got %+v", same)
	}
	diff := p.PickExcludingForRealm(nil, "hy3-x", "")
	if diff == nil || diff.UID != "bad" {
		t.Fatalf("different-model pick should bypass bad soft cooling, got %+v", diff)
	}
}

// TestChat6004WithoutResetFallsBackToBackoff 6004 无时间文案 → 退回 600s 基数软冷却
// （现状不变）。
func TestChat6004WithoutResetFallsBackToBackoff(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		if authz == "Bearer at-bad" {
			return 429, `{"code":6004,"msg":"model usage limit exceeded"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad", AccessToken: "at-bad", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	p.SetCredits("bad", 2000)
	p.SetCredits("good", 1000)
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: time.Minute})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.3","messages":[]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("bad")
	if !st.Cooling || st.CoolKind != "soft_rate" {
		t.Fatalf("bad should be soft cooling: %+v", st)
	}
	// 冷却时长 = 注入 soft 基数(60s)，非解析时间（无重置文案）。
	if st.CoolRemaining <= 0 || st.CoolRemaining > 60 {
		t.Errorf("cool_remaining_sec=%d want ~60 (soft base, not parsed)", st.CoolRemaining)
	}
}

func TestChatAllUnavailableReturns503(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 402, `{"code":1,"msg":"余额不足"}`, false
	})
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999}),
		Upstream: up,
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	json.Unmarshal(rec.Body.Bytes(), &e)
	if e["error"] == nil {
		t.Errorf("want error envelope: %s", rec.Body)
	}
}

func TestChatSessionDeadDisables(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 401, `{"code":12153,"msg":"Offline user session not found"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Errorf("code=%d", rec.Code)
	}
	st, _ := p.Status("u1")
	if !st.Disabled {
		t.Errorf("account should be disabled: %+v", st)
	}
}

func TestChatTransportErrorDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return nil, errors.New("connection refused")
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
	// 传输错误不喂熔断计数：一次 transport error 不应累计 errTotal 也不应熔断。
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("transport error should not penalize account: %+v", st)
	}
}

func TestChatHTTP5xxPenalizes(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：一次 5xx 即触发熔断（连续失败语义并入熔断器）。
	p.SetBreaker(1, time.Hour, time.Hour)
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `{"code":500}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if !st.Cooling {
		t.Fatalf("http 5xx should trip breaker (cooling) with threshold=1: %+v", st)
	}
}

func TestChatHTTP4xxClientDoesNotPenalize(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":400,"msg":"bad request"}`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != 503 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, _ := p.Status("u1")
	if st.Cooling || st.ErrTotal != 0 {
		t.Fatalf("generic 4xx should not penalize account: %+v", st)
	}
}

// TestModelsEndpoint 纯动态：无健康上游（Upstream 指向不可达 base）→ 空列表 + 200。
func TestModelsEndpoint(t *testing.T) {
	resetModelsCache()
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}), Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["object"] != "list" {
		t.Errorf("object=%v", resp["object"])
	}
	data := resp["data"].([]any)
	if len(data) != 0 {
		t.Errorf("models count=%d want 0 (pure dynamic, fetch failed)", len(data))
	}
}

func TestModelsDynamic(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游返回动态模型（含 agents + maxInputTokens/maxOutputTokens）
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[{"id":"dyn-model-a","maxInputTokens":65536,"maxOutputTokens":8192},{"id":"dyn-model-b","maxInputTokens":131072,"maxOutputTokens":16384},{"id":"glm-9.9","maxInputTokens":262144,"maxOutputTokens":32768}],"agents":[{"name":"cli","models":["dyn-model-a","dyn-model-b","glm-9.9"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	if len(data) != 3 {
		t.Fatalf("want 3 dynamic models, got %d: %v", len(data), data)
	}
	ids := map[string]bool{}
	for _, m := range data {
		ids[m.(map[string]any)["id"].(string)] = true
	}
	if !ids["cn:dyn-model-a"] || !ids["cn:glm-9.9"] {
		t.Errorf("dynamic ids (cn-prefixed) missing: %v", ids)
	}

	// 断言字段映射：maxInputTokens → context_length，maxOutputTokens → max_output_tokens
	for _, m := range data {
		mm := m.(map[string]any)
		switch mm["id"] {
		case "cn:dyn-model-a":
			if mm["context_length"].(float64) != 65536 {
				t.Errorf("cn:dyn-model-a context_length=%v want 65536", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 8192 {
				t.Errorf("cn:dyn-model-a max_output_tokens=%v want 8192", mm["max_output_tokens"])
			}
		case "cn:glm-9.9":
			if mm["context_length"].(float64) != 262144 {
				t.Errorf("cn:glm-9.9 context_length=%v want 262144", mm["context_length"])
			}
			if mm["max_output_tokens"].(float64) != 32768 {
				t.Errorf("cn:glm-9.9 max_output_tokens=%v want 32768", mm["max_output_tokens"])
			}
		}
	}

	// 第二次调用走缓存（把上游关掉也成功）
	dynamicModelsCache.RLock()
	cached := len(dynamicModelsCache.ids)
	dynamicModelsCache.RUnlock()
	if cached != 3 {
		t.Errorf("cache not populated: %d", cached)
	}
}

// TestModelsDynamicFetchFailEmpty 纯动态：上游 500 → 空列表（无静态回退）。
func TestModelsDynamicFetchFailEmpty(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// 假上游 500
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp map[string]any
	json.Unmarshal(rec.Body.Bytes(), &resp)
	data := resp["data"].([]any)
	// 纯动态：失败 → 空
	if len(data) != 0 {
		t.Errorf("fetch failure should yield empty list (no static fallback): %d", len(data))
	}
}

// TestModelsFetchFailureDoesNotPenalizeAccount models 拉取失败与 chat 熔断解耦
// （P1-6/发现 6）：/v1/models 的动态拉取失败（Billing/Models 端点网络抖动）不喂
// NoteError——该熔断器保护的是 chat 选号，models 拉取失败 ≠ 账号 chat 不可用，
// 跨界惩罚会让上游 models 端点偶发 5xx 把好号提前打进熔断。失败只进 5min 负缓存。
func TestModelsFetchFailureDoesNotPenalizeAccount(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	p.SetBreaker(1, time.Hour, time.Hour) // 熔断阈值 1：若误喂 NoteError 一次即熔断
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 500, `boom`, false
	})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d (empty list on failure)", rec.Code)
	}
	st, _ := p.Status("u1")
	// 熔断器零观测：不喂 fails（breaker_fails=0）、不熔断（Cooling=false）、
	// 不记 last_err/err_total。负缓存是唯一的失败退避（另测）。
	if st.BreakerFails != 0 {
		t.Errorf("models fetch failure should not feed breaker: breaker_fails=%d", st.BreakerFails)
	}
	if st.Cooling {
		t.Errorf("models fetch failure should not trip breaker with threshold=1: %+v", st)
	}
	if st.ErrTotal != 0 {
		t.Errorf("models fetch failure should not record err_total: %d", st.ErrTotal)
	}
	// 负缓存仍然生效：拉取失败进 lastFail（5min 冷却）。
	dynamicModelsCache.RLock()
	failTs := dynamicModelsCache.lastFail
	dynamicModelsCache.RUnlock()
	if failTs.IsZero() {
		t.Error("negative cache (lastFail) should be set on fetch failure")
	}
}

func TestModelsNegativeCacheOnFetchFailure(t *testing.T) {
	// 清缓存
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = nil
	dynamicModelsCache.fetched = time.Time{}
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()

	// v3-config-merge：单次 fetch 并发打企业端点 + /v3/config，计数须并发安全。
	var mu sync.Mutex
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		mu.Lock()
		calls++
		mu.Unlock()
		return 500, `boom`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})

	// 连续 3 次请求，上游持续 500 → 只应触发 1 次 fetch（负缓存生效），
	// 其余直接空列表（纯动态，仍返回 200）。
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
		if rec.Code != 200 {
			t.Fatalf("req %d: code=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	if calls != 2 {
		// v3-config-merge：单次 fetch 并发打企业端点 + /v3/config 两路 = 2 个上游请求。
		t.Errorf("want 2 upstream calls (single fetch, enterprise + v3), got %d", calls)
	}

	// 冷却期结束（把失败时间戳拨回 10 分钟前）→ 应重新 fetch。
	dynamicModelsCache.Lock()
	dynamicModelsCache.lastFail = time.Now().Add(-10 * time.Minute)
	dynamicModelsCache.Unlock()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("after cooldown: code=%d", rec.Code)
	}
	if calls != 4 {
		// 第二次 fetch 同样两路并发：累计 4 个上游请求。
		t.Errorf("want 4 upstream calls after cooldown (2nd fetch, enterprise + v3), got %d", calls)
	}
}

// TestModelsDynamicZeroContextFallback 动态模型缺 maxInputTokens → context_length 走
// 知识表/1M 兜底（context_catalog 三级查找），其余真实值不得被覆盖
// （issue 提醒：不能全表统一抹平真实 ContextLength）。
func TestModelsDynamicZeroContextFallback(t *testing.T) {
	resetModelsCache()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-zero-ctx","maxInputTokens":0,"maxOutputTokens":4096},
			{"id":"dyn-real-ctx","maxInputTokens":262144,"maxOutputTokens":32768}
		],"agents":[{"name":"cli","models":["dyn-zero-ctx","dyn-real-ctx"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := jsonUnmarshal(rec.Body.String(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	zero := map[string]any{}
	real := map[string]any{}
	for _, m := range resp.Data {
		switch m["id"] {
		case "cn:dyn-zero-ctx":
			zero = m
		case "cn:dyn-real-ctx":
			real = m
		}
	}
	// 缺 maxInputTokens → 知识表/1M 兜底（其余字段保持真实）。
	// dyn-zero-ctx 不在知识表 → 1M；绝不再透出假 131072。
	if zc, _ := zero["context_length"].(float64); zc != 1000000 {
		t.Errorf("zero-ctx context_length=%v want 1000000 (unknown → 1M fallback)", zc)
	}
	if zo, _ := zero["max_output_tokens"].(float64); zo != 4096 {
		t.Errorf("zero-ctx max_output_tokens=%v want 4096 (real value preserved)", zo)
	}
	// 有真实 maxInputTokens → 必须用真实值，不得被兜底抹平。
	if rc, _ := real["context_length"].(float64); rc != 262144 {
		t.Errorf("real-ctx context_length=%v want 262144 (real value must win)", rc)
	}
	if ro, _ := real["max_output_tokens"].(float64); ro != 32768 {
		t.Errorf("real-ctx max_output_tokens=%v want 32768", ro)
	}
}

// TestModelsDynamicPreservesBodies 动态成功时产出全字段条目（连 headers 字段一并保留原样）；
// 失败路径产出空列表（由 TestModelsDynamicFetchFailEmpty 覆盖）。
func TestModelsDynamicPreservesBodies(t *testing.T) {
	resetModelsCache()

	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"dyn-only","maxInputTokens":200000,"maxOutputTokens":20000,"name":"Dyn Only"}
		],"agents":[{"name":"cli","models":["dyn-only"]}]}}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))

	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := jsonUnmarshal(rec.Body.String(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Data) == 0 {
		t.Fatal("no models returned")
	}
	// 动态优先：动态模型出现即证明未走静态 fallback。
	found := false
	for _, m := range resp.Data {
		if m["id"] == "cn:dyn-only" {
			found = true
			if cl, _ := m["context_length"].(float64); cl != 200000 {
				t.Errorf("cn:dyn-only context_length=%v want 200000 (dynamic value)", cl)
			}
		}
	}
	if !found {
		t.Errorf("/v1/models must include cn:dyn-only when dynamic fetch succeeds: %v", resp.Data)
	}

}

func TestAPIKeyAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	// 无 key
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("no key: code=%d", rec.Code)
	}
	// 错 key
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer wrong")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Errorf("wrong key: code=%d", rec.Code)
	}
	// 对 key（请求会继续打到上游，但此处上游 client 会失败 —— 只要不是 401 就行）
	req = httptest.NewRequest("GET", "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("right key: code=%d", rec.Code)
	}
}

// TestAPIKeyAuthConstantTime Bearer 比较的边界回归（P2-8，发现 7）：
// 正确 key 通过；错误/空/前缀相同但长度不同一律 401。
// 常量时间属性（subtle.ConstantTimeCompare）本身无法用单元测试观测，
// 此处锁的是行为等价——换实现前后四条断言必须同样成立。
func TestAPIKeyAuthConstantTime(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	cases := []struct {
		name string
		bear string // 完整 Authorization 头（不含 "Bearer " 前缀则按原样发）
		want int
	}{
		{"correct key", "secret", 200},
		{"wrong key", "wrong", 401},
		{"empty key", "", 401},
		{"same prefix longer", "secret-extra", 401},
		{"same prefix shorter", "sec", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/v1/models", nil)
			req.Header.Set("Authorization", "Bearer "+c.bear)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			// 正确 key 会继续打到上游（GET /v1/models 走静态表 → 200）；
			// 其余必须被 401 挡在鉴权层。
			if rec.Code != c.want {
				t.Errorf("Bearer %q: code=%d want %d", c.bear, rec.Code, c.want)
			}
		})
	}
}

func TestStatusEndpoint(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetCredits("u1", 42)
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"uid":"u1"`) || !strings.Contains(body, `"credits":42`) {
		t.Errorf("body=%s", body)
	}
	if strings.Contains(body, "AccessToken") || strings.Contains(body, `"at"`) {
		t.Error("token leaked in status output")
	}
	// Phase 3 汇总字段。
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["total"] != float64(1) || statusBody["healthy"] != float64(1) ||
		statusBody["cooling"] != float64(0) || statusBody["disabled"] != float64(0) ||
		statusBody["in_flight_full"] != float64(0) {
		t.Errorf("summary=%v want total=1 healthy=1 cooling=0 disabled=0 in_flight_full=0", statusBody)
	}
	// Phase v3：池级 sticky_sessions + redis_mode。
	if statusBody["sticky_sessions"] != float64(0) {
		t.Errorf("sticky_sessions=%v want 0", statusBody["sticky_sessions"])
	}
	if statusBody["redis_mode"] != "noop" {
		t.Errorf("redis_mode=%v want noop", statusBody["redis_mode"])
	}
}

// TestStatusInFlightFull /status 透出满载计数：healthy 且占满在途的账号数。
func TestStatusInFlightFull(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "full", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "free", AccessToken: "at", ExpiresAt: 9999999999},
	)
	p.SetMaxInFlight(1)
	p.Acquire("full")
	defer p.Release("full")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var statusBody map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &statusBody); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if statusBody["in_flight_full"] != float64(1) {
		t.Errorf("in_flight_full=%v want 1", statusBody["in_flight_full"])
	}
	if statusBody["healthy"] != float64(2) {
		t.Errorf("healthy=%v want 2 (full is still healthy by state-machine semantics)", statusBody["healthy"])
	}
}

func TestStatusPortraitFields(t *testing.T) {
	// Phase 3：/status 单账号需返回健康画像字段。
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	p.NoteSuccess("u1")
	p.NoteSuccess("u1")
	p.NoteError("u1") // 记录 last_err + err_total（累计，不冷却）
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Accounts []pool.Status `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if len(body.Accounts) != 1 {
		t.Fatalf("accounts=%d", len(body.Accounts))
	}
	st := body.Accounts[0]
	if !st.Cooling || st.CoolKind != "soft_rate" || st.CoolRemaining <= 0 {
		t.Errorf("cooling portrait=%+v", st)
	}
	if st.SuccessCount != 2 {
		t.Errorf("success_count=%d want 2", st.SuccessCount)
	}
	if st.ErrTotal != 1 {
		t.Errorf("err_total=%d want 1", st.ErrTotal)
	}
	if st.LastSuccessTime.IsZero() {
		t.Error("last_success should be set")
	}
	if st.LastErrTime.IsZero() {
		t.Error("last_err should be set")
	}
}

// TestStatusRateLimitedModelsLedger end-to-end（issue #36）：上游 429 6004 带
// 「将在 … 重置」→ 账号 modelCooldowns 被写入 → /status accounts 输出该模型的
// 限额台账（rate_limited_models[].model + reset_at + until）。
func TestStatusRateLimitedModelsLedger(t *testing.T) {
	reset := time.Now().Add(35 * time.Minute)
	ts := reset.In(upstream.SoftRateResetLoc()).Format("2006-01-02 15:04:05")
	body := `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, body, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at", ExpiresAt: 9999999999},
	)
	const soft = 600 * time.Second
	h := NewHandler(Config{Pool: p, Upstream: up, SoftCooldown: soft})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	// 双号都被 6004 → 全部换完仍限流。末端错误已规范化（冷启动重构）：限流语义
	// 映射为 429 rate_limit_exceeded（不再原样 503 透传上游原文），换号过程已把 u1 冷却。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429 body=%s", rec.Code, rec.Body)
	}
	var e map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if errObj, _ := e["error"].(map[string]any); errObj == nil || errObj["code"] != "rate_limit_exceeded" {
		t.Fatalf("want rate_limit_exceeded envelope: %s", rec.Body)
	}
	// u1 被选中过（已冷却 + 有台账）。
	st, ok := p.Status("u1")
	if !ok {
		t.Fatal("u1 missing")
	}
	if len(st.RateLimitedModels) != 1 || st.RateLimitedModels[0].Model != "glm-5.2" {
		t.Fatalf("u1 应进入 6004 模型级软冷却（glm-5.2 台账）: %+v", st)
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
	var su1 *pool.Status
	for i := range sbody.Accounts {
		if sbody.Accounts[i].UID == "u1" {
			su1 = &sbody.Accounts[i]
			break
		}
	}
	if su1 == nil {
		t.Fatal("u1 不在 /status accounts 里")
	}
	if len(su1.RateLimitedModels) != 1 {
		t.Fatalf("u1 rate_limited_models=%v want 1 行", su1.RateLimitedModels)
	}
	row := su1.RateLimitedModels[0]
	if row.Model != "glm-5.2" {
		t.Errorf("model=%q want glm-5.2", row.Model)
	}
	if d := row.ResetAt.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("reset_at=%v want ~35m 后=%v", row.ResetAt, reset)
	}
	// 6004 模型级冷却不写账号级 until：Status.Until 应为零值，模型截止在台账行里。
	if !su1.Until.IsZero() {
		t.Errorf("Status.Until=%v 应为零值（6004 不写账号级 until）", su1.Until)
	}
	if d := row.Until.Sub(reset); d < -time.Second || d > time.Second {
		t.Errorf("until=%v want ~35m 后=%v", row.Until, reset)
	}
	// 未命中测试账号（u2 也被限流）也会带台账——但只断言 u1（被选中的号）即验证端到端。
}

// TestChatAllRateLimitedPassesThroughUpstream 端到端（error-passthrough）：全部账号都命中
// 上游限流（400 + 速限文案，非 429 状态码）时，末端错误映射为 OpenAI 风格
// 429 rate_limit_exceeded（限流语义保持），但 error.message **透传上游 body 原文**——
// code/msg/requestId 原样保留，客户端必须能看到真实上游错误以便排查，不再规范化成
// 固定文案（任务书：上游错误码/账号语义允许泄露给客户端）。
func TestChatAllRateLimitedPassesThroughUpstream(t *testing.T) {
	const raw = `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again.","requestId":"req-abc-123"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, raw, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d body=%s (want 429 rate_limit_exceeded)", rec.Code, rec.Body)
	}
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Code != "rate_limit_exceeded" {
		t.Errorf("code=%q want rate_limit_exceeded", e.Error.Code)
	}
	// error.message 必须等于上游原文（含 code/msg/requestId），非固定文案。
	if e.Error.Message != raw {
		t.Errorf("message=%q want passthrough of upstream raw body %q", e.Error.Message, raw)
	}
	if !strings.Contains(e.Error.Message, "req-abc-123") {
		t.Errorf("requestId must be preserved in message: %s", e.Error.Message)
	}
	// 两个号都被限流冷却（换号过程完整跑完仍无健康号）。
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := p.Status(uid); !st.Cooling || st.CoolKind != "soft_rate" {
			t.Errorf("%s should be soft_rate cooling: %+v", uid, st)
		}
	}
}

// TestHealthzEmptyPool 空池（healthy=0）→ 503，表示暂不可服务。
func TestHealthzEmptyPool(t *testing.T) {
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("code=%d want 503 (healthy=0)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(0) {
		t.Errorf("healthz json=%v", resp)
	}
}

// TestHealthz503WhenNoHealthy 所有账号禁用/冷却 → 503。
func TestHealthz503WhenNoHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.Disable("u1", "session dead")
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("ct=%q want json", ct)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(0) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=0 total=1", resp)
	}
}

// TestHealthz503WhenAllInFlightFull 全部账号 healthy 但都占满在途 → 503（与 chat 同口径），
// 且 healthy 语义未变（仍为 1）：满载不是状态机健康维度的变化，是探活口径单独叠加。
func TestHealthz503WhenAllInFlightFull(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.SetMaxInFlight(1)
	p.Acquire("u1") // 占满唯一在途名额
	defer p.Release("u1")

	if p.ServableNow() {
		t.Fatal("servable should be false when the only healthy account is in-flight full")
	}
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code=%d want 503 (healthy but all in-flight full)", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	// healthy 语义未变：账号仍是 healthy（只占满在途，非冷却/禁用）。
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1 (healthy semantics unchanged)", resp)
	}
}

// TestHealthz200WhenAllModelExempt 全部账号处于 6004 单模型软冷却（对其他模型仍可选）
// → /healthz 必须 200，与 chat 的模型级豁免（切模型立即可用）同口径。
// 回归：此前 ServableNow 只看账号级 healthy，"全号被 v4.1 限流但 glm 可用"时误报 503。
func TestHealthz200WhenAllModelExempt(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	p.CooldownSoftForModel("u1", time.Minute, time.Now().Add(5*time.Minute), "glm-5.3", "429 rate limit")
	// 账号级 healthy 已为 0（冷却中），但模型豁免使 chat 对非 glm 请求仍可达。
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (model-exempt account keeps pool servable)", rec.Code)
	}
}

// TestHealthz200WhenHealthy 有健康账号 → 200。
func TestHealthz200WithHealthy(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp["healthy"] != float64(1) || resp["total"] != float64(1) {
		t.Errorf("healthz json=%v want healthy=1 total=1", resp)
	}
}

// TestHealthzServiceIdentity /healthz 无论 200 还是 503 都必须带网关身份标识
// （响应体 service 字段 + X-Service 头）：宿主探测打到同端口的旧服务/其他服务时，
// 对方即使返回 2xx 也不带本标识，宿主据此判"假成功"。
func TestHealthzServiceIdentity(t *testing.T) {
	cases := []struct {
		name     string
		setup    func(*pool.Pool)
		wantCode int
	}{
		{"healthy", func(*pool.Pool) {}, http.StatusOK},
		{"unhealthy", func(p *pool.Pool) { p.Disable("u1", "session dead") }, http.StatusServiceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999})
			tc.setup(p)
			h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
			if rec.Code != tc.wantCode {
				t.Fatalf("code=%d want %d", rec.Code, tc.wantCode)
			}
			if got := rec.Header().Get("X-Service"); got != ServiceName {
				t.Errorf("X-Service=%q want %q", got, ServiceName)
			}
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
			}
			if resp["service"] != ServiceName {
				t.Errorf("service=%v want %q", resp["service"], ServiceName)
			}
		})
	}
}

// TestHealthzServiceIdentityWithoutAuth /healthz 保持无鉴权（负载均衡友好）：
// 配了 api_key 也不要求 Bearer，身份字段照常返回。
func TestHealthzServiceIdentityWithoutAuth(t *testing.T) {
	h := NewHandler(Config{
		Pool:     testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at", ExpiresAt: 9999999999}),
		Upstream: upstream.New(),
		APIKey:   "secret",
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz must stay unauthenticated: code=%d", rec.Code)
	}
	if got := rec.Header().Get("X-Service"); got != ServiceName {
		t.Errorf("X-Service=%q want %q", got, ServiceName)
	}
}

func TestStatusRequiresAuth(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1", Nickname: "nick", AccessToken: "at", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: upstream.New(), APIKey: "secret"})

	// 无 token → 401
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 401 {
		t.Errorf("no token: code=%d", rec.Code)
	}

	// 带 token → 200
	rec = httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Errorf("with token: code=%d", rec.Code)
	}

	// /healthz 无鉴权仍 200
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Errorf("healthz: code=%d", rec.Code)
	}
}

// TestContentBlockedTriggersDegradedRetry passthrough 模式下首请求 400（11128 文案）
// → 降级重试（Degraded）→ 200，客户端无感。验证第二次出站 body 为 Degraded。
func TestContentBlockedTriggersDegradedRetry(t *testing.T) {
	// 记录每次出站请求体，断言第二次为 Degraded 文本。
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			// 首次（body 含原始 system）返回 400 内容拦截；后续返回 200。
			if len(bodies) == 1 {
				return &http.Response{
					StatusCode: 400,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
				}, nil
			}
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls (first 400 + retry), got %d", len(bodies))
	}
	// 第二次出站 body 的 messages 头部 system 内容应为 Degraded 文本。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("second body should contain Degraded prompt: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("second body should not contain original system: %s", bodies[1])
	}
}

// TestContentBlockedStickyDegraded 降级后新请求直达 Degraded（不再先撞 400）。
func TestContentBlockedStickyDegraded(t *testing.T) {
	var firstCall bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if !firstCall {
			firstCall = true
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	// 首请求触发降级 → 200。
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"system","content":"x"},{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("first req code=%d", rec.Code)
	}
	// 降级粘性：新请求 Active()=true，body 已被 Rewrite(Degraded)，上游首字节即 200。
	// 但 fake 上游只对 firstCall 返回 400，之后都 200，无法区分"直达"与"重试"。
	// 用 degrade.Active() 直接断言粘性生效。
	if !h.degrade.Active() {
		t.Fatal("degrade should be active after trigger")
	}
}

// TestContentBlockedCustomModeDoesNotDegrade custom 模式不触发降级重试
// （custom 已用自有提示词替换，不应再有 system 来源误报；若仍拦则回 400 content_blocked）。
func TestContentBlockedCustomModeDoesNotDegrade(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"old"},{"role":"user","content":"hi"}]}`)))
	// custom 模式下内容拦截直接回 400 content_blocked，不降级重试、不轮转、不暴露账号语义。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 (custom does not degrade)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"content_blocked"`) {
		t.Errorf("want content_blocked code: %s", body)
	}
	// error-passthrough：message 透传上游 body 原文（code/msg/requestId 原样），非网关固定文案。
	if !strings.Contains(body, "blocked by security policy") || !strings.Contains(body, "11128") {
		t.Errorf("want upstream raw body passthrough in message: %s", body)
	}
	if h.degrade.Active() {
		t.Error("degrade should NOT be active in custom mode")
	}
}

// TestContentBlockedDoesNotPenalizeAccount ErrContentBlocked 不罚账号（无冷却/熔断/NoteError），
// 且 passthrough 降级重试后第二次仍拦 → 立即 400 content_blocked（不轮转），
// message 透传上游 code/msg 原文（error-passthrough）。
func TestContentBlockedDoesNotPenalizeAccount(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	// 熔断阈值 1：若误罚 NoteError 一次即熔断；content_blocked 不应喂熔断。
	p.SetBreaker(1, time.Hour, time.Hour)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))

	// passthrough 首遇（无原始 system，实际无降级重试）→ 内容拦截直接回 400 content_blocked。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"code":"content_blocked"`) {
		t.Errorf("passthrough retry-still-blocked should return content_blocked: %s", rec.Body)
	}

	st, _ := p.Status("u1")
	if st.Cooling || st.Disabled || st.ErrTotal != 0 {
		t.Fatalf("ErrContentBlocked should not penalize account: %+v", st)
	}
}

// TestContentBlockedSecondHitReturns400 passthrough 首遇降级重试、第二次仍拦 → 立即 400
// content_blocked：**不轮转**（多账号池也只打一次）、**不罚账号**、防火墙文案不含账号/错误码。
func TestContentBlockedSecondHitReturns400(t *testing.T) {
	calls := 0
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{
				StatusCode: 400,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":11128,"msg":"blocked by security policy"}`)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "passthrough"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"system","content":"原始指纹"},{"role":"user","content":"hi"}]}`)))

	// passthrough 首遇（body 含原始 system）→ 降级重试一次；第二次仍拦 → 立即 400，不轮转。
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	if calls != 2 {
		t.Errorf("want exactly 2 upstream calls (first 400 + one degraded retry, then stop), got %d", calls)
	}
	body := rec.Body.String()
	if !assertJSONErrorCode(t, body, "content_blocked") {
		return
	}
	// error-passthrough：message 透传上游 body 原文（含 code 11128），任务书授权
	// 上游错误码对客户端可见。仍不泄露账号 UID/冷却/本地调度身份。
	if !strings.Contains(body, "11128") || !strings.Contains(body, "blocked by security policy") {
		t.Errorf("want upstream raw body in message (11128 + policy text): %s", body)
	}
	for _, leak := range []string{"account", "accounts", "账号", "upstream", "cooling", "disabled", "no_healthy", "u1", "u2"} {
		if strings.Contains(strings.ToLower(body), leak) {
			t.Errorf("must not leak %q: %s", leak, body)
		}
	}
	// 不轮转 ⇒ 两个账号都未被施加任何处罚（无冷却/禁用/熔断计数）。
	for _, uid := range []string{"u1", "u2"} {
		st, _ := p.Status(uid)
		if st.Cooling || st.Disabled || st.ErrTotal != 0 {
			t.Fatalf("content_blocked must not penalize any account: uid=%s status=%+v", uid, st)
		}
	}
}

// TestContentBlockedReturnsFirewallMessage 内容拦截最终失败时回 400 content_blocked，
// message 透传上游 body 原文（含 code 11128 与审核文案），不含账号/冷却/本地前缀
// （error-passthrough：上游错误码允许可见，账号 UID 与本地调度信息仍不暴露）。
func TestContentBlockedReturnsFirewallMessage(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, `{"code":11128,"msg":"blocked by security policy: content contains NSFW material"}`, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "u2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: "SYS"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 body=%s", rec.Code, rec.Body)
	}
	// custom 模式不走降级，因此只应打一次上游（不轮转第二个账号）。
	if calls != 1 {
		t.Errorf("content_blocked must not rotate accounts, calls=%d", calls)
	}

	var envelope struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("not openai error json: %v body=%s", err, rec.Body)
	}
	if envelope.Error.Code != "content_blocked" {
		t.Errorf("code=%q want content_blocked", envelope.Error.Code)
	}
	// message 为上游 body 原文（非网关固定文案）：含 code 11128 与审核原文。
	if !strings.Contains(envelope.Error.Message, "11128") ||
		!strings.Contains(envelope.Error.Message, "blocked by security policy") ||
		!strings.Contains(envelope.Error.Message, "NSFW material") {
		t.Errorf("message=%q 应为上游 body 原文（含 11128 与审核文案）", envelope.Error.Message)
	}
	for _, leak := range []string{"account", "accounts", "账号", "upstream", "cooling", "disabled", "no_healthy", "u1", "u2"} {
		if strings.Contains(strings.ToLower(rec.Body.String()), leak) {
			t.Errorf("must not leak %q: %s", leak, rec.Body)
		}
	}
}

// TestCustomModeFingerprintSanitizePreserved custom 端到端：user 消息含 Claude Code
// 指纹句（PR39 fixture 串）→ Rewrite 注入自有 system → 经 sanitize → 出站 body 中
// 该指纹被改写、system 为自有提示词。证明两层（提示词替换 + 清洗）叠加工作。
//
// 两层各自职责（互不替代）：
//   - prompt.Rewrite 替换 system/developer 消息（消灭 system 来源指纹）；
//   - sanitizeMessages 改写 user/assistant 消息中残留的指纹串（兜底用户上下文）。
func TestCustomModeFingerprintSanitizePreserved(t *testing.T) {
	// 捕获出站 body（prepareBody 已强制 stream + 归一 + 清洗后）。
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN:           "https://fake.example",
		SanitizeFingerprints: true, // 开启清洗层（与生产一致）
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// user 消息含 Claude Code 指纹句（PR39 的 fixture 串，逐字精确指纹）。
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"You are Claude Code, Anthropic's official CLI for Claude."},
			{"role":"user","content":"You are Claude Code, Anthropic's official CLI for Claude. Main branch (you will usually use this for PRs)"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	// 1) system 为自有提示词，旧 system 内容零残留。
	if !strings.Contains(out, customSys) {
		t.Errorf("out body should contain custom system prompt: %s", out)
	}
	if strings.Contains(out, "official CLI for Claude.") && strings.Contains(out, "You are Claude Code, Anthropic's") {
		// 旧 system 原文（含句点）不应以 system 角色出现；但 sanitize 把它改写为
		// "...official CLI tool for Claude."，所以原文 fingerprint 串应消失。
	}
	// 原始指纹串（逐字精确匹配）在出站 body 中应被改写：
	// "official CLI for Claude." → "official CLI tool for Claude."
	// "Main branch (" → "Default branch ("
	if strings.Contains(out, "official CLI for Claude.") {
		t.Errorf("identity fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	if strings.Contains(out, "Main branch (you will usually use this for PRs)") {
		t.Errorf("branch fingerprint not rewritten by sanitize in user msg: %s", out)
	}
	// 改写后的痕迹应在（证明 sanitize 层生效，不是"全删了"）。
	if !strings.Contains(out, "official CLI tool for Claude.") {
		t.Errorf("sanitized identity rewrite missing: %s", out)
	}
	if !strings.Contains(out, "Default branch (you will usually use this for PRs)") {
		t.Errorf("sanitized branch rewrite missing: %s", out)
	}
	// 2) messages 头部恰好一条 system = 自有提示词（Rewrite 已删旧 system）。
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs := obj["messages"].([]any)
	var systemCount int
	for _, m := range msgs {
		mm := m.(map[string]any)
		if mm["role"] == "system" {
			systemCount++
			if mm["content"] != customSys {
				t.Errorf("system content=%v want %q", mm["content"], customSys)
			}
		}
	}
	if systemCount != 1 {
		t.Errorf("want exactly 1 system message, got %d (all=%v)", systemCount, msgs)
	}
}

// TestStatusRealmTotals /status 新增 realm_totals 字段：混合池各域计数独立分组，
// 既有 total/healthy/cooling/disabled 汇总键零回归（仍全池口径）。
func TestStatusRealmTotals(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "cn2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "g2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	p.Cooldown("cn2", pool.CoolSoft, time.Hour, "429 rate limit")
	p.Disable("g2", "session dead")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		Total, Healthy, Cooling, Disabled int
		RealmTotals                       map[string]map[string]int `json:"realm_totals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v body=%s", err, rec.Body)
	}
	// 既有汇总键零回归：全池 = 4 账号。
	if body.Total != 4 || body.Healthy != 2 || body.Cooling != 1 || body.Disabled != 1 {
		t.Errorf("summary = %d/%d/%d/%d want 4/2/1/1 (zero regression)", body.Total, body.Healthy, body.Cooling, body.Disabled)
	}
	if body.RealmTotals == nil {
		t.Fatal("realm_totals missing from /status")
	}
	cn := body.RealmTotals["cn"]
	if cn["total"] != 2 || cn["healthy"] != 1 || cn["cooling"] != 1 || cn["disabled"] != 0 {
		t.Errorf("realm_totals.cn=%v want total=2 healthy=1 cooling=1 disabled=0", cn)
	}
	g := body.RealmTotals["global"]
	if g["total"] != 2 || g["healthy"] != 1 || g["cooling"] != 0 || g["disabled"] != 1 {
		t.Errorf("realm_totals.global=%v want total=2 healthy=1 cooling=0 disabled=1", g)
	}
}

// TestHealthzRealmServable /healthz 新增 realm_servable 字段：各域可服务状态独立暴露。
// global 全冷却 + cn 空闲 → realm_servable.global=false, cn=true；HTTP 仍 200（存在性语义零回归）。
func TestHealthzRealmServable(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.codebuddy.cn"},
		&auth.Auth{UID: "g1", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
		&auth.Auth{UID: "g2", AccessToken: "at", ExpiresAt: 9999999999, Domain: "www.workbuddy.ai"},
	)
	// global 全冷却（一软一硬），cn 空闲。
	p.Cooldown("g1", pool.CoolSoft, time.Hour, "429 rate limit")
	p.Cooldown("g2", pool.CoolHard, time.Hour, "余额不足")

	h := NewHandler(Config{Pool: p, Upstream: upstream.New()})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	// 存在性语义零回归：任一域可服务 → 200。
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d want 200 (cn servable keeps existence semantics)", rec.Code)
	}
	var resp struct {
		Healthy       int             `json:"healthy"`
		RealmServable map[string]bool `json:"realm_servable"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("healthz not json: %v body=%s", err, rec.Body)
	}
	if resp.Healthy != 1 {
		t.Errorf("healthy=%d want 1 (only cn1)", resp.Healthy)
	}
	if resp.RealmServable["cn"] != true {
		t.Errorf("realm_servable.cn=%v want true", resp.RealmServable["cn"])
	}
	if resp.RealmServable["global"] != false {
		t.Errorf("realm_servable.global=%v want false", resp.RealmServable["global"])
	}
}

// TestNewHandlerPromptDefaultPassthrough 端到端：未注入 PromptMode 时兜底为 passthrough——
// 客户端原始 system 原样透传（出站 body 中 system 内容逐字保留，网关不注入自有提示词）。
func TestNewHandlerPromptDefaultPassthrough(t *testing.T) {
	var sentBody []byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBody = raw
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up}) // 不注入 PromptMode（缺省 passthrough）

	const sys = "You are a helpful assistant in a test harness."
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{
		"model":"glm-5.2",
		"stream":true,
		"messages":[
			{"role":"system","content":"`+sys+`"},
			{"role":"user","content":"hello"}
		]
	}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	out := string(sentBody)
	if !strings.Contains(out, sys) {
		t.Errorf("default passthrough should keep client system verbatim: %s", out)
	}
	var obj map[string]any
	if err := json.Unmarshal(sentBody, &obj); err != nil {
		t.Fatalf("out body not json: %v %s", err, out)
	}
	msgs, _ := obj["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("expected >=2 messages (system kept + user kept), got %d: %s", len(msgs), out)
	}
	sysCount := 0
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm["role"] == "system" {
			sysCount++
			if mm["content"] != sys {
				t.Errorf("system content=%v want %q (no rewrite in default mode)", mm["content"], sys)
			}
		}
	}
	if sysCount != 1 {
		t.Errorf("system count=%d want 1 (passthrough keeps client system exactly once)", sysCount)
	}
}

// TestContentBlockedCustomIgnoresActiveDegrade 显式 custom 模式在降级期仍走自有提示词、
// 不参与降级：先人为触发 degrade.Active()（模拟 passthrough 首遇后进入降级期），
// 再发 custom 请求 → 出站 body 为自有提示词而非 Degraded，且仍不经降级重试。
//
// 相交矩阵关键格：degrade gate 是 Handler 级全局状态（handler.go:69），passthrough
// 请求可能在不经意间把整实例带入降级期。守卫在 handler.go:450-455 的 if/elseif 结构：
// custom 分支恒优先，Active() 只在 passthrough 分支才被求值 → custom 请求永远
// 命不中降级分支。此测试把该行为锁死，防未来重构把两分支合并后 custom 被降级期带偏。
func TestContentBlockedCustomIgnoresActiveDegrade(t *testing.T) {
	var sentBodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			sentBodies = append(sentBodies, raw)
			// 全部返回 200：本测试只论「custom 请求在降级期内的出站体」，不涉及重试。
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(sseOK)),
			}, nil
		})},
		ChatBaseCN: "https://fake.example",
	}
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	const customSys = "我是网关自有提示词"
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "custom", PromptText: customSys})

	// 人为把降级门拨到激活态（模拟 passthrough 首遇 400 后进入降级期）。
	h.degrade.Trigger()
	if !h.degrade.Active() {
		t.Fatal("precondition: degrade gate should be active after Trigger")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(sentBodies) != 1 {
		t.Fatalf("want exactly 1 upstream call (custom does not degrade/retry), got %d", len(sentBodies))
	}
	out := string(sentBodies[0])
	// custom 请求在降级期仍注入自有提示词，而非降级中性提示词。
	if !strings.Contains(out, customSys) {
		t.Errorf("custom request should rewrite to custom prompt even in degrade period: %s", out)
	}
	if strings.Contains(out, prompt.Degraded) {
		t.Errorf("custom request must NOT use Degraded prompt even in degrade period: %s", out)
	}
}

// TestStaleSnapshotBoundedByPickGate 粘性快照陈旧被 Pick 闸兜底的契约锚
// （pr134-watchlist-analysis.md #10）：
// session.ResolveForModel 在取任何锁之前拿 pool 可用性快照——若快照后、Pick 前
// 粘性号 A 被冷却（快照陈旧判 available），Session 层仍会返回 A 作为建议 uid；
// 但真正的可用性权威闸是 handler 的 Pool.PickByUIDForModel（锁内新鲜判定
// healthyForModel）——A 冷却后返回 nil → unbindSticky 回落普通轮换。
// 本测试锁死该兜底契约：Available 闭包返回含 A 的固定快照（永远陈旧），
// A 预冷却，断言请求 200 落到 good、绑定收敛 good、A 从未被出站选中。
// 若未来有人把 Pick 闸挪走（粘性 uid 不再经锁内校验直取），本测试立刻红。
func TestStaleSnapshotBoundedByPickGate(t *testing.T) {
	st := newBindStore()
	sess := session.New(session.Config{
		TTL:   time.Minute,
		Store: st,
		// 固定快照：永远包含 stale——模拟 ResolveForModel 取快照后 A 才被冷却的
		// 陈旧窗口（此后无论如何查都返回同一份「A 可用」的旧快照）。
		Available: func() []string { return []string{"stale", "good"} },
	})
	p := testPoolWith(
		&auth.Auth{UID: "stale", AccessToken: "at-stale", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good", AccessToken: "at-good", ExpiresAt: 9999999999},
	)
	// Pick 前冷却 A：pool 锁内新鲜状态 A 不可用，但 session 快照仍说可用。
	p.Cooldown("stale", pool.CoolHard, time.Hour, "余额不足")

	var sawStale atomic.Bool
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		if authz == "Bearer at-stale" {
			sawStale.Store(true)
		}
		return 200, sseOK, true
	})
	h := NewHandler(Config{
		Pool:         p,
		Upstream:     up,
		Session:      sess,
		SoftCooldown: time.Minute,
	})
	sess.Bind("conv-1", "stale")
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"metadata":{"conversation_id":"conv-1"}}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（陈旧快照必须被 Pick 闸兜底，请求成功落到 good）", rec.Code, rec.Body)
	}
	if sawStale.Load() {
		t.Fatal("stale 号已冷却，不得被出站选中（PickByUIDForModel 闸失效）")
	}
	if uid, ok := st.lastUID("conv-1"); !ok || uid != "good" {
		t.Fatalf("绑定应收敛到 good（stale 被 Pick 闸拒绝后解绑回落），got %s ok=%v", uid, ok)
	}
}

// ---- prompt.mode=append 端到端（issue #129 C 组） ----

// newBodyCaptureUpstream 返回捕获每次出站请求体的 fake 上游（behavior 按
// 第几次调用返回响应；最后一次之后的调用沿用末次行为）。
func newBodyCaptureUpstream(t *testing.T, bodies *[][]byte, behavior func(call int, body string) (int, string, bool)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			*bodies = append(*bodies, raw)
			status, respBody, isStream := behavior(len(*bodies), string(raw))
			ct := "application/json"
			if isStream {
				ct = "text/event-stream"
			}
			return &http.Response{
				StatusCode: status,
				Header:     http.Header{"Content-Type": []string{ct}},
				Body:       io.NopCloser(strings.NewReader(respBody)),
			}, nil
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
	}
}

// TestHandlerAppendEndToEnd C1：mode=append → 上游收到：开头块原样 + GW 紧跟其后 + user 不动。
func TestHandlerAppendEndToEnd(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "append", PromptText: "网关人格"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
			{"role":"system","content":"项目规范"},
			{"role":"developer","content":"工具约定"},
			{"role":"user","content":"你好"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call, got %d", len(bodies))
	}
	var obj map[string]any
	if err := json.Unmarshal(bodies[0], &obj); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, bodies[0])
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len=%d want 4: %s", len(msgs), bodies[0])
	}
	// 原始 system/developer 逐字在场。
	if msgs[0].(map[string]any)["content"] != "项目规范" {
		t.Errorf("original system missing: %s", bodies[0])
	}
	if msgs[1].(map[string]any)["content"] != "工具约定" {
		t.Errorf("original developer missing: %s", bodies[0])
	}
	// GW 紧跟开头块。
	gw := msgs[2].(map[string]any)
	if gw["role"] != "system" || gw["content"] != "网关人格" {
		t.Errorf("GW msg wrong: %v", gw)
	}
	// user 不动。
	if msgs[3].(map[string]any)["content"] != "你好" {
		t.Errorf("user content changed: %s", bodies[0])
	}
}

// TestHandlerAppendDegradedActiveReplaces C2：降级期 append 退化为 replace(Degraded)——
// 原始 system 不在场、Degraded 在场（降级矩阵第三行）。
func TestHandlerAppendDegradedActiveReplaces(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "append", PromptText: "网关人格"})
	h.degrade.Trigger() // 预置降级期（模拟 append 首遇后进入降级窗口）

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
			{"role":"system","content":"原始指纹"},
			{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if len(bodies) != 1 {
		t.Fatalf("want 1 upstream call (direct to Degraded), got %d", len(bodies))
	}
	out := string(bodies[0])
	if !strings.Contains(out, prompt.Degraded) {
		t.Errorf("body should contain Degraded: %s", out)
	}
	if strings.Contains(out, "原始指纹") {
		t.Errorf("original system must be REMOVED in degraded append (replace fallback): %s", out)
	}
	if strings.Contains(out, "网关人格") {
		t.Errorf("append PromptText should NOT be used in degraded window: %s", out)
	}
}

// TestHandlerAppendContentBlockedTriggersDegrade C3：首遇 400 content-blocked
// → Trigger + 同请求以 Degraded-replace 重试成功；degradeGate.Active()。
func TestHandlerAppendContentBlockedTriggersDegrade(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		if call == 1 {
			return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
		}
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "append", PromptText: "网关人格"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
			{"role":"system","content":"原始指纹"},
			{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s (want 200 after degraded retry)", rec.Code, rec.Body)
	}
	if len(bodies) != 2 {
		t.Fatalf("want 2 upstream calls, got %d", len(bodies))
	}
	// 首次出站是 append 语义（原始 system 在场 + GW 在场）。
	if !strings.Contains(string(bodies[0]), "原始指纹") || !strings.Contains(string(bodies[0]), "网关人格") {
		t.Errorf("first body should be append semantics: %s", bodies[0])
	}
	// 重试出站是 replace(Degraded)（原始 system 移除）。
	if !strings.Contains(string(bodies[1]), prompt.Degraded) {
		t.Errorf("retry body should contain Degraded: %s", bodies[1])
	}
	if strings.Contains(string(bodies[1]), "原始指纹") {
		t.Errorf("retry body should not contain original system: %s", bodies[1])
	}
	if !h.degrade.Active() {
		t.Error("degrade should be active after content-blocked trigger in append mode")
	}
}

// TestHandlerAppendContentBlockedSecondFailPassthrough C4：两次 content-blocked
// → 客户端收 400 + 上游原文（既有透传路径复用）。
func TestHandlerAppendContentBlockedSecondFailPassthrough(t *testing.T) {
	var bodies [][]byte
	up := newBodyCaptureUpstream(t, &bodies, func(call int, body string) (int, string, bool) {
		return 400, `{"code":11128,"msg":"blocked by security policy"}`, false
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "append", PromptText: "网关人格"})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[
			{"role":"system","content":"x"},
			{"role":"user","content":"hi"}]}`)))
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400 (second content-blocked passthrough)", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"code":"content_blocked"`) {
		t.Errorf("want content_blocked code: %s", body)
	}
	// error-passthrough：上游 code/msg 原文在场。
	if !strings.Contains(body, "blocked by security policy") || !strings.Contains(body, "11128") {
		t.Errorf("want upstream raw body passthrough: %s", body)
	}
	if len(bodies) != 2 {
		t.Errorf("want 2 upstream calls (first + one degraded retry), got %d", len(bodies))
	}
}

// TestHandlerAppendTurnKeyStable C7：append 改写不得影响轮级聚合键——
// handler 在改写前提取 turnKey（§3.4 槽位纪律），出站 X-Conversation-Request-ID
// 应等于按**原始 body**（改写前）派生的 TurnRequestID(TurnKey(原))，而非按
// append 后 body 派生的（TurnKey 键含最后一条 user 的下标，插 system 会移位）。
func TestHandlerAppendTurnKeyStable(t *testing.T) {
	var reqIDs []string
	var bodies [][]byte
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			bodies = append(bodies, raw)
			reqIDs = append(reqIDs, r.Header.Get("X-Conversation-Request-ID"))
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
	h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: "append", PromptText: "网关人格"})

	original := `{"model":"glm-5.2","messages":[
		{"role":"system","content":"项目规范"},
		{"role":"user","content":"你好"}]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(original)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	// 出站 body 确为 append 语义（插了 GW）。
	if !strings.Contains(string(bodies[0]), "网关人格") {
		t.Fatalf("outbound body should be append-rewritten: %s", bodies[0])
	}
	// 聚合键按原始 body 派生（改写前提取）。
	want := session.TurnRequestID(session.TurnKey([]byte(original)))
	if want == "" {
		t.Fatal("want non-empty TurnRequestID for original body")
	}
	if len(reqIDs) != 1 || reqIDs[0] != want {
		t.Errorf("X-Conversation-Request-ID=%v want %q (derived from pre-rewrite body)", reqIDs, want)
	}
}

// TestChatModelOutsidePublishList404 发布清单不只是「列表过滤器」：没勾的模型调用时必须
// 404，否则勾选只是把名字从 /v1/models 里藏起来，客户端拿旧名字照样能打通——「对外发什么
// 由你勾」就成了摆设。
//
// 同时锚定三件事：空清单 = 全放（默认零回归）；被清单挡下的请求不消耗上游调用；
// 客户端拿到的是带输出前缀/费率后缀的名字，回传时必须能对上（剥干净再比）。
func TestChatModelOutsidePublishList404(t *testing.T) {
	var calls int
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 200, sseOK, true
	})
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	out := NewOutputStore("")
	h := NewHandler(Config{Pool: p, Upstream: up, Output: out})

	post := func(model string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`)))
		return rec
	}

	// 1) 清单为空 = 全放：任何名字照常打到上游。
	if rec := post("cn:glm-5.3"); rec.Code != 200 {
		t.Fatalf("空清单（全放）不该拦：code=%d body=%s", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("空清单下上游调用次数=%d want 1", calls)
	}

	// 2) 只发布 glm-5.2：清单外的 404，且不计一次上游调用。
	if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Models: []string{"glm-5.2"}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	rec := post("cn:glm-5.3")
	if rec.Code != http.StatusNotFound || !assertJSONErrorCode(t, rec.Body.String(), "model_not_found") {
		t.Fatalf("清单外模型 code=%d body=%s，应为 404 model_not_found", rec.Code, rec.Body)
	}
	if calls != 1 {
		t.Fatalf("被清单挡下的请求不该打到上游：calls=%d want 1", calls)
	}

	// 3) 清单内的照常放行；带费率后缀回传的名字要能对上（客户端看到的就是带后缀的）。
	if rec := post("cn:glm-5.2"); rec.Code != 200 {
		t.Fatalf("清单内模型被误拦：code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := post("cn:glm-5.2-free"); rec.Code != 200 {
		t.Fatalf("清单内模型带费率后缀被误拦：code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := post("cn:glm-5.3-free"); rec.Code != http.StatusNotFound {
		t.Fatalf("清单外模型带费率后缀应 404：code=%d", rec.Code)
	}

	// 4) 清空清单 = 恢复全放。
	if _, err := out.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit}); err != nil {
		t.Fatalf("clear output: %v", err)
	}
	if rec := post("cn:glm-5.3"); rec.Code != 200 {
		t.Fatalf("清空清单后应恢复全放：code=%d body=%s", rec.Code, rec.Body)
	}
}

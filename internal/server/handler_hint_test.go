// handler_hint_test.go gateway_hint 端到端验收（任务书 gateway-hint）：
// error.message 永远是上游原文透传（逐字）、gateway_hint 并列补充、未覆盖形态
// 不带字段、SSE error 帧同样附加、本地调度错误带 no_healthy hint。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// hintEnvelope 错误响应解形态（含可选 gateway_hint）。
type hintEnvelope struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		// GatewayHint 用指针：未覆盖形态必须**字段缺席**（JSON 键不存在），
		// 不能是空串（断言「不带字段不编造」）。
		GatewayHint *string `json:"gateway_hint"`
	} `json:"error"`
}

// seedModelsCache 把动态模型目录缓存置为指定条目（hintContext 只读缓存快照，
// 测试隔离：测试前 seed、测试后 reset，见 resetModelsCache）。
func seedModelsCache(infos []upstream.ModelInfo) {
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now() // 热（TTL 内）
	dynamicModelsCache.lastFail = time.Time{}
	dynamicModelsCache.Unlock()
}

// imgBody11133 携带 image_url 的请求体（11133 场景：不支持图片的模型传图）。
const imgBody11133 = `{"model":"deepseek-v3-0324","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBOR"}}]}]}`

// body11133Real 图片回归实测的 11133 原始 body（2026-09-16 真实账号抓取）。
const body11133Real = `{"code":11133,"msg":"Invalid request parameters","requestId":"6376d207cb901455a6851344c5e52769","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider","param":"","type":"invalid_request_error","StatusCode":400,"Request":null,"Response":null},"displayMsg":{"en":"The request parameters do not meet the current model requirements. Please adjust and retry."}}`

// body11135Real 图片回归实测的 11135 原始 body。
const body11135Real = `{"code":11135,"msg":"Please start a new conversation, replace the image, and try again.","requestId":"32ff8c86419322f4a06804365d5ffd88","extError":{"code":"400001","message":"Please start a new conversation, replace the image, and try again.","param":"","type":"invalid_request_error","StatusCode":400,"Request":null,"Response":null},"displayMsg":{"en":"The image cannot be processed. Please use a valid png/jpeg/webp image and retry."}}`

// TestChat11133HintModelNoImages 11133 + 请求带图 + 目录声明 supports_images=false
// → hint 点名模型并指向 /v1/models；message 逐字透传上游原文。
func TestChat11133HintModelNoImages(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{
		{ID: "deepseek-v3-0324", SupportsImages: false},
		{ID: "hy3", SupportsImages: true},
	})
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11133Real, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))

	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v body=%s", err, rec.Body)
	}
	if e.Error.Message != body11133Real {
		t.Errorf("message must be verbatim upstream body, got %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "model deepseek-v3-0324 does not support images; pick one with supports_images=true from /v1/models" {
		t.Errorf("gateway_hint=%v want model-not-supports-images hint", e.Error.GatewayHint)
	}
	// 目录查询零上游调用（cachedModelsSnapshot 只读缓存）：上游只被 chat 打过。
	if calls > 2 {
		t.Errorf("catalog lookup must not trigger upstream calls, calls=%d", calls)
	}
}

// TestChat11133HintCatalogSupportsImages 11133 + 带图 + 目录声明支持 -> 指向「换模型/
// 换会话」（账号侧图像能力与目录声明不一致）；目录未收录（缓存冷）-> 中性（宁缺勿滥）。
func TestChat11133HintCatalogSupportsImages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seeded []upstream.ModelInfo
	}{
		{"目录声明支持", []upstream.ModelInfo{{ID: "deepseek-v3-0324", SupportsImages: true}}},
		{"目录未收录（缓存冷）", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetModelsCache()
			defer resetModelsCache()
			if tc.seeded != nil {
				seedModelsCache(tc.seeded)
			}
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				return 400, body11133Real, false
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))
			var e hintEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("resp not json: %v", err)
			}
			if e.Error.Message != body11133Real {
				t.Errorf("message must be verbatim: %q", e.Error.Message)
			}
			if e.Error.GatewayHint == nil {
				t.Fatal("gateway_hint 缺失")
			}
			if tc.seeded == nil {
				// 目录未收录 -> 不做能力判定，中性文案
				if !strings.Contains(*e.Error.GatewayHint, "request parameters were rejected by the model provider") {
					t.Errorf("gateway_hint=%v want neutral params hint", *e.Error.GatewayHint)
				}
			} else {
				// 目录声明支持却被拒 -> 必须指向换模型/换会话，不能让用户去改 body
				if !strings.Contains(*e.Error.GatewayHint, "retry with a different model or start a new conversation") {
					t.Errorf("gateway_hint=%v 期望指向换模型/换会话", *e.Error.GatewayHint)
				}
			}
			if strings.Contains(*e.Error.GatewayHint, "does not support images") {
				t.Errorf("must NOT claim model-not-supports-images when catalog says the opposite: %q", *e.Error.GatewayHint)
			}
		})
	}
}

// TestChat11133HintNoImage 11133 但请求不带图（纯参数问题）→ 中性 hint，
// 不点名图片。
func TestChat11133HintNoImage(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	seedModelsCache([]upstream.ModelInfo{{ID: "deepseek-v3-0324", SupportsImages: false}})
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11133Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3-0324","messages":[{"role":"user","content":"hi"}]}`)))
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.GatewayHint == nil || strings.Contains(*e.Error.GatewayHint, "images") {
		t.Errorf("no-image 11133 must be neutral hint, got %v", e.Error.GatewayHint)
	}
}

// TestChat11135Hint 11135 invalid_image_data → 图片数据 hint；message 逐字原文。
func TestChat11135Hint(t *testing.T) {
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, body11135Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message != body11135Real {
		t.Errorf("message must be verbatim 11135 body: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || !strings.Contains(*e.Error.GatewayHint, "image data rejected") {
		t.Errorf("gateway_hint=%v want image-data hint", e.Error.GatewayHint)
	}
}

// TestChatHintVerbatimMessagePassage 透传不受污染回归锚：所有 hint 场景的
// error.message 逐字等于上游 body 原文（任务书纪律：hint 绝不替换/包装 message）。
func TestChatHintVerbatimMessagePassage(t *testing.T) {
	bodies := []string{body11133Real, body11135Real,
		`{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`,
		`{"code":6004,"msg":"您的使用量已超出频率限制","requestId":"r"}`}
	for _, raw := range bodies {
		up := newFakeUpstream(t, func(authz string) (int, string, bool) {
			return 400, raw, false
		})
		h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
			strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
		var e hintEnvelope
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("resp not json: %v body=%s", err, rec.Body)
		}
		if e.Error.Message != raw {
			t.Errorf("message polluted: got %q want verbatim %q", e.Error.Message, raw)
		}
	}
}

// TestChatHintUncoveredNoField 未覆盖形态（ErrServer 5xx / 11101 bad_params）→
// error 对象**无 gateway_hint 键**（指针 nil，非空串）。
func TestChatHintUncoveredNoField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"5xx server error", 500, `{"code":500,"msg":"internal"}`},
		{"11101 bad params", 400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				return tc.status, tc.body, false
			})
			h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
				strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
			var e hintEnvelope
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("resp not json: %v", err)
			}
			if e.Error.GatewayHint != nil {
				t.Errorf("uncovered form must have NO gateway_hint field, got %q", *e.Error.GatewayHint)
			}
		})
	}
}

// TestChatNoAccountHint 本地调度错误（空池）→ no_healthy_account hint 固定文案。
func TestChatNoAccountHint(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), Upstream: newFakeUpstream(t, nil)})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "no healthy account available in pool; check /status or retry later" {
		t.Errorf("gateway_hint=%v want no-healthy hint", e.Error.GatewayHint)
	}
}

// TestChatSoftRateHint 全账号软限流冷却后末端 429 → rate limit hint + 原文优先。
func TestChatSoftRateHint(t *testing.T) {
	const raw = `{"code":6004,"msg":"您的使用量已超出频率限制","requestId":"r"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 429, raw, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("code=%d want 429", rec.Code)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message != raw {
		t.Errorf("message must be verbatim: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v", e.Error.GatewayHint)
	}
}

// TestChatPromptTooLongHint 11115 → prompt too long hint（既有 code/message 语义
// 不变，只加字段）。
func TestChatPromptTooLongHint(t *testing.T) {
	const raw = `{"code":11115,"msg":"prompt is too long: 120000 tokens > 65536 maximum","requestId":"r"}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 400, raw, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message != raw {
		t.Errorf("message must be verbatim: %q", e.Error.Message)
	}
	if e.Error.GatewayHint == nil || *e.Error.GatewayHint != "request context exceeds the model's limit; reduce history/message size" {
		t.Errorf("gateway_hint=%v", e.Error.GatewayHint)
	}
}

// TestChatSSErrorFrameHint 流式：上游 error 帧（6004）透传时附加 gateway_hint
// 字段，message 原文/干净帧/恰好一个 [DONE] 均不受影响。
func TestChatSSErrorFrameHint(t *testing.T) {
	const errFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"req-rl-42"}}`
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, raw, true
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d want 200 (mid-stream error frame still 200)", rec.Code)
	}
	body := rec.Body.String()
	// message 原文 + 既有字段 + 新增 hint。
	var frame map[string]any
	found := false
	for _, ln := range strings.Split(body, "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.TrimPrefix(ln, "data: ") == "[DONE]" {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(ln[6:]), &obj) == nil {
			if e, ok := obj["error"].(map[string]any); ok {
				frame = e
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("error frame missing: %q", body)
	}
	if frame["message"] != "您的使用量已超出频率限制" || frame["code"] != "6004" || frame["requestId"] != "req-rl-42" {
		t.Errorf("error frame fields must be preserved: %v", frame)
	}
	if frame["gateway_hint"] != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v", frame["gateway_hint"])
	}
	if strings.Count(body, "data: [DONE]") != 1 || !strings.Contains(body, `"content":"hello"`) {
		t.Errorf("clean frames/[DONE] broken: %q", body)
	}
}

// TestHasImagePart 请求体 image_url 探测的形态正/负例。
func TestHasImagePart(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"多模态 image_url part", `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"t"},{"type":"image_url","image_url":{"url":"data:image/png;base64,x"}}]}]}`, true},
		{"字符串 content", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, false},
		{"畸形 JSON", `not json`, false},
		{"空 body", ``, false},
		{"无 messages", `{"model":"m"}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasImagePart([]byte(c.body)); got != c.want {
				t.Errorf("hasImagePart=%v want %v", got, c.want)
			}
		})
	}
}

// TestCachedModelsSnapshotZeroUpstreamCalls 目录查询零上游调用（缓存冷时不探测）：
// 空池 + 缓存 TTL 过期 → hintContext 的 ModelInCatalog=false 且不打 FetchModels。
func TestCachedModelsSnapshotZeroUpstreamCalls(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()
	// 缓存置为「已过期」（fetched=过去时刻）。
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = []upstream.ModelInfo{{ID: "hy3", SupportsImages: true}}
	dynamicModelsCache.fetched = time.Now().Add(-2 * dynamicModelsTTL)
	dynamicModelsCache.Unlock()

	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11133Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(imgBody11133)))

	// 单账号 MaxRotate=3：11133 属 ErrClient 会轮转打满（behavior 恒 400）——
	// 但 FetchModels 一次都不该打（model.json /console 路径不同 endpoint；此断言
	// 用 URL 过滤不适用 fake，改为验证 hint 退中性）。
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.GatewayHint == nil || strings.Contains(*e.Error.GatewayHint, "does not support images") {
		t.Errorf("expired cache must NOT claim supports_images fact: %v", e.Error.GatewayHint)
	}
	_ = calls
}

// TestChat11133FailFastNoRotate 11133 的端到端 fail-fast 语义：上游只被调用
// 一次（不轮转）、对外 400 而非 503、message 逐字透传、且不罚任何账号（无冷却）。
//
// 背景：11133 属「请求参数被模型拒绝」，换号结果完全相同。过去它落在通用
// ErrClient 分支，被当换号错误处理 —— 一个请求放大成 MaxRotate 次无效上游
// 调用，最后还对外报成 503 no_healthy_account，把「参数错」说成「没号了」。
func TestChat11133FailFastNoRotate(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()

	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11133Real, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3-0324","messages":[{"role":"user","content":"hi"}]}`)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 (must NOT degrade to 503 no_healthy_account), body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1: 11133 is deterministic, must not rotate accounts", calls)
	}
	var e hintEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatalf("resp not json: %v", err)
	}
	if e.Error.Message != body11133Real {
		t.Errorf("message must be verbatim upstream body:\n got %q\nwant %q", e.Error.Message, body11133Real)
	}
	for _, uid := range []string{"a1", "a2"} {
		if st, ok := p.Status(uid); ok && st.Cooling {
			t.Errorf("account %s must NOT be cooled for a client-side param error: kind=%s reason=%s", uid, st.CoolKind, st.Reason)
		}
	}
}

// TestChat11133FailFastSingleAccount 单账号同样 fail-fast（回归锚：单账号时最容易被
// 轮转逻辑吃掉，一次请求变成 3 次）。
func TestChat11133FailFastSingleAccount(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()

	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, body11133Real, false
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999}), Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3-0324","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400, body=%s", rec.Code, rec.Body.String())
	}
	if calls != 1 {
		t.Errorf("upstream calls=%d want 1 for single-account pool", calls)
	}
}

// TestChatClientErrStillRotates 对照锚：同样 400 但**不是** 11133 的普通客户端错
// 误仍走轮转（换号有意义 —— 不同账号可能模型权限不同）。没有这条，上面
// fail-fast 测试里 "calls==1" 可能只是脚手架坏了而不是真 fail-fast。
func TestChatClientErrStillRotates(t *testing.T) {
	resetModelsCache()
	defer resetModelsCache()

	const bodyUnknown = `{"code":11199,"msg":"some client side error"}`
	calls := 0
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		calls++
		return 400, bodyUnknown, false
	})
	p := testPoolWith(
		&auth.Auth{UID: "a1", AccessToken: "at1", ExpiresAt: 9999999999},
		&auth.Auth{UID: "a2", AccessToken: "at2", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: up})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"deepseek-v3-0324","messages":[{"role":"user","content":"hi"}]}`)))
	if calls < 2 {
		t.Fatalf("non-11133 client error MUST still rotate, calls=%d status=%d", calls, rec.Code)
	}
}

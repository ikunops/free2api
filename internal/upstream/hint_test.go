// hint_test.go gateway_hint 单一事实来源的单元测试：形态映射正/负例、
// 11133 上下文分野（带图+目录不支持 → 换模型指向；缺上下文 → 中性）、
// 未覆盖形态无 hint（空串）、SSE error 帧附加与原样透传、message 不受污染。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 11133/11135 实测 body（图片回归报告，2026-09-16 真实账号抓取）。
const (
	body11133 = `{"code":11133,"msg":"Invalid request parameters","requestId":"r1","extError":{"code":"model_param_invalid","message":"the request parameters were rejected by the model provider","param":"","type":"invalid_request_error"}}`
	body11135 = `{"code":11135,"msg":"Please start a new conversation, replace the image, and try again.","requestId":"r2","extError":{"code":"400001","message":"Please start a new conversation, replace the image, and try again."}}`
)

// TestGatewayHintKindMap Kind 一对一映射表正/负例。
func TestGatewayHintKindMap(t *testing.T) {
	cases := []struct {
		name string
		kind ErrKind
		msg  string
		want string
	}{
		{"11115 prompt too long", ErrPromptTooLong, `{"code":11115,"msg":"prompt is too long"}`, "request context exceeds the model's limit; reduce history/message size"},
		{"WAF 403", ErrWafBlock, `<html><head><title>403 Forbidden</title></head></html>`, "upstream WAF blocked the gateway; retry after the block window"},
		{"429 soft rate", ErrSoftRate, `{"code":6004,"msg":"rate limited"}`, "rate limited by upstream; retry after reset"},
		{"11140 account fault", ErrAccountFault, `{"code":11140,"msg":"request illegal"}`, "account-level fault at upstream (auth/quota state); the gateway will rotate or disable this account"},
		{"12153 session dead", ErrSessionDead, `{"code":12153,"msg":"Offline user session not found"}`, "account session expired at upstream; the account is disabled until re-login"},
		{"402 hard credit", ErrHardCredit, `{"code":1,"msg":"余额不足"}`, "account credits exhausted at upstream; waiting for daily check-in to restore"},
		{"11102 model blocked", ErrModelBlocked, `{"code":11102,"msg":"model [glm-4.6v] service info not found"}`, "upstream has no such model on this backend; switch model or retry on another account"},
		{"content blocked", ErrContentBlocked, `{"code":11128,"msg":"blocked by security policy"}`, "request content was rejected by content policy; adjust the prompt and retry"},
		// 未覆盖形态 → 空串（不带字段）。
		{"ErrNone 无 hint", ErrNone, "", ""},
		{"ErrServer 无 hint", ErrServer, `{"code":500,"msg":"internal"}`, ""},
		{"ErrNotFound 无 hint", ErrNotFound, "not found", ""},
		{"ErrBadParams 无 hint", ErrBadParams, `{"code":11101,"msg":"Unmarshal chat params failed"}`, ""},
		{"ErrClient 无 hint", ErrClient, "misc 4xx", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := GatewayHint(c.kind, c.msg, HintContext{})
			if got != c.want {
				t.Errorf("GatewayHint(%s)=%q want %q", c.kind, got, c.want)
			}
		})
	}
}

// TestGatewayHintImageForms 11133/11135 图片形态的上下文分野（任务书映射表核心）。
// 图片两族用例的 Kind 一律传 ErrClient：它们在 Classify 落 ErrClient/ErrBadParams
// 皆有可能，hint 层自带形态判定、不得依赖权威分类。
func TestGatewayHintImageForms(t *testing.T) {
	kind := ErrClient
	cases := []struct {
		name string
		msg  string
		ctx  HintContext
		want string
	}{
		{
			// 11133 + 带图 + 目录声明不支持 → 点名模型 + 换模型指向（含 /v1/models 提示）。
			"11133 带图且目录不支持",
			body11133,
			HintContext{Model: "deepseek-v3-0324", HasImage: true, ModelInCatalog: true, ModelSupportsImages: false},
			"model deepseek-v3-0324 does not support images; pick one with supports_images=true from /v1/models",
		},
		{
			// 11133 + 带图 + 目录声明支持（图片数据非法撞 11133）→ 中性参数形态。
			"11133 带图但目录声明支持",
			body11133,
			HintContext{Model: "hy3", HasImage: true, ModelInCatalog: true, ModelSupportsImages: true},
			"request parameters were rejected by the model provider; check message format and model capabilities",
		},
		{
			// 11133 + 带图 + 目录未收录 → 不做「不支持」判定（宁缺勿滥），退中性。
			"11133 带图但目录未收录",
			body11133,
			HintContext{Model: "mystery", HasImage: true, ModelInCatalog: false, ModelSupportsImages: false},
			"request parameters were rejected by the model provider; check message format and model capabilities",
		},
		{
			// 11133 + 不带图（纯参数问题）→ 中性参数形态，不点名图片。
			"11133 不带图",
			body11133,
			HintContext{Model: "deepseek-v3-0324", HasImage: false, ModelInCatalog: true, ModelSupportsImages: false},
			"request parameters were rejected by the model provider; check message format and model capabilities",
		},
		{
			// 11135 invalid_image_data → 图片数据无效指向（不需要带图上下文——
			// 上游明说 image 就是图片问题）。
			"11135",
			body11135,
			HintContext{},
			"image data rejected by upstream; use a real/valid image, may need a new conversation",
		},
		{
			// 11133 body 但 Kind 是 ErrClient（分类词表不含 11133）→ hint 层自带
			// 形态判定照样给出（marker 先于 Kind 表）。
			"11133 ErrClient 仍带形态 hint",
			body11133,
			HintContext{Model: "deepseek-v3-0324", HasImage: true, ModelInCatalog: true, ModelSupportsImages: false},
			"model deepseek-v3-0324 does not support images; pick one with supports_images=true from /v1/models",
		},
		{
			// 非 11133/11135 的 body → 纯 Kind 表（ErrSoftRate 例子；6004 码字不撞
			// 11133/11135 marker）。
			"6004 body 走 Kind 表",
			`{"code":6004,"msg":"您的使用量已超出频率限制"}`,
			HintContext{},
			"rate limited by upstream; retry after reset",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := kind
			if c.name == "6004 body 走 Kind 表" {
				k = ErrSoftRate // 该例验证 Kind 表路径本身
			}
			got := GatewayHint(k, c.msg, c.ctx)
			if got != c.want {
				t.Errorf("GatewayHint=%q want %q", got, c.want)
			}
		})
	}
}

// TestGatewayHintMarkerTolerance 11133/11135 判定对实测 body 的多种形态命中。
// marker 与既有 code 判定（IsModelBlocked 等）同口径：`"code":11133` 紧凑形态 +
// 字符串形态 + extError 深层 marker + displayMsg 文案家族。
func TestGatewayHintMarkerTolerance(t *testing.T) {
	variants := []string{
		body11133,                                                     // 实测原始 body
		`{"code":"11133","msg":"..."}`,                                // code 字符串形态
		`{"extError":{"code":"model_param_invalid"}}`,                 // extError 深层 marker
		`{"msg":"The request parameters do not meet the current model requirements"}`, // displayMsg en 文案
	}
	for _, v := range variants {
		if got := GatewayHint(ErrClient, v, HintContext{}); got == "" {
			t.Errorf("11133 variant should yield neutral hint: %q", v)
		}
	}
	v11135 := []string{
		body11135,                                     // 实测原始 body
		`{"extError":{"code":"invalid_image_data"}}`, // extError 深层 marker
	}
	for _, v := range v11135 {
		if got := GatewayHint(ErrClient, v, HintContext{}); got == "" {
			t.Errorf("11135 variant should yield hint: %q", v)
		}
	}
	// 不相关码（11102/11128 等）不撞 marker → 走 Kind 表（ErrClient → 无 hint）。
	if got := GatewayHint(ErrClient, `{"code":11102,"msg":"model [x] service info not found"}`, HintContext{}); got != "" {
		t.Errorf("11102 under ErrClient must have no hint, got %q", got)
	}
}

// TestStreamHintAttachesGatewayHint SSE error 帧附加 gateway_hint：message 原文
// 不动、既有键（code/requestId）保留、新增并列字段；hintFn 惰性（只有 error 帧
// 才调用）；干净帧不受影响。
func TestStreamHintAttachesGatewayHint(t *testing.T) {
	const errFrame = `{"error":{"message":"您的使用量已超出频率限制","code":"6004","requestId":"req-rl-42"}}`
	raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"}}]}\n\n" +
		"data: " + errFrame + "\n\n" +
		"data: [DONE]\n\n"

	calls := 0
	hintFn := func(payload string) string {
		calls++
		return GatewayHint(FrameKind(payload), payload, HintContext{})
	}
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), hintFn); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()

	// message 原文逐字保留 + 既有字段 + 新增 hint 字段。
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
	if frame["message"] != "您的使用量已超出频率限制" {
		t.Errorf("message changed: %v", frame["message"])
	}
	if frame["code"] != "6004" || frame["requestId"] != "req-rl-42" {
		t.Errorf("existing fields must be preserved: %v", frame)
	}
	if frame["gateway_hint"] != "rate limited by upstream; retry after reset" {
		t.Errorf("gateway_hint=%v", frame["gateway_hint"])
	}
	// hintFn 只对 error 帧调用一次（干净帧/[DONE] 不触发）。
	if calls != 1 {
		t.Errorf("hintFn calls=%d want 1 (only error frame)", calls)
	}
	// 干净帧照常透传 + 恰好一个 [DONE]。
	if !strings.Contains(body, `"content":"hello"`) || strings.Count(body, "data: [DONE]") != 1 {
		t.Errorf("clean frames/[DONE] broken: %q", body)
	}
}

// TestStreamHintNoHintLeavesVerbatim hintFn 返回空串 → 帧原样透传
// （与 Stream 行为一致，无 gateway_hint 键、无 JSON 重排）。
func TestStreamHintNoHintLeavesVerbatim(t *testing.T) {
	const errFrame = `{"error":{"message":"misc","code":"x"}}`
	raw := "data: " + errFrame + "\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), func(string) string { return "" }); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); !strings.Contains(got, errFrame) || strings.Contains(got, "gateway_hint") {
		t.Errorf("empty hint must leave frame verbatim: %q", got)
	}
}

// TestStreamHintNilFuncEqualsStream nil hintFn 与 Stream 逐字节一致（零开销回归锚）。
func TestStreamHintNilFuncEqualsStream(t *testing.T) {
	raw := "data: {\"id\":\"x1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"error\":{\"message\":\"m\"}}\n\ndata: [DONE]\n\n"
	a, b := httptest.NewRecorder(), httptest.NewRecorder()
	if err := Stream(a, strings.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if err := StreamHint(b, strings.NewReader(raw), nil); err != nil {
		t.Fatal(err)
	}
	if a.Body.String() != b.Body.String() {
		t.Errorf("nil hintFn must be byte-identical to Stream:\nStream=%q\nStreamHint=%q", a.Body, b.Body)
	}
}

// TestStreamHintNonJSONFrame hint 返回非空但帧是纯文本：attach 判不出 JSON →
// 原样透出（不 panic、不破坏原文）。混入干净 JSON 帧的流正常完成。
func TestStreamHintNonJSONFrame(t *testing.T) {
	raw := "data: not-json\n\ndata: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHint(rec, strings.NewReader(raw), func(string) string { return "some hint" }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "not-json") {
		t.Errorf("non-JSON payload must pass through: %q", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("DONE missing: %q", rec.Body)
	}
}

// TestFrameKind SSE 帧 Kind 判定：6004 → ErrSoftRate；error.message 文案 →
// Classify 口径；非 JSON → ErrNone。
func TestFrameKind(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    ErrKind
	}{
		{"6004 帧直接命中", `{"error":{"message":"您的使用量已超出频率限制","code":"6004"}}`, ErrSoftRate},
		{"message 走 Classify", `{"error":{"message":"prompt is too long"}}`, ErrPromptTooLong},
		{"message 走 Classify-11115", `{"error":{"message":"prompt is too long: 1 > 0"}}`, ErrPromptTooLong},
		{"message 命中限流文案", `{"error":{"message":"too many requests"}}`, ErrSoftRate},
		{"非 JSON", "not-json", ErrNone},
		{"空", "", ErrNone},
		{"无 error 对象", `{"choices":[]}`, ErrNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FrameKind(c.payload); got != c.want {
				t.Errorf("FrameKind(%q)=%v want %v", c.payload, got, c.want)
			}
		})
	}
}

// TestAttachHintToErrorFrame 帧附加实现：只加键不改既有键；非 JSON/无 error 原样。
func TestAttachHintToErrorFrame(t *testing.T) {
	const payload = `{"error":{"message":"m","code":"6004","requestId":"r"}}`
	got := attachHintToErrorFrame(payload, "hint text")
	var obj map[string]any
	if err := json.Unmarshal([]byte(got), &obj); err != nil {
		t.Fatalf("attached frame not json: %v", got)
	}
	e := obj["error"].(map[string]any)
	if e["message"] != "m" || e["code"] != "6004" || e["requestId"] != "r" || e["gateway_hint"] != "hint text" {
		t.Errorf("attach must only add gateway_hint: %v", e)
	}
	// 非 JSON / 无 error 对象 → 原样返回。
	if got := attachHintToErrorFrame("not-json", "h"); got != "not-json" {
		t.Errorf("non-json must return verbatim: %q", got)
	}
	if got := attachHintToErrorFrame(`{"message":"no-error-obj"}`, "h"); got != `{"message":"no-error-obj"}` {
		t.Errorf("no error object must return verbatim: %q", got)
	}
}

package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{402, ``, ErrHardCredit},
		{400, `{"code":1,"msg":"余额不足"}`, ErrHardCredit},
		{403, `insufficient credits`, ErrHardCredit},
		{403, `credits exhausted`, ErrHardCredit},
		{200, `{"code":1,"msg":"credits exhausted, please top up"}`, ErrHardCredit},
		{200, `{"code":10001,"msg":"积分不足，请充值"}`, ErrHardCredit},
		{400, `{"code":1,"msg":"额度用尽"}`, ErrHardCredit},
		{429, ``, ErrSoftRate},
		// 限流文案（issue #28）：状态码不是 429 时也必须识别为软限流，
		// 否则账号不会被冷却，下次请求仍会被选中。
		{200, `{"code":11140,"msg":"The model provider is rate-limiting requests. Please wait a moment and try again."}`, ErrSoftRate},
		{400, `rate limit`, ErrSoftRate},
		{403, `usage limit reached`, ErrSoftRate},
		// "model usage limit exceeded" 不是余额语义（无 credit/quota/积分/额度 等计费词），
		// 属于模型侧用量节流 → 短冷却（误判为硬冷却会把有余量的号停到次日 04:00）。
		{200, `{"code":1,"msg":"model usage limit exceeded"}`, ErrSoftRate},
		{200, `{"code":1,"msg":"too many requests"}`, ErrSoftRate},
		{500, `rate-limited upstream`, ErrSoftRate}, // 限流文案优先于 5xx 分类
		// 内容策略拦截（HTTP 400 + 审核文案）：误报信号，不罚账号，走降级重试。
		{400, `Illegal API invocation from an unapproved channel`, ErrContentBlocked},
		{400, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{400, `unapproved channel`, ErrContentBlocked},
		// 通用 4xx（非审核文案）：仍判 ErrClient，只换号不罚。
		{400, `bad request`, ErrClient},
		// ErrBadParams：请求体解析失败（HTTP 400 + Unmarshal chat params failed / code 11101）。
		// 这是"发给上游的 body 有问题"（网关截断已由 413 消灭，剩余为客户端畸形 JSON），
		// 换了账号也一样 400，不罚号。具体词优先于通用 4xx。
		{400, `{"code":11101,"msg":"Unmarshal chat params failed with error: unexpected EOF"}`, ErrBadParams},
		{400, `Unmarshal chat params failed`, ErrBadParams},
		{400, `{"code":11101,"msg":"x"}`, ErrBadParams},
		// 图片格式/数据错误是确定性请求错误，分类后不轮转、不罚号。
		{400, `{"code":11101,"msg":"Parse message failed: invalid image_url content at index 2: json: cannot unmarshal string into Go value of type v2.ImageContent"}`, ErrImageInvalid},
		{400, `{"code":11135,"msg":"invalid_image_data"}`, ErrImageInvalid},
		{400, `invalid_image_data`, ErrImageInvalid},
		// code 11135 的 JSON 空白容差（Copilot review #184 finding）：字面量 marker
		// 只能命中紧凑形态，带空格的合法 body 会退化成 ErrClient 并继续轮转。
		// 与 hint.go 的 isInvalidImageData / codeMarker 同口径。
		{400, `{"code": 11135, "msg": "image rejected"}`, ErrImageInvalid},
		{400, `{"code": "11135", "msg": "image rejected"}`, ErrImageInvalid},
		{400, `{"error": {"code": 11135, "message": "image rejected"}}`, ErrImageInvalid},
		// 其他 code 不得被 11135 口径误伤（防过宽）。用 11199 而不是 11133：
		// 11133 已独立归 ErrModelParamInvalid（见下一组用例），拿它当「不该被
		// 11135 抓住」的对照就变成了同义反复，防过宽这条就白测了。
		{400, `{"code": 11199, "msg": "other business error"}`, ErrClient},
		// 11133 model_param_invalid：请求参数被模型供应商拒绝，与 11135 同性质
		// （请求的问题不是账号的问题）。归独立类别是为了**不轮转**——早先落进
		// ErrClient 会轮转 MaxRotate 个号，每个号都拿到同样的 11133，最后对外
		// 报成 503 no_healthy_account：上游说「参数不对」，客户端收到「没号可用」。
		{400, `{"code":11133,"msg":"Invalid request parameters"}`, ErrModelParamInvalid},
		{400, `{"code": 11133, "msg": "Invalid request parameters"}`, ErrModelParamInvalid},
		{400, `{"extError":{"code":"model_param_invalid"},"msg":"x"}`, ErrModelParamInvalid},
		{400, `{"msg":"Invalid request parameters"}`, ErrModelParamInvalid},
		// 非 400 的 11133 不归此类（状态码语义不同，不走 fail-fast 路径）。
		{500, `{"code":11133,"msg":"Invalid request parameters"}`, ErrServer},
		{200, `quota exceeded`, ErrHardCredit},
		// 账号级授权/配额故障（与 429 一起纳入轮换）：11140 request illegal = auth_forbidden
		// 风控（需重登），14017 = quota_not_activated（试用未激活，需完成 register）。修复前
		// 11140 走 4xx → ErrClient 只换号不罚，坏号留在池内被反复选中刷风控。
		// 注意：11140 不按 code 单独判定——该 code 也承载 rate-limiting 软限流文案
		// （上方 {200, "code":11140 rate-limiting} 必须仍是 ErrSoftRate），只能靠 msg 区分。
		{403, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		{403, `request illegal`, ErrAccountFault},
		{429, `{"error":{"data":{"code":14017,"msg":"The trial version is not yet activated. Please log out of your current account and log in again to activate it immediately and start your free trial."}}}`, ErrAccountFault},
		{400, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
		// session 死亡优先于限流文案（401+12153 需人工重登，短冷却无意义）。
		{401, `{"code":12153,"msg":"Offline user session not found, rate limit"}`, ErrSessionDead},
		{401, `Offline user session not found`, ErrSessionDead},
		{401, `{"code":12153,"msg":"Offline user session not found"}`, ErrSessionDead},
		{401, `{"code":9999,"msg":"bad token"}`, ErrClient},
		{500, `boom`, ErrServer},
		{503, `unavailable`, ErrServer},
		{200, ``, ErrNone},
		// 11102「该后端无此模型」：确定性答复，归 ErrModelBlocked（(账号,模型) 负缓存避让）。
		{404, `{"code":11102,"msg":"model [deepseek-v3-2-volc] service info not found"}`, ErrModelBlocked},
		{400, `{"error":{"code":"11102","message":"model service info not found"}}`, ErrModelBlocked},
		{400, `{"msg":"service info not found"}`, ErrModelBlocked},
		// 11102 撞在 requestId 上不算（不得误避让可用模型）。
		{404, `{"requestId":"11102","msg":"ok"}`, ErrNotFound},
		// 429 + 11102 → 限流语义（ErrSoftRate），不是模型不存在。
		{429, `{"code":11102,"msg":"service info not found"}`, ErrSoftRate},
		// 429 + 余额措辞 → 限流语义（fork-scan-absorb T-3，本次修复点）：限流响应
		// body 高频携带 "quota exceeded"/"额度不足" 等跨计费/限流两界的措辞，
		// hardRule 在 429 之前会误判 ErrHardCredit 硬冷却到次日 04:00，白扔号约 12h。
		// 状态码是比关键词更权威的信号：真余额耗尽走 402，非 429 的 quota 措辞
		// 仍归 hardRule（上方 {200,"quota exceeded"} 语义不变）。
		{429, `quota exceeded`, ErrSoftRate},
		{429, `{"code":1,"msg":"quota exceeded, please wait"}`, ErrSoftRate},
		{429, `insufficient credits`, ErrSoftRate},
		{429, `{"code":1,"msg":"额度不足"}`, ErrSoftRate},
		{429, `积分不足，请充值`, ErrSoftRate},
		// 429 + 账号级故障码防回归（accountFault 仍先于 429 判定）：429+14017 若
		// 落到 status==429 兜底会误归 soft_rate，账号级故障等不来自愈。
		{429, `{"code":14017,"msg":"trial not activated"}`, ErrAccountFault},
		{429, `{"error":{"data":{"code":11140,"msg":"request illegal"}}}`, ErrAccountFault},
		// Issue #175：14018 明确表示账号积分耗尽，即使 HTTP 状态是 429 也必须
		// 走硬积分冷却；仅有相同文案而无该业务码的普通 429 仍保持软限流。
		{429, `{"code":14018,"msg":"Credits exhausted"}`, ErrHardCredit},
		{429, `{"error":{"data":{"code":"14018","msg":"Credits exhausted"}}}`, ErrHardCredit},
		{429, `{"requestId":"14018","msg":"Credits exhausted"}`, ErrSoftRate},
		{429, `{"code":1,"msg":"Credits exhausted"}`, ErrSoftRate},
		// WAF 403（P0-1）：403 + 无业务信封（无 "code":/"msg": 字段）→ ErrWafBlock。
		// 空体 / HTML 拦截页 / 纯文本 / 非信封 JSON 均命中。
		{403, ``, ErrWafBlock},
		{403, `<html><body>403 Forbidden</body></html>`, ErrWafBlock},
		{403, `Forbidden`, ErrWafBlock},
		{403, `{"message":"blocked by waf"}`, ErrWafBlock},
		{403, `<head><script>...</script></head><body>blocked</body>`, ErrWafBlock},
		// 403 带业务信封的仍走既有分类（P0-1 约束：不劫持业务 403）。
		{403, `{"code":11128,"msg":"blocked by security policy"}`, ErrContentBlocked},
		{403, `{"code":60001,"msg":"quota exceeded"}`, ErrHardCredit},
		{403, `{"code":1,"msg":"unknown business error"}`, ErrClient},
		// 非 403 的无信封错误体不进 WAF 分类（WAF 判定绑定 403 形态）。
		{400, `bad request`, ErrClient},
		{429, ``, ErrSoftRate},
	}
	for _, c := range cases {
		if got := Classify(c.status, c.body); got != c.want {
			t.Errorf("Classify(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestIsWafBlocked WAF 403 形态判定的直接回归（Classify 的第 7 层）：
// 只认 403 + 无业务信封；带信封/其他状态码一律 false。
func TestIsWafBlocked(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		{403, "", true},
		{403, "<html>blocked</html>", true},
		{403, `{"code":1}`, false},               // 有 "code": 字段
		{403, `{"msg":"request illegal"}`, false}, // 有 "msg": 字段（且该文案本就该走 accountFault）
		{402, "", false},                          // 非 403
		{429, "", false},
		{500, "<html>gateway</html>", false},
	}
	for _, c := range cases {
		if got := IsWafBlocked(c.status, c.body); got != c.want {
			t.Errorf("IsWafBlocked(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestParseRetryAfter P1-2：Retry-After / retry-after-ms / x-ratelimit-reset
// 头解析（有效/缺失/非法三形态）。语义对齐 intl CLI parseRetryAfterMs /
// parseRateLimitResetMs（头族与数字口径）。
func TestParseRetryAfter(t *testing.T) {
	t.Run("retry-after seconds", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "30")
		if d, ok := ParseRetryAfter(h); !ok || d != 30*time.Second {
			t.Fatalf("ParseRetryAfter(30)=%v,%v want 30s,true", d, ok)
		}
	})
	t.Run("retry-after-ms", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After-Ms", "1500")
		if d, ok := ParseRetryAfter(h); !ok || d != 1500*time.Millisecond {
			t.Fatalf("ParseRetryAfter(1500ms)=%v,%v want 1.5s,true", d, ok)
		}
	})
	t.Run("x-ratelimit-reset epoch seconds", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(90*time.Second).Unix()))
		d, ok := ParseRetryAfter(h)
		if !ok || d < 80*time.Second || d > 100*time.Second {
			t.Fatalf("ParseRetryAfter(epoch+90s)=%v,%v want ~90s", d, ok)
		}
	})
	t.Run("x-ratelimit-reset epoch millis", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(45*time.Second).UnixMilli()))
		d, ok := ParseRetryAfter(h)
		if !ok || d < 35*time.Second || d > 55*time.Second {
			t.Fatalf("ParseRetryAfter(epochMilli+45s)=%v,%v want ~45s", d, ok)
		}
	})
	t.Run("missing headers", func(t *testing.T) {
		if d, ok := ParseRetryAfter(http.Header{}); ok || d != 0 {
			t.Fatalf("missing headers must return 0,false, got %v,%v", d, ok)
		}
	})
	t.Run("invalid values", func(t *testing.T) {
		for _, v := range []string{"abc", "", "-5", "1.5", "0"} {
			h := http.Header{}
			h.Set("Retry-After", v)
			if d, ok := ParseRetryAfter(h); ok {
				t.Errorf("Retry-After=%q must be rejected, got %v", v, d)
			}
		}
	})
	t.Run("oversized sanity cap", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "999999") // > retryAfterSanity(2h)
		if d, ok := ParseRetryAfter(h); ok {
			t.Errorf("oversized Retry-After must fall back, got %v", d)
		}
	})
	t.Run("expired reset epoch", func(t *testing.T) {
		h := http.Header{}
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(-time.Minute).Unix()))
		if d, ok := ParseRetryAfter(h); ok {
			t.Errorf("expired reset must be rejected, got %v", d)
		}
	})
	t.Run("priority retry-after first", func(t *testing.T) {
		h := http.Header{}
		h.Set("Retry-After", "10")
		h.Set("X-Ratelimit-Reset", fmt.Sprintf("%d", time.Now().Add(300*time.Second).Unix()))
		if d, ok := ParseRetryAfter(h); !ok || d != 10*time.Second {
			t.Fatalf("Retry-After must take priority, got %v,%v", d, ok)
		}
	})
}

// TestChatStreamErrorCarriesRetryAfter 端到端：上游 429 带 Retry-After 头时，
// ChatStreamContext 返回的 *Error 信封携带解析后的 RetryAfter（P1-2 挂载点验收）。
func TestChatStreamErrorCarriesRetryAfter(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		resp := jsonResp(429, `{"code":1,"msg":"rate limit"}`)
		resp.Header.Set("Retry-After", "77")
		return resp, nil
	})
	_, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %v", err)
	}
	if ue.Kind != ErrSoftRate {
		t.Fatalf("kind=%v want soft_rate", ue.Kind)
	}
	if ue.RetryAfter != 77*time.Second {
		t.Fatalf("RetryAfter=%v want 77s", ue.RetryAfter)
	}
}

// TestChatStreamErrorRetryAfterAbsent 头缺失时 RetryAfter 零值（回落调用方计算）。
func TestChatStreamErrorRetryAfterAbsent(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(429, `{"code":1,"msg":"rate limit"}`), nil
	})
	_, _, _, err := c.ChatStream(&auth.Auth{AccessToken: "at", UID: "u1"}, []byte(`{}`), "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrSoftRate {
		t.Fatalf("want *Error{soft_rate}, got %v", err)
	}
	if ue.RetryAfter != 0 {
		t.Fatalf("RetryAfter=%v want 0 (absent header)", ue.RetryAfter)
	}
}

// TestClassifyContentBlocked 内容拦截分类仍由 Classify 负责（error-passthrough 后
// 固定文案生成器已删除，分类仍按 contentBlockedRule 识别内容审核——识别是为了不罚号
// 与降级重试，客户端文案改为直接透传上游原文）。
func TestClassifyContentBlocked(t *testing.T) {
	for _, body := range []string{
		`{"code":11128,"msg":"blocked by security policy"}`,
		`{"code":"11128","msg":"blocked by security policy"}`,
		`Illegal API invocation from an unapproved channel`,
		`{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`,
	} {
		if got := Classify(400, body); got != ErrContentBlocked {
			t.Errorf("Classify(400, %q)=%v want ErrContentBlocked", body, got)
		}
	}
}

// TestIsModelRateLimit 判断 429 body 是否明确指向模型级限流（code 6004）。
func TestIsModelRateLimit(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		// 6004：模型级限流（issue #31 的核心场景）。
		{`{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`, true},
		{`{"code": 6004,"msg":"x"}`, true},
		// 其他 code（非模型级限流）→ 不算。
		{`{"code":11140,"msg":"The model provider is rate-limiting requests."}`, false},
		{`{"code":1,"msg":"429 rate limit"}`, false},
	}
	for _, c := range cases {
		if got := IsModelRateLimit(c.body); got != c.want {
			t.Errorf("IsModelRateLimit(%q)=%v want %v", c.body, got, c.want)
		}
	}
}

// TestIsModelBlocked 11102「该后端无此模型」判定：只认 code 精确等于 11102 或 msg 命中
// 窄短语 "service info not found"，且仅在 400/404 下判。覆盖 reference 报告「11102 撞在
// ID 上」的坑——requestId 里的 11102 不得误判。
func TestIsModelBlocked(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   bool
	}{
		// 顶层 code 字段。
		{404, `{"code":11102,"msg":"model [x] service info not found"}`, true},
		// error 子对象 code 字段（OpenAI 信封形态）。
		{400, `{"error":{"code":"11102","message":"model service info not found"}}`, true},
		// msg 短语命中（无 code 字段）。
		{400, `{"msg":"model service info not found"}`, true},
		// 11102 撞在 requestId 上不算（reference converter test_model_site_blocks.py:55 同款）。
		{404, `{"requestId":"11102","code":0,"msg":"ok"}`, false},
		{400, `{"requestId":"11102","msg":"boom"}`, false},
		// 429 带 11102 属限流语义，不算模型不存在。
		{429, `{"code":11102,"msg":"service info not found"}`, false},
		// 非 400/404 不算。
		{500, `{"code":11102,"msg":"service info not found"}`, false},
		// code 非 11102 且无短语 → 不算。
		{404, `{"code":11103,"msg":"x"}`, false},
		// 空 body 不算。
		{404, ``, false},
	}
	for _, c := range cases {
		if got := IsModelBlocked(c.status, c.body); got != c.want {
			t.Errorf("IsModelBlocked(%d,%q)=%v want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestParseRateReset 统一解析任意限流响应（6004 **和** 非 6004，如 11140 rate-limiting）
// msg 里的「将在 … 重置」时间（UTC+8）。旧语义（非 6004 带时间 → false）是有意推翻的：
// 11140 的 rate-limiting 变体带重置时间时同样应被精确对齐到上游重置墙钟。
func TestParseRateReset(t *testing.T) {
	future := time.Now().Add(35 * time.Minute)
	ts := future.In(softRateResetLoc).Format("2006-01-02 15:04:05")
	cases := []struct {
		name string
		body string
		ok   bool
	}{
		{"6004 带时间+UTC+8 后缀", `{"code":6004,"msg":"将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 带时间无后缀", `{"code":6004,"msg":"将在 ` + ts + ` 重置"}`, true},
		{"11140 rate-limiting 带时间(账号级也应对齐)", `{"code":11140,"msg":"The model provider is rate-limiting requests. 将在 ` + ts + ` UTC+8 重置"}`, true},
		{"6004 无时间文案", `{"code":6004,"msg":"model usage limit exceeded"}`, false},
		{"非法时间格式", `{"code":6004,"msg":"将在 明天 重置"}`, false},
		{"空 body", ``, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := ParseRateReset(c.body)
			if ok != c.ok {
				t.Fatalf("ok=%v want %v (body=%s)", ok, c.ok, c.body)
			}
			if ok {
				// 解析结果 = ts 在 UTC+8 解释下的墙钟（截断到分钟），应与 future 相差 ±2 分钟。
				if d := got.Sub(future); d < -2*time.Minute || d > 2*time.Minute {
					t.Errorf("parsed=%v want ~%v (diff %v)", got, future, d)
				}
				if got.Location() != time.UTC {
					// 不同指针的 FixedZone 实例相等性按 offset 判，这里只断言 offset。
					if _, off := got.Zone(); off != 8*60*60 {
						t.Errorf("zone offset=%d want +08:00", off)
					}
				}
			}
		})
	}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func jsonResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func testClient(fn rtFunc) *Client {
	return &Client{
		HTTP:          &http.Client{Transport: fn},
		ChatBaseCN:    "https://chat.example",
		BillingBaseCN: "https://billing.example",
	}
}

func TestRefreshSuccess(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Header.Get("X-Refresh-Token") != "oldrt" {
			return nil, errors.New("missing X-Refresh-Token")
		}
		return jsonResp(200, `{"code":0,"msg":"ok","data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "oldrt", ExpiresAt: 1}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
	if a.ExpiresAt <= 1 {
		t.Errorf("expiresAt not advanced: %d", a.ExpiresAt)
	}
}

func TestRefreshPreservesExpiryWhenOmitted(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat"}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != 1753600000 {
		t.Errorf("expiresAt should be preserved, got %d", a.ExpiresAt)
	}
	if a.RefreshToken != "rt" {
		t.Errorf("refreshToken should be preserved, got %s", a.RefreshToken)
	}
}

func TestRefreshSessionDead(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 401,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":12153,"msg":"Offline user session not found"}`)),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	err := c.RefreshToken(a)
	if err == nil {
		t.Fatal("want error")
	}
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *Error, got %T %v", err, err)
	}
	if ue.Kind != ErrSessionDead {
		t.Errorf("kind=%v want ErrSessionDead", ue.Kind)
	}
}

func TestChatStreamSendsHeadersAndStreamTrue(t *testing.T) {
	var gotAuth, gotUID, gotProduct string
	var gotBody []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		gotAuth = r.Header.Get("Authorization")
		gotUID = r.Header.Get("X-User-Id")
		gotProduct = r.Header.Get("X-Product")
		gotBody, _ = io.ReadAll(r.Body)
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1", EnterpriseID: "e1"}
	rc, status, respBody, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	if respBody != nil {
		t.Errorf("200 response should carry nil body, got %q", respBody)
	}
	rc.Close()
	if gotAuth != "Bearer at" || gotUID != "u1" || gotProduct != "WorkBuddy" {
		t.Errorf("headers: auth=%q uid=%q product=%q", gotAuth, gotUID, gotProduct)
	}
	if !bytes.Contains(gotBody, []byte(`"stream":true`)) {
		t.Errorf("stream not forced: %s", gotBody)
	}
}

func TestFetchModelsEffortsDriveBodyDowngrade(t *testing.T) {
	var outbound []byte
	c := testClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/console/enterprises/personal/models"),
			strings.HasSuffix(r.URL.Path, "/v3/config"):
			// v3-config-merge：FetchModels 并发打 console + /v3/config 两路，同一份
			// glm-5.2 模型表（v3 主条目进 effort 桶，口径不变）。
			return jsonResp(200, `{"code":0,"data":{"models":[
				{"id":"glm-5.2","name":"GLM-5.2","maxInputTokens":131072,"maxOutputTokens":8192,"reasoning":{"effort":"high","supportedEfforts":["low","high"]}}
			],"agents":[{"name":"cli","models":["glm-5.2"]}]}}`), nil
		default:
			outbound, _ = io.ReadAll(r.Body)
			return &http.Response{
				StatusCode: 200,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
			}, nil
		}
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	infos, err := c.FetchModels(a)
	if err != nil {
		t.Fatalf("fetch models: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("infos=%+v", infos)
	}
	// ModelInfo.Efforts 应携带 supportedEfforts
	if len(infos[0].Efforts) != 2 || infos[0].Efforts[0] != "low" {
		t.Errorf("infos[0].Efforts=%v", infos[0].Efforts)
	}

	// glm-5.2 只支持 low/high，请求 max → 降级为 high
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("outbound unmarshal: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (outbound=%s)", m["reasoning_effort"], outbound)
	}
}

func TestChatStreamHardCreditError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(402, `{"code":1,"msg":"余额不足"}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	_, status, respBody, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if status != 402 {
		t.Errorf("status=%d", status)
	}
	// 错误路径返回已分类的 *Error 信封（WAF 403 修复后的新契约），
	// 同时 respBody 原样返回（错误透传语义不变）。
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrHardCredit {
		t.Fatalf("hard credit should return classified *Error, got %v", err)
	}
	if Classify(status, string(respBody)) != ErrHardCredit {
		t.Errorf("body=%q not classified hard credit", respBody)
	}
}

// TestChatStreamReadsMultipleChunksOverRealTransport 走真实 net/http 传输层，
// 回归 defer cancel() 导致第二块起 body Read 返回 context canceled 的断流 bug。
func TestChatStreamReadsMultipleChunksOverRealTransport(t *testing.T) {
	const frames = 6
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("http.ResponseWriter does not implement http.Flusher")
			return
		}
		for i := 1; i <= frames; i++ {
			if _, err := fmt.Fprintf(w, "data: chunk-%d\n\n", i); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer srv.Close()

	c := New()
	c.ChatBaseCN = srv.URL
	c.IdleTimeout = 5 * time.Second

	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{"model":"glm-5.2","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	defer rc.Close()

	buf := make([]byte, 1)
	var got string
	for i := 0; i < frames; i++ {
		if _, err := io.ReadFull(rc, buf); err != nil {
			t.Fatalf("read %d: %v (real transport body must not be cut)", i, err)
		}
		got += string(buf)
	}
	if strings.Contains(got, "context canceled") {
		t.Fatalf("body read hit context canceled, got %q", got)
	}
}

// TestResourceSummaryAggregation 断言 ResourceSummary 聚合口径：
// remain 取 Cycle 期剩余、size 取 CycleSize（TotalDosage 作 size 下限）、used 派生。
func TestResourceSummaryAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	remain, used, size, packs, err := c.ResourceSummary(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if remain != 1500 || size != 3000 {
		t.Errorf("summary remain=%d size=%d want 1500/3000 (TotalDosage 作 size 下限)", remain, size)
	}
	// used = TotalDosage(3000) - remain(1500) = 1500。
	if used != 1500 {
		t.Errorf("used=%d want 1500", used)
	}
	if packs != 2 {
		t.Errorf("packs=%d want 2", packs)
	}
}

// TestResourceSummaryGlobalRealm 断言 global 账号走 global billing base + /billing/meter/*
// （无 /v2 前缀），且 404 时 fallback /v2——realm 感知双路径，供 cmd/credit 复用。
func TestResourceSummaryGlobalRealm(t *testing.T) {
	var billingCalls []string
	billSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		billingCalls = append(billingCalls, r.URL.Path)
		if r.URL.Path == "/billing/meter/get-user-resource" {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"code":404,"msg":"nope"}`))
			return
		}
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"g","CycleCapacitySize":500,"CycleCapacityRemain":200,"CycleCapacityUsed":300}
		]}}}}`))
	}))
	defer billSrv.Close()

	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	c := &Client{
		HTTP:              &http.Client{},
		BillingBaseCN:     "https://billing.cn",
		BillingBaseGlobal: strings.TrimSuffix(billSrv.URL, "/"),
		GlobalEnabled:     true,
	}
	a := &auth.Auth{AccessToken: "at", UID: "g1", Domain: "www.workbuddy.ai"}
	remain, used, size, packs, err := c.ResourceSummary(a)
	if err != nil {
		t.Fatalf("global summary: %v", err)
	}
	if remain != 200 || used != 300 || size != 500 || packs != 1 {
		t.Errorf("global summary=%d/%d/%d/%d want 200/300/500/1", remain, used, size, packs)
	}
	if len(billingCalls) != 2 ||
		billingCalls[0] != "/billing/meter/get-user-resource" ||
		billingCalls[1] != "/v2/billing/meter/get-user-resource" {
		t.Errorf("global billing fallback calls=%v", billingCalls)
	}
}

// TestResourceSummaryCNUnchanged 零回归：CN 账号仍是 /v2/billing/meter/get-user-resource 单路径。
func TestResourceSummaryCNUnchanged(t *testing.T) {
	var calls []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})
	_, _, _, _, err := c.ResourceSummary(&auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"})
	if err != nil {
		t.Fatalf("cn summary: %v", err)
	}
	if len(calls) != 1 || calls[0] != "/v2/billing/meter/get-user-resource" {
		t.Errorf("cn billing calls=%v want single /v2 path", calls)
	}
}

func TestUserResourceAggregation(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/get-user-resource") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		if r.Method != http.MethodPost {
			return nil, errors.New("want POST")
		}
		body, _ := io.ReadAll(r.Body)
		if !bytes.Contains(body, []byte(`"ProductCode":"p_tcaca"`)) {
			return nil, errors.New("missing ProductCode: " + string(body))
		}
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"TotalCount":2,"TotalDosage":3000,"Accounts":[
			{"PackageName":"签到包","CapacitySize":2000,"CapacityRemain":1200,"CapacityUsed":800,"CycleCapacitySize":2000,"CycleCapacityRemain":1200,"CycleCapacityUsed":800},
			{"PackageName":"体验包","CapacitySize":1000,"CapacityRemain":300,"CapacityUsed":700,"CycleCapacitySize":1000,"CycleCapacityRemain":300,"CycleCapacityUsed":700}
		]}}}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	remain, err := c.UserResource(a)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if remain != 1500 {
		t.Errorf("remain=%d want 1500", remain)
	}
}

func TestUserResourceNegativeClamped(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":-50,"CycleCapacityUsed":150}
		]}}}}`), nil
	})
	remain, err := c.UserResource(&auth.Auth{AccessToken: "at"})
	if err != nil || remain != 0 {
		t.Errorf("remain=%d err=%v, want 0 (clamped)", remain, err)
	}
}

func TestDailyCheckinAlready(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/daily-checkin") {
			return nil, errors.New("wrong path")
		}
		return jsonResp(200, `{"code":14001,"msg":"今日已签到"}`), nil
	})
	err := c.DailyCheckin(&auth.Auth{AccessToken: "at"})
	if err == nil || !strings.Contains(err.Error(), "已签到") {
		t.Errorf("err=%v", err)
	}
}

// TestIsAlreadyCheckin "今天已签到"判定为幂等成功（中文/英文 markers 均命中）。
func TestIsAlreadyCheckin(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"今天已签到"}`), nil
	})
	if !IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("今天已签到 应判为 already")
	}

	c2 := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10001,"msg":"Already checked in today"}`), nil
	})
	if !IsAlreadyCheckin(c2.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("already(英文) 应判为 already")
	}
}

// TestIsAlreadyCheckinRejectsNonUpstream 网络层/解析层错误不得当作幂等成功。
// 误判会让当天实际未签到的账号被标成正常（停机补签遇到抖动时尤其危险）。
func TestIsAlreadyCheckinRejectsNonUpstream(t *testing.T) {
	if IsAlreadyCheckin(errors.New("dial tcp: connection refused")) {
		t.Error("网络错误不得判为 already")
	}
	if IsAlreadyCheckin(errors.New("parse failed: unexpected EOF")) {
		t.Error("解析错误不得判为 already")
	}
	// 非"已签到"语义的上游业务错误（如余额不足）也不得判为 already。
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":10002,"msg":"积分不足，请充值"}`), nil
	})
	if IsAlreadyCheckin(c.DailyCheckin(&auth.Auth{AccessToken: "at"})) {
		t.Error("余额不足不得判为 already")
	}
	// nil 不判为 already。
	if IsAlreadyCheckin(nil) {
		t.Error("nil 不得判为 already")
	}
}

func TestBasesAlwaysCN(t *testing.T) {
	c := testClient(nil)
	cn := &auth.Auth{Domain: ""}
	other := &auth.Auth{Domain: "example.com"}
	if c.chatBase(cn) != "https://chat.example" || c.billingBase(cn) != "https://billing.example" {
		t.Error("cn bases wrong")
	}
	// 恒 CN：domain 不同不改变上游 host。
	if c.chatBase(other) != c.chatBase(cn) || c.billingBase(other) != c.billingBase(cn) {
		t.Error("bases must be CN regardless of domain")
	}
}

func TestNewChatClientNoTotalTimeoutAndSharedTransport(t *testing.T) {
	c := New()
	if c.ChatHTTP == nil {
		t.Fatal("ChatHTTP should be initialized")
	}
	if c.ChatHTTP.Timeout != 0 {
		t.Errorf("ChatHTTP.Timeout=%v want 0 (no total cap)", c.ChatHTTP.Timeout)
	}
	// 共享同一个 Transport 实例，连接池不重复。
	if c.ChatHTTP.Transport != c.HTTP.Transport {
		t.Errorf("ChatHTTP and HTTP must share the same *http.Transport")
	}
	htr, ok := c.ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", c.ChatHTTP.Transport)
	}
	// 连接层加固后 New() 的缺省 ResponseHeaderTimeout=60s（详断言见
	// TestNewTransportHardening；SSE 长流不受影响——该超时只计首包前）。
	if htr.ResponseHeaderTimeout != 60*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 60s", htr.ResponseHeaderTimeout)
	}
}

// TestNewTransportHardening 连接层加固配置断言（transport.go 集中参数的回读验证）：
// 真正禁 h2 / Dial 超时与 keepalive / TLS 握手超时 / ResponseHeaderTimeout 收紧。
// 挂在 New() 的成品 Transport 上（而非 newTransport() 裸返回）——同一对象同时被
// HTTP 与 ChatHTTP 持有，任何字段断言都直接对应生产出站行为。
func TestNewTransportHardening(t *testing.T) {
	tr, ok := New().ChatHTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport type=%T", New().ChatHTTP.Transport)
	}
	// 1. 真正禁 h2：TLSNextProto 必须是「非 nil 且不含 h2」的空映射。
	//    nil = 标准库注入默认 h2 映射（ForceAttemptHTTP2 陷阱，见 transport.go）。
	if tr.TLSNextProto == nil {
		t.Fatal("TLSNextProto must be non-nil empty map to disable HTTP/2 (nil = stdlib re-enables h2)")
	}
	if _, registered := tr.TLSNextProto["h2"]; registered {
		t.Error("TLSNextProto must not register h2")
	}
	if len(tr.TLSNextProto) != 0 {
		t.Errorf("TLSNextProto must be empty, got %d entries", len(tr.TLSNextProto))
	}
	// 2. DialContext 超时与 keepalive：无法直接回读 Dialer 字段（Transport 只存
	//    闭包），行为由 transport_test.go 的拨号计时测试验证。
	// 3. TLS 握手超时（现役此前缺失）。
	if tr.TLSHandshakeTimeout != 10*time.Second {
		t.Errorf("TLSHandshakeTimeout=%v want 10s", tr.TLSHandshakeTimeout)
	}
	// 4. ResponseHeaderTimeout 收紧（120s → 60s，语义：只计首包前，SSE 长流不受影响）。
	if tr.ResponseHeaderTimeout != 60*time.Second {
		t.Errorf("ResponseHeaderTimeout=%v want 60s", tr.ResponseHeaderTimeout)
	}
	// 5. 空闲连接池（既有值，从 90s 收到 30s）。
	if tr.IdleConnTimeout != 30*time.Second {
		t.Errorf("IdleConnTimeout=%v want 30s", tr.IdleConnTimeout)
	}
	if tr.MaxIdleConns != 100 || tr.MaxIdleConnsPerHost != 20 {
		t.Errorf("pool sizes=(%d, %d) want (100, 20)", tr.MaxIdleConns, tr.MaxIdleConnsPerHost)
	}
	// 6. DisableKeepAlives 必须保持 false：与连接复用意图相反，不吸收（报告说明）。
	if tr.DisableKeepAlives {
		t.Error("DisableKeepAlives must stay false (keep-alive reuse is intentional)")
	}
}

func TestChatStreamRoutesToChatHTTP(t *testing.T) {
	// 显式注入 ChatHTTP（可辨识标记），验证 ChatStream 走它而非 HTTP。
	chatHit, httpHit := false, false
	c := testClient(func(*http.Request) (*http.Response, error) {
		httpHit = true
		return jsonResp(200, `{}`), nil
	})
	c.ChatHTTP = &http.Client{Transport: rtFunc(func(*http.Request) (*http.Response, error) {
		chatHit = true
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if !chatHit {
		t.Error("ChatStream should use ChatHTTP")
	}
	if httpHit {
		t.Error("ChatStream must not use HTTP")
	}
}

func TestChatHTTPNilFallsBackToHTTP(t *testing.T) {
	c := testClient(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	if c.chatHTTP() != c.HTTP {
		t.Error("chatHTTP() should fall back to HTTP when ChatHTTP is nil")
	}
}

// TestRateRegexesPrecompiledConcurrent 正则预编译为包级 var 后（P2-9，发现 8），
// 两个限流判定函数在高并发下结果恒定。旧实现（函数体内 MustCompile）在此
// 测试下同样通过（纯只读），该测试锁的是「预编译不改变语义」+ 并发安全，
// 防止未来有人把包级 var 改回带状态的调用侧编译。
func TestRateRegexesPrecompiledConcurrent(t *testing.T) {
	const bodies = 50
	const workers = 8
	rlBody := `{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`
	resetBody := `{"code":6004,"msg":"将在 2026-09-11 18:33:27 UTC+8 重置"}`

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < bodies; i++ {
				if !IsModelRateLimit(rlBody) {
					errs <- fmt.Errorf("IsModelRateLimit concurrent miss")
					return
				}
				if _, ok := ParseRateReset(resetBody); !ok {
					errs <- fmt.Errorf("ParseRateReset concurrent miss")
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// TestChatHeadersRacesRefreshToken 并发下 ChatHeaders 读 AccessToken 与 RefreshToken
// 写 AccessToken 的数据竞争（auth.Auth.mu 未覆盖出站读取侧）。
//
// auth.Auth.mu 的既有契约只覆盖「RefreshToken 写 ↔ SaveAtomic 读」（见 auth.go 注释
// 「mu 串行化 RefreshToken 写与 SaveAtomic 读，防止并发写回半更新 token」）。写侧确实
// 全程持锁（RefreshToken 第 2 段：锁内写 AccessToken/RefreshToken/Domain/ExpiresAt），
// 但**所有出站请求头构造**都是无锁直读 a.AccessToken：
//   - Client.ChatHeaders（headers.go）
//   - Client.BillingHeaders（headers.go）
//   - Client.fetchEnterpriseModels / fetchV3Models（client.go）
//   - Client.CommonHeaders（headers.go）
//
// 生产上这两侧真的会并发：Scheduler.RunKeepaliveNow 定时对**每个**非禁用账号调
// RefreshToken（与该账号是否有在途请求无关），而 handler 正基于同一个 *auth.Auth
// 指针构造 chat 请求头——Pool.AuthByUID/List 返回的就是池内同一个对象。
// 本测试用 -race 复现该竞争；修复后应无告警。
func TestChatHeadersRacesRefreshToken(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(r.URL.Path, "/v2/plugin/auth/token/refresh") {
			return nil, errors.New("wrong path: " + r.URL.Path)
		}
		// domain 也带非 global 值：让 RefreshToken 的 `a.Domain = tok.Domain` 真正执行，
		// 从而使 ChatHeaders 分支里的 X-Domain 读取同样进入竞争面（否则该写入被
		// `if tok.Domain != ""` 挡掉，Domain 竞争不可见）。
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","domain":"chat.example.com","expiresIn":3600}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写侧：模拟 keepalive 周期刷新（在 a.mu 内改写 AccessToken/RefreshToken/Domain/ExpiresAt，
	// 与请求流量无关）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = c.RefreshToken(a)
		}
	}()

	// 读侧：模拟在途请求构造出站头（读 AccessToken）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		req, err := http.NewRequest(http.MethodPost, "https://chat.example/v2/chat/completions", nil)
		if err != nil {
			t.Errorf("new request: %v", err)
			close(stop)
			return
		}
		for i := 0; i < 300; i++ {
			c.ChatHeaders(req, a, "", ChatMeta{})
			_ = req.Header.Get("Authorization")
		}
		close(stop)
	}()

	wg.Wait()
}

// TestRefreshTokenExpiresInSanityCap expiresIn 量级上限：上游脏值（如
// 99999999999 秒 ≈ 3170 年）不得把 ExpiresAt 推到荒谬未来（NeedsRefresh 永假
// → token 永不刷新反而真过期失效）。依据 pr134-watchlist-analysis.md #4 可选加固：
// 上限 10 年（实测 R-D 响应恒 expiresIn=5184000=60d，10 年是纯防御量级）。
// 超限按脏值处理：保留旧 ExpiresAt（与缺省分支同语义）。
func TestRefreshTokenExpiresInSanityCap(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":99999999999}}`), nil
	})
	oldExpiry := time.Now().Add(time.Hour).Unix()
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: oldExpiry}
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if a.ExpiresAt != oldExpiry {
		t.Errorf("脏 expiresIn 应保留旧 ExpiresAt=%d, got %d（被推到荒谬未来）", oldExpiry, a.ExpiresAt)
	}
	// token 本身仍应写回（脏 expiresIn 只否决过期时间，不否决凭证）。
	if a.AccessToken != "newat" || a.RefreshToken != "newrt" {
		t.Errorf("tokens not updated: %+v", a)
	}
}

// TestRefreshTokenExpiresInWithinCapApplied 正常量级（60d，实测 R-D 恒 5184000）
// 不受上限影响：ExpiresAt 照常推进。
func TestRefreshTokenExpiresInWithinCapApplied(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return jsonResp(200, `{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":5184000}}`), nil
	})
	a := &auth.Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1}
	before := time.Now().Unix()
	if err := c.RefreshToken(a); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	want := before + 5184000
	if a.ExpiresAt < want-2 || a.ExpiresAt > want+2 {
		t.Errorf("ExpiresAt=%d want ~%d (60d 正常推进)", a.ExpiresAt, want)
	}
}

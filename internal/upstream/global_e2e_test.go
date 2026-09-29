//go:build global_e2e

// global_e2e_test.go 国际版（Global Realm）真实账号端到端实测探针。
//
// 用法（逐项独立运行，只记录响应不断言）：
//
//	go test -tags=global_e2e -run TestGlobalE2EChatPathOrdering -v -count=1
//	go test -tags=global_e2e -run TestGlobalE2ESSEUsageCredit  -v -count=1
//	go test -tags=global_e2e -run TestGlobalE2E -v -count=1   # 全部
//
// 日常 `go test ./...` 不编译本文件（build tag global_e2e 隔离）。
//
// 边界：只对真实 global 账号做只读探测（除任务明确列出的 chat/trial 探测外不写上游）；
// 不调用 SaveAtomic（绝不改 auths/ 落盘凭证）；token 只以脱敏形式出现在日志。
package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

func init() {
	auth.SetGlobalEnabled(true)
}

// globalE2EBase 缺省 global base（与 Client 缺省同源，不依赖具体配置）。
const globalE2EBase = "https://www.workbuddy.ai"

// globalE2EClient 构造带真实 global 配置的 Client（global base 空 → 默认 workbuddy.ai）。
// 60s 兜底超时：防止探测偶发挂起拖死测试。
func globalE2EClient() *Client {
	return &Client{
		HTTP:          &http.Client{Timeout: 60 * time.Second},
		ChatHTTP:      &http.Client{Timeout: 60 * time.Second},
		GlobalEnabled: true,
	}
}

// findGlobalAuthFile 在候选路径里找 realm=global 的 auth 文件并返回绝对路径。
func findGlobalAuthFile(t *testing.T) string {
	t.Helper()
	dirs := []string{"auths", "../auths", "../../auths", "/root/free2api/auths"}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "workbuddy*.json"))
		if err != nil {
			continue
		}
		for _, f := range files {
			raw, err := os.ReadFile(f)
			if err != nil {
				continue
			}
			var probe struct {
				Auth struct {
					Realm  string `json:"realm"`
					Domain string `json:"domain"`
				} `json:"auth"`
				Realm  string `json:"realm"`
				Domain string `json:"domain"`
			}
			_ = json.Unmarshal(raw, &probe)
			realm := probe.Auth.Realm
			if realm == "" {
				realm = probe.Realm
			}
			domain := probe.Auth.Domain
			if domain == "" {
				domain = probe.Domain
			}
			if realm == "global" || (domain != "" && strings.Contains(strings.ToLower(domain), "workbuddy.ai")) {
				return f
			}
		}
	}
	t.Skipf("no realm=global auth file found (searched %v)", dirs)
	return ""
}

// loadGlobalAcct 从 auths/ 读取真实 global 账号。每次调用重新解析磁盘文件
// （绝不复用已被 RefreshToken 改写的实例，保证各探测相互独立）。
func loadGlobalAcct(t *testing.T) *auth.Auth {
	t.Helper()
	fp := findGlobalAuthFile(t)
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Skipf("read auth file: %v", err)
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Skipf("parse auth file: %v", err)
	}
	if a.Realm() != "global" {
		t.Skipf("auth not global: realm=%s domain=%s", a.Realm(), a.Domain)
	}
	a.FilePath = fp
	return a
}

// redactToken 日志脱敏：只露出前缀 + 长度，绝不打印完整 token。
func redactToken(tok string) string {
	if len(tok) <= 12 {
		return "<redacted>"
	}
	return tok[:12] + "...(" + strconv.Itoa(len(tok)) + " chars)"
}

// chatProbeBody 组装 chat 探测请求体（与生产同链路：prepareBody + ensureConsoleSystem）。
// 首条消息非 system（探针用 user），确保 console 域兜底 system 注入被触发验证。
func chatProbeBody(cl *Client, a *auth.Auth, model string, maxTokens int) []byte {
	raw, _ := json.Marshal(map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": maxTokens,
		"messages": []any{
			map[string]any{"role": "user", "content": "Reply with the single word 'ok'."},
		},
	})
	prepared := cl.prepareBody(raw, a.Realm(), a.UID, "")
	if cl.globalOn(a) {
		prepared = ensureConsoleSystem(prepared)
	}
	return prepared
}

// doProbe 发一个原样请求并返回 HTTP status + body（上限 1MB）。
// headerFn 形如 func(req, auth)（不携带 clientIP 的为 nil 感知包装，见调用处）。
func doProbe(cl *Client, a *auth.Auth, method, url string, headerFn func(*http.Request, *auth.Auth), body []byte) (int, []byte) {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return 0, []byte("construct req: " + err.Error())
	}
	if headerFn != nil {
		headerFn(req, a)
	}
	resp, err := cl.HTTP.Do(req)
	if err != nil {
		return 0, []byte("http: " + err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

// logResp 打印一次探测的状态码 + body（截断到 max 字符，超长补省略号）。
func logResp(t *testing.T, label string, status int, body []byte, max int) {
	t.Helper()
	bodyStr := strings.TrimSpace(string(body))
	if max > 0 && len(bodyStr) > max {
		bodyStr = bodyStr[:max] + "...(truncated)"
	}
	t.Logf("=== %s ===\nstatus=%d\nbody=%s", label, status, bodyStr)
}

// captureNonZeroCodes 从 body 抓业务 code 键（不同于 0 的）打日志，供 R4 错误码对比。
func captureNonZeroCodes(t *testing.T, label string, status int, body []byte) {
	t.Helper()
	codes := findNonZeroCodes(string(body))
	if len(codes) > 0 {
		t.Logf("R4 %s 携带非 0 code: %s (http=%d)", label, strings.Join(codes, " | "), status)
	}
}

// findNonZeroCodes 扫描 body 中 JSON 形态的 `"code":N`（N!=0），返回去重后的取值列表。
// 提示：billing 信封的 `"code":0`（OK）被跳过；后续出现 `"code":14051` 之类即被捕获。
func findNonZeroCodes(body string) []string {
	seen := map[string]bool{}
	var out []string
	for i := 0; i < len(body); {
		j := strings.Index(body[i:], `"code"`)
		if j < 0 {
			break
		}
		i += j + len(`"code"`)
		// 跳过 "code": 后的空白、可选引号，取连续数字段。
		k := i
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if k < len(body) && body[k] == ':' {
			k++
		}
		for k < len(body) && (body[k] == ' ' || body[k] == '\t') {
			k++
		}
		if k < len(body) && body[k] == '"' {
			k++
		}
		start := k
		for k < len(body) && body[k] >= '0' && body[k] <= '9' {
			k++
		}
		if start < k {
			v := body[start:k]
			if v != "0" && !seen[v] {
				seen[v] = true
				out = append(out, v)
			}
		}
		if k <= i {
			i++ // 无数字段（如 "code": 后跟非数字），防止死循环
		} else {
			i = k
		}
	}
	return out
}

// resourceBody get-user-resource 请求体（生产 UserResource 同款）。
func resourceBody() map[string]any {
	now := time.Now()
	return map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format("2006-01-02 15:04:05"),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format("2006-01-02 15:04:05"),
	}
}

// ============================================================================
// 1. Chat 路径定序（R2）：console 优先还是 v2 才通？
// ============================================================================
func TestGlobalE2EChatPathOrdering(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)
	body := chatProbeBody(cl, a, "gpt-5.4", 1)
	headers := func(req *http.Request, ac *auth.Auth) { cl.ChatHeaders(req, ac, "", ChatMeta{}) }

	results := map[string]int{}
	for _, p := range []string{"/console/chat/completions", "/v2/chat/completions"} {
		status, raw := doProbe(cl, a, http.MethodPost, globalE2EBase+p, headers, body)
		captureNonZeroCodes(t, "chat:"+p, status, raw)
		logResp(t, "Chat "+p, status, raw, 500)
		results[p] = status
	}
	// 无论 console 结果如何，都记录两者状态；生产 fallback 只在 console 为 404/405 时触发。
	console, v2 := results["/console/chat/completions"], results["/v2/chat/completions"]
	switch {
	case console >= 200 && console < 300:
		t.Logf("结论#1: console %d 直接通 → console 优先成立", console)
	case console == 404 || console == 405:
		t.Logf("结论#1: console %d → 生产 ChatStream 会 fallback v2（v2=%d）", console, v2)
	default:
		t.Logf("结论#1: console %d（非 404/405）且 v2=%d → 生产 ChatStream 当场返回不换路；需判定 v2 是否可用/路径定序是否该放宽", console, v2)
	}
}

// TestGlobalE2EProbeChatVariants 探测 chat 403「request illegal」(code 11140) 的成因：
// 尝试模型名 / 设备头 / 归因头 / Content-Type 等变量，判断是模型名问题还是风控头问题。
// 只记录响应，不断言。
func TestGlobalE2EProbeChatVariants(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)

	type variant struct {
		name  string
		model string
		extra func(*http.Request)
	}
	variants := []variant{
		{"bare-model-gpt5.4", "gpt-5.4", nil},
		{"bare-model-default", "default-model", nil},
		{"token-dummy", "gpt-5.4", func(r *http.Request) { r.Header.Set("X-Device-Token", "test-dummy-token") }},
		{"agent-purpose", "gpt-5.4", func(r *http.Request) {
			r.Header.Set("X-Agent-Purpose", "conversation")
			r.Header.Set("X-IDE-Name", "WorkBuddy")
			r.Header.Set("X-IDE-Type", "WorkBuddy")
			r.Header.Set("X-IDE-Version", "5.5.4")
			r.Header.Set("X-Product", "WorkBuddy")
		}},
		{"no-content-type", "gpt-5.4", func(r *http.Request) { r.Header.Del("Content-Type") }},
	}
	for _, v := range variants {
		body := chatProbeBody(cl, a, v.model, 1)
		req, err := http.NewRequest(http.MethodPost, globalE2EBase+"/console/chat/completions", bytes.NewReader(body))
		if err != nil {
			t.Logf("variant %s: construct %v", v.name, err)
			continue
		}
		cl.ChatHeaders(req, a, "", ChatMeta{})
		if v.extra != nil {
			v.extra(req)
		}
		resp, err := cl.HTTP.Do(req)
		if err != nil {
			t.Logf("variant %s: transport %v", v.name, err)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		t.Logf("variant %-18s http=%d body=%s", v.name, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	t.Logf("补充#1: 若全部 403 → 非模型名/头变量导致，指向账号级推理门禁（与 billing/refresh/trial 可用形成对照）；结合无token=401、真token=403，判定为后端业务层拦截，非网关/路径/网络")
}

// ============================================================================
// 2. Billing 路径定序（R1）：get-user-resource 无 /v2 是否可用？
// ============================================================================
func TestGlobalE2EBillingOrdering(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)
	raw, _ := json.Marshal(resourceBody())
	headers := func(req *http.Request, ac *auth.Auth) { cl.BillingHeaders(req, ac) }

	for i, p := range []string{"/billing/meter/get-user-resource", "/v2/billing/meter/get-user-resource"} {
		status, body := doProbe(cl, a, http.MethodPost, globalE2EBase+p, headers, raw)
		captureNonZeroCodes(t, "billing:"+p, status, body)
		logResp(t, "Billing "+p, status, body, 600)
		if status >= 200 && status < 300 {
			t.Logf("结论#2: %s 直接通 → workbuddy.ai 走无 /v2 前缀（R1 成立）", p)
			return
		}
		if i == 0 {
			t.Logf("结论#2 (首探): 无 /v2 路径返回 %d（非 2xx）→ 生产 billingMeterJSON 只在 404 时换 /v2，需人工判定是否要放宽", status)
		}
	}
}

// ============================================================================
// 3. SSE 末帧 usage.credit 同构性（R9）—— 最重要
// ============================================================================
func TestGlobalE2ESSEUsageCredit(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)

	// 模型候选：gpt-5.4 首选；若上游模型级拒绝则换 default-model 再打一次。
	for _, model := range []string{"gpt-5.4", "default-model"} {
		body := chatProbeBody(cl, a, model, 10)
		rc, status, respBody, err := cl.ChatStream(a, body, "", ChatMeta{})
		if err != nil {
			t.Logf("R9 尝试 model=%s: transport error: %v", model, err)
			continue
		}
		if status >= 400 {
			captureNonZeroCodes(t, "chat stream:"+model, status, respBody)
			logResp(t, "R9 chat "+model+" (http err)", status, respBody, 500)
			continue
		}
		usage, frames, lastFrame := readSSESummary(rc)
		t.Logf("R9 model=%s SSE frames=%d lastFrame=%s", model, frames, lastFrame)
		reportUsageR9(t, usage)
		if usage != nil {
			return // 拿到带 usage 的末帧即定论
		}
	}
	t.Logf("R9: 两个模型都未取到带 usage 的末帧（可能被上游拦截/限流），见上方原始响应")
}

// readSSESummary 完整读 SSE 流：返回末帧 JSON 对象、有效 data 帧数、末帧原文。
func readSSESummary(rc io.ReadCloser) (map[string]any, int, string) {
	defer rc.Close()
	br := bufio.NewReaderSize(rc, 64*1024)
	var lastFrame string
	frames := 0
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			trimmed := strings.TrimRight(line, "\r\n")
			if strings.HasPrefix(trimmed, "data: ") {
				p := strings.TrimPrefix(trimmed, "data: ")
				if p == "[DONE]" {
					break
				}
				frames++
				lastFrame = p
			}
		}
		if err != nil {
			break
		}
	}
	var obj map[string]any
	if lastFrame != "" {
		_ = json.Unmarshal([]byte(lastFrame), &obj)
	}
	return obj, frames, lastFrame
}

// reportUsageR9 输出 R9 三态判定证据（credit 有无、token 字段名、与 CN 口径是否同构）。
func reportUsageR9(t *testing.T, usage map[string]any) {
	t.Helper()
	if usage == nil {
		t.Logf("R9 判定: 末帧无 usage 对象 → 三态(c)：缺 credit 观测，账本退化为 unknown（当前代码安全处理：缺失不记账）")
		return
	}
	uJSON, _ := json.Marshal(usage)
	creditRaw, hasCreditKey := usage["credit"]
	_, hasPrompt := usage["prompt_tokens"]
	_, hasCompletion := usage["completion_tokens"]
	hasTokens := hasPrompt || hasCompletion

	t.Logf("R9 usage(末帧) JSON: %s", string(uJSON))
	switch {
	case hasCreditKey:
		creditStr := "<nil>"
		if creditRaw != nil {
			if f, ok := creditRaw.(float64); ok {
				creditStr = strconv.FormatFloat(f, 'f', -1, 64)
			} else {
				raw, _ := json.Marshal(creditRaw)
				creditStr = string(raw)
			}
		}
		t.Logf("R9 判定: usage 带 credit=%s → 三态(a)：账本方案成立（global 免费/0 消耗号自动进 tier0）", creditStr)
	case hasTokens:
		t.Logf("R9 判定: usage 有 token 字段但无 credit → 口径带 token 缺 credit（缺失≠0，账本不污染），当前代码安全处理")
	default:
		t.Logf("R9 判定: usage 存在但既无 credit 也无 token 字段 → 口径不同，parseSSELine 无需适配亦可安全工作")
	}
}

// ============================================================================
// 4. Token refresh 路径
// ============================================================================
func TestGlobalE2ERefreshToken(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)
	oldAt, oldExp, oldRt := a.AccessToken, a.ExpiresAt, a.RefreshToken

	err := cl.RefreshToken(a)
	if err != nil {
		logResp(t, "RefreshToken error(无 HTTP status)", 0, []byte(err.Error()), 500)
		t.Logf("结论#4: RefreshToken 失败: %v", err)
		return
	}
	t.Logf("结论#4: RefreshToken 成功")
	t.Logf("  旧 accessToken=%s", redactToken(oldAt))
	t.Logf("  新 accessToken=%s (变化=%v)", redactToken(a.AccessToken), a.AccessToken != oldAt)
	t.Logf("  refreshToken 变化=%v (旧=%s 新=%s)", a.RefreshToken != oldRt, redactToken(oldRt), redactToken(a.RefreshToken))
	t.Logf("  expiresAt old=%d new=%d delta=%ds", oldExp, a.ExpiresAt, a.ExpiresAt-oldExp)
	if a.AccessToken == oldAt {
		t.Logf("  (新 token 与旧相同——上游可能返回同 token，路径本身可用)")
	}
	// 本轮在内存改写了 token：不落盘（禁止改 auths/），后续测试各自重新解析磁盘。
}

// ============================================================================
// 5. FetchModels 路径（global 模型目录探测）
// ============================================================================
func TestGlobalE2EFetchModels(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)
	headers := func(req *http.Request, ac *auth.Auth) { cl.CommonHeaders(req, ac) }

	paths := []string{
		"/v2/enterprises/personal/models",      // 家族首选（PR #20 实测 200 完整模型表）
		"/console/enterprises/personal/models", // /console 兜底（旧路径或 500）
	}
	probed := false
	for _, p := range paths {
		status, body := doProbe(cl, a, http.MethodGet, globalE2EBase+p, headers, nil)
		captureNonZeroCodes(t, "models:"+p, status, body)
		if status == 200 {
			logResp(t, "GlobalModels "+p, status, body, 1200)
			names, infos, _, _, perr := parseGlobalModelNames(body)
			if perr != nil {
				t.Logf("结论#5: %s 200 但解析失败: %v", p, perr)
			} else {
				t.Logf("结论#5: %s 200 返回 %d 个模型名: %v", p, len(names), names)
				if len(infos) > 0 {
					mi := infos[0]
					t.Logf("  富字段样本[0]: id=%s name=%s desc=%.40s credits=%s tags=%v vendor=%s toolCall=%v onlyReasoning=%v maxAllowed=%d reasoning(effort=%s summary=%s) ctx=%d maxout=%d",
						mi.ID, mi.Name, mi.Description, mi.Credits, mi.Tags, mi.Vendor, mi.SupportsToolCall, mi.OnlyReasoning, mi.MaxAllowedSize, mi.ReasoningEffort, mi.ReasoningSummary, mi.ContextWindow, mi.MaxTokens)
				} else {
					t.Logf("  对象形态未命中（窄表）→ infos nil")
				}
				probed = true
				if len(names) == len(GlobalModelNames) {
					t.Logf("  数量恰等于静态名单 %d → 探测独有=0，overlay 无新增", len(GlobalModelNames))
				}
			}
			break
		}
		logResp(t, "GlobalModels "+p, status, body, 300)
	}
	// 再调 FetchGlobalModels 看合并结果与负缓存行为。
	merged := cl.FetchGlobalModels(a)
	t.Logf("结论#5b: FetchGlobalModels 合并结果 %d 名（探测=%v）", len(merged), probed)
	if !probed {
		t.Logf("  探测未通 → 回落静态名单（符合 design：失败负缓存 + 静态兜底）")
	}
}

// ============================================================================
// 6. Trial 端点（只读探测：GET 优先，POST 记录响应）
// ============================================================================
func TestGlobalE2ETrial(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)
	headers := func(req *http.Request, ac *auth.Auth) { cl.BillingHeaders(req, ac) }

	status, body := doProbe(cl, a, http.MethodGet, globalE2EBase+"/billing/ide/trial", headers, nil)
	captureNonZeroCodes(t, "trial GET", status, body)
	logResp(t, "Trial GET", status, body, 500)

	status2, body2 := doProbe(cl, a, http.MethodPost, globalE2EBase+"/billing/ide/trial", headers, nil)
	captureNonZeroCodes(t, "trial POST", status2, body2)
	logResp(t, "Trial POST", status2, body2, 500)

	claimed, cerr := cl.ClaimTrial(a)
	t.Logf("结论#6: ClaimTrial claimed=%v err=%v", claimed, cerr)
	switch {
	case cerr == nil && claimed:
		t.Logf("  —— 新领取成功（该号此前未领过 trial 包）")
	case cerr == nil:
		t.Logf("  —— 幂等已领（POST 返回码 14051 被识别为正常）")
	default:
		t.Logf("  —— ClaimTrial 报错：%v（见上方原始响应判定）", cerr)
	}
}

// ============================================================================
// 7. 错误码语义（R4）：跨端点聚焦扫描非 0 code（各探测已各自 capture）。
// ============================================================================
func TestGlobalE2EErrorCodeSemantics(t *testing.T) {
	cl := globalE2EClient()
	a := loadGlobalAcct(t)
	raw, _ := json.Marshal(resourceBody())
	rawChat := chatProbeBody(cl, a, "gpt-5.4", 1)
	chatHeaders := func(req *http.Request, ac *auth.Auth) { cl.ChatHeaders(req, ac, "", ChatMeta{}) }
	billHeaders := func(req *http.Request, ac *auth.Auth) { cl.BillingHeaders(req, ac) }
	commonHeaders := func(req *http.Request, ac *auth.Auth) { cl.CommonHeaders(req, ac) }

	type endpoint struct {
		label  string
		method string
		url    string
		header func(*http.Request, *auth.Auth)
		body   []byte
	}
	eps := []endpoint{
		{"chat console", http.MethodPost, globalE2EBase + "/console/chat/completions", chatHeaders, rawChat},
		{"chat v2", http.MethodPost, globalE2EBase + "/v2/chat/completions", chatHeaders, rawChat},
		{"billing get-resource", http.MethodPost, globalE2EBase + "/billing/meter/get-user-resource", billHeaders, raw},
		{"models v2", http.MethodGet, globalE2EBase + "/v2/enterprises/personal/models", commonHeaders, nil},
		{"trial POST", http.MethodPost, globalE2EBase + "/billing/ide/trial", billHeaders, nil},
	}
	for _, ep := range eps {
		status, body := doProbe(cl, a, ep.method, ep.url, ep.header, ep.body)
		captureNonZeroCodes(t, ep.label, status, body)
	}
	t.Logf("结论#7: 各端点非 0 code 见上 → global 侧错误码对照表已采集（11140 chat 风控 / 14051 trial 已领 / 401-无效-Unauthorized），Classify 是否需适配见报告")
}

// ============================================================================
// 8/9/10. 网关侧验证（调本地 :7863）
// ============================================================================

// gatewayConfig 从 config.json 读 listen/api_key（读失败回落默认 :7863 / test-key）。
type gatewayConfig struct {
	Listen string `json:"listen"`
	APIKey string `json:"api_key"`
}

func loadGatewayConfig() gatewayConfig {
	gc := gatewayConfig{Listen: ":7863", APIKey: "test-key"}
	for _, cand := range []string{"config.json", "../config.json", "../../config.json", "/root/free2api/config.json"} {
		if raw, err := os.ReadFile(cand); err == nil {
			if err := json.Unmarshal(raw, &gc); err == nil {
				break
			}
		}
	}
	return gc
}

// gwBase 归一化网关 listen 地址为 http://host:port。
func gwBase(gc gatewayConfig) string {
	h := strings.TrimSpace(gc.Listen)
	h = strings.TrimPrefix(h, ":")
	if h == "" {
		h = "7863"
	}
	return "http://127.0.0.1:" + h
}

func gwGet(t *testing.T, gc gatewayConfig, path string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, gwBase(gc)+path, nil)
	req.Header.Set("Authorization", "Bearer "+gc.APIKey)
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		t.Logf("gateway %s unreachable: %v", path, err)
		return 0, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw
}

// TestGlobalE2EGatewayModels — #8 /v1/models 含 global:* 前缀。
func TestGlobalE2EGatewayModels(t *testing.T) {
	gc := loadGatewayConfig()
	status, body := gwGet(t, gc, "/v1/models")
	if status == 0 {
		return
	}
	logResp(t, "GW /v1/models", status, body, 600)
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Logf("结论#8: /v1/models 解析失败: %v", err)
		return
	}
	var globals, cn []string
	for _, m := range out.Data {
		switch {
		case strings.HasPrefix(m.ID, "global:"):
			globals = append(globals, m.ID)
		case strings.HasPrefix(m.ID, "cn:"):
			cn = append(cn, m.ID)
		}
	}
	t.Logf("结论#8: 模型总数=%d global:%d cn:%d", len(out.Data), len(globals), len(cn))
	t.Logf("  global 样例: %v", globals)
	t.Logf("  双前缀输出 %s", map[bool]string{true: "成立", false: "缺失"}[len(globals) > 0 && len(cn) > 0])
}

// TestGlobalE2EGatewayStatus — #9 /status realm 呈现。
func TestGlobalE2EGatewayStatus(t *testing.T) {
	gc := loadGatewayConfig()
	status, body := gwGet(t, gc, "/status")
	if status == 0 {
		return
	}
	logResp(t, "GW /status", status, body, 1500)
	var out struct {
		Accounts    []map[string]any          `json:"accounts"`
		RealmTotals map[string]map[string]int `json:"realm_totals"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Logf("结论#9: /status 解析失败: %v", err)
		return
	}
	for i, acct := range out.Accounts {
		if realm, _ := acct["realm"].(string); realm == "global" {
			t.Logf("结论#9: 找到 global 账号（index=%d） realm=%s uid=%v nickname=%v", i, realm, acct["uid"], acct["nickname"])
		}
	}
	t.Logf("结论#9: realm_totals = %v", out.RealmTotals)
	t.Logf("  realm_totals 双分组 %s", map[bool]string{true: "成立", false: "缺分组"}[len(out.RealmTotals) == 2])
}

// TestGlobalE2EGatewayChat — #10 网关端到端 chat（global: 前缀路由全链路）。
func TestGlobalE2EGatewayChat(t *testing.T) {
	gc := loadGatewayConfig()
	reqBody, _ := json.Marshal(map[string]any{
		"model":      "global:gpt-5.4",
		"stream":     true,
		"max_tokens": 10,
		"messages": []any{
			map[string]any{"role": "user", "content": "Reply with the single word 'ok'."},
		},
	})
	req, _ := http.NewRequest(http.MethodPost, gwBase(gc)+"/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+gc.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		t.Logf("结论#10: 网关 chat unreachable: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	t.Logf("结论#10: 网关 chat HTTP status=%d content-type=%s", resp.StatusCode, resp.Header.Get("Content-Type"))
	if resp.StatusCode == 200 {
		usage, frames, lastFrame := readSSESummary(io.NopCloser(bytes.NewReader(body)))
		t.Logf("  网关 SSE frames=%d lastFrame=%s", frames, lastFrame)
		reportUsageR9(t, usage)
		// 首帧 model 应为裸名（前缀已被网关剥掉）。
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "data: ") && line != "data: [DONE]" {
				var first map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &first) == nil {
					if m, ok := first["model"].(string); ok {
						t.Logf("  出站 model 字段 = %q（不含 global: 前缀 %s）", m, map[bool]string{true: "✓裸名", false: "✗仍带前缀"}[!strings.HasPrefix(m, "global:")])
					}
					break
				}
			}
		}
	} else {
		bodyStr := strings.TrimSpace(string(body))
		if len(bodyStr) > 500 {
			bodyStr = bodyStr[:500] + "...(truncated)"
		}
		t.Logf("  非 2xx body: %s", bodyStr)
	}
	t.Logf("  注#10: 选号/出站域/日志见容器 docker logs（uid 见 auths/ 下 global 账号，host workbuddy.ai）")
}

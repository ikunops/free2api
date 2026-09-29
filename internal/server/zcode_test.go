package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/source"
	"free2api/internal/upstream"
)

// zcodeServerFake 智谱上游 fake：按路径分流 zcode 侧（/api/paas/v4/...）与
// workbuddy 侧（/console/... 、/v3/config，一律 404），记录每次请求的路径/鉴权头/请求体。
//
// 为什么必须按路径而不仅是 host 分流：roundTripFunc 忽略 host，两类上游共用同一
// transport，只有路径能把「智谱 /models」与「workbuddy /console/enterprises/personal/models」
// 区分开——否则 workbuddy 动态模型拉取会误读到智谱清单，把「归属推断」测成恒真。
type zcodeServerFake struct {
	mu    sync.Mutex
	paths []string
	authz []string
	bodys []string
}

func (f *zcodeServerFake) snap() (paths, authz, bodys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.paths...), append([]string(nil), f.authz...), append([]string(nil), f.bodys...)
}

// newZCodeServerFake 返回 fake 与指向它的 upstream.Client（ChatBaseZCode 覆盖）。
func newZCodeServerFake(t *testing.T) (*zcodeServerFake, *upstream.Client) {
	t.Helper()
	f := &zcodeServerFake{}
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			// GET 请求的 Body 为 nil（net/http 只在有实体时给 Body），必须先判空。
			var body []byte
			if r.Body != nil {
				body, _ = io.ReadAll(r.Body)
			}
			f.mu.Lock()
			f.paths = append(f.paths, r.URL.Path)
			f.authz = append(f.authz, r.Header.Get("Authorization"))
			f.bodys = append(f.bodys, string(body))
			f.mu.Unlock()
			reply := func(status int, ct, body string) (*http.Response, error) {
				return &http.Response{StatusCode: status,
					Header: http.Header{"Content-Type": []string{ct}},
					Body:   io.NopCloser(strings.NewReader(body))}, nil
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/api/paas/v4/models"):
				return reply(200, "application/json", `{"data":[{"id":"glm-4.6"},{"id":"glm-5.3"}]}`)
			case strings.HasSuffix(r.URL.Path, "/api/paas/v4/chat/completions"):
				return reply(200, "text/event-stream", sseOK)
			default:
				// workbuddy 侧（console 模型表 / v3 config / token refresh）一律 404：
				// 本测试的池里只有 zcode 号，任何落到 workbuddy 上游的调用都是错的
				//（刷新端点若被调用，恰恰就是 503 那个 bug 的指纹）。
				return reply(404, "application/json", `{"code":404,"msg":"not found"}`)
			}
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
		// base 带 /api/paas/v4 尾巴（与真实智谱 base 同形）：出站路径 = base + "/chat/completions"，
		// fake 按这个尾巴把两类上游分流。
		ChatBaseZCode: "https://zcode-fake.example/api/paas/v4",
	}
	return f, up
}

// zcodeTestPool 建一个只有 zcode 号的池：凭据 = 静态 API Key（expiresAt=0，
// 无 refreshToken），producer 落在凭证自带字段上（单文件整体迁移语义）。
func zcodeTestPool(t *testing.T) (*upstream.Client, *zcodeServerFake, *Handler) {
	t.Helper()
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	f, up := newZCodeServerFake(t)
	a := &auth.Auth{UID: "zc1", AccessToken: "key.secret", ExpiresAt: 0,
		Domain: "https://open.bigmodel.cn/api/paas/v4", Nickname: "zcode-一号"}
	a.SetProducer(upstream.ProducerZCode)
	p := testPoolWith(a)
	h := NewHandler(Config{Pool: p, Upstream: up})
	return up, f, h
}

// TestZCodeChatExplicitPrefix 回归：zcode 号（静态密钥、expiresAt=0）必须能直接反代
// 出请求，而不是 503 no_healthy_account。
//
// 曾经的失败链路：NeedsRefresh 把「零 expiresAt」判成「需刷新」→ handler 每个请求
// 先打 workbuddy 的 /v2/plugin/auth/token/refresh → zcode 没有 refreshToken →
// 报错 → NoteError + 换号 → 池里每个号轮一遍 → 末端回固定文案 no_healthy_account。
// 现象极具误导性：/status 全绿、/v1/models 正常，只有 chat 恒 503。
func TestZCodeChatExplicitPrefix(t *testing.T) {
	_, f, h := zcodeTestPool(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:zcode:glm-4.6","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（want 200：zcode 号必须能直接反代）", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "你好") {
		t.Errorf("body 缺上游内容: %s", rec.Body)
	}

	paths, authz, bodys := f.snap()
	if len(paths) != 1 {
		t.Fatalf("上游调用 %d 次 (%v)，want 恰好 1 次 chat（刷新/列模型都不该发生）", len(paths), paths)
	}
	if !strings.HasSuffix(paths[0], "/api/paas/v4/chat/completions") {
		t.Errorf("路径 = %q, want 智谱 /api/paas/v4/chat/completions", paths[0])
	}
	// 凭据自带的 key 原样出站（不掺 workbuddy 头族、不改写）。
	if authz[0] != "Bearer key.secret" {
		t.Errorf("Authorization = %q, want Bearer key.secret", authz[0])
	}
	// 出站模型名剥掉路由前缀：上游只认裸名，收到 cn:zcode:glm-4.6 会 400。
	var sent struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal([]byte(bodys[0]), &sent); err != nil {
		t.Fatalf("出站 body 不是 JSON: %v (%s)", err, bodys[0])
	}
	if sent.Model != "glm-4.6" {
		t.Errorf("出站 model = %q, want glm-4.6（路由前缀必须剥干净）", sent.Model)
	}
}

// TestZCodeChatNonStreamAggregates 非流式客户端（stream:false）必须能拿到聚合后的
// 完整回复，而不是 502 upstream_parse。
//
// 曾经的失败链路：zcode 适配器「body 原样透传」→ 智谱收到 stream:false 回非流式
// JSON → handler 的 Aggregate 只认 SSE 帧 → 报 upstream_parse 502。修法是把出站
// stream 强制 true（upstream.zcodeForceStream），非流式由网关自己聚合。
func TestZCodeChatNonStreamAggregates(t *testing.T) {
	_, f, h := zcodeTestPool(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:zcode:glm-4.6","stream":false,"messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s（want 200：非流式由网关聚合，不该 502 upstream_parse）", rec.Code, rec.Body)
	}
	var resp struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("聚合响应不是 JSON: %v (%s)", err, rec.Body)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "你好" {
		t.Errorf("聚合内容 = %+v, want content=你好", resp.Choices)
	}

	// 出站 body 的 stream 必须是 true（网关统一走流式通道）。
	_, _, bodys := f.snap()
	if len(bodys) != 1 || !strings.Contains(bodys[0], `"stream":true`) {
		t.Errorf("出站 body 未强制流式: %v", bodys)
	}
}

// TestZCodeChatNoPenaltyOnSuccess 成功路径不罚号：ErrTotal/熔断计数保持 0
// （对照失败链路的副作用：每个号都会被 NoteError 记一笔 err_total）。
func TestZCodeChatNoPenaltyOnSuccess(t *testing.T) {
	_, _, h := zcodeTestPool(t)
	p := h.cfg.Pool

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"cn:zcode:glm-4.6","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	st, ok := p.Status("zc1")
	if !ok {
		t.Fatal("pool.Status(zc1) 未命中")
	}
	if st.ErrTotal != 0 || st.BreakerFails != 0 || st.Cooling || st.Disabled {
		t.Errorf("成功请求不该罚号: %+v", st)
	}
}

// TestZCodeModelsAndBareNameRouting /v1/models 列出 cn:zcode:* 段；且请求只写裸名
// "glm-4.6" 时按「账号自带模型归属」自动落到 zcode 号池（modelOwner 推断）。
func TestZCodeModelsAndBareNameRouting(t *testing.T) {
	_, f, h := zcodeTestPool(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("/v1/models code=%d", rec.Code)
	}
	var list struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("unmarshal /v1/models: %v", err)
	}
	var gotZCode bool
	for _, m := range list.Data {
		if m.ID == "cn:zcode:glm-4.6" {
			gotZCode = true
			if m.OwnedBy != upstream.ProducerZCode {
				t.Errorf("owned_by = %q, want zcode（模型归属要可见）", m.OwnedBy)
			}
		}
	}
	if !gotZCode {
		ids := make([]string, 0, len(list.Data))
		for _, m := range list.Data {
			ids = append(ids, m.ID)
		}
		t.Fatalf("/v1/models 缺 cn:zcode:glm-4.6，实际 = %v", ids)
	}

	// 裸名 "glm-4.6"：workbuddy 目录里没有（fake 对它 404）→ 归属判给 zcode。
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`)))
	if rec.Code != 200 {
		t.Fatalf("裸名 chat code=%d body=%s（want 200：归属应自动判给 zcode）", rec.Code, rec.Body)
	}
	paths, _, bodys := f.snap()
	if !contains(paths, "/api/paas/v4/chat/completions") {
		t.Errorf("裸名请求没落到智谱上游: %v", paths)
	}
	found := false
	for i := range paths {
		if paths[i] == "/api/paas/v4/chat/completions" && strings.Contains(bodys[i], `"model":"glm-4.6"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("智谱侧的出站 body 未带裸模型名: paths=%v bodys=%v", paths, bodys)
	}
}

// TestZCodeKeyHintFingerprint 管理页上的 zcode 视图不能回明文 key（KeyHint 只给指纹：
// 前 6 + 后 4）。
func TestZCodeKeyHintFingerprint(t *testing.T) {
	// 合成 key（同形状：32 hex + "." + 16 字符）；绝不放真实凭证进版本库。
	full := "0123456789abcdef0123456789abcdef.FAKEsuffix123456"
	hint := source.KeyHint(full)
	if strings.Contains(hint, "89abcdef") {
		t.Errorf("KeyHint(%q) = %q 泄露了中段", full, hint)
	}
	if !strings.HasPrefix(hint, "012345") {
		t.Errorf("KeyHint(%q) = %q want 前 6 位前缀", full, hint)
	}
}

// TestProducerLiteralsAgree 三处 ProducerZCode 常量必须逐字同值：auth 侧（NeedsRefresh
// 的静态密钥判据）不导出，靠行为锚定——producer 写 zcode 的凭证 NeedsRefresh 恒 false。
func TestProducerLiteralsAgree(t *testing.T) {
	if source.ProducerZCode != upstream.ProducerZCode {
		t.Fatalf("source.ProducerZCode=%q != upstream.ProducerZCode=%q",
			source.ProducerZCode, upstream.ProducerZCode)
	}
	a, err := auth.Parse([]byte(`{"accessToken":"k","expiresAt":0,"uid":"z","producer":"` + source.ProducerZCode + `"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if a.NeedsRefresh(0) {
		t.Errorf("producer=%q 的凭据 NeedsRefresh=true，auth 侧的静态密钥判据没对上这个常量",
			source.ProducerZCode)
	}
}

// TestOutputWhitelistPerProducer 回归：CN 与 zcode 的裸 id 重名（都叫 glm-4.6）时，
// 输出白名单必须按 producer 分开——勾 zcode 的那个不能把 CN 的一起放开。
//
// 背景：白名单在 output.json 里是字符串集合，历史实现用「裸 id」当键。zcode 接进来后
// 两家的裸 id 会重名（CN 有 cn:glm-4.6，智谱实时目录也有 glm-4.6），共用裸 id 会出现
// 「勾一个放开两个」的串台。修法：非 workbuddy 的模型用 "<producer>:<裸 id>" 当键。
func TestOutputWhitelistPerProducer(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)

	// 双路上游 fake：CN /v3/config 与智谱 /api/paas/v4/models 都吐 glm-4.6（故意重名），
	// 各自再带一个对方没有的 id——用来验证「没勾的确实被挡住」，而不是「碰巧没出现」。
	up := &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			reply := func(status int, ct, body string) (*http.Response, error) {
				return &http.Response{StatusCode: status,
					Header: http.Header{"Content-Type": []string{ct}},
					Body:   io.NopCloser(strings.NewReader(body))}, nil
			}
			switch {
			case strings.HasSuffix(r.URL.Path, "/api/paas/v4/models"):
				return reply(200, "application/json", `{"data":[{"id":"glm-4.6"},{"id":"glm-5.3"}]}`)
			case strings.HasSuffix(r.URL.Path, "/v3/config"):
				return reply(200, "application/json",
					`{"code":0,"data":{"models":[{"id":"glm-4.6"},{"id":"kimi-k2.5"}]}}`)
			default:
				return reply(404, "application/json", `{"code":404,"msg":"not found"}`)
			}
		})},
		ChatBaseCN:    "https://fake.example",
		BillingBaseCN: "https://fake.example",
		ChatBaseZCode: "https://zcode-fake.example/api/paas/v4",
	}
	cn := &auth.Auth{UID: "cn1", AccessToken: "cn.token", ExpiresAt: 4102444800,
		Domain: "https://fake.example"}
	zc := &auth.Auth{UID: "zc1", AccessToken: "key.secret", ExpiresAt: 0,
		Domain: "https://open.bigmodel.cn/api/paas/v4"}
	zc.SetProducer(upstream.ProducerZCode)
	h := NewHandler(Config{Pool: testPoolWith(cn, zc), Upstream: up, Output: NewOutputStore("")})

	// 1) 待发布清单里两家的 glm-4.6 都要在，且键不同。
	keys := map[string]string{}
	for _, m := range h.availableOutputModels() {
		if m["id"] == "glm-4.6" {
			keys[m["producer"].(string)] = m["key"].(string)
		}
	}
	if keys["workbuddy"] != "glm-4.6" {
		t.Fatalf("workbuddy 侧 glm-4.6 的键 = %q, want 裸 id（旧配置零回归）", keys["workbuddy"])
	}
	if keys[upstream.ProducerZCode] != "zcode:glm-4.6" {
		t.Fatalf("zcode 侧 glm-4.6 的键 = %q, want %q",
			keys[upstream.ProducerZCode], "zcode:glm-4.6")
	}

	// 2) 只发布 zcode 的 glm-4.6：CN 的 glm-4.6 与 CN 的 kimi-k2.5 都不该出现。
	if _, err := h.cfg.Output.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Models: []string{"zcode:glm-4.6"}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	got := map[string]bool{}
	for _, e := range h.modelList() {
		got[e["id"].(string)] = true
	}
	if !got["cn:zcode:glm-4.6"] {
		t.Errorf("勾了 zcode:glm-4.6，对外清单却没有 cn:zcode:glm-4.6（got=%v）", got)
	}
	if got["cn:glm-4.6"] {
		t.Errorf("勾 zcode 的 glm-4.6 把 CN 的也放开了（got=%v）：白名单又在用裸 id 当键", got)
	}
	if got["cn:kimi-k2.5"] {
		t.Errorf("没勾的 CN 模型 cn:kimi-k2.5 出现在对外清单（got=%v）", got)
	}
	if got["cn:zcode:glm-5.3"] {
		t.Errorf("没勾的 zcode 模型 cn:zcode:glm-5.3 出现在对外清单（got=%v）", got)
	}
}

// TestZCodeCatalogNoAccountNoNegativeCache 回归：池里还没有 zcode 号时拉目录不算失败，
// 不能写 5 分钟负缓存——否则「先开控制台（空池）→ 再导入 zcode 号」之后的 5 分钟里
// /v1/models 会少一整段，用户看到的是「导入了但模型没出来」。
func TestZCodeCatalogNoAccountNoNegativeCache(t *testing.T) {
	resetModelsCache()
	t.Cleanup(resetModelsCache)
	_, up := newZCodeServerFake(t)

	cn := &auth.Auth{UID: "cn1", AccessToken: "cn.token", ExpiresAt: 4102444800,
		Domain: "https://fake.example"}
	p := testPoolWith(cn)
	h := NewHandler(Config{Pool: p, Upstream: up})

	if got := h.fetchZCodeCatalog(); len(got) != 0 {
		t.Fatalf("池里没有 zcode 号时目录应为空，got=%v", got)
	}
	// 导入一个 zcode 号后必须立刻拉得到（没被上一次的「空池」写进负缓存）。
	zc := &auth.Auth{UID: "zc1", AccessToken: "key.secret", ExpiresAt: 0,
		Domain: "https://open.bigmodel.cn/api/paas/v4"}
	zc.SetProducer(upstream.ProducerZCode)
	p.Add(zc)

	got := h.fetchZCodeCatalog()
	if len(got) == 0 {
		t.Fatalf("导入 zcode 号后目录仍为空：空池那次被错误地写进了 5min 负缓存")
	}
	seen := map[string]bool{}
	for _, mi := range got {
		seen[mi.ID] = true
	}
	if !seen["glm-4.6"] {
		t.Errorf("目录缺 glm-4.6（got=%v）", seen)
	}
}

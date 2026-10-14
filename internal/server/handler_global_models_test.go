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
	"free2api/internal/upstream"
)

// globalModelsHandlerFake 探测 fake：记录模型目录 GET 请求次数/路径/鉴权头，并可控响应。
// 只服务于模型目录端点；chat 路径一律 404（本测试不触发 chat）。
type globalModelsHandlerFake struct {
	up *upstream.Client

	mu   sync.Mutex
	cnt  int    // 模型目录探测请求计数（含 console 与 /v2 家族）
	path string // 最近一次探测路径
	auth string // 最近一次探测鉴权头
	host string // 最近一次探测 Host

	status int
	body   string
}

func newGlobalModelsHandlerFake(t *testing.T, status int, body string) *globalModelsHandlerFake {
	t.Helper()
	cf := &globalModelsHandlerFake{up: &upstream.Client{}, status: status, body: body}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isCN := r.Header.Get("Authorization") == "Bearer at_cn"
		cf.mu.Lock()
		if !isCN {
			// cnt/path/auth/host 只记 global 探测请求；CN 动态拉取（FetchModels，
			// Bearer at_cn）不计——纯动态后 CN 面也需要 fake 数据源，但不能污染
			// global 探测计数断言（本文件所有 CN 号均为 at_cn）。
			cf.cnt++
			cf.path = r.URL.Path
			cf.auth = r.Header.Get("Authorization")
			cf.host = r.Host
		}
		status, body := cf.status, cf.body
		cf.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if isCN {
			// CN 动态拉取：返回含 cli agent 的动态模型表，让 CN 面在纯动态下有产出
			//（无静态兜底后需要真实数据源）。console 路径与 global 探测家族的
			// /console fallback 同名，故按鉴权头而非路径分流。
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"code":0,"data":{"models":[
				{"id":"cn-dyn-model","maxInputTokens":65536,"maxOutputTokens":8192}
			],"agents":[{"name":"cli","models":["cn-dyn-model"]}]}}`)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	cf.up = &upstream.Client{
		HTTP:           &http.Client{},
		ChatBaseCN:     strings.TrimSuffix(ts.URL, "/"), // CN 动态拉取同 fake（鉴权头分流）
		ChatBaseGlobal: strings.TrimSuffix(ts.URL, "/"),
		GlobalEnabled:  true,
	}
	return cf
}

// setProbe 更新 fake 的响应（并发安全：handler 侧读 status/body 也在同一把锁里）。
func (cf *globalModelsHandlerFake) setProbe(status int, body string) {
	cf.mu.Lock()
	cf.status = status
	cf.body = body
	cf.mu.Unlock()
}

func (cf *globalModelsHandlerFake) snapshot() (cnt int, path, auth, host string) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	return cf.cnt, cf.path, cf.auth, cf.host
}

// modelsProbeBody 构造探测端点对象数组响应。
func modelsProbeBody(ids ...string) string {
	var b strings.Builder
	b.WriteString(`{"code":0,"data":{"models":[`)
	for i, id := range ids {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"` + id + `","name":"` + id + `"}`)
	}
	b.WriteString(`]}}`)
	return b.String()
}

// TestModelListTwoFamilies 断言 /v1/models 同时含 cn:* 与 global:* 两族；
// global 名单 = 纯探测结果（不合并静态）；探测走 global base（httptest host）。
func TestModelListTwoFamilies(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/v1/models", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /v1/models code=%d", rec.Code)
	}
	var resp struct {
		Data []map[string]any `json:"data"`
	}
	if err := jsonUnmarshal(rec.Body.String(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	var cnIDs, globIDs []string
	for _, m := range resp.Data {
		id, _ := m["id"].(string)
		switch {
		case strings.HasPrefix(id, "global:"):
			globIDs = append(globIDs, strings.TrimPrefix(id, "global:"))
		case strings.HasPrefix(id, "cn:"):
			cnIDs = append(cnIDs, strings.TrimPrefix(id, "cn:"))
		}
	}
	if len(cnIDs) == 0 {
		t.Error("no cn:* models in /v1/models")
	}
	if len(globIDs) == 0 {
		t.Fatal("no global:* models in /v1/models")
	}
	// global 名单 = 纯探测结果：探测独有/下发全在；静态历史名单成员不出现。
	if !contains(globIDs, "probe-only-x") {
		t.Errorf("global models missing probe-only-x: %v", globIDs)
	}
	if contains(globIDs, "default-model") || contains(globIDs, "kimi-k2.6") {
		t.Errorf("global models must not contain unprobed static names: %v", globIDs)
	}
	if countOf(globIDs, "gpt-5.4") != 1 {
		t.Errorf("global models dedupe failed: gpt-5.4 count=%d", countOf(globIDs, "gpt-5.4"))
	}

	// 探测走 global base（httptest Host）+ Bearer 鉴权头；v3-config-merge 后单次探测
	// = /v3/config + /v2 企业路并发（cnt=2，path 记录的是最近一次——两路之一）。
	cnt, path, authz, host := cf.snapshot()
	if cnt != 2 {
		t.Errorf("probe cnt=%d want 2 (v3/config + v2 enterprise, concurrent)", cnt)
	}
	if path != "/v2/enterprises/personal/models" && path != "/v3/config" {
		t.Errorf("probe path=%q want one of [/v2/enterprises/personal/models /v3/config]", path)
	}
	if authz != "Bearer at_gl" {
		t.Errorf("probe authz=%q want Bearer at_gl", authz)
	}
	if host == "" || host == "fake.example" {
		t.Errorf("probe host=%q want global base (httptest)", host)
	}
	// CN 动态拉取走同一 fake（ChatBaseCN 同 host，console 路径分流返回动态模型表），
	// cn-dyn-model 已在上面 cnIDs 断言覆盖——不断言 CN 请求计数，避免耦合 CN 缓存重置时序。
}

// TestAdminOutputRefreshForcesProbe 管理面 ?refresh=1 强制重探：
//   - 首次探测失败（500）→ 负缓存，默认 /admin/output 不再打上游（cnt 不涨）；
//   - ?refresh=1 清掉负缓存 → 重探（cnt 增）且这次成功 → 清单里出现新模型。
// 这条对应「刷新按钮必须能刷新」：负缓存期内只能硬刷新/重启的旧行为由这个参数修掉。
func TestAdminOutputRefreshForcesProbe(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	failFirst := newGlobalModelsHandlerFake(t, 500, `{"code":500,"msg":"boom"}`)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: failFirst.up, GlobalEnabled: true, AdminEnabled: true})
	get := func(path string) string {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 {
			t.Fatalf("GET %s code=%d body=%s", path, rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	// 首探失败 → 负缓存生效。
	get("/admin/output")
	first, _, _, _ := failFirst.snapshot()
	if first == 0 {
		t.Fatal("首次 /admin/output 应当触发一次 global 探测")
	}
	// 负缓存期内默认路径不再打上游。
	get("/admin/output")
	again, _, _, _ := failFirst.snapshot()
	if again != first {
		t.Fatalf("负缓存期内默认 /admin/output 又探测了：cnt %d -> %d", first, again)
	}
	// 换成成功响应后 ?refresh=1：清负缓存 + 重探成功。
	failFirst.setProbe(200, modelsProbeBody("recovered-model"))
	body := get("/admin/output?refresh=1")
	after, _, _, _ := failFirst.snapshot()
	if after == again {
		t.Fatalf("?refresh=1 没有触发重探：cnt 仍为 %d", after)
	}
	if !strings.Contains(body, "recovered-model") {
		t.Fatalf("刷新后 available_models 应含 recovered-model；body 前 400 字：%s", truncateStr(body, 400))
	}
}

// truncateStr 测试辅助：截断长 body 便于失败信息打印。
func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TestModelListNoGlobalAccountEmpty 无 global 账号：global 名单为空（纯动态，无静态兜底），探测零调用。
func TestModelListNoGlobalAccountEmpty(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	got := h.modelList()
	var globIDs []string
	for _, m := range got {
		if id, ok := m["id"].(string); ok && strings.HasPrefix(id, "global:") {
			globIDs = append(globIDs, strings.TrimPrefix(id, "global:"))
		}
	}
	if len(globIDs) != 0 {
		t.Fatalf("no-global-account: global ids=%v want empty (pure dynamic)", globIDs)
	}
	cnt, _, _, _ := cf.snapshot()
	if cnt != 0 {
		t.Errorf("no-global-account: probe calls=%d want 0 (zero upstream calls)", cnt)
	}
}

// TestModelListProbeFailureEmpty 探测失败（家族全 500）→ global 名单为空（无静态回落）。
func TestModelListProbeFailureEmpty(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 500, `{"code":500,"msg":"boom"}`)
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	got := h.modelList()
	var globIDs []string
	for _, m := range got {
		if id, ok := m["id"].(string); ok && strings.HasPrefix(id, "global:") {
			globIDs = append(globIDs, strings.TrimPrefix(id, "global:"))
		}
	}
	if len(globIDs) != 0 {
		t.Errorf("probe-failure: global ids=%v want empty (no static fallback)", globIDs)
	}
}

// TestModelListProbeCacheWithinTTL 首次探测成功 → 同 Client 二次 /v1/models 不重复探测（零新上游请求）。
func TestModelListProbeCacheWithinTTL(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	h := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})

	h.modelList()
	cnt1, _, _, _ := cf.snapshot()
	// v3-config-merge：单次探测 = v3/config + /v2 企业路并发 = 2 个请求。
	if cnt1 != 2 {
		t.Fatalf("first probe calls=%d want 2 (v3 + v2, concurrent)", cnt1)
	}
	h.modelList()
	cnt2, _, _, _ := cf.snapshot()
	if cnt2 != cnt1 {
		t.Errorf("cache: second modelList probe calls=%d want %d (hit 1h cache)", cnt2, cnt1)
	}
}

// jsonUnmarshal 单一用途反序列化（httptest body 而非直接 resp）。
func jsonUnmarshal(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func countOf(list []string, s string) int {
	n := 0
	for _, v := range list {
		if v == s {
			n++
		}
	}
	return n
}

// TestExitModelListScopedToProducers 出口的「对外清单」必须和「能不能调」同口径。
// 反例（修复前实测）：zcode 专用口的 /v1/models 里躺着整个 global 族（global:gpt-5.4-* 等
// 27 个），客户端在模型选择器里选得到，一调就被「不在本出口的服务范围（本出口只服务：zcode）」
// 404 挡回来——列表和实际能力打架。根因是 modelList 的 global 分支漏了 producer 判定
// （CN 与 zcode 两段都有，只有 global 那段没有）。
func TestExitModelListScopedToProducers(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	resetModelsCache()

	cf := newGlobalModelsHandlerFake(t, 200, modelsProbeBody("gpt-5.4", "probe-only-x"))
	p := testPoolWith(
		&auth.Auth{UID: "cn1", AccessToken: "at_cn", Domain: "www.codebuddy.cn", ExpiresAt: 9999999999},
		&auth.Auth{UID: "g1", AccessToken: "at_gl", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)

	// 基线：主口（不设 ProducerAllow）两族照旧都列，global 族确实有产出。
	main := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true})
	mainIDs := map[string]string{}
	for _, m := range main.modelList() {
		id, _ := m["id"].(string)
		mainIDs[id] = id
	}
	if len(mainIDs) == 0 {
		t.Fatal("主口模型清单为空，本测试的前提（global 族有产出）不成立")
	}
	var baseGlobal int
	for id := range mainIDs {
		if strings.HasPrefix(id, "global:") {
			baseGlobal++
		}
	}
	if baseGlobal == 0 {
		t.Fatal("基线里没有 global:* 模型，没法验证「出口不该列它」")
	}

	// zcode 专用口：不许出现任何非 zcode 的模型（global 族就是本次回归的靶子）。
	exit := NewHandler(Config{Pool: p, Upstream: cf.up, GlobalEnabled: true,
		ProducerAllow: []string{"zcode"}})
	for _, m := range exit.modelList() {
		id, _ := m["id"].(string)
		if !strings.Contains(id, "zcode") {
			t.Errorf("zcode 专用口列出了别家的模型 %q：清单与可调用性不同口径", id)
		}
	}
}

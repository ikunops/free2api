package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeModelsDevDoc 构造一份 models.dev api.json 形态的 fake 文档（多 provider 采值
// 用例可控）。
func fakeModelsDevDoc(providers map[string]map[string][2]int64) string {
	var b strings.Builder
	b.WriteString("{")
	firstP := true
	for p, models := range providers {
		if !firstP {
			b.WriteString(",")
		}
		firstP = false
		b.WriteString(`"` + p + `":{"models":{`)
		firstM := true
		for id, lim := range models {
			if !firstM {
				b.WriteString(",")
			}
			firstM = false
			b.WriteString(`"` + id + `":{"limit":{"context":` +
				itoa(lim[0]) + `,"output":` + itoa(lim[1]) + `}}`)
		}
		b.WriteString("}}")
	}
	b.WriteString("}")
	return b.String()
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// fakeModelsDevServer 起 fake models.dev HTTP 服务（响应体/状态码/延迟可控）。
// 注意 client()：HTTP（非 TLS）httptest server 的 Client().Transport 是裸
// Transport（不重定向其他 host——实测请求会打到真实网络），必须用
// 自制 redirectTransport 把任意 URL 的请求都路由到 fake handler。
type fakeModelsDevServer struct {
	ts     *httptest.Server
	mu     sync.Mutex
	hits   int
	body   string
	status int
	delay  time.Duration
}

func newModelsDevServer(body string) *fakeModelsDevServer {
	f := &fakeModelsDevServer{body: body, status: 200}
	f.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		body, status, delay := f.body, f.status, f.delay
		f.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	return f
}

// client 返回把任意请求（含真实 models.dev URL）路由到 fake handler 的 client
// ——保证测试永不打真网。
func (f *fakeModelsDevServer) client() *http.Client {
	return &http.Client{Transport: redirectTransport{f}}
}

// redirectTransport 把所有请求转投给 fake models.dev handler。
type redirectTransport struct{ f *fakeModelsDevServer }

func (t redirectTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// 转投 fake server：重写请求 host，复用其 handler（计数/延迟/状态码全生效）。
	scheme := "http"
	if strings.HasPrefix(t.f.ts.URL, "https") {
		scheme = "https"
	}
	newURL := *r.URL
	newURL.Scheme = scheme
	newURL.Host = strings.TrimPrefix(t.f.ts.URL, scheme+"://")
	r2 := r.Clone(r.Context())
	r2.URL = &newURL
	return http.DefaultTransport.RoundTrip(r2)
}

func (f *fakeModelsDevServer) setBody(body string) {
	f.mu.Lock()
	f.body = body
	f.mu.Unlock()
}

// ---- parseModelsDevDoc 单测 ----

// TestParseModelsDevDocSchema 逆向 schema 解析：provider→models→id→limit{context,
// output}；带命名空间 id（openai/gpt-x）取尾段做索引 key。
func TestParseModelsDevDocSchema(t *testing.T) {
	raw := fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {
			"glm-5.2":           {1000000, 131072},
			"openai/gpt-fake-x": {400000, 128000},
		},
	})
	doc, err := parseModelsDevDoc([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e, ok := doc["glm-5.2"]; !ok || e.Context != 1000000 || e.Output != 131072 {
		t.Errorf("glm-5.2: %+v ok=%v", e, ok)
	}
	// 命名空间 id 尾段索引。
	if e, ok := doc["gpt-fake-x"]; !ok || e.Context != 400000 {
		t.Errorf("namespaced id should index by tail: %+v ok=%v", e, ok)
	}
}

// TestParseModelsDevDocVendorPriority 同名分歧采值：官方 vendor 源（zai）压过
// 多个聚合网关的众数（merge-gateway/nano-gpt/vivgrid 三家 262144 vs zai 1000000）。
func TestParseModelsDevDocVendorPriority(t *testing.T) {
	raw := fakeModelsDevDoc(map[string]map[string][2]int64{
		"merge-gateway": {"glm-5.2": {262144, 131072}},
		"nano-gpt":      {"glm-5.2": {262144, 131072}},
		"vivgrid":       {"glm-5.2": {262144, 131072}},
		"zai":           {"glm-5.2": {1000000, 131072}},
	})
	doc, err := parseModelsDevDoc([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e := doc["glm-5.2"]; e.Context != 1000000 {
		t.Errorf("vendor source should win: context=%d want 1000000", e.Context)
	}
}

// TestParseModelsDevDocMajorityVote 无官方源分歧 → 众数共识（3 家 1M vs 2 家 200K）。
func TestParseModelsDevDocMajorityVote(t *testing.T) {
	raw := fakeModelsDevDoc(map[string]map[string][2]int64{
		"agg-a": {"kimi-k3": {1000000, 131072}},
		"agg-b": {"kimi-k3": {1000000, 131072}},
		"agg-c": {"kimi-k3": {1000000, 131072}},
		"agg-d": {"moonshot/kimi-k3": {262144, 131072}},
		"agg-e": {"kimi-k3": {200000, 131072}},
	})
	doc, err := parseModelsDevDoc([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if e := doc["kimi-k3"]; e.Context != 1000000 {
		t.Errorf("majority vote: context=%d want 1000000", e.Context)
	}
}

// TestParseModelsDevDocValueValidation 值校验：非正 context / 超 1e9 上限 / 负 output
// 的脏条目拒绝进索引（防脏数据写进缓存，任务书 §2）。
func TestParseModelsDevDocValueValidation(t *testing.T) {
	raw := fakeModelsDevDoc(map[string]map[string][2]int64{
		"p": {
			"bad-zero":     {0, 100},
			"bad-neg":      {-5, 100},
			"bad-huge":     {2000000000, 100},
			"bad-out-neg":  {100000, -1},
			"bad-out-huge": {100000, 5000000000},
			"ok-model":     {100000, 8192},
		},
	})
	doc, err := parseModelsDevDoc([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, bad := range []string{"bad-zero", "bad-neg", "bad-huge", "bad-out-neg", "bad-out-huge"} {
		if _, ok := doc[bad]; ok {
			t.Errorf("%s: dirty value must be rejected", bad)
		}
	}
	if _, ok := doc["ok-model"]; !ok {
		t.Error("ok-model must be indexed")
	}
}

// TestParseModelsDevDocEmptyLimit 无 limit 字段的模型条目跳过（models.dev 部分模型
// 无收录值）。
func TestParseModelsDevDocEmptyLimit(t *testing.T) {
	raw := `{"p":{"models":{"no-limit-model":{"id":"x"},"with-limit":{"limit":{"context":1000,"output":100}}}}}`
	doc, err := parseModelsDevDoc([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := doc["no-limit-model"]; ok {
		t.Error("model without limit must be skipped")
	}
	if _, ok := doc["with-limit"]; !ok {
		t.Error("model with limit must be indexed")
	}
}

// ---- 四级查找链单测 ----

// TestLookupChainAllFourLevels 四级查找各层命中（任务书 §单测：四级查找各层命中）：
// 1) remote 权威；2) 静态种子表；3) model.json 缓存；4) miss → 1M + 异步拉取触发。
func TestLookupChainAllFourLevels(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	// 级 1：remote 权威（压过种子表与缓存的一切值）。
	if got := ContextWindowListingV4("glm-5.2", 262144, nil); got != 262144 {
		t.Errorf("level1 remote: %d want 262144", got)
	}
	if got, ok := MaxOutputTokensListingV4("glm-5.2", 999, nil); !ok || got != 999 {
		t.Errorf("level1 remote output: %d,%v want 999,true", got, ok)
	}
	// remote 权威压过 model.json 更值（上游才是权威，任务书 §清理）。
	modelCatalogPut("glm-5.2", 555555, 77777)
	if got := ContextWindowListingV4("glm-5.2", 262144, nil); got != 262144 {
		t.Errorf("remote must beat model.json: %d want 262144", got)
	}

	// 级 2：静态种子表（远端零值）。
	if got := ContextWindowListingV4("gpt-5.3-codex", 0, nil); got != 400000 {
		t.Errorf("level2 static table: %d want 400000", got)
	}

	// 级 3：model.json 缓存（种子表未收录、缓存有值）。
	modelCatalogPut("custom-cache-model", 123456, 4096)
	if got := ContextWindowListingV4("custom-cache-model", 0, nil); got != 123456 {
		t.Errorf("level3 model.json: %d want 123456", got)
	}
	if got, ok := MaxOutputTokensListingV4("custom-cache-model", 0, nil); !ok || got != 4096 {
		t.Errorf("level3 model.json output: %d,%v want 4096,true", got, ok)
	}

	// 级 4：全链 miss → 1M 兜底 + max_output_tokens 省略（不发网络：nil client +
	// 拉取异步 goroutine 里才用到 client，此处负缓存前首次会起 goroutine，但
	// client nil 时 fetchDoc 回落 DefaultClient 打真网——测试必须避免。
	// ensureDocAsync 传 nil client 的形态在下面 dedicated 测试里用 fake server 覆盖）。
	resetModelsDev() // 清掉上一轮可能记录的负缓存
	if got := ContextWindowListingV4("never-anywhere", 0, nil); got != DefaultContextWindow {
		t.Errorf("level4 miss: %d want %d (1M fallback)", got, DefaultContextWindow)
	}
	if _, ok := MaxOutputTokensListingV4("never-anywhere", 0, nil); ok {
		t.Error("level4 miss: max_output_tokens must be omitted")
	}
}

// TestLookupChainLevel4FetchAndBackfill 第 4 级完整闭环（任务书 §单测核心形态）：
// miss → 异步拉 models.dev → 值写 model.json → 下次查找命中第 3 级。
func TestLookupChainLevel4FetchAndBackfill(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	loadModelCatalogAt(filepath.Join(dir, "model.json"))

	fake := newModelsDevServer(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"brand-new-glm": {2000000, 262144}},
	}))
	defer fake.ts.Close()
	httpc := fake.client()

	// 第一次 miss：1M 兜底（异步拉取已触发，不阻塞本次返回）。
	first := ContextWindowListingV4("brand-new-glm", 0, httpc)
	if first != DefaultContextWindow {
		t.Fatalf("first lookup: %d want 1M (fetch async, immediate fallback)", first)
	}
	// 等异步拉取 + 回流完成（有界轮询，防挂死）。
	deadline := time.Now().Add(5 * time.Second)
	var cached ModelCapEntry
	ok := false
	for time.Now().Before(deadline) {
		if cached, ok = modelCatalogGet("brand-new-glm"); ok {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ok {
		t.Fatal("model.json cache not backfilled after fetch")
	}
	if cached.ContextLength != 2000000 || cached.MaxOutputTokens != 262144 {
		t.Errorf("backfilled entry: %+v want 2000000/262144", cached)
	}
	if cached.Source != "modelsdev" {
		t.Errorf("backfilled source: %q want modelsdev", cached.Source)
	}
	// 第二次查找：命中第 3 级（model.json 缓存，不再 1M）。
	if got := ContextWindowListingV4("brand-new-glm", 0, httpc); got != 2000000 {
		t.Errorf("second lookup: %d want 2000000 (model.json hit)", got)
	}
	// 落盘可读：文件存在且条目形态正确（含 source/fetched_at）。
	raw, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		t.Fatalf("model.json not persisted: %v", err)
	}
	if !strings.Contains(string(raw), `"brand-new-glm"`) ||
		!strings.Contains(string(raw), `"modelsdev"`) ||
		!strings.Contains(string(raw), `"fetched_at"`) {
		t.Errorf("persisted model.json missing expected fields: %s", raw)
	}
	// 拉取只发生一次（同模型不重查 + 文档级节流）。
	fake.mu.Lock()
	hits := fake.hits
	fake.mu.Unlock()
	if hits != 1 {
		t.Errorf("models.dev hits=%d want 1 (fetch once, reuse doc)", hits)
	}
}

// TestLookupChainNegativeCache 负缓存（任务书 §单测）：models.dev 收录里没有的模型
// 查一次后 24h 内不重查（第二次 lookup 不触发新拉取、无重试风暴）。
func TestLookupChainNegativeCache(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	fake := newModelsDevServer(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"known-model": {100000, 8192}},
	}))
	defer fake.ts.Close()
	httpc := fake.client()

	// 手动把索引灌好（等一次拉取完成），避免异步时序不确定。
	modelsDev.mu.Lock()
	modelsDev.fetched = true
	modelsDev.lastFetch = time.Now()
	modelsDev.mu.Unlock()
	modelsDev.mu.Lock()
	doc, _ := parseModelsDevDoc([]byte(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"known-model": {100000, 8192}},
	})))
	modelsDev.doc = doc
	modelsDev.mu.Unlock()

	// 查一个文档里没有的模型：lookup 记负缓存。
	if got := ContextWindowListingV4("ghost-model", 0, httpc); got != DefaultContextWindow {
		t.Fatalf("ghost: %d want 1M", got)
	}
	if !modelsDev.negativeFresh("ghost-model") {
		t.Fatal("ghost-model should be in negative cache")
	}
	// 负缓存有效期内：再查不触发 ensureDocAsync（fetched=true + 冷却内本就短路，
	// 这里验证 negativeFresh 直接短路——负缓存条目存在即不触发拉取）。
	fake.mu.Lock()
	hitsBefore := fake.hits
	fake.mu.Unlock()
	for i := 0; i < 3; i++ {
		ContextWindowListingV4("ghost-model", 0, httpc)
	}
	fake.mu.Lock()
	hitsAfter := fake.hits
	fake.mu.Unlock()
	if hitsAfter != hitsBefore {
		t.Errorf("negative cache violated: hits %d → %d", hitsBefore, hitsAfter)
	}
}

// TestEnsureDocAsyncDoesNotBlock 不阻塞主路径（任务书 §拉取不阻塞主路径）：
// models.dev 慢（500ms 延迟）时 ContextWindowListingV4 立即返回 1M。
func TestEnsureDocAsyncDoesNotBlock(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	fake := newModelsDevServer(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"slow-model": {100000, 8192}},
	}))
	fake.mu.Lock()
	fake.delay = 500 * time.Millisecond
	fake.mu.Unlock()
	defer fake.ts.Close()
	httpc := fake.client()

	start := time.Now()
	got := ContextWindowListingV4("slow-model", 0, httpc)
	elapsed := time.Since(start)
	if got != DefaultContextWindow {
		t.Fatalf("slow fetch: %d want immediate 1M", got)
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("lookup blocked on models.dev: %v (must be async)", elapsed)
	}
	// 等慢拉取收尾，避免 defer 期间 goroutine 泄漏误报 race。
	time.Sleep(600 * time.Millisecond)
}

// TestEnsureDocAsyncFetchFailureSilent 拉取失败静默降级（任务书 §失败静默降级）：
// 5xx / 非法 JSON → 不 panic、不重试风暴、查找落 1M。
func TestEnsureDocAsyncFetchFailureSilent(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	fake := newModelsDevServer("not-json-at-all")
	fake.mu.Lock()
	fake.status = 500
	fake.mu.Unlock()
	defer fake.ts.Close()
	httpc := fake.client()

	for i := 0; i < 5; i++ {
		if got := ContextWindowListingV4("fail-model", 0, httpc); got != DefaultContextWindow {
			t.Fatalf("failure iteration %d: %d want 1M", i, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// 冷却期内只打过一次（in-flight 去重 + 5min 冷却）。
	fake.mu.Lock()
	hits := fake.hits
	fake.mu.Unlock()
	if hits != 1 {
		t.Errorf("failure refetch storm: hits=%d want 1 (cooldown dedup)", hits)
	}
}

// ---- model.json 读写并发 / 损坏降级单测 ----

// TestModelCatalogConcurrentReadWrite 读写并发（任务书 §单测：model.json 读写并发；
// -race 重点）：多 goroutine 同时 get/put（同模型与不同模型混合）+ 落盘。
func TestModelCatalogConcurrentReadWrite(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	loadModelCatalogAt(filepath.Join(dir, "model.json"))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			model := "concurrent-model"
			if n%4 == 0 {
				model = "concurrent-other-" + itoa(int64(n))
			}
			for j := 0; j < 50; j++ {
				modelCatalogPut(model, int64(100000+n), 8192)
				ContextWindowListingV4(model, 0, nil) // 同 miss 并发触发第 4 级判定路径
				MaxOutputTokensListingV4(model, 0, nil)
				modelCatalogGet(model)
			}
		}(i)
	}
	wg.Wait()
	// 落盘文件在并发写后仍合法可解析。
	raw, err := os.ReadFile(filepath.Join(dir, "model.json"))
	if err != nil {
		t.Fatalf("read model.json after concurrent writes: %v", err)
	}
	var file map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("model.json corrupted after concurrent writes: %v", err)
	}
}

// TestModelCatalogCorruptFileDegradesToSeed 损坏文件降级（任务书 §单测）：
// model.json 整体非法 JSON → WARN + 种子表生效（不崩溃、查找链正常）。
func TestModelCatalogCorruptFileDegradesToSeed(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("write corrupt file: %v", err)
	}
	loadModelCatalogAt(path)

	// 种子表值可用（种子 = 静态表迁移：glm-5.2 → 1M/131072）。
	// 注意：查找链级 2（静态表）本来就会兜住 glm-5.2，这里直接断言级 3 缓存读：
	e, ok := modelCatalogGet("glm-5.2")
	if !ok || e.ContextLength != 1000000 || e.MaxOutputTokens != 131072 {
		t.Errorf("corrupt file must fall back to seed: %+v ok=%v", e, ok)
	}
	// 完全未收录模型仍 1M（链路健康）。
	if got := ContextWindowListingV4("unheard-of", 0, nil); got != DefaultContextWindow {
		t.Errorf("unheard-of: %d want 1M", got)
	}
}

// TestModelCatalogInvalidEntriesSkipped 文件合法 JSON 但条目非法（context 零/负）：
// 剔除该条目（WARN）其余正常（条目级容错）。
func TestModelCatalogInvalidEntriesSkipped(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	body := `{
		"good-entry": {"context_length": 123456, "max_output_tokens": 4096, "source": "manual"},
		"bad-zero": {"context_length": 0, "source": "manual"},
		"bad-neg": {"context_length": -1, "source": "manual"},
		"bad-out": {"context_length": 1000, "max_output_tokens": -5, "source": "manual"}
	}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loadModelCatalogAt(path)

	if e, ok := modelCatalogGet("good-entry"); !ok || e.ContextLength != 123456 {
		t.Errorf("good-entry: %+v ok=%v (must survive)", e, ok)
	}
	for _, bad := range []string{"bad-zero", "bad-neg", "bad-out"} {
		if _, ok := modelCatalogGet(bad); ok {
			t.Errorf("%s: invalid entry must be skipped", bad)
		}
	}
}

// TestModelCatalogManualMaintenance 手动维护入口（任务书 §清理）：用户直接编辑
// model.json 写入自定义值 → 重启（重新加载）后生效。
func TestModelCatalogManualMaintenance(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	body := `{"my-private-model": {"context_length": 77777, "max_output_tokens": 3333, "source": "manual"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	loadModelCatalogAt(path)

	if got := ContextWindowListingV4("my-private-model", 0, nil); got != 77777 {
		t.Errorf("manual entry: %d want 77777", got)
	}
	if got, ok := MaxOutputTokensListingV4("my-private-model", 0, nil); !ok || got != 3333 {
		t.Errorf("manual entry output: %d,%v want 3333,true", got, ok)
	}
}

// TestModelCatalogPutSkipsInvalidValues put 值校验（任务书 §值校验写入侧）：
// 非法值（零/负 context、负 output）不进缓存不落盘。
func TestModelCatalogPutSkipsInvalidValues(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	loadModelCatalogAt(filepath.Join(dir, "model.json"))

	modelCatalogPut("bad-put-a", 0, 100)
	modelCatalogPut("bad-put-b", -1, 100)
	modelCatalogPut("bad-put-c", 100, -1)
	modelCatalogPut("", 100, 100)
	for _, m := range []string{"bad-put-a", "bad-put-b", "bad-put-c", ""} {
		if _, ok := modelCatalogGet(m); ok {
			t.Errorf("%q: invalid put must be rejected", m)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "model.json")); err == nil {
		t.Error("model.json must not be written for invalid puts only")
	}
}

// TestModelCatalogSeedOnFirstStartup 首次启动（无 model.json）：种子 embed 初值生效，
// 不预写文件（保持「用户尚无缓存」状态）。
func TestModelCatalogSeedOnFirstStartup(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	dir := t.TempDir()
	path := filepath.Join(dir, "model.json")
	SetModelCatalogPath(path) // 惰性加载：首次 get 触发

	e, ok := modelCatalogGet("kimi-k2.6")
	if !ok || e.ContextLength != 256000 || e.MaxOutputTokens != 262144 {
		t.Errorf("seed entry kimi-k2.6: %+v ok=%v", e, ok)
	}
	if e.Source != "seed" {
		t.Errorf("seed source: %q want seed", e.Source)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("first startup must not pre-write model.json (stat err=%v)", err)
	}
}

// TestRemoteBeatsNewerModelJSON 上游动态值压过缓存（任务书 §单测：上游动态值压过
// 缓存）：model.json 值更新（后写入）也不得覆盖 remote 权威。
func TestRemoteBeatsNewerModelJSON(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	modelCatalogPut("fresh-cached-model", 999999, 12345)
	// remote（哪怕更小）永远权威。
	if got := ContextWindowListingV4("fresh-cached-model", 32000, nil); got != 32000 {
		t.Errorf("remote must always win: %d want 32000", got)
	}
	if got, ok := MaxOutputTokensListingV4("fresh-cached-model", 4096, nil); !ok || got != 4096 {
		t.Errorf("remote output must win: %d,%v want 4096,true", got, ok)
	}
}

// TestEnsureDocAsyncInflightDedup in-flight 去重：并发多 miss 只起一次拉取。
func TestEnsureDocAsyncInflightDedup(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()
	fake := newModelsDevServer(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"dedup-model": {100000, 8192}},
	}))
	fake.mu.Lock()
	fake.delay = 200 * time.Millisecond
	fake.mu.Unlock()
	defer fake.ts.Close()
	httpc := fake.client()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ContextWindowListingV4("dedup-model", 0, httpc)
		}()
	}
	wg.Wait()
	time.Sleep(400 * time.Millisecond) // 等慢拉取收尾
	fake.mu.Lock()
	hits := fake.hits
	fake.mu.Unlock()
	if hits != 1 {
		t.Errorf("in-flight dedup: hits=%d want 1", hits)
	}
}

// TestModelsDevNegativesEvictsExpired 负缓存条目必须在 24h TTL 到期后被淘汰。
//
// negatives 是进程级单例（modelsDev）上的 map，全库唯一删除点在 backfillMisses——
// 且仅删「models.dev 文档里**查到**的模型」。models.dev 永远不收录的模型名
// （model 由客户端任意指定，/v1/models 走四级链第 4 级时逐条查询）查一次就永久留在
// map 里：条目只增不减、TTL 到期也不回收，进程生命周期内无界增长（内存泄漏）。
//
// 本测试：灌入远超软上限的未知模型名 → 把全部条目时间回拨到 TTL 之外（等价 24h 后
// 这些模型仍未收录）→ 再查一次，断言过期条目被惰性淘汰、规模回落，而不是继续累积。
func TestModelsDevNegativesEvictsExpired(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	// 索引就绪（doc 非 nil）+ fetched=true：ensureDocAsync 直接短路，全程零网络。
	modelsDev.mu.Lock()
	modelsDev.doc = map[string]modelsDevEntry{"known-model": {Context: 100000, Output: 8192}}
	modelsDev.fetched = true
	modelsDev.lastFetch = time.Now()
	modelsDev.mu.Unlock()

	// 灌入远超软上限的未知模型名：每一个都落负缓存（无重复键，故条数 == 探测数）。
	const probes = modelsDevNegativesSoftCap + 200
	for i := 0; i < probes; i++ {
		ContextWindowListingV4("ghost-"+itoa(int64(i)), 0, nil)
	}
	modelsDev.mu.Lock()
	n := len(modelsDev.negatives)
	modelsDev.mu.Unlock()
	if n != probes {
		t.Fatalf("negatives=%d want %d（每个未知模型都应记一条负缓存）", n, probes)
	}

	// 把全部条目的时间回拨到 TTL 之外。
	stale := time.Now().Add(-modelsDevNegativeTTL - time.Minute)
	modelsDev.mu.Lock()
	for m := range modelsDev.negatives {
		modelsDev.negatives[m] = stale
	}
	modelsDev.mu.Unlock()

	// 再查一个新的未知模型：应触发惰性淘汰，过期条目全部回收。
	ContextWindowListingV4("ghost-after-ttl", 0, nil)
	modelsDev.mu.Lock()
	after := len(modelsDev.negatives)
	modelsDev.mu.Unlock()
	if after > 2 {
		t.Errorf("过期负缓存条目未被淘汰: negatives=%d（应为 ~1；旧行为累积到 %d 且永不回收）",
			after, probes+1)
	}
}

// TestModelsDevNegativesReStampBlocksEviction 反复被查询的负缓存条目永不淘汰（#121 同类残留）。
//
// 缺陷：lookup 未命中的收尾无条件执行 `f.negatives[model] = now`，把条目的记录时刻
// **重置为本次查询时刻**——TTL 因此从「最近一次查询」而非「首次未命中」起算。
// 而 /v1/models 每次列出一个模型都会经 ContextWindowListingV4 +
// MaxOutputTokensListingV4 各查一次，未知模型名又完全由客户端指定：只要这份名单
// 稳定存在（每轮都被列出），其条目就永远 fresh，惰性淘汰（now.Sub(t) >= TTL）
// 一条也扫不掉，且因为淘汰只在 len > 软上限时才扫，规模超限后**只增不减**。
// #121 修的是「不再被查询的条目永不回收」，持续被查询的条目仍无界驻留。
//
// 断言：多轮重复列出同一批（超过软上限的）未知模型，map 规模必须保持有界。
// 修复前：每轮都重新盖章，三轮后条目数 = 3*perRound 且永不回收 → RED。
func TestModelsDevNegativesReStampBlocksEviction(t *testing.T) {
	resetModelsDev()
	resetModelCatalog()

	modelsDev.mu.Lock()
	modelsDev.doc = map[string]modelsDevEntry{"known-model": {Context: 100000, Output: 8192}}
	modelsDev.fetched = true
	modelsDev.lastFetch = time.Now()
	modelsDev.mu.Unlock()

	// 三轮，每轮把此前所有幽灵模型连同新增的一批一起再列一次（等价 /v1/models 反复输出）。
	const perRound = modelsDevNegativesSoftCap + 200
	const rounds = 3
	for round := 0; round < rounds; round++ {
		for i := 0; i < perRound*(round+1); i++ {
			ContextWindowListingV4("ghost-"+itoa(int64(i)), 0, nil)
		}
	}

	modelsDev.mu.Lock()
	after := len(modelsDev.negatives)
	modelsDev.mu.Unlock()
	// 上限口径：软上限 + 一轮的新增（淘汰是惰性的，允许跨一个扫描窗口）。
	if max := 2 * modelsDevNegativesSoftCap; after > max {
		t.Errorf("反复被查询的负缓存条目无界增长: negatives=%d want <= %d（旧行为下为 %d 且永不回收）",
			after, max, perRound*rounds)
	}
}

// TestParseModelsDevDocTieBreakDeterministic T2 真平票（无官方源、3 家 vs 3 家
// 同票不同值）的确定性：map 迭代序随机化必须被 provider 字典序 tie-break 压平。
// 双断言锁死：多次解析结果两两相等，且等于 provider 字典序最小候选（"a-vendor"）。
// 依据 pr134-watchlist-analysis.md #2：旧实现「先出现胜」依赖 map 迭代序——
// 同 binary 两次拉取同一文档可能落不同值（/v1/models 可观测抖动 +
// model.json 不可复现覆盖）。
func TestParseModelsDevDocTieBreakDeterministic(t *testing.T) {
	raw := fakeModelsDevDoc(map[string]map[string][2]int64{
		"agg-a": {"kimi-k3": {1000000, 131072}},
		"agg-b": {"kimi-k3": {1000000, 131072}},
		"agg-c": {"kimi-k3": {1000000, 131072}},
		"z-vendor": {"kimi-k3": {200000, 131072}},
		"y-vendor": {"kimi-k3": {200000, 131072}},
		"a-vendor": {"kimi-k3": {200000, 131072}},
	})
	run := func() int64 {
		doc, err := parseModelsDevDoc([]byte(raw))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return doc["kimi-k3"].Context
	}
	first := run()
	if first != 200000 {
		t.Errorf("tie-break context=%d want 200000 (provider 字典序最小 a-vendor 一方)", first)
	}
	// map 迭代序随机化（-count 多次 + Go 每轮随机起点）下结果必须稳定。
	for i := 0; i < 20; i++ {
		if got := run(); got != first {
			t.Fatalf("tie-break 不确定: run %d got %d want %d", i, got, first)
		}
	}
}

// TestParseModelsDevDocVendorTieBreakByProviderName T1 多官方源同票分歧
//（zai 与 moonshotai 都报 kimi-k3 且值不同）：两候选都 vendor=true 同票，
// 旧实现「先到先得」不确定，必须由 provider 字典序（moonshotai < zai）收敛。
func TestParseModelsDevDocVendorTieBreakByProviderName(t *testing.T) {
	raw := fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai":        {"kimi-k3": {1000000, 131072}},
		"moonshotai": {"kimi-k3": {200000, 131072}},
	})
	run := func() int64 {
		doc, err := parseModelsDevDoc([]byte(raw))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return doc["kimi-k3"].Context
	}
	first := run()
	if first != 200000 {
		t.Errorf("vendor tie-break context=%d want 200000 (provider 字典序 moonshotai < zai)", first)
	}
	for i := 0; i < 20; i++ {
		if got := run(); got != first {
			t.Fatalf("vendor tie-break 不确定: run %d got %d want %d", i, got, first)
		}
	}
}

// TestFetchDocNilClientDoesNotHitNetwork fetchDoc 拒绝 nil client：不回落
// http.DefaultClient（无超时 + 打真网）。依据 pr134-watchlist-analysis.md #5：
// 生产路径恒传非 nil（handler 恒传 cfg.Upstream.HTTP），nil 只出现在测试疏漏——
// 静默打真网是最坏行为。断言 fake server 零命中 + doc 仍 nil（与 fetch 失败
// 同语义：静默降级 1M 兜底）。
func TestFetchDocNilClientDoesNotHitNetwork(t *testing.T) {
	resetModelsDev()
	f := newModelsDevServer(fakeModelsDevDoc(map[string]map[string][2]int64{
		"zai": {"glm-5.2": {1000000, 131072}},
	}))
	defer f.ts.Close()

	modelsDev.fetchDoc(nil, f.ts.URL)

	f.mu.Lock()
	hits := f.hits
	f.mu.Unlock()
	if hits != 0 {
		t.Fatalf("nil client 不应打网络（回落 DefaultClient 打真网/被路由到 fake）: hits=%d", hits)
	}
	modelsDev.mu.Lock()
	doc := modelsDev.doc
	modelsDev.mu.Unlock()
	if doc != nil {
		t.Fatalf("nil client 时 doc 应保持 nil（拉取失败语义），got %d 条", len(doc))
	}
}

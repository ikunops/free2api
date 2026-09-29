package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"free2api/internal/auth"
)

// modelsBodyCLI 构造 /enterprises/personal/models 形态响应（models + cli agent），
// 含 maxInputTokens/maxOutputTokens（供 modelList 输出真实 context_length/max_output_tokens）。
func modelsBodyCLI(ids ...string) string {
	var sb strings.Builder
	sb.WriteString(`{"code":0,"data":{"agents":[{"name":"cli","models":[`)
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"` + id + `"`)
	}
	sb.WriteString(`]}],"models":[`)
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`{"id":"` + id + `","name":"` + id + `","maxInputTokens":65536,"maxOutputTokens":8192,"disabled":false}`)
	}
	sb.WriteString(`]}}`)
	return sb.String()
}

func TestModelsPathCNUnchanged(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// v3-config-merge：两路并发探测，路径记录须并发安全。
	var mu sync.Mutex
	var paths []string
	c := testClient(func(r *http.Request) (*http.Response, error) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		return jsonResp(200, modelsBodyCLI("glm-5.2")), nil
	})
	cn := &auth.Auth{AccessToken: "at", UID: "cn1", Domain: "www.codebuddy.cn"}

	if _, err := c.FetchModels(cn); err != nil {
		t.Fatalf("cn fetch models: %v", err)
	}
	// CN 零回归：企业端点仍走 /console/enterprises/personal/models（现状逐字）；
	// v3-config-merge 后另并发打 /v3/config（主路）。两路各一次，顺序不保证。
	if len(paths) != 2 || !containsStr(paths, "/console/enterprises/personal/models") || !containsStr(paths, "/v3/config") {
		t.Errorf("cn FetchModels paths=%v want [/console/enterprises/personal/models /v3/config]", paths)
	}
}

// containsStr 切片成员判定（并发双路探测无固定顺序，按成员断言）。
func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func TestModelsPathGlobalV2(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// v3-config-merge：两路并发探测，路径记录须并发安全。
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(modelsBodyCLI("gpt-5.4")))
	}))
	defer srv.Close()

	c := &Client{
		HTTP:           http.DefaultClient,
		ChatBaseGlobal: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled:  true,
	}
	if _, err := c.FetchModels(globalAcct()); err != nil {
		t.Fatalf("global fetch models: %v", err)
	}
	// global 企业端点走 /v2 家族首选（PR #20 实测 /console 500 → /v2 200 完整模型表）；
	// v3-config-merge 后另并发打 /v3/config（主路）。两路各一次，顺序不保证。
	if len(paths) != 2 || !containsStr(paths, "/v2/enterprises/personal/models") || !containsStr(paths, "/v3/config") {
		t.Errorf("global FetchModels paths=%v want [/v2/enterprises/personal/models /v3/config]", paths)
	}
}

func TestGlobalModelsProbeV2First(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// /v2 首选返回 200，/console 不再被探（fallback 只在 v2 失败时触发）。
	// v3-config-merge：v3 主路 + /v2 企业路并发各一次。
	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		switch path {
		case "/v3/config":
			return 200, modelsResp("gpt-5.4")
		case "/v2/enterprises/personal/models":
			return 200, modelsResp("gpt-5.4", "probe-x")
		case "/console/enterprises/personal/models":
			return 500, `{"code":500,"msg":"boom"}`
		}
		return 404, `{"code":404,"msg":"nope"}`
	})
	defer srv.Close()

	got := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	if len(calls) != 2 || !containsStr(calls, "/v3/config") || !containsStr(calls, "/v2/enterprises/personal/models") {
		t.Fatalf("probe calls=%v want [/v3/config /v2/enterprises/personal/models] (v2-first, no console)", calls)
	}
	counts := map[string]int{}
	for _, id := range got {
		counts[id]++
	}
	// v3 主 + v2 补缺并集：gpt-5.4（两路共有，v3 条目为主）+ probe-x（v2 独有补进）。
	if counts["probe-x"] != 1 || counts["gpt-5.4"] != 1 {
		t.Fatalf("probe names not merged: %v", counts)
	}
}

func TestGlobalModelsProbeConsoleFallback(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	// /v2 失败 → fallback /console 成功（探活顺序正确：新路径优先，旧路径兜底）。
	// v3-config-merge：v3 主路 + 企业家族（v2 500 → console 200）。
	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		switch path {
		case "/v3/config":
			return 500, `{"code":500,"msg":"v3 down"}`
		case "/v2/enterprises/personal/models":
			return 500, `{"code":500,"msg":"boom"}`
		case "/console/enterprises/personal/models":
			return 200, modelsResp("legacy-only")
		}
		return 404, `{"code":404,"msg":"nope"}`
	})
	defer srv.Close()

	got := globalModelsClient(t, srv).FetchGlobalModels(globalAcct())

	if len(calls) != 3 ||
		!containsStr(calls, "/v3/config") ||
		!containsStr(calls, "/v2/enterprises/personal/models") ||
		!containsStr(calls, "/console/enterprises/personal/models") {
		t.Fatalf("fallback calls=%v want [/v3/config /v2/... /console/...]", calls)
	}
	counts := map[string]int{}
	for _, id := range got {
		counts[id]++
	}
	// v3 失败降级为企业家族结果（console 兜底名单原样透出）。
	if counts["legacy-only"] != 1 {
		t.Errorf("console fallback name not merged: %v", counts)
	}
}

// TestFetchGlobalModelsProbeSoundNoPanic probePaths 顺序测试（探测成功后调用）为
// /console 旧路径取消的响应；本测试占位保 probe 代码继续编译（无函数体引用被删）。
func TestGlobalModelsProbePathsOrderStable(t *testing.T) {
	if len(globalModelsProbePaths) != 2 {
		t.Fatalf("globalModelsProbePaths=%v want 2 entries (v2 + console fallback)", globalModelsProbePaths)
	}
	if globalModelsProbePaths[0] != "/v2/enterprises/personal/models" {
		t.Errorf("probePaths[0]=%q want /v2/enterprises/personal/models (newest first)", globalModelsProbePaths[0])
	}
	if globalModelsProbePaths[1] != "/console/enterprises/personal/models" {
		t.Errorf("probePaths[1]=%q want /console/enterprises/personal/models (fallback)", globalModelsProbePaths[1])
	}
}

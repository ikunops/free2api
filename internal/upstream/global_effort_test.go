package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// TestParseGlobalModelNamesEfforts 对象形态：reasoning.supportedEfforts/defaultEffort 解析进桶。
func TestParseGlobalModelNamesEfforts(t *testing.T) {
	raw := `{"code":0,"data":{"models":[
		{"id":"gpt-5.4","name":"GPT-5.4","reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}},
		{"id":"deepseek-v4.1-flash","reasoning":{"supportedEfforts":["high"]}},
		{"id":"no-effort"}
	]}}`
	names, _, efforts, defaults, err := parseGlobalModelNames([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("names=%v want 3", names)
	}
	if !sameStrings(efforts["gpt-5.4"], []string{"low", "high"}) {
		t.Errorf("gpt-5.4 efforts=%v want [low high]", efforts["gpt-5.4"])
	}
	if defaults["gpt-5.4"] != "high" {
		t.Errorf("gpt-5.4 default=%v want high", defaults["gpt-5.4"])
	}
	if !sameStrings(efforts["deepseek-v4.1-flash"], []string{"high"}) {
		t.Errorf("deepseek-v4.1-flash efforts=%v want [high]", efforts["deepseek-v4.1-flash"])
	}
	if _, ok := efforts["no-effort"]; ok {
		t.Error("no-effort should not appear in efforts bucket")
	}
}

// TestParseGlobalModelNamesSingleEffort reasoning.effort 单档字符串（无 supportedEfforts 数组）→ 视作单档表。
func TestParseGlobalModelNamesSingleEffort(t *testing.T) {
	raw := `{"code":0,"data":{"models":[
		{"id":"glm-5.2","reasoning":{"effort":"high"}}
	]}}`
	_, _, efforts, defaults, err := parseGlobalModelNames([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !sameStrings(efforts["glm-5.2"], []string{"high"}) {
		t.Errorf("single-effort glm-5.2=%v want [high]", efforts["glm-5.2"])
	}
	if len(defaults) != 0 {
		t.Errorf("defaults=%v want empty", defaults)
	}
}

// TestFetchGlobalModelsStoresEffortBucket 探测下发档位 → global effort 桶写入 (storeEfforts)。
func TestFetchGlobalModelsStoresEffortBucket(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	srv := globalModelsSrv(t, &calls, nil, func(path string) (int, string) {
		return 200, `{"code":0,"data":{"models":[
			{"id":"gpt-5.4","reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}}
		]}}`
	})
	defer srv.Close()

	c := globalModelsClient(t, srv)
	c.FetchGlobalModels(globalAcct())

	efforts, defaults := c.GlobalEffortSnapshot()
	if !sameStrings(efforts["gpt-5.4"], []string{"low", "high"}) {
		t.Errorf("global bucket gpt-5.4=%v want [low high]", efforts["gpt-5.4"])
	}
	if defaults["gpt-5.4"] != "high" {
		t.Errorf("defaults gpt-5.4=%v want high", defaults)
	}
}

// globalEffortChatSrv 构造一个同时服务模型探测与 chat 的 global fake，并捕获出站 chat body。
func globalEffortChatSrv(t *testing.T, modelsBody string, capture func([]byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/enterprises/personal/models"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, modelsBody)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			body, _ := io.ReadAll(r.Body)
			if capture != nil {
				capture(body)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
		default:
			w.WriteHeader(404)
		}
	}))
}

// TestGlobalEffortDowngradeStatic 探测失败（负缓存）→ global 请求按静态兜底表降级：
// deepseek-v4.1-flash 只认 high，客户端传 low → 降级 high（issue #84 核心）。
func TestGlobalEffortDowngradeStatic(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var outbound []byte
	srv := globalEffortChatSrv(t, `{"code":500}`, func(b []byte) { outbound = b })
	defer srv.Close()

	c := &Client{
		HTTP:           &http.Client{},
		ChatBaseGlobal: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled:  true,
	}
	c.FetchGlobalModels(globalAcct()) // models endpoint 返回 code:500 → 解析失败 → 负缓存，桶空

	rc, status, _, err := c.ChatStream(globalAcct(), []byte(`{"model":"deepseek-v4.1-flash","reasoning_effort":"low","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat status=%d err=%v", status, err)
	}
	rc.Close()

	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("unmarshal outbound: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (global 静态兜底降级 low→high)", m["reasoning_effort"])
	}
}

// TestGlobalEffortDowngradeRemote global 探测下发档位 → 降级以远端桶为权威
// （gpt-5.4 remote=[low high]，传 max → 降级 high）。
func TestGlobalEffortDowngradeRemote(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var outbound []byte
	srv := globalEffortChatSrv(t, `{"code":0,"data":{"models":[
		{"id":"gpt-5.4","reasoning":{"supportedEfforts":["low","high"],"defaultEffort":"high"}}
	]}}`, func(b []byte) { outbound = b })
	defer srv.Close()

	c := &Client{
		HTTP:           &http.Client{},
		ChatBaseGlobal: strings.TrimSuffix(srv.URL, "/"),
		GlobalEnabled:  true,
	}
	c.FetchGlobalModels(globalAcct()) // 探测成功写桶

	rc, status, _, err := c.ChatStream(globalAcct(), []byte(`{"model":"gpt-5.4","reasoning_effort":"max","messages":[]}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("chat status=%d err=%v", status, err)
	}
	rc.Close()

	var m map[string]any
	if err := json.Unmarshal(outbound, &m); err != nil {
		t.Fatalf("unmarshal outbound: %v (%s)", err, outbound)
	}
	if got, _ := m["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort=%v want high (remote bucket 降级)", m["reasoning_effort"])
	}
}

// sameStrings 切片内容等价（等长且逐位相同，顺序敏感）。
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

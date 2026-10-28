package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"free2api/internal/upstream"
)

// TestZCodeCandidatesProbeAndChatAreSeparated 锁住计费边界：
//   - 默认列表：不发任何上游请求
//   - ?probe=1：只发 GET /models
//   - ?chat=1：才允许发一次 POST /chat/completions
func TestZCodeCandidatesProbeAndChatAreSeparated(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantModels int
		wantChat   int
	}{
		{name: "list", query: "", wantModels: 0, wantChat: 0},
		{name: "probe", query: "?probe=1", wantModels: 1, wantChat: 0},
		{name: "chat", query: "?chat=1", wantModels: 0, wantChat: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			models, chats := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models"):
					models++
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3-flash"}]}`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions"):
					chats++
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					_, _ = w.Write([]byte(`{"error":{"code":"1113","message":"余额不足或无可用资源包,请充值。"}}`))
				default:
					t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			appDir := t.TempDir()
			creds := `{
  "oauth:active_provider":"zai",
  "oauth:zai:user_info":"{\"displayName\":\"静态号\",\"id\":\"u-app\"}",
  "zcodejwttoken":"head.payload.signature",
  "account-provider:coding-plan:account:zai-individual-coding-plan:account:1:api-key":"0123456789abcdef0123456789abcdef.secretPart"
}`
			if err := os.WriteFile(filepath.Join(appDir, "credentials.json"), []byte(creds), 0o600); err != nil {
				t.Fatal(err)
			}
			config := `{"provider":{"builtin:zai-coding-plan":{"models":{"glm-5.3-flash":{"limit":{"context":1000000,"output":128000}}}}}}`
			if err := os.WriteFile(filepath.Join(appDir, "config.json"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}

			h := NewHandler(Config{
				AdminEnabled: true,
				AuthDir:      t.TempDir(),
				ZCodeAppDir:  appDir,
				Upstream: &upstream.Client{
					HTTP: srv.Client(),
				},
			})
			// 静态 key 的 BaseURL 由凭据键名映射到 api.z.ai；测试里把请求透明改到本地 fake。
			h.cfg.Upstream.HTTP = &http.Client{Transport: rewriteHost{srv: srv.URL, base: srv.Client().Transport}}
			req := httptest.NewRequest(http.MethodGet, "/admin/sources/zcode-app/candidates"+tc.query, nil)
			req.RemoteAddr = "127.0.0.1:12345"
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if got := len(body["candidates"].([]any)); got != 1 {
				t.Fatalf("candidates=%d body=%s", got, rec.Body.String())
			}
			mu.Lock()
			gotModels, gotChats := models, chats
			mu.Unlock()
			if gotModels != tc.wantModels || gotChats != tc.wantChat {
				t.Fatalf("upstream requests models=%d chat=%d, want models=%d chat=%d", gotModels, gotChats, tc.wantModels, tc.wantChat)
			}
			if tc.query == "?chat=1" {
				c := body["candidates"].([]any)[0].(map[string]any)
				if strings.TrimSpace(asString(c["static_probe"])) == "" || c["static_usable"] != false {
					t.Fatalf("1113 should be surfaced as non-usable static_probe: %#v", c)
				}
			}
		})
	}
}

func asString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

type rewriteHost struct {
	srv  string
	base http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(r.srv, "http://")
	req2 := req.Clone(req.Context())
	req2.URL = &u
	base := r.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req2)
}

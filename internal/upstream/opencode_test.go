package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// TestOpenCodeForceStream 锁定 opencode 出站体的唯一改写（与 zcode 同形）：只动
// stream 一个键，其余字段逐字保留。
func TestOpenCodeForceStream(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    map[string]any
		rawSame bool
	}{
		{
			name: "stream:false 改 true，其余字段保留",
			in:   `{"model":"glm-5.3-flash","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{"stream": true, "model": "glm-5.3-flash", "temperature": 0.7},
		},
		{
			name: "无 stream 字段 → 注入 true",
			in:   `{"model":"kimi-k3","messages":[]}`,
			want: map[string]any{"stream": true, "model": "kimi-k3"},
		},
		{name: "已是流式 → 字节级原样", in: `{"model":"glm-5.3","stream":true,"messages":[]}`, rawSame: true},
		{name: "坏 JSON → 字节级原样", in: `{"model":`, rawSame: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := opencodeForceStream([]byte(c.in))
			if c.rawSame {
				if string(out) != c.in {
					t.Fatalf("want 原样 %q, got %q", c.in, string(out))
				}
				return
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("出站体不是 JSON: %v (%s)", err, out)
			}
			for k, v := range c.want {
				if obj[k] != v {
					t.Errorf("%s = %#v, want %#v（出站体=%s）", k, obj[k], v, out)
				}
			}
		})
	}
}

// TestOpenCodeChatHeadersAndStream opencode 出站端到端（httptest）：路径 = base +
// /chat/completions、Bearer 鉴权、x-opencode-session 必须存在（缺它上游按裸第三方
// 客户端处理）、stream 被强制 true，且不掺 workbuddy 专有头。
func TestOpenCodeChatHeadersAndStream(t *testing.T) {
	var gotPath, gotAuth, gotSession, gotUA, gotBody string
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(b)
		gotSession, gotUA = r.Header.Get("x-opencode-session"), r.Header.Get("User-Agent")
		gotHdr = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{}, ChatBaseOpenCode: srv.URL}
	a := &auth.Auth{UID: "oc1", AccessToken: "sk-test-key"}
	a.SetProducer(ProducerOpenCode)

	body := []byte(`{"model":"glm-5.3-flash","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	rc.Close()

	if gotPath != "/chat/completions" {
		t.Errorf("路径 = %q, want /chat/completions", gotPath)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("Authorization = %q, want Bearer sk-test-key", gotAuth)
	}
	if !strings.HasPrefix(gotSession, "ses_") {
		t.Errorf("x-opencode-session = %q, want ses_ 前缀（缺它上游按裸第三方客户端处理）", gotSession)
	}
	if !strings.Contains(gotUA, "opencode/") {
		t.Errorf("User-Agent = %q, want 含 opencode/（Cloudflare 会拦默认 Go UA）", gotUA)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("出站 body 不是 JSON: %v", err)
	}
	if sent["stream"] != true {
		t.Errorf("出站 stream = %#v, want true", sent["stream"])
	}
	for _, h := range []string{"X-Enterprise-Id", "X-Domain", "X-Device-Token"} {
		if v := gotHdr.Get(h); v != "" {
			t.Errorf("workbuddy 专有头 %s = %q 不该出现在 opencode 请求上", h, v)
		}
	}
}

// TestOpenCodeSessionIDStableAndNotLeaky 会话 id 必须同账号稳定、且不含凭据原文
// （值会出现在上游日志里，泄露 key 不可接受）。
func TestOpenCodeSessionIDStableAndNotLeaky(t *testing.T) {
	a := &auth.Auth{UID: "oc1", AccessToken: "sk-super-secret-key-value"}
	a.SetProducer(ProducerOpenCode)
	s1, s2 := opencodeSessionID(a), opencodeSessionID(a)
	if s1 != s2 {
		t.Errorf("同账号两次派生不一致: %q vs %q", s1, s2)
	}
	if strings.Contains(s1, "super-secret") {
		t.Errorf("会话 id 泄露了凭据原文: %q", s1)
	}
	b := &auth.Auth{UID: "oc1", AccessToken: "sk-another-key"}
	b.SetProducer(ProducerOpenCode)
	if opencodeSessionID(b) == s1 {
		t.Errorf("不同凭据派生出了同一个会话 id")
	}
}

// TestOpenCodeClassify 错误分类口径：401/Invalid credential → 会话失效；
// 免费层门禁与模型不可用 → 池级死模型（换号无意义）；其余回落通用 Classify。
func TestOpenCodeClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, `{"error":{"type":"server_error","message":"Upstream request failed: Invalid credential"}}`, ErrSessionDead},
		{403, `{"type":"error","error":{"type":"FreeTierError","message":"OpenCode's free tier can only be used from within OpenCode"}}`, ErrModelBlocked},
		{400, `{"error":{"type":"server_error","message":"Upstream request failed: Model is unavailable."}}`, ErrModelBlocked},
		{400, `{"error":{"message":"ModelProtocolUnsupported"}}`, ErrModelBlocked},
		{200, `{}`, ErrNone},
	}
	for _, c := range cases {
		if got := classifyOpenCode(c.status, c.body); got != c.want {
			t.Errorf("classifyOpenCode(%d, %s) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestOpenCodeFreeTierFilter 免费层判定："-free" 后缀与 big-pickle 都要被剔，
// 付费模型不能被误剔。
func TestOpenCodeFreeTierFilter(t *testing.T) {
	free := []string{"big-pickle", "mimo-v2.5-free", "x-preview-f-free", "LING-3.0-FLASH-FREE"}
	paid := []string{"glm-5.3-flash", "deepseek-v4.1-flash", "kimi-k3", "minimax-m3", "freeform-model", "freed"}
	for _, id := range free {
		if !isOpenCodeFreeTierModel(id) {
			t.Errorf("isOpenCodeFreeTierModel(%q) = false, want true", id)
		}
	}
	for _, id := range paid {
		if isOpenCodeFreeTierModel(id) {
			t.Errorf("isOpenCodeFreeTierModel(%q) = true, want false", id)
		}
	}
}

// TestOpenCodeFetchModelsFiltersFree FetchOpenCodeModels 解析 Zen /models 的
// {"object":"list","data":[...]} 形态，并剔除免费层模型。
func TestOpenCodeFetchModelsFiltersFree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("路径 = %q, want /models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer sk-test-key" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if !strings.Contains(r.Header.Get("User-Agent"), "opencode/") {
			t.Errorf("UA = %q", r.Header.Get("User-Agent"))
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[`+
			`{"id":"big-pickle","object":"model","owned_by":"opencode"},`+
			`{"id":"mimo-v2.5-free","object":"model","owned_by":"opencode"},`+
			`{"id":"glm-5.3-flash","object":"model","owned_by":"opencode"},`+
			`{"id":"kimi-k3","object":"model","owned_by":"opencode"}]}`)
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{}, ChatBaseOpenCode: srv.URL}
	a := &auth.Auth{UID: "oc1", AccessToken: "sk-test-key"}
	a.SetProducer(ProducerOpenCode)

	infos, err := c.FetchOpenCodeModels(context.Background(), a)
	if err != nil {
		t.Fatalf("FetchOpenCodeModels: %v", err)
	}
	got := make([]string, 0, len(infos))
	for _, m := range infos {
		got = append(got, m.ID)
	}
	want := []string{"glm-5.3-flash", "kimi-k3"}
	if len(got) != len(want) {
		t.Fatalf("infos = %v, want %v（免费层必须被剔）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("infos[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestOpenCodeUpstreamBasePriority 凭据自带 upstream_base 优先于 Client 覆盖
// （「用户自己选 url」的落点），缺省回落 zen 直连 base。
func TestOpenCodeUpstreamBasePriority(t *testing.T) {
	c := &Client{ChatBaseOpenCode: "https://client-override.example/zen/v1"}
	a := &auth.Auth{UID: "oc1", AccessToken: "k"}
	a.SetProducer(ProducerOpenCode)
	a.SetUpstreamBase("https://from-credential.example/zen/v1/")
	if got := c.opencodeBase(a); got != "https://from-credential.example/zen/v1" {
		t.Errorf("opencodeBase = %q, want 凭据自带（且剥掉尾斜杠）", got)
	}

	b := &auth.Auth{UID: "oc2", AccessToken: "k"}
	b.SetProducer(ProducerOpenCode)
	if got := c.opencodeBase(b); got != "https://client-override.example/zen/v1" {
		t.Errorf("opencodeBase = %q, want Client 覆盖", got)
	}

	empty := &Client{}
	d := &auth.Auth{UID: "oc3", AccessToken: "k"}
	d.SetProducer(ProducerOpenCode)
	if got := empty.opencodeBase(d); !strings.Contains(got, "opencode.ai") {
		t.Errorf("opencodeBase = %q, want 内置缺省 zen base", got)
	}
}

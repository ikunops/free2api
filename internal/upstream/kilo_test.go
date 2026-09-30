package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

func TestKiloModelIsFree(t *testing.T) {
	mk := func(id string, isFree bool, prompt, completion string) kiloModelEntry {
		var m kiloModelEntry
		m.ID = id
		m.IsFree = isFree
		m.Pricing.Prompt = prompt
		m.Pricing.Completion = completion
		return m
	}
	cases := []struct {
		name string
		m    kiloModelEntry
		want bool
	}{
		{"isFree flag", mk("x", true, "1", "2"), true},
		{"pricing zero strings", mk("y", false, "0", "0"), true},
		{"pricing zero decimal", mk("y2", false, "0.0", "0.000000"), true},
		{"pricing dollar zero", mk("y3", false, "$0", "$0.00"), true},
		{"pricing nonzero", mk("z", false, "0.1", "0"), false},
		{"sfx :free", mk("a/b:free", false, "", ""), true},
		{"plain paid no suffix", mk("a/b", false, "", ""), false},
	}
	for _, c := range cases {
		if got := kiloModelIsFree(c.m); got != c.want {
			t.Errorf("%s: kiloModelIsFree=%v want %v", c.name, got, c.want)
		}
	}
}

func TestKiloForceStream(t *testing.T) {
	// stream:false -> true，其余字段原样保留
	out := kiloForceStream([]byte(`{"model":"m","stream":false,"max_tokens":5}`))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["stream"] != true {
		t.Errorf("stream=%v want true", obj["stream"])
	}
	if obj["max_tokens"].(float64) != 5 || obj["model"] != "m" {
		t.Errorf("other fields mutated: %v", obj)
	}
	// 已是 true：原字节返回（不重排）
	same := []byte(`{"stream":true}`)
	if got := kiloForceStream(same); string(got) != string(same) {
		t.Errorf("stream:true should pass through unchanged")
	}
	// 非法 JSON：原字节返回
	bad := []byte(`not json`)
	if got := kiloForceStream(bad); string(got) != string(bad) {
		t.Errorf("invalid json should pass through unchanged")
	}
}

func TestClassifyKiloAuthIsClientNotSessionDead(t *testing.T) {
	// Kilo 是全局单条匿名通道：401/403 绝不能判 ErrSessionDead（会禁用唯一那条号，
	// 整家能力消失）。必须退化成 ErrClient（不罚号、只轮转）。
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		if got := classifyKilo(status, `{"error":"Unauthorized"}`); got != ErrClient {
			t.Errorf("classifyKilo(%d)=%v want ErrClient", status, got)
		}
	}
	if got := classifyKilo(http.StatusTooManyRequests, "rate limit exceeded"); got != ErrSoftRate {
		t.Errorf("429 should stay ErrSoftRate, got %v", got)
	}
}

func TestFetchKiloModelsFiltersFreeAndKeepsOrder(t *testing.T) {
	payload := map[string]any{
		"data": []map[string]any{
			{"id": "paid/model", "name": "Paid", "context_length": 1000,
				"pricing": map[string]any{"prompt": "0.5", "completion": "1"}},
			{"id": "free/model:free", "name": "Free One", "context_length": 2000,
				"pricing": map[string]any{"prompt": "0", "completion": "0"},
				"architecture": map[string]any{"input_modalities": []string{"text", "image"}}},
			{"id": "flagged/free", "name": "Flagged", "isFree": true,
				"pricing": map[string]any{"prompt": "0.1", "completion": "0.2"}},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/models") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	c := New()
	c.HTTP = srv.Client()
	c.HTTP.Timeout = 5 * time.Second
	a := &auth.Auth{}
	a.SetProducer(ProducerKilo)
	a.SetUpstreamBase(srv.URL)

	infos, err := c.FetchKiloModels(context.Background(), a)
	if err != nil {
		t.Fatalf("FetchKiloModels: %v", err)
	}
	if len(infos) != 2 {
		t.Fatalf("got %d free models want 2: %+v", len(infos), infos)
	}
	if infos[0].ID != "free/model:free" || infos[1].ID != "flagged/free" {
		t.Errorf("unexpected ids: %q %q", infos[0].ID, infos[1].ID)
	}
	if infos[0].ContextWindow != 2000 {
		t.Errorf("context window not carried: %d", infos[0].ContextWindow)
	}
	if !infos[0].SupportsImages {
		t.Errorf("image modality should set SupportsImages")
	}
}

func TestKiloHeadersFallsBackToAnonymous(t *testing.T) {
	c := New()
	a := &auth.Auth{}
	a.SetProducer(ProducerKilo)
	req, _ := http.NewRequest(http.MethodPost, "http://x/y", nil)
	c.kiloHeaders(req, a)
	if got := req.Header.Get("Authorization"); got != "Bearer "+kiloAnonymousKey {
		t.Errorf("auth header = %q want anonymous fallback", got)
	}
	if req.Header.Get("User-Agent") != kiloUserAgent {
		t.Errorf("user agent = %q", req.Header.Get("User-Agent"))
	}
}

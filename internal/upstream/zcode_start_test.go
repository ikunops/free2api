package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

func TestZCodeStartDressBodyProtocol(t *testing.T) {
	a := &auth.Auth{UID: "z1"}
	a.SetProducer(ProducerZCode)
	a.SetZCodeJWT("jwt-test")
	a.SetZCodeDeviceMID("device-test")
	body := []byte(`{"model":"glm-5.3-flash","stream":false,"max_completion_tokens":123,"messages":[{"role":"system","content":"own system"},{"role":"user","content":"hello"},{"role":"assistant","content":"hi","tool_calls":[{"id":"tc1","type":"function","function":{"name":"f","arguments":"{\"x\":1}"}}]},{"role":"tool","tool_call_id":"tc1","content":"ok"}],"tools":[{"type":"function","function":{"name":"f","description":"d","parameters":{"type":"object"}}}]}`)
	out, sid, err := zcodeStartDressBody(body, a, ChatMeta{ConversationID: "conv-1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sid, "-") {
		t.Fatalf("session id not UUID-shaped: %q", sid)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	sys, _ := got["system"].([]any)
	if len(sys) != 3 {
		t.Fatalf("system blocks=%d want 3: %s", len(sys), out)
	}
	for i, raw := range sys {
		b, _ := raw.(map[string]any)
		if b["cache_control"] == nil {
			t.Errorf("system[%d] missing cache_control", i)
		}
	}
	msgs, _ := got["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages=%d want 4 (prefix+3): %s", len(msgs), out)
	}
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	p0, _ := parts[0].(map[string]any)
	if !strings.HasPrefix(p0["text"].(string), "<system-reminder>") {
		t.Errorf("first message is not context prefix: %#v", first)
	}
	meta, _ := got["metadata"].(map[string]any)
	uid, _ := meta["user_id"].(string)
	var mu map[string]string
	if err := json.Unmarshal([]byte(uid), &mu); err != nil {
		t.Fatalf("metadata.user_id not JSON: %q", uid)
	}
	if mu["device_id"] != "device-test" || mu["session_id"] != sid {
		t.Errorf("metadata.user_id=%v sid=%s", mu, sid)
	}
	if got["max_tokens"].(float64) != 123 || got["stream"] != true {
		t.Errorf("max_tokens/stream wrong: %s", out)
	}
	last, _ := msgs[len(msgs)-1].(map[string]any)
	lastParts, _ := last["content"].([]any)
	lastBlock, _ := lastParts[len(lastParts)-1].(map[string]any)
	if lastBlock["cache_control"] == nil {
		t.Errorf("last message block missing cache_control: %s", out)
	}
	if got["tools"] == nil {
		t.Errorf("tools not mapped: %s", out)
	}
}

func TestZCodeStartChatHeadersAndSSE(t *testing.T) {
	oldURL := zcodeStartMessagesURL
	defer func() { zcodeStartMessagesURL = oldURL }()
	var gotBody string
	var gotHeader http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		gotHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"glm-5.3-flash\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"think\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":2,\"cache_read_input_tokens\":4}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	zcodeStartMessagesURL = srv.URL

	c := &Client{HTTP: &http.Client{}}
	a := &auth.Auth{UID: "z1", Nickname: "zc"}
	a.SetProducer(ProducerZCode)
	a.SetZCodeJWT("jwt-test")
	a.SetZCodeDeviceMID("device-test")
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hello"}]}`)
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{ConversationID: "conv"})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	defer rc.Close()
	if status != 200 {
		t.Fatalf("status=%d", status)
	}
	if gotHeader.Get("X-Device-Mid") != "" {
		t.Errorf("chat request must not send X-Device-Mid")
	}
	if gotHeader.Get("x-session-id") == "" || gotHeader.Get("Authorization") != "Bearer jwt-test" {
		t.Errorf("headers missing: %#v", gotHeader)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatal(err)
	}
	if sent["model"] != "GLM-5.3-Flash" {
		t.Errorf("model=%v want GLM-5.3-Flash", sent["model"])
	}
	raw, _ := io.ReadAll(rc)
	sse := string(raw)
	for _, want := range []string{`"reasoning_content":"think"`, `"content":"pong"`, `"finish_reason":"stop"`, `"prompt_cache_hit_tokens":4`, "data: [DONE]"} {
		if !strings.Contains(sse, want) {
			t.Errorf("converted SSE missing %q:\n%s", want, sse)
		}
	}
}

func TestZCodeStartOnlyFlashAndClassify(t *testing.T) {
	c := &Client{}
	a := &auth.Auth{}
	a.SetProducer(ProducerZCode)
	a.SetZCodeJWT("jwt")
	if !c.zcodeStartOn(a, []byte(`{"model":"glm-5.3-flash"}`)) {
		t.Fatal("flash should route to Start Plan")
	}
	if c.zcodeStartOn(a, []byte(`{"model":"glm-5.2"}`)) {
		t.Fatal("non-flash must stay on static channel")
	}
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, `{"error":{"message":"token expired"}}`, ErrSessionDead},
		{429, `{"code":1113,"msg":"insufficient balance"}`, ErrHardCredit},
		{405, `{"code":3012,"msg":"unusual activity"}`, ErrAccountFault},
		{429, `{"code":1005,"msg":"insufficient quota"}`, ErrHardCredit},
	}
	for _, tc := range cases {
		if got := classifyZCodeStart(tc.status, tc.body); got != tc.want {
			t.Errorf("classify(%d,%s)=%v want %v", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestZCodeStartSSEThinkingOnlyStillHasChatFrame(t *testing.T) {
	raw := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"id":"msg_t","model":"glm-5.3-flash","usage":{"input_tokens":1,"output_tokens":1}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"only think"}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	rc := newZCodeStartSSEConverter(io.NopCloser(strings.NewReader(raw)))
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	if !strings.Contains(s, `"reasoning_content":"only think"`) {
		t.Errorf("missing reasoning delta: %s", s)
	}
	if !strings.Contains(s, `"role":"assistant"`) {
		t.Errorf("missing assistant role frame: %s", s)
	}
	if !strings.Contains(s, "data: [DONE]") {
		t.Errorf("missing done: %s", s)
	}
}

func TestZCodeStartEmptyUpstreamReturns502(t *testing.T) {
	oldURL := zcodeStartMessagesURL
	defer func() { zcodeStartMessagesURL = oldURL }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	zcodeStartMessagesURL = srv.URL

	c := &Client{HTTP: &http.Client{}}
	a := &auth.Auth{UID: "z1", Nickname: "zc"}
	a.SetProducer(ProducerZCode)
	a.SetZCodeJWT("jwt-test")
	a.SetZCodeDeviceMID("device-test")
	body := []byte(`{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hello"}]}`)
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{ConversationID: "conv"})
	if rc != nil {
		rc.Close()
	}
	if err == nil {
		t.Fatal("empty upstream must return an error before handler writes 200")
	}
	if status != http.StatusBadGateway {
		t.Fatalf("status=%d want 502", status)
	}
	ue, ok := err.(*Error)
	if !ok || ue.Kind != ErrServer {
		t.Fatalf("error=%T %v want *Error{Kind:ErrServer}", err, err)
	}
}

func TestPreReadZCodeStartKeepsFirstBytes(t *testing.T) {
	first := "data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\n"
	rest := "data: {\"choices\":[{\"delta\":{\"content\":\"ng\"}}]}\n\ndata: [DONE]\n\n"
	rc := io.NopCloser(strings.NewReader(first + rest))
	prefix, err := preReadZCodeStart(rc, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(&prefixReadCloser{prefix: prefix, rc: rc})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != first+rest {
		t.Fatalf("prefix bytes lost:\n got %q\nwant %q", got, first+rest)
	}
}

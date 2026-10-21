package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"free2api/internal/auth"
)

func TestGrowthWebChatCreatesConversationAndReadsSSE(t *testing.T) {
	var conversationCalls, chatCalls atomic.Int32
	var chatBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case growthWebConversationsPath:
			conversationCalls.Add(1)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("conversation body: %v", err)
			}
			if _, ok := body["name"].(string); !ok {
				t.Errorf("conversation name missing: %#v", body)
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"conversationId":"conv-123"}}`))
		case growthWebChatPath:
			chatCalls.Add(1)
			if got := r.Header.Get("Accept"); got != "text/event-stream" {
				t.Errorf("Accept=%q", got)
			}
			if got := r.Header.Get("x-codebuddy-request"); got != "1" {
				t.Errorf("x-codebuddy-request=%q", got)
			}
			_ = json.NewDecoder(r.Body).Decode(&chatBody)
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
				"data: [DONE]\n\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &Client{HTTP: srv.Client(), ChatHTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	reply, err := c.GrowthWebChat(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "说你好", "glm-5.2", map[string]any{"expert": "x"})
	if err != nil {
		t.Fatalf("GrowthWebChat: %v", err)
	}
	if conversationCalls.Load() != 1 || chatCalls.Load() != 1 {
		t.Fatalf("calls conversation=%d chat=%d", conversationCalls.Load(), chatCalls.Load())
	}
	if reply.ConversationID != "conv-123" || reply.Text != "你好" {
		t.Fatalf("reply=%+v", reply)
	}
	if chatBody["conversationId"] != "conv-123" || chatBody["model"] != "glm-5.2" || chatBody["stream"] != true {
		t.Fatalf("chat body=%#v", chatBody)
	}
	meta, ok := chatBody["_meta"].(map[string]any)
	if !ok || meta["expert"] != "x" {
		t.Fatalf("_meta=%#v", chatBody["_meta"])
	}
}

func TestGrowthWebChatRejectsEmptySSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == growthWebConversationsPath {
			_, _ = w.Write([]byte(`{"code":0,"data":{"conversationId":"conv-empty"}}`))
			return
		}
		if r.URL.Path == growthWebChatPath {
			_, _ = w.Write([]byte("data: {\"choices\":[]}\n\ndata: [DONE]\n\n"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatHTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.GrowthWebChat(context.Background(), &auth.Auth{UID: "u1", AccessToken: "at"}, "hi", "m", nil); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty SSE err=%v", err)
	}
}

func TestReadGrowthChatSSEHandlesMessageAndDone(t *testing.T) {
	body := strings.NewReader("data: {\"choices\":[{\"message\":{\"content\":\"完整\"}}]}\n\ndata: [DONE]\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"不应读取\"}}]}\n\n")
	text, err := readGrowthChatSSE(io.NopCloser(body))
	if err != nil {
		t.Fatal(err)
	}
	if text != "完整" {
		t.Fatalf("text=%q", text)
	}
}

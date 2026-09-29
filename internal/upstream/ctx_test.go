package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestChatStreamContextCancelPropagates 客户端 ctx 取消应立即中断在途 SSE 流。
// 回归 issue:ctx 不传播 → 断连后幽灵请求占满在途名额直到 IdleTimeout。
func TestChatStreamContextCancelPropagates(t *testing.T) {
	// 上游：发一帧后阻塞,直到请求 ctx 被取消才返回（模拟长流）。
	unblock := make(chan struct{})
	c := testClient(func(r *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			// 阻塞到请求 ctx 取消;随后关流让 Read 返回。
			<-r.Context().Done()
			pw.CloseWithError(r.Context().Err())
			close(unblock)
		}()
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
		}, nil
	})
	c.IdleTimeout = 5 * time.Second // 足够大:若只靠 idle 掐流则测试会变慢/超时

	ctx, cancel := context.WithCancel(context.Background())
	rc, status, _, err := c.ChatStreamContext(ctx, &auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-ctx-test"})
	if err != nil || status != 200 {
		t.Fatalf("ChatStreamContext: status=%d err=%v", status, err)
	}
	// 读首帧(确认流已建立)。
	buf := make([]byte, 4096)
	n, _ := rc.Read(buf)
	if n == 0 {
		t.Fatalf("expected first frame, got EOF")
	}
	// 取消客户端 ctx → 后续 Read 应立即返回错误(而非阻塞到 IdleTimeout)。
	cancel()
	done := make(chan error, 1)
	go func() {
		_, rerr := rc.Read(buf)
		done <- rerr
	}()
	select {
	case rerr := <-done:
		if rerr == nil {
			t.Errorf("read after cancel: want non-nil error (ctx canceled)")
		}
	case <-time.After(2 * time.Second):
		t.Errorf("read after cancel did not return within 2s (ctx 取消未传播到在途流)")
	}
	rc.Close()
	<-unblock
}

// TestChatStreamContextNilFallback nil ctx 回落 Background,不报 panic。
func TestChatStreamContextNilFallback(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	// 传 nil ctx(显式断言不 panic 且正常返回)。
	rc, status, _, err := c.ChatStreamContext(nil, &auth.Auth{AccessToken: "at"}, []byte(`{}`), "", ChatMeta{ConversationRequestID: "req-nil-ctx"})
	if err != nil || status != 200 {
		t.Fatalf("nil ctx: status=%d err=%v", status, err)
	}
	rc.Close()
}

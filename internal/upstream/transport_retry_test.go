// transport_retry_test.go 传输层抖动原地重试的回归。
//
// 现场：开/关 VPN（TUN + fake-ip）时，路由表切换会把所有在途 TCP 打死，表现为
// EOF / wsasend 强制关闭。旧行为把这个错误原样抛回 handler，由它换号——号池里
// 每个号都被打一遍再回 503，体感就是「一开 VPN 网关就挂」。
package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"free2api/internal/auth"
)

// sseResp 最小可用的 SSE 200 响应。
func sseResp() *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")),
	}
}

// TestChatStreamRetriesTransportFlap 首次 EOF、第二次成功：调用方拿到 200，
// 不应该把「网络抖了一下」升级成换号/503。
func TestChatStreamRetriesTransportFlap(t *testing.T) {
	var calls int32
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, errors.New("read: EOF")
		}
		return sseResp(), nil
	})
	c := testClient(rt)

	rc, status, _, err := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-retry-1"})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v want 200 nil", status, err)
	}
	if rc == nil {
		t.Fatal("rc=nil want stream")
	}
	rc.Close()
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("upstream calls=%d want 2 (1 fail + 1 retry)", got)
	}
}

// TestChatStreamRetryBounded 一直失败：重试次数有上限（1 + transportRetries 次调用），
// 最终仍把传输层错误交回调用方（由 handler 决定换号），不会无限重试。
func TestChatStreamRetryBounded(t *testing.T) {
	var calls int32
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("wsasend: An existing connection was forcibly closed by the remote host.")
	})
	c := testClient(rt)

	_, _, _, err := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-retry-2"})
	if err == nil {
		t.Fatal("err=nil want transport error")
	}
	want := int32(1 + transportRetries)
	if got := atomic.LoadInt32(&calls); got != want {
		t.Fatalf("upstream calls=%d want %d", got, want)
	}
}

// TestChatStreamRetryAbortsOnCancel 客户端断开（ctx 取消）后不再重试。
func TestChatStreamRetryAbortsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int32
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		cancel() // 第一次失败的同时客户端撤了
		return nil, errors.New("EOF")
	})
	c := testClient(rt)

	if _, _, _, err := c.ChatStreamContext(ctx,
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-retry-3"}); err == nil {
		t.Fatal("err=nil want transport error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("upstream calls=%d want 1 (no retry after cancel)", got)
	}
}

// TestDialTargetLabel 日志标注：fake-ip 段显式标 TUN，公网 IP 原样，空值说明连都没连上。
func TestDialTargetLabel(t *testing.T) {
	cases := map[string]string{
		"":                   "未建立连接",
		"198.18.0.28:443":    "198.18.0.28:443(fake-ip/TUN)",
		"198.19.5.5:443":     "198.19.5.5:443(fake-ip/TUN)",
		"43.160.158.125:443": "43.160.158.125:443",
		"115.150.39.190:443": "115.150.39.190:443",
	}
	for in, want := range cases {
		if got := dialTargetLabel(in); got != want {
			t.Errorf("dialTargetLabel(%q)=%q want %q", in, got, want)
		}
	}
}

package upstream

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestNewDialerParams DialContext 拨号参数回读断言（Transport 只存闭包，参数
// 断言落在 newDialer 上——集中定义的另一头）。
func TestNewDialerParams(t *testing.T) {
	d := newDialer()
	if d.Timeout != dialTimeout {
		t.Errorf("dialer.Timeout=%v want %v", d.Timeout, dialTimeout)
	}
	if d.KeepAlive != dialKeepAlive {
		t.Errorf("dialer.KeepAlive=%v want %v", d.KeepAlive, dialKeepAlive)
	}
	if d.Timeout != 10*time.Second || d.KeepAlive != 15*time.Second {
		t.Errorf("dial params=(%v, %v) want (10s, 15s)", d.Timeout, d.KeepAlive)
	}
}

// TestTransportDisableKeepAlivesStaysFalse 显式锁定「DisableKeepAlives 不吸收」
// 决策：保持 false（连接复用是本网关连接池的既有意图，MaxIdleConnsPerHost=20）。
// 若未来有人误开此开关，此断言强制其先读到报告里的 trade-off 分析。
func TestTransportDisableKeepAlivesStaysFalse(t *testing.T) {
	if tr := newTransport(); tr.DisableKeepAlives {
		t.Error("DisableKeepAlives must stay false: per-request TLS handshake contradicts MaxIdleConnsPerHost=20 reuse intent (see transport-hardening.md)")
	}
}

// TestResponseHeaderTimeoutDoesNotKillSSELongStream 流式场景回归（任务书设计
// 纪律）：ResponseHeaderTimeout 只约束「响应头到达前」的时长，头到达后无论流
// 持续多久都不受它影响。用真实 Transport + 慢头（> 超时，必失败）与慢体（头
// 快到、体长于超时且持续吐数据，必须成功）双向验证语义。
func TestResponseHeaderTimeoutDoesNotKillSSELongStream(t *testing.T) {
	tr := newTransport()
	// 场景 A：响应头超过 ResponseHeaderTimeout → 必须失败（这是该超时唯一
	// 管辖的窗口）。用一台「延迟 300ms 才写头」的服务器（远小于 60s，故把
	// 本测试的 Transport 超时临时收紧到 100ms——语义验证不依赖生产量级）。
	tr.ResponseHeaderTimeout = 100 * time.Millisecond
	slowHeader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer slowHeader.Close()
	c := &http.Client{Transport: tr, Timeout: 0} // 同 ChatHTTP：无总时长上限
	start := time.Now()
	_, err := c.Get(slowHeader.URL)
	if err == nil {
		t.Fatal("slow-header request should fail with ResponseHeaderTimeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("slow-header failure took %v, want ~ResponseHeaderTimeout", elapsed)
	}

	// 场景 B：头即时到达、流持续远超 ResponseHeaderTimeout 且持续吐数据 →
	// 必须成功（长 SSE 流不被误杀）。
	slowBody := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200) // 头立即写
		w.(http.Flusher).Flush()
		// 持续 600ms 吐帧（> 100ms 超时 6 倍），期间每 50ms 一帧。
		for i := 0; i < 12; i++ {
			time.Sleep(50 * time.Millisecond)
			fmt.Fprintf(w, "data: tick %d\n\n", i)
			w.(http.Flusher).Flush()
		}
	}))
	defer slowBody.Close()
	resp, err := c.Get(slowBody.URL)
	if err != nil {
		t.Fatalf("long-stream request must not be killed by ResponseHeaderTimeout: %v", err)
	}
	defer resp.Body.Close()
	n, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read long stream: %v", err)
	}
	if len(n) == 0 {
		t.Fatal("long stream body empty")
	}
}

// TestRoundTripCloseIdleCleansRealTransport 传输层失败清理的端到端验证：
// 真实 *http.Transport 挂空闲连接 → roundTripCloseIdle → 服务器侧连接关闭。
func TestRoundTripCloseIdleCleansRealTransport(t *testing.T) {
	srv := startIdleConnTracker(t)
	tr := newTransport()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	for i := 0; i < 2; i++ { // keep-alive 复用同一条连接并回池
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		resp.Body.Close()
	}
	if n := srv.idleConns(); n < 1 {
		t.Fatalf("expected >=1 idle conn after keep-alive requests, got %d", n)
	}

	roundTripCloseIdle(tr)

	// CloseIdleConnections 传播到服务器侧需要一小段时间（FIN 往返）。
	deadline := time.Now().Add(2 * time.Second)
	for srv.idleConns() > 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := srv.idleConns(); n != 0 {
		t.Errorf("idle conns after roundTripCloseIdle = %d, want 0", n)
	}
}

// TestRoundTripCloseIdleToleratesForeignTransport roundTripCloseIdle 是
// best-effort：nil 与不实现 closeIdler 的 RoundTripper（测试注入 rtFunc）静默
// 跳过，不 panic、不误伤。
func TestRoundTripCloseIdleToleratesForeignTransport(t *testing.T) {
	roundTripCloseIdle(nil)
	roundTripCloseIdle(rtFunc(func(*http.Request) (*http.Response, error) { return nil, nil }))
}

// idleCountingTransport 包装 rtFunc 并实现 closeIdler（记录清池调用），
// 验证 ChatStreamContext 挂载点「传输层 Do 失败 → 必清池」。
type idleCountingTransport struct {
	rt       rtFunc
	closeIds *int32
}

func (t *idleCountingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return t.rt(r)
}

func (t *idleCountingTransport) CloseIdleConnections() {
	*t.closeIds++
}

// TestChatStreamTransportErrorCleansIdleConnections 挂载点行为验证：ChatHTTP 的
// Transport Do 失败（传输层错误）后 CloseIdleConnections 恰好被调用一次；
// 成功路径不触发（成功连接留在池里供复用是连接池的本意）。
func TestChatStreamTransportErrorCleansIdleConnections(t *testing.T) {
	var closeCalls int32
	c := testClient(func(*http.Request) (*http.Response, error) {
		t.Fatal("ChatHTTP is set; plain HTTP client must not be used for chat")
		return nil, nil
	})
	c.ChatHTTP = &http.Client{Timeout: 0, Transport: &idleCountingTransport{
		closeIds: &closeCalls,
		rt: func(r *http.Request) (*http.Response, error) {
			// 首次请求成功（200 + SSE 流）；第二次起模拟连接被对端掐掉
			// （net.OpError，Do 返回错误——正是传输层失败形态）。
			if closeCalls == 0 && r.Header.Get("X-Sim-Fail") == "" {
				return &http.Response{
					StatusCode: 200,
					Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
					Body:       io.NopCloser(io.LimitReader(zeroReader{}, 0)),
				}, nil
			}
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: opErrConnReset{}}
		},
	}}
	a := &auth.Auth{AccessToken: "at", UID: "u1"}

	// 成功路径：不触发清池。
	rc, status, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{})
	if err != nil || status != 200 {
		t.Fatalf("first chat: status=%d err=%v", status, err)
	}
	rc.Close()
	if closeCalls != 0 {
		t.Errorf("CloseIdleConnections calls after success = %d, want 0", closeCalls)
	}

	// 传输层失败路径：必须触发清池一次。给请求打标记让 RoundTripper 失败——
	// 通过 body 里夹私有标记不可行（会进上游 body），用 meta 无关方式：直接
	// 第二次调用（RoundTripper 按 closeCalls==0 判首轮已过）。
	c2 := testClient(func(*http.Request) (*http.Response, error) { return nil, nil })
	c2.ChatHTTP = c.ChatHTTP
	// 第二次调用前先把首轮标记置位：直接复用同一 Client 更贴近生产（连接池
	// 状态持续）。用同一 c 再调一次即可——RoundTripper 逻辑：closeCalls==0 且
	// 无失败标记 → 成功；否则失败。为进入失败分支，需要触发条件翻转，故这里
	// 换一个「恒失败」的 Transport 共享同一计数器。
	c.ChatHTTP = &http.Client{Timeout: 0, Transport: &idleCountingTransport{
		closeIds: &closeCalls,
		rt: func(*http.Request) (*http.Response, error) {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: opErrConnReset{}}
		},
	}}
	if _, _, _, err := c.ChatStream(a, []byte(`{}`), "", ChatMeta{}); err == nil {
		t.Fatal("second chat should fail with transport error")
	}
	if closeCalls != 1 {
		t.Errorf("CloseIdleConnections calls after transport error = %d, want 1", closeCalls)
	}
}

// opErrConnReset 模拟 connection reset（net.OpError 内层错误形态）。
type opErrConnReset struct{}

func (opErrConnReset) Error() string { return "connection reset by peer" }

// zeroReader 空 Reader（构造最小 SSE 响应体）。
type zeroReader struct{}

func (zeroReader) Read([]byte) (int, error) { return 0, io.EOF }

// idleConnTracker 测试服务器 + 服务器侧空闲连接集合（ConnState 钩子）。
// 用集合而非计数器：同一连接每次复用回空闲态都会再触发 StateIdle，计数会虚增。
type idleConnTracker struct {
	*httptest.Server
	mu   sync.Mutex
	idle map[net.Conn]struct{}
}

func startIdleConnTracker(t *testing.T) *idleConnTracker {
	t.Helper()
	tr := &idleConnTracker{idle: make(map[net.Conn]struct{})}
	tr.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	tr.Server.Config.ConnState = func(c net.Conn, st http.ConnState) {
		tr.mu.Lock()
		defer tr.mu.Unlock()
		switch st {
		case http.StateIdle:
			tr.idle[c] = struct{}{}
		case http.StateClosed:
			delete(tr.idle, c)
		}
	}
	tr.Server.Start()
	t.Cleanup(func() { tr.Server.Close() })
	return tr
}

func (s *idleConnTracker) idleConns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.idle)
}

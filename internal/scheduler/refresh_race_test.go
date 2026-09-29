package scheduler

// refresh_race_test.go keepalive 前置守卫的锁外直读回归（#125 同族残留）。
//
// 生产并发面：Scheduler.RunKeepaliveNow 由定时排程在 keepalive_hours 窗口触发，
// 对**每个**非禁用账号调 upstream.RefreshToken——后者在 a.mu 内改写
// AccessToken/RefreshToken/Domain/ExpiresAt（client.go「第 2 段（锁内）：校验快照
// 一致后写回」）。而 cmd/login 的登录流程与同进程内其他刷新路径可在同一个
// *auth.Auth 上并发发起刷新（upstream 的刷新去重只保证「不重复写回」，不排斥并发
// 进入）。守卫 `a.RefreshToken == ""` 与写回落在同一字段上且无同步 → 数据竞争。
//
// 修法对齐已有的 auth.AccessTokenValue / auth.DomainValue（#125）：新增同族加锁
// 访问器 auth.RefreshTokenValue，守卫改走访问器，不另造机制。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

// TestKeepaliveSkipsAccountWithoutRefreshToken 守卫的既有语义不能因改走访问器而漂移：
// 无 refreshToken 的账号必须被跳过（不发起任何上游刷新）。与并发回归测试配对：
// 前者防漏读、后者防语义改变。
func TestKeepaliveSkipsAccountWithoutRefreshToken(t *testing.T) {
	var refreshCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token/refresh") {
			refreshCalls.Add(1)
		}
		w.Write([]byte(`{"code":0,"data":{"accessToken":"newat","expiresIn":3600}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "no-rt", AccessToken: "at", ExpiresAt: 9999999999})
	p.Add(&auth.Auth{UID: "has-rt", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	s.RunKeepaliveNow()

	if got := refreshCalls.Load(); got != 1 {
		t.Errorf("只应有 has-rt 一个账号发起刷新，实际 %d 次", got)
	}
}

// TestKeepaliveGuardRacesRefreshToken 并发下 RunKeepaliveNow 的 RefreshToken
// 守卫读与 RefreshToken 写回构成数据竞争（go test -race 实证）。
//
// 读侧 = keepalive 前置守卫（`a.RefreshToken == ""`）；写侧 = 同账号刷新写回。
func TestKeepaliveGuardRacesRefreshToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/token/refresh") {
			http.Error(w, "not found", 404)
			return
		}
		// refreshToken 返回非空值：让写回的 `if tok.RefreshToken != ""` 分支真正执行，
		// 守卫读与写落在同一字段上（否则该写入被挡掉，竞争不可见）。
		w.Write([]byte(`{"code":0,"data":{"accessToken":"newat","refreshToken":"newrt","expiresIn":3600}}`))
	}))
	defer srv.Close()

	p := pool.New("")
	p.Add(&auth.Auth{UID: "u1", AccessToken: "at", RefreshToken: "rt", ExpiresAt: 9999999999})
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	s := New(Config{Pool: p, Upstream: up})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 写侧：模拟同账号上的并发刷新（RefreshToken 在 a.mu 内改写 RefreshToken）。
	// 多个 goroutine 提高写频次，让守卫读与写回落在同一时间窗内（否则单写者的
	// 网络往返间隔会把两者错开，竞争在 -race 下偶然可见）。
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = up.RefreshToken(p.AuthByUID("u1"))
			}
		}()
	}

	// 读侧：keepalive 每号前置守卫（锁外直读 a.RefreshToken）。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			s.RunKeepaliveNow()
		}
		close(stop)
	}()

	wg.Wait()
}

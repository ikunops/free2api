package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// TestIsAlready 幂等码识别：today-already / 未开启 / 已过期 / inactive 类
// 业务错误应判为"幂等/不适用"（不是 FAIL），对 global 账号是手跑 signin 的兜底。
func TestIsAlready(t *testing.T) {
	errClient := func(msg string) error {
		return &upstream.Error{Kind: upstream.ErrClient, Status: 409, Msg: msg}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// 今天已签到：实测 code=10001（login.sh），另有 14001 变体。
		{name: "10001 today already", err: errClient("code=10001 msg=今天已签到"), want: true},
		{name: "14001 today already", err: errClient("code=14001 msg=今天已签到"), want: true},
		// HTTP 4xx 原始 JSON body（doJSON 非 2xx 分支 Msg=raw body）：冒号形式照样命中。
		{name: "10001 colon body", err: errClient(`{"code":10001,"msg":"今天已签到"}`), want: true},
		// global 无签到体系类：功能未开启 / 已过期 / inactive。
		{name: "not enabled", err: errClient("功能未开启"), want: true},
		{name: "expired", err: errClient("已过期"), want: true},
		{name: "inactive session", err: errClient("session inactive"), want: true},
		{name: "inactive body code", err: errClient("code=20003 msg=inactive"), want: true},
		// 非幂等：各种真实失败。
		{name: "server 500", err: errClient("boom"), want: false},
		{name: "generic body", err: errClient("code=50003 msg=network"), want: false},
		// 数字盲区边界：12001（含 2001? 不，含 10001 子串？12001 不含 10001）——
		// 但 10001 前是 '2'，需确认不误命中；2010001 也不含独立 10001。
		{name: "2010001 no hit", err: errClient("code=2010001 msg=foo"), want: false},
		{name: "12001 no hit", err: errClient("code=12001 msg=foo"), want: false},
		// 网络/解析层错误（非 *Error）：不带已签到关键词 → 不判幂等。
		{name: "transport error", err: errors.New("dial tcp: connection refused"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isAlready(c.err); got != c.want {
				t.Errorf("isAlready(%q)=%v want %v", c.err, got, c.want)
			}
		})
	}
}

// TestIsAlreadyStringFallback 非 *Error 错误退回中文文案匹配：
// 携带"已签到"字样仍判幂等；普通传输层错误不判。
func TestIsAlreadyStringFallback(t *testing.T) {
	if !isAlready(errors.New("failed to checkin: 已签到")) {
		t.Error("带'已签到'字样的非结构化错误应判幂等")
	}
	if isAlready(errors.New("failed to checkin: connection reset")) {
		t.Error("普通传输层错误不应判幂等")
	}
}

// TestIsAlreadyStringFallbackEnglishFalsePositive 英文短词只在业务 *Error 里判幂等；
// 裸错误（网络/代理文本）含 already/inactive 不得误判——"address already in use"
// 是 TCP 绑定失败的常见文本，若误判成已签到会在停机补签时掩盖真实故障。
func TestIsAlreadyStringFallbackEnglishFalsePositive(t *testing.T) {
	bad := []string{
		"bind: address already in use",
		"proxy session inactive, retrying",
		"tls handshake failed: already exited",
	}
	for _, s := range bad {
		if isAlready(errors.New(s)) {
			t.Errorf("裸错误 %q 不应判幂等（英文短词误命中）", s)
		}
	}
	// 同文本如果是业务 *Error（Msg 来自上游），already 就是真实业务信号，仍须命中。
	if !isAlready(&upstream.Error{Kind: upstream.ErrClient, Status: 200, Msg: "session inactive"}) {
		t.Error("业务 *Error 中的 inactive 应判幂等")
	}
}

// TestCheckinStatusOf signin 主循环的 DailyCheckin 分支状态机对幂等码产出
// ALREADY 而非 FAIL（避免手跑 signin 时把 global 账号成片标红）。
func TestCheckinStatusOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "ok", err: nil, want: "OK"},
		{name: "already 10001", err: &upstream.Error{Kind: upstream.ErrClient, Status: 409, Msg: "code=10001 msg=今天已签到"}, want: "ALREADY"},
		{name: "already 14001", err: &upstream.Error{Kind: upstream.ErrClient, Status: 409, Msg: "code=14001 msg=今天已签到"}, want: "ALREADY"},
		{name: "generic fail", err: &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}, want: "FAIL"},
		{name: "transport err", err: errors.New("dial tcp: connection refused"), want: "FAIL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := checkinStatusOf(c.err); got != c.want {
				t.Errorf("checkinStatusOf(%q)=%q want %q", c.err, got, c.want)
			}
		})
	}
}

// TestSigninFileGlobViaAuthLoadFiles (P2-10)：signin 的文件清单改走
// auth.LoadAuthFiles 后，不带连字符的文件（workbuddy_new.json）与网关
// LoadDir 同口径加载。main() 不可直测，此处锁共享契约 + signin 侧无
// 私有 glob 残留（源码层面 sort/filepath.Glob 依赖已随重构移除，编译期
// 由未用 import 守卫）。
func TestSigninFileGlobViaAuthLoadFiles(t *testing.T) {
	dir := t.TempDir()
	doc := `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy_new.json"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	files, err := auth.LoadAuthFiles(dir)
	if err != nil || len(files) != 1 {
		t.Fatalf("LoadAuthFiles: files=%v err=%v want 1", files, err)
	}
}

// TestRefreshStatusOfWrappedError (P2-11 RED)：refresh 失败分支此前用
// err.(*upstream.Error) 类型断言，错误被 %w 包装时漏判 ErrSessionDead →
// 已死号被标成普通 FAIL 而非 AUTH_INVALID。同文件 isAlready 早已用
// errors.As 的正确写法，此处对齐。
func TestRefreshStatusOfWrappedError(t *testing.T) {
	plain := &upstream.Error{Kind: upstream.ErrSessionDead, Status: 401, Msg: "code=12153"}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"bare session dead", plain, "AUTH_INVALID"},
		{"wrapped session dead", fmt.Errorf("refresh: %w", plain), "AUTH_INVALID"},
		{"double wrapped", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", plain)), "AUTH_INVALID"},
		{"other upstream error", &upstream.Error{Kind: upstream.ErrServer, Status: 500, Msg: "boom"}, "FAIL"},
		{"transport error", errors.New("dial tcp: connection refused"), "FAIL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := refreshStatusOf(c.err); got != c.want {
				t.Errorf("refreshStatusOf(%v)=%q want %q", c.err, got, c.want)
			}
		})
	}
}

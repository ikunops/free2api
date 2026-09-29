package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// readLoginState 读回 url 落盘的 state 文件。
func readLoginState(t *testing.T, path string) loginState {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var ls loginState
	if err := json.Unmarshal(raw, &ls); err != nil {
		t.Fatalf("parse state: %v", err)
	}
	return ls
}

// fileExists 判断路径是否存在。
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fakeUpstream 记录 fake 上游收到的请求，供断言端点路径与 Origin/Referer 头。
type fakeUpstream struct {
	stateCalls   atomic.Int32
	statePath    string
	stateOrigin  string
	stateReferer string
	tokenPath    string
	tokenOrigin  string
}

// newFakeUpstream 启动模拟上游：/v2/plugin/auth/state → {state,authUrl}；
// /v2/plugin/auth/token → {accessToken,...}；/v2/plugin/login/account → {uid,nickname}。
func newFakeUpstream(t *testing.T) (*httptest.Server, *fakeUpstream) {
	t.Helper()
	st := &fakeUpstream{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/plugin/auth/state", func(w http.ResponseWriter, r *http.Request) {
		st.stateCalls.Add(1)
		st.statePath = r.URL.Path
		st.stateOrigin = r.Header.Get("Origin")
		st.stateReferer = r.Header.Get("Referer")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"state":   "fake-state",
				"authUrl": "https://" + r.Host + "/authorize?state=fake-state",
			},
		})
	})
	mux.HandleFunc("/v2/plugin/auth/token", func(w http.ResponseWriter, r *http.Request) {
		st.tokenPath = r.URL.Path
		st.tokenOrigin = r.Header.Get("Origin")
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{
				"accessToken":  "at",
				"refreshToken": "rt",
				"expiresIn":    3600,
				"domain":       "www.workbuddy.ai",
			},
		})
	})
	mux.HandleFunc("/v2/plugin/login/account", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"code": 0,
			"data": map[string]any{"uid": "u1", "nickname": "n1"},
		})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, st
}

// TestLoginURLGlobalEndpoint fake upstream 验证 --realm=global url：POST 打到
// base 的 /v2/plugin/auth/state，且 Origin/Referer 为 workbuddy.ai（global 上游走对）。
// base 由调用方注入（真实运行时来自 realmConfig(global)，已由 TestRealmConfig 覆盖率保证
// 是 www.workbuddy.ai）；本测试证明请求确实发往注入的 base 路径且携带正确来源头。
func TestLoginURLGlobalEndpoint(t *testing.T) {
	ts, st := newFakeUpstream(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	var out bytes.Buffer
	runURL(ts.URL, "https://www.workbuddy.ai", realmGlobal, statePath, ts.Client(), &out)

	if got := st.stateCalls.Load(); got != 1 {
		t.Fatalf("state calls=%d want 1", got)
	}
	if st.statePath != "/v2/plugin/auth/state" {
		t.Errorf("state path=%q want /v2/plugin/auth/state", st.statePath)
	}
	if st.stateOrigin != "https://www.workbuddy.ai" {
		t.Errorf("Origin=%q want https://www.workbuddy.ai", st.stateOrigin)
	}
	if st.stateReferer != "https://www.workbuddy.ai/" {
		t.Errorf("Referer=%q want https://www.workbuddy.ai/", st.stateReferer)
	}
	if !strings.Contains(out.String(), "/authorize?state=fake-state") {
		t.Errorf("authUrl printed=%q want contains authUrl", out.String())
	}
	// state 落盘内容应带 realm=global
	res := readLoginState(t, statePath)
	if res.State != "fake-state" || res.Realm != realmGlobal {
		t.Errorf("state file=%+v want state=fake-state realm=global", res)
	}
}

// TestLoginURLCNEndpoint 零回归：login url（默认 cn）打到 CN base 的 state 端点，
// Origin/Referer 为 codebuddy.cn。
func TestLoginURLCNEndpoint(t *testing.T) {
	ts, st := newFakeUpstream(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	var out bytes.Buffer
	runURL(ts.URL, "https://www.codebuddy.cn", realmCN, statePath, ts.Client(), &out)

	if got := st.stateCalls.Load(); got != 1 {
		t.Fatalf("state calls=%d want 1", got)
	}
	if st.statePath != "/v2/plugin/auth/state" {
		t.Errorf("state path=%q want /v2/plugin/auth/state", st.statePath)
	}
	if st.stateOrigin != "https://www.codebuddy.cn" {
		t.Errorf("Origin=%q want https://www.codebuddy.cn", st.stateOrigin)
	}
	if st.stateReferer != "https://www.codebuddy.cn/" {
		t.Errorf("Referer=%q want https://www.codebuddy.cn/", st.stateReferer)
	}
}

// TestPollRealmMatchAndHitsGlobal 完整登录流：url 落盘 realm=global → poll 用 global
// realm 读回，校验通过，token 请求打到 /v2/plugin/auth/token 且 Origin 为 workbuddy.ai，
// account 请求命中 /v2/plugin/login/account，输出 JSON 含 access_token 与 realm=global。
func TestPollRealmMatchAndHitsGlobal(t *testing.T) {
	ts, st := newFakeUpstream(t)
	statePath := filepath.Join(t.TempDir(), "state.json")

	var urlOut bytes.Buffer
	runURL(ts.URL, "https://www.workbuddy.ai", realmGlobal, statePath, ts.Client(), &urlOut)

	var pollOut bytes.Buffer
	runPoll(ts.URL, "https://www.workbuddy.ai", realmGlobal, statePath, ts.Client(), &pollOut)

	if st.tokenPath != "/v2/plugin/auth/token" {
		t.Errorf("token path=%q want /v2/plugin/auth/token", st.tokenPath)
	}
	if st.tokenOrigin != "https://www.workbuddy.ai" {
		t.Errorf("token Origin=%q want https://www.workbuddy.ai", st.tokenOrigin)
	}
	var out map[string]any
	if err := json.Unmarshal(pollOut.Bytes(), &out); err != nil {
		t.Fatalf("poll output not json: %v", err)
	}
	if out["access_token"] != "at" {
		t.Errorf("access_token=%v want at", out["access_token"])
	}
	if out["realm"] != realmGlobal {
		t.Errorf("realm=%v want global", out["realm"])
	}
	// poll 成功后 state 文件应移除
	if fileExists(statePath) {
		t.Errorf("state file should be removed after poll")
	}
}

// TestPollRealmMismatch 进程内捕获 fatal：state 文件 realm=cn，命令行 --realm=global
// 时 runPoll 在发起任何 token 请求前 fatal（防混域）。通过替换 exitFunc 为 panic
// 捕获退出码与错误文本，验证「混域被拒绝」的真实 runPoll 路径。
func TestPollRealmMismatch(t *testing.T) {
	ts, st := newFakeUpstream(t)
	statePath := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(statePath, []byte(`{"state":"s1","realm":"cn"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// 替换 exitFunc → panic（进程内捕获），跑完还原。
	prev := exitFunc
	exitFunc = func(code int) { panic(fmt.Sprintf("exit %d", code)) }
	defer func() { exitFunc = prev }()

	var out bytes.Buffer
	func() {
		defer func() {
			r := recover()
			if r == nil {
				t.Fatal("runPoll did not fatal on realm mismatch")
			}
			exitMsg, ok := r.(string)
			if !ok || !strings.HasPrefix(exitMsg, "exit 1") {
				t.Fatalf("exitFunc called with %v, want exit 1", r)
			}
		}()
		runPoll(ts.URL, "https://www.workbuddy.ai", realmGlobal, statePath, ts.Client(), &out)
	}()

	// 校验在 token 请求前发生：fake 不应收到 token 请求
	if st.tokenPath != "" {
		t.Errorf("token path=%q want empty (should fatal before token call)", st.tokenPath)
	}
}

// TestValidateRealmMatch state 文件 realm 与命令行 --realm 一致性校验：
// 不一致 → error（含 "realm mismatch"）；一致或 state 无 realm（旧文件）→ nil。
// runPoll 在发起 token 请求前调用此校验，fatal 兜底（此处直测纯函数，进程内安全）。
func TestValidateRealmMatch(t *testing.T) {
	cases := []struct {
		name    string
		file    string
		cli     string
		wantErr string
	}{
		{name: "cn 匹配", file: "cn", cli: "cn"},
		{name: "global 匹配", file: "global", cli: "global"},
		{name: "cn vs global 不匹配", file: "cn", cli: "global", wantErr: "realm mismatch"},
		{name: "global vs cn 不匹配", file: "global", cli: "cn", wantErr: "realm mismatch"},
		{name: "state 无 realm（旧文件）放行", file: "", cli: "global"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateRealmMatch(c.file, c.cli)
			if c.wantErr == "" {
				if err != nil {
					t.Errorf("validateRealmMatch(%q,%q)=%v want nil", c.file, c.cli, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validateRealmMatch(%q,%q)=nil want %q", c.file, c.cli, c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("err=%v want contains %q", err, c.wantErr)
			}
		})
	}
}
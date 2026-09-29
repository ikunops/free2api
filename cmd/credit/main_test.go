package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// fakeUpstreamCredit 构造一个记录每个账号出站 host/path 的 fake upstream。
// ResourceSummary 走 billingMeterJSON → billingJSON → c.HTTP.Do，因此注入 Transport
// 即可捕获 realm 路由（base 由 billingBase(a) 决定）。
func fakeUpstreamCredit(t *testing.T, fn func(r *http.Request) (*http.Response, error)) *upstream.Client {
	t.Helper()
	return &upstream.Client{
		HTTP: &http.Client{Transport: roundTripFn(fn)},
		// CN base 假域名 + global base 假域名：断言 host 归属即可区分 realm。
		ChatBaseCN:        "https://chat.cn.example",
		BillingBaseCN:     "https://billing.cn.example",
		BillingBaseGlobal: "https://billing.global.example",
		GlobalEnabled:     true,
	}
}

type roundTripFn func(*http.Request) (*http.Response, error)

func (f roundTripFn) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func creditResp(body string) *http.Response {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func writeTestAuth(t *testing.T, dir, uid, token, domain, realm string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 嵌套形（与 login.sh 落盘同构）。realm 写进 auth.realm；domain 用于无 realm 回落。
	auth := map[string]any{
		"auth": map[string]any{
			"accessToken": token,
			"domain":      domain,
			"expiresAt":   int64(9999999999),
		},
		"account": map[string]any{
			"uid":      uid,
			"nickname": "n-" + uid,
		},
	}
	if realm != "" {
		auth["auth"].(map[string]any)["realm"] = realm
	}
	raw, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "workbuddy-"+uid[:8]+".json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestCollectGlobalAccountGoesToGlobalBase (RED→GREEN, 任务书验收点 2): global 账号
// 查积分必须打到 global base（billing.global.example），CN 账号打 CN base。
func TestCollectGlobalAccountGoesToGlobalBase(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var requests []string // "auth: host+path"
	up := fakeUpstreamCredit(t, func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.Header.Get("Authorization")+"|"+r.Host+r.URL.Path)
		return creditResp(`{"code":0,"data":{"Response":{"Data":{"Accounts":[
			{"PackageName":"p","CycleCapacitySize":100,"CycleCapacityRemain":80,"CycleCapacityUsed":20}
		]}}}}`), nil
	})

	dir := t.TempDir()
	writeTestAuth(t, dir, "globaluid-00000001", "at-global", "www.workbuddy.ai", "global")
	writeTestAuth(t, dir, "cnuid-00000002", "at-cn", "www.codebuddy.cn", "")

	accounts := collect(dir, up)
	if len(accounts) != 2 {
		t.Fatalf("accounts=%d want 2", len(accounts))
	}
	// 断言 realm 路由：global → global base（billing.global.example），CN → CN base。
	got := map[string]string{}
	for _, r := range requests {
		parts := strings.SplitN(r, "|", 2)
		got[parts[0]] = parts[1]
	}
	gHost, ok := got["Bearer at-global"]
	if !ok {
		t.Fatalf("global token auth 未被调用: got=%v", got)
	}
	if !strings.HasPrefix(gHost, "billing.global.example") {
		t.Errorf("global 账号打到 %q, want global base", gHost)
	}
	cnHost, ok := got["Bearer at-cn"]
	if !ok {
		t.Fatalf("cn token auth 未被调用: got=%v", got)
	}
	if !strings.HasPrefix(cnHost, "billing.cn.example") {
		t.Errorf("CN 账号打到 %q, want CN base", cnHost)
	}
}

// TestCollectGlobalPreferNoV2Path global 账号优先 /billing/meter/*（无 /v2）；
// 404 时 fallback /v2（双路径）。CN 账号单路径 /v2。
func TestCollectGlobalPreferNoV2Path(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var gz, vz int
	up := fakeUpstreamCredit(t, func(r *http.Request) (*http.Response, error) {
		if r.Host == "billing.global.example" {
			if r.URL.Path == "/billing/meter/get-user-resource" {
				gz++
				return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")),
					Header: http.Header{"Content-Type": []string{"application/json"}}}, nil
			}
			vz++
			return creditResp(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
		}
		return creditResp(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})

	dir := t.TempDir()
	writeTestAuth(t, dir, "globaluid-00000001", "at-global", "www.workbuddy.ai", "global")
	collect(dir, up)

	if gz != 1 || vz != 1 {
		t.Errorf("global 路径调用: 无/v2=%d /v2=%d, want 1/1 (404 双路径 fallback)", gz, vz)
	}
}

// TestCollectCNZeroRegression CN 账号仍单路径 /v2/billing/meter/get-user-resource，
// 不发 /billing/meter/* 探测（零回归）。
func TestCollectCNZeroRegression(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	var calls []string
	up := fakeUpstreamCredit(t, func(r *http.Request) (*http.Response, error) {
		calls = append(calls, r.URL.Path)
		return creditResp(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})

	dir := t.TempDir()
	writeTestAuth(t, dir, "cnuid-00000002", "at-cn", "www.codebuddy.cn", "")
	collect(dir, up)

	if len(calls) != 1 || calls[0] != "/v2/billing/meter/get-user-resource" {
		t.Errorf("CN billing calls=%v want single /v2 path", calls)
	}
}

// TestCollectEmptyTokenFileSkipped 无 accessToken 的文件被 auth.Parse 拒收
// （parse_error: missing accessToken），collect 静默跳过、不发请求（与
// signin/trial 对损坏文件的处理一致）。
func TestCollectEmptyTokenFileSkipped(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	dir := t.TempDir()
	writeTestAuth(t, dir, "notoken-0000001", "", "www.codebuddy.cn", "")
	up := fakeUpstreamCredit(t, func(r *http.Request) (*http.Response, error) {
		t.Fatal("无 token 不应发起请求")
		return nil, nil
	})
	accounts := collect(dir, up)
	if len(accounts) != 0 {
		t.Errorf("accounts=%+v want 0（空 token 文件被 Parse 跳过）", accounts)
	}
}
// TestCollectLoadsNonHyphenAuthFile (P2-10 RED)：collect 此前私用
// workbuddy-*.json 窄 glob，不带连字符的文件被跳过；改为 auth.LoadAuthFiles
// 后应与网关口径一致（宽侧 workbuddy*.json）。
func TestCollectLoadsNonHyphenAuthFile(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	dir := t.TempDir()
	// 不带连字符的文件名（网关 LoadDir 一直加载它，credit 曾跳过）。
	writeTestAuth(t, dir, "cnuid-00000002", "at-cn", "www.codebuddy.cn", "")
	if err := os.Rename(
		filepath.Join(dir, "workbuddy-cnuid-00.json"),
		filepath.Join(dir, "workbuddy_new.json"),
	); err != nil {
		t.Fatal(err)
	}
	up := fakeUpstreamCredit(t, func(r *http.Request) (*http.Response, error) {
		return creditResp(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`), nil
	})
	accounts := collect(dir, up)
	if len(accounts) != 1 {
		t.Fatalf("accounts=%d want 1（不带连字符文件应被加载）", len(accounts))
	}
	if !accounts[0].OK {
		t.Errorf("account ok=%v error=%s（应成功查询）", accounts[0].OK, accounts[0].Error)
	}
}

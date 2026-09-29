package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/source"
)

// ─── /admin/credits 的两条口径（2026-09-27）──────────────────────────────
//
// 改这条链路的动因是管理页上真实出现过的东西：zcode 号走的是套餐权益，不是
// billing 积分，混用 workbuddy 的口径只会拿到「upstream client (http 401): <html>」
// 这种整页 HTML；而「该账号当前能用哪些模型」也只能从套餐 capabilities 读。

// TestHumanizeUpstreamErr 上游报错必须压成一句人话：
//   - 4xx/5xx 的响应体常是整页 HTML（登录页/网关页），不能原样进表格；
//   - 状态码要带一句含义，让运维分清「凭据失效」与「路由错域」。
func TestHumanizeUpstreamErr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"upstream client (http 401): <html><body>blocked</body></html>", "上游 HTTP 401（凭据不被上游接受）"},
		{"zcode balance: HTTP 401: <html>nope</html>", "上游 HTTP 401（凭据不被上游接受）"},
		{"upstream client (http 429): slow down", "上游 HTTP 429（上游限流）"},
		{"upstream client (http 599): weird", "上游 HTTP 599（上游返回该状态码）"},
		{"dial tcp: i/o timeout", "dial tcp: i/o timeout"},
		{"   ", "上游没有返回可读信息"},
	}
	for _, c := range cases {
		if got := humanizeUpstreamErr(c.in); got != c.want {
			t.Errorf("humanizeUpstreamErr(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// 整页 HTML 必须被切掉（不能带着 body 进表格）。
	if got := humanizeUpstreamErr("oops: <html>" + strings.Repeat("x", 500)); strings.Contains(got, "xxx") {
		t.Errorf("HTML 正文没被切掉: %q", got)
	}
}

// TestResolveZCodeCredsSkipsWithoutZCode 池里没有 zcode 号时不该去读盘：
// 这条路径是 /admin/credits 每次刷新都会走的，无谓 IO 会拖慢整页。
func TestResolveZCodeCredsSkipsWithoutZCode(t *testing.T) {
	h := &Handler{}
	if got := h.resolveZCodeCreds(nil); len(got) != 0 {
		t.Fatalf("空池应返回空 map，得到 %v", got)
	}
	if got := h.resolveZCodeCreds([]pool.Status{{UID: "u1", Producer: "workbuddy"}}); len(got) != 0 {
		t.Fatalf("只有 workbuddy 号时应返回空 map，得到 %v", got)
	}
}

// TestFillZCodeCreditExplainsMissingCred 没解析到读额度凭据时必须说清原因，
// 而不是回一个空行让人以为这号没额度。
func TestFillZCodeCreditExplainsMissingCred(t *testing.T) {
	h := &Handler{}

	var row creditRow
	h.fillZCodeCredit(&row, zcodeCredPlan{note: "号池文件与 zcode 账本里都没有这个号的 zcodejwttoken"}, 0)
	if row.OK || row.Error == "" || !strings.Contains(row.Error, "zcodejwttoken") {
		t.Fatalf("缺凭据时必须报错并点明 zcodejwttoken，得到 ok=%v err=%q", row.OK, row.Error)
	}

	var row2 creditRow
	h.fillZCodeCredit(&row2, zcodeCredPlan{}, 0)
	if row2.OK || !strings.Contains(row2.Error, "zcodejwttoken") {
		t.Fatalf("空凭据时必须报 zcodejwttoken，得到 ok=%v err=%q", row2.OK, row2.Error)
	}
}

// testZCodeAuth 造一个 zcode 号池账号（只有 zcode 侧看得懂的字段）。
func testZCodeAuth(uid string) *auth.Auth {
	a := &auth.Auth{UID: uid, AccessToken: "up." + uid}
	a.SetProducer(source.ProducerZCode)
	return a
}

// TestResolveZCodeCredsPrefersPoolFile 号池文件自带凭据时必须**优先**用它：
// 它是唯一随文件跨机器走的一层，换机器 / 删账本都还在。
func TestResolveZCodeCredsPrefersPoolFile(t *testing.T) {
	a := testZCodeAuth("zcode-abc")
	a.SetZCodeJWT("jwt-pool")
	a.SetZCodeDeviceMID("dmid-pool")
	h := &Handler{}
	h.cfg.Pool = testPoolWith(a)
	h.cfg.ZCodeDir = t.TempDir()    // 空账本目录：证明命中的不是账本
	h.cfg.ZCodeAppDir = t.TempDir() // 空应用目录

	plan, ok := h.resolveZCodeCreds(h.cfg.Pool.List())["zcode-abc"]
	if !ok || plan.cred.JWT != "jwt-pool" || plan.cred.DeviceMID != "dmid-pool" {
		t.Fatalf("应优先用号池文件凭据，得到 %+v", plan)
	}
	if plan.cred.Source != "号池文件" {
		t.Fatalf("来源应标为号池文件，得到 %q", plan.cred.Source)
	}
}

// TestResolveZCodeCredsFallsBackToLedger 号池文件没带凭据时回落到 zcode 账本。
func TestResolveZCodeCredsFallsBackToLedger(t *testing.T) {
	dir := t.TempDir()
	body := `{"id":"abc","name":"t","credentials":{"oauth:active_provider":"zai",` +
		`"zcodejwttoken":"eyJhbGciOiJIUzI1NiJ9.eyJhIjoxfQ.x"},"virtual_device_mid":"dmid-ledger"}`
	if err := os.WriteFile(filepath.Join(dir, "abc.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写账本：%v", err)
	}
	h := &Handler{}
	h.cfg.Pool = testPoolWith(testZCodeAuth("zcode-abc"))
	h.cfg.ZCodeDir = dir
	h.cfg.ZCodeAppDir = t.TempDir()

	plan := h.resolveZCodeCreds(h.cfg.Pool.List())["zcode-abc"]
	if plan.cred.JWT == "" || plan.cred.Source != "zcode 账本" {
		t.Fatalf("应回落到账本，得到 %+v", plan)
	}
	if plan.cred.DeviceMID != "dmid-ledger" {
		t.Fatalf("device mid 应取自账本，得到 %q", plan.cred.DeviceMID)
	}
}

// writeAppCred 造一份 ZCode 应用登录态（明文 jwt 也走同一条自动解密路径）。
func writeAppCred(t *testing.T, dir, jwt, dmid string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"),
		[]byte(`{"zcodejwttoken":"`+jwt+`"}`), 0o600); err != nil {
		t.Fatalf("写 credentials.json：%v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "telemetry-state.json"),
		[]byte(`{"deviceMid":"`+dmid+`"}`), 0o600); err != nil {
		t.Fatalf("写 telemetry-state.json：%v", err)
	}
}

// TestResolveZCodeCredsAppFallbackUsedWhenUnclaimed 前两层都没有时用应用登录态兜底，
// 并且来源要如实标注（它只覆盖应用里正登录的那一个号）。
func TestResolveZCodeCredsAppFallbackUsedWhenUnclaimed(t *testing.T) {
	appDir := t.TempDir()
	writeAppCred(t, appDir, "jwt-app", "dmid-app")
	h := &Handler{}
	h.cfg.Pool = testPoolWith(testZCodeAuth("zcode-abc"))
	h.cfg.ZCodeDir = t.TempDir()
	h.cfg.ZCodeAppDir = appDir

	plan := h.resolveZCodeCreds(h.cfg.Pool.List())["zcode-abc"]
	if plan.cred.JWT != "jwt-app" || plan.cred.Source != "ZCode 应用登录态" {
		t.Fatalf("应回落到应用登录态并如实标注，得到 %+v", plan)
	}
	if plan.cred.DeviceMID != "dmid-app" {
		t.Fatalf("device mid 应取自 telemetry-state.json，得到 %q", plan.cred.DeviceMID)
	}
}

// TestResolveZCodeCredsAppFallbackNotMisattributed 应用登录态只属于一个号：
// 它若已归属别的 uid，就不能再安到这一行上（宁可报读不到，也不串额度）。
func TestResolveZCodeCredsAppFallbackNotMisattributed(t *testing.T) {
	appDir := t.TempDir()
	writeAppCred(t, appDir, "jwt-app", "dmid-app")

	a := testZCodeAuth("zcode-a")
	a.SetZCodeJWT("jwt-app") // 应用里正登录的就是 A
	b := testZCodeAuth("zcode-b")
	h := &Handler{}
	h.cfg.Pool = testPoolWith(a, b)
	h.cfg.ZCodeDir = t.TempDir()
	h.cfg.ZCodeAppDir = appDir

	got := h.resolveZCodeCreds(h.cfg.Pool.List())
	if got["zcode-a"].cred.JWT != "jwt-app" {
		t.Fatalf("A 应命中自己的凭据，得到 %+v", got["zcode-a"])
	}
	if got["zcode-b"].cred.JWT != "" || got["zcode-b"].note == "" {
		t.Fatalf("B 不该拿到 A 的凭据，应给说明，得到 %+v", got["zcode-b"])
	}
}

// TestResolveZCodeCredsExplainsWhenNothingFound 三层全空时必须给一句人话。
func TestResolveZCodeCredsExplainsWhenNothingFound(t *testing.T) {
	h := &Handler{}
	h.cfg.Pool = testPoolWith(testZCodeAuth("zcode-abc"))
	h.cfg.ZCodeDir = t.TempDir()
	h.cfg.ZCodeAppDir = t.TempDir()

	plan := h.resolveZCodeCreds(h.cfg.Pool.List())["zcode-abc"]
	if plan.cred.JWT != "" || plan.note == "" {
		t.Fatalf("三层都空时应给说明且不给凭据，得到 %+v", plan)
	}
	if !strings.Contains(plan.note, "重新导入") {
		t.Fatalf("说明应点出补救办法（重新导入即落盘），得到 %q", plan.note)
	}
	if !strings.Contains(plan.note, "账本里没有这条号") {
		t.Fatalf("说明应点出 A/B 两层没命中的原因，得到 %q", plan.note)
	}
}

package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 用本机真实登录态目录做端到端验证（目录不存在时自动跳过，CI 上不会红）。
func TestListWorkBuddyDesktopRealMachine(t *testing.T) {
	dir := WbDeskAuthDir("")
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("本机没有 WorkBuddy 登录态目录：%s", dir)
	}
	res, err := ListWorkBuddyDesktop("")
	if err != nil {
		t.Fatalf("ListWorkBuddyDesktop: %v", err)
	}
	t.Logf("可用静态钥：%v", res.KeyIDs)
	t.Logf("账号 %d 个，跳过 %d 个", len(res.Accounts), len(res.Skipped))
	for _, a := range res.Accounts {
		if !strings.HasPrefix(a.AccessToken, "eyJ") {
			t.Errorf("uid=%s 的 accessToken 不是 JWT（前 20 字符 %q）", a.UID, trunc(a.AccessToken, 20))
		}
		if a.Realm != "cn" && a.Realm != "global" {
			t.Errorf("uid=%s 的 realm 非法：%q", a.UID, a.Realm)
		}
		t.Logf("  uid=%s label=%q realm=%s variant=%s domain=%s exp=%d atLen=%d rtLen=%d key=%s",
			a.UID, a.Label(), a.Realm, a.Variant, a.Domain, a.ExpiresAt,
			len(a.AccessToken), len(a.RefreshToken), a.KeySource)
	}
	for f, why := range res.Skipped {
		t.Logf("  跳过 %s：%s", f, why)
	}
}

func TestWbDeskFieldAADShape(t *testing.T) {
	// AAD 必须是确定性的、且随 keyId 变化（防止拼错后所有钥都解不开却看不出）。
	a := wbDeskFieldAAD("9127dea1b44020a7", 1)
	b := wbDeskFieldAAD("9127dea1b44020a7", 1)
	c := wbDeskFieldAAD("ffffffffffffffff", 1)
	if string(a) != string(b) {
		t.Fatal("AAD 不确定")
	}
	if string(a) == string(c) {
		t.Fatal("AAD 未随 keyId 变化")
	}
	if !strings.HasPrefix(string(a), "WB-AAD\x00") {
		t.Fatalf("AAD 前缀不对：%q", string(a[:8]))
	}
}

func TestWbDeskKeyFromSecretMatchesBuiltinKeyID(t *testing.T) {
	k, err := wbDeskKeyFromSecret(wbDeskBuiltinSecret, "test")
	if err != nil {
		t.Fatalf("派生失败：%v", err)
	}
	if k.keyID != wbDeskKeyIDExpected {
		t.Fatalf("内置钥 keyId=%s，期望 %s", k.keyID, wbDeskKeyIDExpected)
	}
	if len(k.key) != 32 {
		t.Fatalf("钥长度 %d，期望 32", len(k.key))
	}
}

// 登出标记与无关文件必须被排除。
func TestWbDeskVariantOf(t *testing.T) {
	cases := map[string]string{
		"workbuddy-desktop.2026-09-24T09-29-21-833Z.24764.abc.info":    "cn",
		"workbuddy-desktop-ai.2026-09-26T19-47-28-971Z.37072.abc.info": "global",
		"workbuddy-desktop.info.logged-out":                            "",
		"workbuddy-desktop-ai.info.logged-out":                         "",
		"unrelated.txt":                                                "",
	}
	for name, want := range cases {
		got := wbDeskVariantOf(name)
		if want == "global" {
			// 文件名的 ai 前缀对应档位 "ai"；realm 推断在 realmOfWbDeskDomain 里做。
			want = "ai"
		}
		if got != want {
			t.Errorf("wbDeskVariantOf(%q)=%q，期望 %q", name, got, want)
		}
	}
}

func TestRealmOfWbDeskDomain(t *testing.T) {
	cases := map[string]string{
		"www.workbuddy.ai": "global",
		"workbuddy.ai":     "global",
		"www.codebuddy.cn": "cn",
		"www.workbuddy.cn": "cn",
		"":                 "cn",
	}
	for d, want := range cases {
		if got := realmOfWbDeskDomain(d); got != want {
			t.Errorf("realmOfWbDeskDomain(%q)=%q，期望 %q", d, got, want)
		}
	}
}

// 多份快照必须收敛成「每个 uid 一条、取最新」。
func TestListWorkBuddyDesktopDedupesByUID(t *testing.T) {
	dir := t.TempDir()
	// 同一个 uid 两份（旧明文 + 新加密），另一个 uid 一份。
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := `{"account":{"uid":"u-1","nickname":"旧"},"auth":{"accessToken":"eyJold","domain":"www.codebuddy.cn","expiresAt":1000000000000}}`
	write("workbuddy-desktop.2026-09-01T00-00-00-000Z.1.a.info", old)
	write("workbuddy-desktop.2026-09-02T00-00-00-000Z.1.b.info", old)
	write("workbuddy-desktop.2026-09-03T00-00-00-000Z.1.c.info",
		`{"account":{"uid":"u-2","nickname":"新"},"auth":{"accessToken":"eyJnew","domain":"www.workbuddy.ai","expiresAt":1000000000000}}`)
	res, err := ListWorkBuddyDesktop(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Accounts) != 2 {
		t.Fatalf("期望 2 个账号（按 uid 去重），实得 %d：%+v", len(res.Accounts), res.Accounts)
	}
	for _, a := range res.Accounts {
		if a.UID == "u-2" && a.Realm != "global" {
			t.Errorf("u-2 的 realm=%q，期望 global", a.Realm)
		}
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

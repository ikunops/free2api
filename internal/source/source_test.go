package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"free2api/internal/auth"
)

// TestNormalizeExpiry 毫秒→秒一次性归一（>=1e12 视为毫秒）。
func TestNormalizeExpiry(t *testing.T) {
	cases := []struct{ in, want int64 }{
		{1795104000888, 1795104000}, // wb-switch 毫秒
		{1795104000, 1795104000},    // 已是秒，原样
		{0, 0},
	}
	for _, c := range cases {
		if got := normalizeExpiry(c.in); got != c.want {
			t.Errorf("normalizeExpiry(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestFromPortableWBSwitch 真实 wb-switch 条目 → Flat：毫秒归一 + variant→realm。
func TestFromPortableWBSwitch(t *testing.T) {
	raw := []byte(`{
		"uid": "420ca400-cd1c-4d9b-9b66-538567aeb598",
		"nickname": "平平无奇小天才",
		"access_token": "AT",
		"refresh_token": "RT",
		"expiresAt": 1795104000888,
		"domain": "www.codebuddy.cn",
		"variant": "cn",
		"enterpriseId": "ent-1"
	}`)
	var p portable
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	f, err := fromPortable(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "AT" || f.RefreshToken != "RT" {
		t.Fatalf("tokens not mapped: %+v", f)
	}
	if f.ExpiresAt != 1795104000 {
		t.Fatalf("expiresAt = %d, want 1795104000 (ms→s)", f.ExpiresAt)
	}
	if f.Realm != "cn" {
		t.Fatalf("realm = %q, want cn (from variant)", f.Realm)
	}
	if f.UID == "" {
		t.Fatal("uid empty")
	}
}

// TestFromPortableNested 插件 OAuth 嵌套形也能吃。
func TestFromPortableNested(t *testing.T) {
	raw := []byte(`{
		"auth": {"accessToken": "NAT", "refreshToken": "NRT", "expiresAt": 1795104000, "domain": "www.workbuddy.ai", "realm": ""},
		"account": {"uid": "u-nested", "nickname": "国际号"}
	}`)
	var p portable
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	f, err := fromPortable(p)
	if err != nil {
		t.Fatal(err)
	}
	if f.AccessToken != "NAT" || f.UID != "u-nested" {
		t.Fatalf("nested not mapped: %+v", f)
	}
	if f.Realm != "global" {
		t.Fatalf("realm = %q, want global (domain 推断)", f.Realm)
	}
}

// TestFromPortableMissingToken 无 token 必须报错（不写空凭证）。
func TestFromPortableMissingToken(t *testing.T) {
	if _, err := fromPortable(portable{UID: "x"}); err == nil {
		t.Fatal("want error for missing access token")
	}
}

// TestImportThenAuthParse 端到端：导入 → 文件必须能被 internal/auth 解析（网关口径）。
func TestImportThenAuthParse(t *testing.T) {
	dir := t.TempDir()
	blob := []byte(`[
		{"uid":"u1","nickname":"A","access_token":"T1","refresh_token":"R1","expiresAt":1795104000888,"domain":"www.codebuddy.cn","variant":"cn"},
		{"uid":"u2","nickname":"B","access_token":"T2","refresh_token":"R2","expiresAt":1795104000888,"domain":"www.workbuddy.ai","variant":"global"}
	]`)
	res, err := ImportJSON(dir, blob, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Imported) != 2 {
		t.Fatalf("imported = %v, want 2", res.Imported)
	}
	// 网关口径解析：uid/refresh/realm/expiry 全部要对。
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("LoadDir = %d accounts, want 2", len(auths))
	}
	byUID := map[string]*auth.Auth{}
	for _, a := range auths {
		byUID[a.UID] = a
	}
	a := byUID["u1"]
	if a == nil {
		t.Fatal("u1 missing")
	}
	if a.AccessTokenValue() != "T1" || a.RefreshTokenValue() != "R1" {
		t.Fatalf("u1 creds wrong: %+v", a)
	}
	if a.ExpiresAt != 1795104000 {
		t.Fatalf("u1 expiresAt = %d, want 1795104000", a.ExpiresAt)
	}
	if a.Realm() != "cn" {
		t.Fatalf("u1 realm = %q, want cn", a.Realm())
	}
	if byUID["u2"].Realm() != "global" {
		t.Fatalf("u2 realm = %q, want global", byUID["u2"].Realm())
	}
	// 文件名必须匹配网关 glob workbuddy*.json。
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			t.Fatalf("unexpected file %s", e.Name())
		}
	}
}

// TestImportIdempotent 重复导入同一 uid 走 Skipped（不覆盖现有凭证）。
func TestImportIdempotent(t *testing.T) {
	dir := t.TempDir()
	blob := []byte(`{"uid":"dup","access_token":"T","refresh_token":"R","expiresAt":1795104000,"domain":"www.codebuddy.cn"}`)
	if _, err := ImportJSON(dir, blob, nil); err != nil {
		t.Fatal(err)
	}
	res, err := ImportJSON(dir, blob, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skipped) != 1 || len(res.Imported) != 0 {
		t.Fatalf("second import = imported:%v skipped:%v, want skip", res.Imported, res.Skipped)
	}
}

// TestListWBSwitchAndImportedMark 列出候选 + 标注已在池。
func TestListWBSwitchAndImportedMark(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(dir, "accounts.json")
	blob := []byte(`[{"uid":"a","nickname":"A","access_token":"T","expiresAt":1795104000888,"domain":"www.codebuddy.cn"},{"uid":"b","nickname":"B","access_token":"T2","expiresAt":1795104000888,"domain":"www.codebuddy.cn"}]`)
	if err := os.WriteFile(ledger, blob, 0o600); err != nil {
		t.Fatal(err)
	}
	authDir := t.TempDir()
	if _, err := ImportJSON(authDir, []byte(`{"uid":"a","access_token":"T","expiresAt":1795104000,"domain":"www.codebuddy.cn"}`), nil); err != nil {
		t.Fatal(err)
	}
	cands, err := parseCandidates(blob, authDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cands) != 2 {
		t.Fatalf("cands = %d, want 2", len(cands))
	}
	mark := map[string]bool{}
	for _, c := range cands {
		mark[c.ID] = c.Imported
	}
	if !mark["a"] || mark["b"] {
		t.Fatalf("imported marks wrong: %+v", mark)
	}
}

// TestVariantAiMapsToGlobal wb-switch 的 variant=ai 必须落成 global（不是 "ai"）。
// 回归背景：初版直接把 variant 当显式 realm 写盘，产生 realm:"ai" 的脏值；
// domain 为空时 auth.Realm() 会判错域、走错上游 base。
func TestVariantAiMapsToGlobal(t *testing.T) {
	cases := []struct{ variant, domain, want string }{
		{"ai", "www.workbuddy.ai", "global"},
		{"ai", "", "global"}, // 无 domain 也必须靠别名判对
		{"cn", "www.codebuddy.cn", "cn"},
		{"", "www.workbuddy.ai", "global"}, // 未指定 → domain 推断
		{"", "www.codebuddy.cn", "cn"},
		{"weird", "www.codebuddy.cn", "cn"}, // 未知别名 → 回落 domain
	}
	for _, c := range cases {
		f, err := fromPortable(portable{AccessToken: "T", UID: "u", Variant: c.variant, Domain: c.domain})
		if err != nil {
			t.Fatal(err)
		}
		if f.Realm != c.want {
			t.Errorf("variant=%q domain=%q → realm=%q, want %q", c.variant, c.domain, f.Realm, c.want)
		}
	}
}

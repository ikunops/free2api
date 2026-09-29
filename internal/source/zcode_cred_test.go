package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── enc:v1: 解密（zcode 读额度凭据的唯一门槛）──────────────────────────
//
// 口径抄自 ZCode 应用自己的 credentialService：enc:v1:<iv>.<tag>.<ct>，
// AES-256-GCM，key = sha256("zcode-credential-fallback:<platform>:<home>:<user>")，
// base64url 无填充。这几条测试锁的就是「形状 + 口令派生 + 失败必须报错」。

// TestZCodeCredRoundTrip 自己封、自己解，形状与长度都要与应用一致。
func TestZCodeCredRoundTrip(t *testing.T) {
	secret := ZCodeCredSecret("win32", `C:\Users\x`, "u")
	enc, err := encryptZCodeCred("hello-jwt", secret)
	if err != nil {
		t.Fatalf("加密：%v", err)
	}
	if !strings.HasPrefix(enc, zcodeCredPrefix) {
		t.Fatalf("密文缺前缀：%q", enc)
	}
	parts := strings.Split(strings.TrimPrefix(enc, zcodeCredPrefix), ".")
	if len(parts) != 3 {
		t.Fatalf("密文应是 <iv>.<tag>.<ct> 三段，得到 %d 段", len(parts))
	}
	got, err := DecryptZCodeCred(enc, secret)
	if err != nil {
		t.Fatalf("解密：%v", err)
	}
	if got != "hello-jwt" {
		t.Fatalf("往返不一致：%q", got)
	}
}

// TestZCodeCredWrongSecretIsRejected 换口令必须**认证失败**，绝不能返回垃圾字节
// （拿半截 token 去打上游会直接触发风控）。
func TestZCodeCredWrongSecretIsRejected(t *testing.T) {
	enc, err := encryptZCodeCred("hello-jwt", ZCodeCredSecret("win32", `C:\a`, "u"))
	if err != nil {
		t.Fatalf("加密：%v", err)
	}
	if got, err := DecryptZCodeCred(enc, ZCodeCredSecret("win32", `C:\b`, "u")); err == nil {
		t.Fatalf("口令不匹配时必须报错，却得到明文 %q", got)
	}
}

// TestZCodeCredPlainPassthrough 明文原样返回：调用方无需先判形态。
func TestZCodeCredPlainPassthrough(t *testing.T) {
	for _, in := range []string{"eyJhbGciOiJIUzI1NiJ9.x.y", `"quoted-token"`} {
		got, err := DecryptZCodeCred(in, "whatever")
		if err != nil || got != strings.Trim(in, "\"") {
			t.Fatalf("明文应原样返回（去引号），in=%q got=%q err=%v", in, got, err)
		}
	}
}

// TestZCodeCredAutoUsesEnvOverride 应用支持环境变量覆盖口令，本网关同样认。
func TestZCodeCredAutoUsesEnvOverride(t *testing.T) {
	secret := "unit-test-secret"
	t.Setenv(zcodeCredEnvOverride, secret)
	enc, err := encryptZCodeCred("jwt-from-env", secret)
	if err != nil {
		t.Fatalf("加密：%v", err)
	}
	got, used, err := DecryptZCodeCredAuto(enc)
	if err != nil {
		t.Fatalf("自动解密：%v", err)
	}
	if got != "jwt-from-env" || used != secret {
		t.Fatalf("应命中环境变量口令，得到 got=%q used=%q", got, used)
	}
}

// TestZCodeCredMalformedIsRejected 形状不对要报错，不能静默返回空串。
func TestZCodeCredMalformedIsRejected(t *testing.T) {
	cases := []string{
		"enc:v1:onlyonepart",
		"enc:v1:aaa.bbb",        // 少一段
		"enc:v1:.tag.ct",        // iv 空
		"enc:v1:AAAA.BBBB.CCCC", // 长度不对
		"enc:v1:!!!!.????.####", // 非法 base64url
	}
	for _, c := range cases {
		if _, err := DecryptZCodeCred(c, "s"); err == nil {
			t.Errorf("畸形密文 %q 应报错", c)
		}
	}
}

// TestZCodeLedgerQuotaCredDecryptsEncryptedJWT 账本里的 enc:v1 token 也必须能解出来：
// 它不是「读不出」，只是要按本机口令解（同机可解、换机器解不开是设计如此）。
func TestZCodeLedgerQuotaCredDecryptsEncryptedJWT(t *testing.T) {
	cands := zcodeCredSecretCandidates()
	if len(cands) == 0 {
		t.Skip("本机没有可用的口令候选")
	}
	enc, err := encryptZCodeCred("jwt-from-ledger", cands[0])
	if err != nil {
		t.Fatalf("加密：%v", err)
	}
	dir := t.TempDir()
	body := `{"id":"acc-1","name":"t","credentials":{"oauth:active_provider":"zai",` +
		`"zcodejwttoken":"` + enc + `"},"virtual_device_mid":"dmid-1"}`
	if err := os.WriteFile(filepath.Join(dir, "acc-1.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写账本：%v", err)
	}
	got, ok, miss := zcodeLedgerQuotaCred(dir, ZCodeUID("acc-1"))
	if !ok {
		t.Fatalf("账本的 enc:v1 token 应能解出，却判为不可用：%s", miss)
	}
	if got.JWT != "jwt-from-ledger" || got.DeviceMID != "dmid-1" {
		t.Fatalf("解出的凭据不对：%+v", got)
	}
}

// TestZCodeLedgerQuotaCredMarksUndecryptable 本机解不开时必须**如实标注**
// （不是失败、也不是硬塞一个空串），号本身仍能反代。
func TestZCodeLedgerQuotaCredMarksUndecryptable(t *testing.T) {
	// 用一条格式合法但口令不可能是本机的密文（换成另一台机器的派生值）。
	enc, err := encryptZCodeCred("jwt-x", ZCodeCredSecret("win32", `C:\someone-else`, "other-user"))
	if err != nil {
		t.Fatalf("加密：%v", err)
	}
	dir := t.TempDir()
	body := `{"id":"acc-2","name":"t","credentials":{"zcodejwttoken":"` + enc + `"},` +
		`"virtual_device_mid":"dmid-2"}`
	if err := os.WriteFile(filepath.Join(dir, "acc-2.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("写账本：%v", err)
	}
	accs, err := ListZCodeAccounts(dir)
	if err != nil || len(accs) != 1 {
		t.Fatalf("读账本：err=%v n=%d", err, len(accs))
	}
	if accs[0].JWT != "" {
		t.Fatalf("解不开时不该给出 jwt，得到 %q", accs[0].JWT)
	}
	if !accs[0].JWTEncrypted {
		t.Fatalf("解不开时应标注 JWTEncrypted（管理页要能说清原因）")
	}
	if _, ok, miss := zcodeLedgerQuotaCred(dir, ZCodeUID("acc-2")); ok {
		t.Fatalf("解不开时不该判为可用凭据")
	} else if miss == "" {
		t.Fatalf("解不开时要给出原因，不能空着")
	}
}

// TestZCodeAppQuotaCredRealFile 现场验一次本机应用登录态（没装 / 换了机器就跳过）。
func TestZCodeAppQuotaCredRealFile(t *testing.T) {
	dir := ZCodeAppCredDir()
	if _, err := os.Stat(filepath.Join(dir, "credentials.json")); err != nil {
		t.Skip("本机没有 ZCode 应用登录态，跳过")
	}
	cred, err := ReadZCodeAppQuotaCred(dir)
	if err != nil {
		// 换了机器 / 换了账户名时解不开是预期行为，跳过而不是判失败。
		t.Skipf("本机解不开 ZCode 应用凭据（换机器或换了账户名属预期）：%v", err)
	}
	if !strings.HasPrefix(cred.JWT, "eyJ") {
		t.Fatalf("解出来的应是 JWT，得到 %.24q", cred.JWT)
	}
	if cred.Source != "ZCode 应用登录态" {
		t.Fatalf("来源标注不对：%q", cred.Source)
	}
}

// TestResolveZCodeQuotaCredPrefersOwn 号池文件自带凭据时不该再去读盘。
func TestResolveZCodeQuotaCredPrefersOwn(t *testing.T) {
	own := ZCodeQuotaCred{JWT: "jwt-own", DeviceMID: "dmid-own"}
	got, ok, miss := ResolveZCodeQuotaCred(own, "zcode-nope", filepath.Join(t.TempDir(), "missing"))
	if !ok || got.JWT != "jwt-own" || got.Source != "号池文件" {
		t.Fatalf("应优先用自己的凭据，得到 ok=%v %+v miss=%q", ok, got, miss)
	}
}

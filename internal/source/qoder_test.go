//go:build windows

package source

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// dpapiProtect 测试侧封一段 blob（生产只解不封，这里模拟 Chromium 的
// os_crypt.encrypted_key 生成过程）。
func dpapiProtect(blob []byte) ([]byte, error) {
	in := windows.DataBlob{Size: uint32(len(blob)), Data: &blob[0]}
	var out windows.DataBlob
	if err := windows.CryptProtectData(&in, nil, nil, 0, nil, 0, &out); err != nil {
		return nil, err
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	buf := make([]byte, out.Size)
	copy(buf, unsafe.Slice(out.Data, out.Size))
	return buf, nil
}

// writeQoderTestApp 在临时目录造一份与 Qoder 客户端同形的登录态：
// Local State（DPAPI 保护的 AES key）+ auth.v1.dat（v10 AES-GCM）+ machine-id。
func writeQoderTestApp(t *testing.T, uid, name, token string) string {
	t.Helper()
	dir := t.TempDir()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	wrapped, err := dpapiProtect(key)
	if err != nil {
		t.Fatalf("DPAPI 封装：%v", err)
	}
	localState := map[string]any{
		"os_crypt": map[string]any{
			"encrypted_key": base64.StdEncoding.EncodeToString(append([]byte("DPAPI"), wrapped...)),
		},
	}
	lsRaw, _ := json.Marshal(localState)
	if err := os.WriteFile(filepath.Join(dir, "Local State"), lsRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{
		"token":        token,
		"refreshToken": "rt-" + token,
		"expiresAt":    "2030-01-02T03:04:05Z",
		"user":         map[string]any{"id": uid, "name": name, "email": "u@example.com"},
	}
	plain, _ := json.Marshal(doc)
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	blob := append([]byte("v10"), nonce...)
	blob = append(blob, gcm.Seal(nil, nonce, plain, nil)...)
	if err := os.WriteFile(filepath.Join(dir, "auth.v1.dat"), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "auth.machine-id"), []byte("11111111-2222-3333-4444-555555555555"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestQoderLoadAccountRoundTrip 锁定 v10+AES-GCM 解密与 machine-id 读取：
// 形状抄的是客户端真实文件，改任一字节偏移都会让真实登录态读不出来。
func TestQoderLoadAccountRoundTrip(t *testing.T) {
	dir := writeQoderTestApp(t, "u-123", "测试号", "device-token-abc")
	acc, err := LoadQoderAccount(dir)
	if err != nil {
		t.Fatalf("读登录态：%v", err)
	}
	if acc.UID != "u-123" || acc.Name != "测试号" || acc.DeviceToken != "device-token-abc" {
		t.Errorf("解出的账号 = %+v", acc)
	}
	if acc.RefreshToken != "rt-device-token-abc" {
		t.Errorf("refreshToken = %q", acc.RefreshToken)
	}
	if acc.MachineID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("machineID = %q", acc.MachineID)
	}
	if acc.ExpiresAt == 0 {
		t.Error("expiresAt 未解析")
	}
}

// TestQoderLoadAccountRejectsBadPrefix 非 v10 文件必须报错（别把明文/别的格式
// 当密文硬解，错误信息要能指到形态问题）。
func TestQoderLoadAccountRejectsBadPrefix(t *testing.T) {
	dir := writeQoderTestApp(t, "u-1", "n", "t")
	if err := os.WriteFile(filepath.Join(dir, "auth.v1.dat"), []byte("v11-"+strings.Repeat("x", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadQoderAccount(dir); err == nil || !strings.Contains(err.Error(), "v10") {
		t.Fatalf("want v10 形态错误，got %v", err)
	}
}

// TestQoderUIDStable uid 前缀 + 路径安全化：账号 id 里出现路径分隔符不能逃出 auths。
func TestQoderUIDStable(t *testing.T) {
	if got := QoderUID("abc"); got != "qoder-abc" {
		t.Errorf("QoderUID(abc) = %q", got)
	}
	got := QoderUID(`a/b\c:d*e?f"g<h>i|j`)
	if strings.ContainsAny(got, `/\:*?"<>|`) {
		t.Errorf("QoderUID 未安全化：%q", got)
	}
	if !strings.HasPrefix(got, "qoder-") {
		t.Errorf("缺 producer 前缀：%q", got)
	}
}

// TestQoderImportWritesMachineID 导入后凭证文件必须自带 qoder_machine_id：
// 出站 COSY 签名全绑这一串，丢了就 403 Signature invalid。
func TestQoderImportWritesMachineID(t *testing.T) {
	dir := writeQoderTestApp(t, "u-9", "导入号", "dt-9")
	authDir := t.TempDir()
	res, err := ImportQoderAs(dir, authDir, nil, ImportOptions{Method: MethodApp})
	if err != nil {
		t.Fatalf("导入：%v", err)
	}
	if len(res.Imported) != 1 {
		t.Fatalf("导入结果 = %+v", res)
	}
	raw, err := os.ReadFile(filepath.Join(authDir, AuthFileName(res.Imported[0])))
	if err != nil {
		t.Fatal(err)
	}
	var f Flat
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("凭证不是合法 JSON：%v", err)
	}
	if f.Producer != ProducerQoder || f.QoderMachineID == "" || f.UpstreamBase != qoderGatewayBase {
		t.Errorf("凭证字段 = %+v", f)
	}
	// 二次导入必须跳过，不能覆盖已有凭据（refresh token 一次性，覆盖会丢新值）。
	res2, err := ImportQoderAs(dir, authDir, nil, ImportOptions{Method: MethodApp})
	if err != nil {
		t.Fatalf("二次导入：%v", err)
	}
	if len(res2.Imported) != 0 || len(res2.Skipped) != 1 {
		t.Errorf("二次导入应跳过，得到 %+v", res2)
	}
}

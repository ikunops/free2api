// qoder.go Qoder（CN）账号的「取源」侧：读 %APPDATA%\com.qodercn.app.stable 里
// 客户端自己的登录态，解出 device token 与 machine id，归一化成网关认得的扁平形 auth。
//
// 登录态文件是 Chromium/Electron 的 v10 AES-GCM：
//
//	3 bytes   "v10"
//	12 bytes  nonce
//	N bytes   ciphertext
//	16 bytes  GCM tag
//
// 密钥来自同目录 Local State 的 os_crypt.encrypted_key：base64 → 去掉 5 字节 ASCII
// "DPAPI" → CryptUnprotectData(CurrentUser) → 32 字节 AES key。DPAPI 只对当前 Windows
// 用户有效，所以网关必须与 Qoder 客户端在同一用户下运行（这也是「读本机登录态」的
// 固有边界，不是本实现的限制）。
//
// 与 zcode 的差别：Qoder 的 device token 是**会过期**的 OAuth 凭据（带 refreshToken），
// 不是静态 api key；出站时还要用 machine id 做 COSY 签名（见 internal/upstream/qoder.go）。
// 因此凭证文件除了 accessToken/refreshToken 还额外落一个 qoder_machine_id。
package source

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// qoderAppDirName Qoder 桌面端（CN）的 Electron userData 目录名。
const qoderAppDirName = "com.qodercn.app.stable"

// qoderGatewayBase Qoder CN 的对话/目录网关（COSY 签名端点）。
const qoderGatewayBase = "https://gateway.qoder.com.cn"

// QoderAccount 一个 Qoder 登录态里抽出来的可用部分。
type QoderAccount struct {
	UID          string
	Name         string
	Email        string
	DeviceToken  string
	RefreshToken string
	ExpiresAt    int64 // Unix 秒；0 = 上游没给
	MachineID    string
	SourceFile   string
}

// QoderAppDir Qoder 桌面端登录态目录（APPDATA 优先，非 Windows 回落 ~/AppData/Roaming）。
func QoderAppDir() string {
	if v := strings.TrimSpace(os.Getenv("APPDATA")); v != "" {
		return filepath.Join(v, qoderAppDirName)
	}
	return filepath.Join(HomeDir(), "AppData", "Roaming", qoderAppDirName)
}

// qoderAuthFile Qoder 登录态密文文件名。
const qoderAuthFile = "auth.v1.dat"

// qoderMachineFile 机器标识文件名（明文 UUID 文本）。
const qoderMachineFile = "auth.machine-id"

// qoderAuthDoc auth.v1.dat 解密后的 JSON 形态（只取需要的字段）。
type qoderAuthDoc struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refreshToken"`
	ExpiresAt    string `json:"expiresAt"`
	User         struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Email string `json:"email"`
		Phone string `json:"phone"`
	} `json:"user"`
}

// LoadQoderAccount 读并解密 Qoder 客户端当前登录态。目录为空时取 QoderAppDir()。
func LoadQoderAccount(dir string) (QoderAccount, error) {
	if strings.TrimSpace(dir) == "" {
		dir = QoderAppDir()
	}
	authPath := filepath.Join(dir, qoderAuthFile)
	raw, err := os.ReadFile(authPath)
	if err != nil {
		return QoderAccount{}, fmt.Errorf("读 Qoder 登录态失败：%w", err)
	}
	plain, err := decryptQoderAuthFile(dir, raw)
	if err != nil {
		return QoderAccount{}, err
	}
	var doc qoderAuthDoc
	if err := json.Unmarshal(plain, &doc); err != nil {
		return QoderAccount{}, fmt.Errorf("Qoder 登录态不是合法 json：%w", err)
	}
	acc := QoderAccount{
		UID:          strings.TrimSpace(doc.User.ID),
		Name:         strings.TrimSpace(doc.User.Name),
		Email:        strings.TrimSpace(doc.User.Email),
		DeviceToken:  strings.TrimSpace(doc.Token),
		RefreshToken: strings.TrimSpace(doc.RefreshToken),
		SourceFile:   authPath,
	}
	if acc.UID == "" {
		return QoderAccount{}, errors.New("Qoder 登录态里没有 user.id")
	}
	if acc.DeviceToken == "" {
		return QoderAccount{}, errors.New("Qoder 登录态里没有 token（未登录或已退出）")
	}
	if acc.Name == "" {
		acc.Name = strings.TrimSpace(doc.User.Phone)
	}
	if acc.Name == "" {
		acc.Name = acc.UID
	}
	if t, perr := time.Parse(time.RFC3339, strings.TrimSpace(doc.ExpiresAt)); perr == nil {
		acc.ExpiresAt = t.Unix()
	}
	mid, merr := os.ReadFile(filepath.Join(dir, qoderMachineFile))
	if merr != nil {
		return QoderAccount{}, fmt.Errorf("读 Qoder machine-id 失败：%w", merr)
	}
	acc.MachineID = strings.TrimSpace(string(mid))
	if acc.MachineID == "" {
		return QoderAccount{}, errors.New("Qoder machine-id 为空")
	}
	return acc, nil
}

// decryptQoderAuthFile 解 v10 AES-GCM 登录态文件。
func decryptQoderAuthFile(dir string, raw []byte) ([]byte, error) {
	const prefix = "v10"
	const nonceLen = 12
	const tagLen = 16
	if len(raw) < len(prefix)+nonceLen+tagLen {
		return nil, fmt.Errorf("Qoder 登录态文件过短（%d bytes）", len(raw))
	}
	if string(raw[:len(prefix)]) != prefix {
		return nil, fmt.Errorf("Qoder 登录态文件前缀不是 v10（实际 %q）", string(raw[:minInt(len(raw), 4)]))
	}
	nonce := raw[len(prefix) : len(prefix)+nonceLen]
	body := raw[len(prefix)+nonceLen:]
	key, err := qoderCookieKey(dir)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("Qoder 登录态 AES 初始化失败：%w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("Qoder 登录态 GCM 初始化失败：%w", err)
	}
	plain, err := gcm.Open(nil, nonce, body, nil)
	if err != nil {
		return nil, fmt.Errorf("Qoder 登录态解密失败（DPAPI 钥与文件不匹配？）：%w", err)
	}
	return plain, nil
}

// qoderCookieKey 从 Local State 的 os_crypt.encrypted_key 解出 32 字节 AES key。
func qoderCookieKey(dir string) ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Local State"))
	if err != nil {
		return nil, fmt.Errorf("读 Qoder Local State 失败：%w", err)
	}
	var st struct {
		OSCrypt struct {
			EncryptedKey string `json:"encrypted_key"`
		} `json:"os_crypt"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("Qoder Local State 不是合法 json：%w", err)
	}
	enc, err := base64.StdEncoding.DecodeString(strings.TrimSpace(st.OSCrypt.EncryptedKey))
	if err != nil {
		return nil, fmt.Errorf("Qoder encrypted_key base64 解码失败：%w", err)
	}
	const dpapiTag = "DPAPI"
	if len(enc) <= len(dpapiTag) || string(enc[:len(dpapiTag)]) != dpapiTag {
		return nil, errors.New("Qoder encrypted_key 不是 DPAPI 形态")
	}
	key, err := dpapiUnprotect(enc[len(dpapiTag):])
	if err != nil {
		return nil, fmt.Errorf("Qoder encrypted_key DPAPI 解密失败：%w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("Qoder AES key 长度异常：%d", len(key))
	}
	return key, nil
}

// QoderUID 号池内的稳定 uid（与 zcode 同形：producer 前缀 + 安全化账号 id）。
func QoderUID(accountID string) string { return "qoder-" + sanitizeUID(accountID) }

// QoderCandidates 管理页用：应用登录态最多产生一个候选。
func QoderCandidates(dir, authDir string) ([]Candidate, error) {
	acc, err := LoadQoderAccount(dir)
	if err != nil {
		return nil, err
	}
	c := Candidate{
		ID:         acc.UID,
		Label:      acc.Name,
		Realm:      "cn",
		HasRefresh: acc.RefreshToken != "",
		Producer:   ProducerQoder,
		Usable:     true,
		ExpiresAt:  acc.ExpiresAt,
		Expired:    acc.ExpiresAt > 0 && acc.ExpiresAt <= time.Now().Unix(),
		Upstream:   qoderGatewayBase,
		APIKeyHint: keyHint(acc.DeviceToken),
		Detail:     "Qoder 应用登录态 · machine " + keyHint(acc.MachineID),
		Note:       "COSY 签名走 Qoder 官方网关；token 过期时用 refreshToken 续期",
	}
	if existing := importedIndex(authDir); existing[QoderUID(acc.UID)] != "" {
		c.Imported = true
		c.ImportedAs = existing[QoderUID(acc.UID)]
	}
	return []Candidate{c}, nil
}

// ImportQoderAs 导入 Qoder 应用当前登录态（method=app，最多一个号）。
func ImportQoderAs(dir, authDir string, ids []string, opt ImportOptions) (ImportResult, error) {
	acc, err := LoadQoderAccount(dir)
	if err != nil {
		return ImportResult{}, err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if len(want) > 0 && !want[acc.UID] && !want[acc.Name] {
		return ImportResult{Kind: "qoder-app", AuthDir: authDir}, nil
	}
	opt = opt.normalized()
	if opt.Method == "" {
		opt.Method = MethodApp
	}
	if opt.Detail == "" {
		opt.Detail = dir
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Kind: "qoder-app", AuthDir: authDir}
	uid := QoderUID(acc.UID)
	dest := filepath.Join(authDir, AuthFileName(uid))
	if _, serr := os.Stat(dest); serr == nil {
		res.Skipped = append(res.Skipped, uid)
		return res, nil
	}
	f := Flat{
		AccessToken:    acc.DeviceToken,
		RefreshToken:   acc.RefreshToken,
		ExpiresAt:      acc.ExpiresAt,
		Domain:         qoderGatewayBase,
		Realm:          "cn",
		UID:            uid,
		Nickname:       acc.Name,
		Producer:       ProducerQoder,
		UpstreamBase:   qoderGatewayBase,
		QoderMachineID: acc.MachineID,
	}
	if werr := writeFlatAtomic(dest, f); werr != nil {
		res.Errors = append(res.Errors, uid+": "+werr.Error())
		return res, nil
	}
	res.Imported = append(res.Imported, uid)
	_ = LoadRegistry(authDir).Mark(map[string]Origin{
		uid: {Method: opt.Method, Producer: ProducerQoder, Label: acc.Name, Detail: opt.Detail},
	})
	sort.Strings(res.Imported)
	return res, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

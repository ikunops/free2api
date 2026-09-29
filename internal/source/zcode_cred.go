// zcode_cred.go zcode 侧的「读额度凭据」：把散在三处的 zcodejwttoken + device mid
// 归一成一对凭据，供 /admin/credits 直查套餐到期与剩余额度。
//
// 为什么需要它：查一个 zcode 号的额度，光有反代用的 accessToken 不够，必须两样：
//
//  1. zcodejwttoken  —— Bearer，打 https://zcode.z.ai/api/v1/zcode-plan/billing/balance
//  2. X-Device-Mid   —— 少这个头，上游直接回 400 {"code":3001,"msg":"parameter error"}
//
// 这两样在 zcode 的生态里**都不在账号自带的上游凭据里**，而是散在三处：
//
//	A 号池凭证文件（导入时落盘；随文件跨机器走，最稳）—— 见 auth.zcode_jwt / zcode_device_mid
//	B zcode-switch 账本快照 ~/.zcode-switch/accounts/*.json（credentials.zcodejwttoken
//	  + 顶层 virtual_device_mid）
//	C ZCode 应用自己的登录态 ~/.zcode/v2/（credentials.json 的 zcodejwttoken +
//	  telemetry-state.json 的 deviceMid）—— 字段级加密，但密钥按「机器 + 用户名」派生，
//	  本机能解；代价是只覆盖应用里**当前登录的那一个号**。
//
// 解密口径抄自 ZCode 应用自己的 credentialService（enc:v1: + AES-256-GCM，
// key = sha256(secret)，secret = "zcode-credential-fallback:<platform>:<home>:<user>"，
// 可用环境变量 ZCODE_CREDENTIAL_SECRET 覆盖）。不「猜」任何一步：解不开就如实报错，
// 绝不返回半截 token 去打上游——那会直接触发风控。
//
// 三条硬线的取舍（用户口径「宁可不报数，也不编造」）：
//   - 换机器后解不开是**设计如此**（密钥派生带 home + 用户名），所以 A 层落盘才是主路；
//   - C 层的 JWT 属于「应用当前登录那个号」，若已归属别的 uid 就不给这一行用，
//     免得把 A 号的额度显示到 B 号行上。
package source

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	// zcodeCredIVLen / zcodeCredTagLen AES-256-GCM 的 IV 与 AuthTag 定长（与应用逐字一致）。
	zcodeCredIVLen  = 12
	zcodeCredTagLen = 16
	// zcodeCredEnvOverride 应用侧允许用环境变量覆盖派生口令；本网关同样认它。
	zcodeCredEnvOverride = "ZCODE_CREDENTIAL_SECRET"
)

// ZCodeCredSecret 按 ZCode 的 fallback 规则拼出口令原文（导出供测试与排障复现）。
func ZCodeCredSecret(platform, home, user string) string {
	return "zcode-credential-fallback:" + platform + ":" + home + ":" + user
}

// zcodeNodePlatform 把 Go 的 GOOS 映射成 Node 的 process.platform 取值。
// 应用侧写的是 process.platform（Windows 上是 "win32"），不是 Go 的 "windows"。
func zcodeNodePlatform(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return goos
	}
}

// zcodeCredSecretCandidates 本机可能命中的口令候选（按可能性排序）。
// 环境变量优先（应用侧语义），其次按「平台 + 主目录 + 用户名」派生，
// 最后带上应用在 userInfo() 取不到用户名时的回落值 "unknown"。
func zcodeCredSecretCandidates() []string {
	plat := zcodeNodePlatform(runtime.GOOS)
	home := HomeDir()
	user := strings.TrimSpace(os.Getenv("USERNAME"))
	if user == "" {
		user = strings.TrimSpace(os.Getenv("USER"))
	}
	out := make([]string, 0, 3)
	if ov := strings.TrimSpace(os.Getenv(zcodeCredEnvOverride)); ov != "" {
		out = append(out, ov)
	}
	if user != "" {
		out = append(out, ZCodeCredSecret(plat, home, user))
	}
	out = append(out, ZCodeCredSecret(plat, home, "unknown"))
	return out
}

// DecryptZCodeCred 解一条 enc:v1:<iv>.<tag>.<ct>（AES-256-GCM，URL-safe base64 无填充）。
// 非 enc:v1: 前缀的输入原样返回——调用方可以无脑调，不必先判形态。
func DecryptZCodeCred(enc, secret string) (string, error) {
	s := zcodeCredTrim(enc)
	if !strings.HasPrefix(s, zcodeCredPrefix) {
		return s, nil
	}
	parts := strings.Split(strings.TrimPrefix(s, zcodeCredPrefix), ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("zcode 密文格式非法（期望 enc:v1:<iv>.<tag>.<ct>）")
	}
	iv, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || len(iv) != zcodeCredIVLen {
		return "", fmt.Errorf("zcode 密文 IV 非法（长度 %d，期望 %d）", len(iv), zcodeCredIVLen)
	}
	tag, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(tag) != zcodeCredTagLen {
		return "", fmt.Errorf("zcode 密文 AuthTag 非法（长度 %d，期望 %d）", len(tag), zcodeCredTagLen)
	}
	ct, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("zcode 密文主体不是合法 base64url")
	}
	key := sha256.Sum256([]byte(secret))
	blk, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	// Go 的 GCM 要求 tag 尾随密文（Node 侧是分开存的），拼回去再认证解密。
	body := make([]byte, 0, len(ct)+len(tag))
	body = append(body, ct...)
	body = append(body, tag...)
	pt, err := gcm.Open(nil, iv, body, nil)
	if err != nil {
		return "", fmt.Errorf("zcode 密文解密失败（口令不匹配或密文已损坏）")
	}
	return string(pt), nil
}

// DecryptZCodeCredAuto 用本机口令候选依次试解，返回明文 + 命中的口令原文。
// 明文输入直接返回（secret 为空串），便于调用方统一入口。
func DecryptZCodeCredAuto(enc string) (string, string, error) {
	s := zcodeCredTrim(enc)
	if !strings.HasPrefix(s, zcodeCredPrefix) {
		return s, "", nil
	}
	var lastErr error
	for _, sec := range zcodeCredSecretCandidates() {
		pt, err := DecryptZCodeCred(s, sec)
		if err == nil {
			return pt, sec, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("zcode 密文解密失败：本机没有可用的口令候选")
	}
	return "", "", lastErr
}

// encryptZCodeCred 按应用同一形状封一条密文（测试与自检用；生产只解密不加密）。
func encryptZCodeCred(plain, secret string) (string, error) {
	key := sha256.Sum256([]byte(secret))
	blk, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	iv := make([]byte, zcodeCredIVLen)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nil, iv, []byte(plain), nil)
	ct := sealed[:len(sealed)-zcodeCredTagLen]
	tag := sealed[len(sealed)-zcodeCredTagLen:]
	enc := zcodeCredPrefix +
		base64.RawURLEncoding.EncodeToString(iv) + "." +
		base64.RawURLEncoding.EncodeToString(tag) + "." +
		base64.RawURLEncoding.EncodeToString(ct)
	return enc, nil
}

// zcodeCredTrim 归一化一个凭证字面量：去首尾空白与可能的外层 json 引号。
func zcodeCredTrim(s string) string {
	return strings.Trim(strings.TrimSpace(s), "\"")
}

// ---------- 应用登录态（B/C 层） ----------

// ZCodeAppCredDir ZCode 应用自己的登录态目录（~/.zcode/v2）。
func ZCodeAppCredDir() string { return filepath.Join(HomeDir(), ".zcode", "v2") }

// ZCodeQuotaCred 查一个 zcode 号额度所需的一对凭据（明文只在进程内流转）。
type ZCodeQuotaCred struct {
	JWT       string `json:"-"`
	DeviceMID string `json:"-"`
	// Source 命中来源的人话标注（"号池文件" / "zcode 账本" / "ZCode 应用登录态"）。
	Source string `json:"source,omitempty"`
	// Note 未命中 / 命中但有保留时的说明（空 = 没问题）。
	Note string `json:"note,omitempty"`
}

// Empty 这对凭据能不能真拿去查额度（JWT 为空即不可用）。
func (c ZCodeQuotaCred) Empty() bool { return strings.TrimSpace(c.JWT) == "" }

// ReadZCodeAppQuotaCred 读 ZCode 应用自己的登录态，解密出当前登录号的读额度凭据。
// appDir 为空时用本机缺省（~/.zcode/v2）。
//
// 只覆盖应用里**当前登录那一个号**——这一点调用方必须知道（见 ResolveZCodeQuotaCred
// 的防误标规则）。
func ReadZCodeAppQuotaCred(appDir string) (ZCodeQuotaCred, error) {
	if strings.TrimSpace(appDir) == "" {
		appDir = ZCodeAppCredDir()
	}
	raw, err := os.ReadFile(filepath.Join(appDir, "credentials.json"))
	if err != nil {
		return ZCodeQuotaCred{}, fmt.Errorf("读 zcode 应用 credentials.json 失败：%w", err)
	}
	var creds map[string]json.RawMessage
	if err := json.Unmarshal(raw, &creds); err != nil {
		return ZCodeQuotaCred{}, fmt.Errorf("zcode 应用 credentials.json 不是合法 json：%w", err)
	}
	rawJWT, ok := creds[zcodeCredJWTToken]
	if !ok {
		return ZCodeQuotaCred{}, fmt.Errorf("zcode 应用 credentials.json 里没有 %s 字段", zcodeCredJWTToken)
	}
	plain, _, derr := DecryptZCodeCredAuto(string(rawJWT))
	if derr != nil {
		return ZCodeQuotaCred{}, fmt.Errorf("zcode 应用凭据解密失败：%w", derr)
	}
	cred := ZCodeQuotaCred{JWT: strings.TrimSpace(plain), Source: "ZCode 应用登录态"}
	// device mid 取自 telemetry-state.json；读不到就留空——上游对空头也认（只是限额口径可能变）。
	cred.DeviceMID = readZCodeDeviceMID(filepath.Join(appDir, "telemetry-state.json"))
	return cred, nil
}

// readZCodeDeviceMID 从 telemetry-state.json 抠 deviceMid（文件缺失/形状不对一律返回空）。
func readZCodeDeviceMID(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var st struct {
		DeviceMID string `json:"deviceMid"`
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return ""
	}
	return strings.TrimSpace(st.DeviceMID)
}

// ZCodeAppDeviceMID 读本机应用级的 device mid（导入时给「账本里没带 dmid」的号兜底）。
func ZCodeAppDeviceMID(appDir string) string {
	if strings.TrimSpace(appDir) == "" {
		appDir = ZCodeAppCredDir()
	}
	return readZCodeDeviceMID(filepath.Join(appDir, "telemetry-state.json"))
}

// ---------- 三层兜底 ----------

// ResolveZCodeQuotaCred 解析号池里一个 zcode 号的读额度凭据（A / B 两层）。
//
//	own       号池凭证文件自带的那份（导入时落盘）—— 最优，随文件跨机器走
//	uid       号池 uid（ZCodeUID 派生）
//	ledgerDir zcode-switch 账本目录（空 = 本机缺省）
//
// C 层（应用登录态）不在这里做：它需要「是否已被别的 uid 占用」的全局视野，
// 由调用方（server 层）在 A/B 全部解析完之后统一裁决。命中返回 true。
func ResolveZCodeQuotaCred(own ZCodeQuotaCred, uid, ledgerDir string) (ZCodeQuotaCred, bool, string) {
	if !own.Empty() {
		c := own
		c.Source = "号池文件"
		return c, true, ""
	}
	return zcodeLedgerQuotaCred(ledgerDir, uid)
}

// zcodeLedgerQuotaCred 从 zcode-switch 账本里按 uid 找这条号的读额度凭据。
// ok=false 时 miss 是「为什么没找到」的人话说明——管理页要靠它把「读不到」
// 的原因说清（账本没这条号 / 账本里是密文解不开 / 账本里压根没有该字段）。
func zcodeLedgerQuotaCred(dir, uid string) (cred ZCodeQuotaCred, ok bool, miss string) {
	if strings.TrimSpace(dir) == "" {
		dir = DefaultZCodeSwitchDir()
	}
	accs, err := ListZCodeAccounts(dir)
	if err != nil {
		return ZCodeQuotaCred{}, false, "读不到 zcode 账本目录（" + zcodeShortErr(err) + "）"
	}
	for _, acc := range accs {
		if ZCodeUID(acc.ID) != uid {
			continue
		}
		if strings.TrimSpace(acc.JWT) != "" {
			return ZCodeQuotaCred{JWT: acc.JWT, DeviceMID: acc.DeviceMID, Source: "zcode 账本"}, true, ""
		}
		if acc.JWTEncrypted {
			return ZCodeQuotaCred{}, false, "账本里这条号的 zcodejwttoken 是 enc:v1 密文，本机解不开（密钥按机器 + 用户名派生，换机器就变了）"
		}
		return ZCodeQuotaCred{}, false, "账本里这条号没有 zcodejwttoken"
	}
	return ZCodeQuotaCred{}, false, "zcode 账本里没有这条号"
}

// zcodeShortErr 单行化错误（读盘错误一般很短，这里只做保险）。
func zcodeShortErr(err error) string {
	s := strings.TrimSpace(err.Error())
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

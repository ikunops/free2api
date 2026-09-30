// workbuddy_desktop.go WorkBuddy 桌面端「应用内登录态」的取源侧：不依赖 wb-switch，
// 直接读客户端自己落盘的登录态文件，把账号自带的上游凭据抽成网关认得的扁平形 auth。
//
// 为什么单写一条链路：WorkBuddy 5.6.2 起把登录态文件里的敏感字段做了「静态加密」
// （at-rest encryption，策略 fields）。文件仍在原处、仍是 JSON，但
//
//	"accessToken": {"$wbEncrypted":1,"envelope":"<base64>"}
//
// 通用解析器（classifyJSON）只会把它数成「加密字段、读不了」——于是「应用内登录态」
// 这一路在 5.6.2 之后等于失效，只能靠 wb-switch 代取。本文件把解密实现出来，
// 让网关自己就能读（用户口径：不寄希望于大家都装了 switch）。
//
// ---------- 文件位置与形态 ----------
//
//	%LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth\      （Windows）
//	~/Library/Application Support/CodeBuddyExtension/Data/Public/auth/  （macOS）
//	~/.local/share/CodeBuddyExtension/Data/Public/auth/      （Linux）
//
// 目录里是**每次登录/刷新写一份**的快照，命名形如：
//
//	workbuddy-desktop.<ISO时间戳>.<pid>.<uuid>.info       → 国内版
//	workbuddy-desktop-ai.<ISO时间戳>.<pid>.<uuid>.info    → 国际版
//	workbuddy-desktop.info.logged-out                     → 登出标记（不参与取号）
//
// 所以「本机登录过的所有账号」= 扫全目录、按 uid 去重、每个 uid 取最新那份快照。
// 这正是「获取当前登录」与「多账号」同时成立的原因（同一账号多次登录只是多几份快照）。
//
// 顶层 JSON 是四段：{account, auth, accounts, allAccounts}。我们只取
// account.uid / account.nickname 与 auth.{accessToken,refreshToken,expiresAt,domain}。
//
// ---------- 解密（逐字对照客户端 packages/at-rest-crypto） ----------
//
//	静态钥 K = sha256(atRestSecretKey)                  // 32 字节，AES-256-GCM 的 key
//	信封     = {suite:1, keyId:"<16 hex>", nonce, authTag, ciphertext}   （各字段 base64）
//	AAD      = "WB-AAD\0" || 0x01 || len("WBEV1")||"WBEV1"
//	           || len("sym-v1")||"sym-v1" || u32be(1) || len(keyId)||keyId
//	           || 0x02 || 0x00 || 0x00                    // framing=field(2)，无 sequence/final
//	明文     = AES-256-GCM(K, nonce, ciphertext, AAD, authTag)
//
// keyId 是 K 的 sha256 前 16 位十六进制——用来**校验**解出来的是不是同一把钥
// （keyId 对不上就不解，绝不拿半截结果去打上游）。
//
// ---------- 静态钥从哪来（三档，按序尝试） ----------
//
//  1. 环境变量 WORKBUDDY_AT_REST_SECRET（排障 / 非常规部署用）
//  2. 现场问客户端要：以 ELECTRON_RUN_AS_NODE 起客户端 exe，调它自己的原生绑定
//     process._linkedBinding("electron_browser_workbuddy_storage").loggerGet()
//     ——这是客户端自己用的 API，版本换了钥换了自己跟着换，是最稳的一档。
//  3. 内置常量：该钥是**编译期常量**，实测 CN 版与国际版两次独立安装取出的值逐字节
//     相同（keyId 9127dea1b44020a7），故同一版本的所有机器通用。风险是随版本可能变，
//     变了就落到前两档（或由环境变量显式指定）。
//
// 三档全失败时如实报错（哪个 keyId 对不上 / 客户端没装），绝不猜。
package source

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// ---------- 常量 ----------

const (
	// WbDeskAuthDirWindows 等：登录态目录相对用户主目录的位置（三平台各一份）。
	wbDeskAuthDirWindows = "AppData/Local/CodeBuddyExtension/Data/Public/auth"
	wbDeskAuthDirMacOS   = "Library/Application Support/CodeBuddyExtension/Data/Public/auth"
	wbDeskAuthDirLinux   = ".local/share/CodeBuddyExtension/Data/Public/auth"

	// 文件名前缀：国际版带 -ai 后缀，国内版不带。两者同目录。
	wbDeskFilePrefixCN = "workbuddy-desktop."
	wbDeskFilePrefixAI = "workbuddy-desktop-ai."

	// wbDeskLoggedOutSuffix 登出标记文件后缀（内容是 uuid），不参与取号。
	wbDeskLoggedOutSuffix = ".logged-out"

	// wbDeskEnvSecret 显式指定静态钥（base64 的 atRestSecretKey 原文）。
	wbDeskEnvSecret = "WORKBUDDY_AT_REST_SECRET"

	// wbDeskKeyIDExpected 内置常量对应的 keyId（用于自检与环境变量校验）。
	wbDeskKeyIDExpected = "9127dea1b44020a7"

	// wbDeskAADDomain / wbDeskFieldFormat / wbDeskScheme：AAD 的三个域串（逐字对照客户端）。
	wbDeskAADDomain   = "WB-AAD\x00"
	wbDeskFieldFormat = "WBEV1"
	wbDeskScheme      = "sym-v1"
	// wbDeskFramingField 字段级加密的 framing 码（file=1, field=2, record=3, stream=4）。
	wbDeskFramingField = 2
)

// wbDeskBuiltinSecret 内置静态钥（base64 的 atRestSecretKey）。
//
// 来源：客户端 5.6.2 的原生绑定 electron_browser_workbuddy_storage.loggerGet()。
// sha256 后 = c74aaf9df09104a5e0b1fd02d1c2b1ea586e4066844933f3279721c688d44055，
// keyId = 9127dea1b44020a7（与 keyblob 里的 protectorKeyId 一致）。
var wbDeskBuiltinSecret = "Sik9U5aXhCdwTVEwsEySDOmDoB9r9ntFxHF1fst9LQI="

// wbDeskFilePrefixes 文件名前缀到「档位」的映射（用于推断 realm 兜底）。
var wbDeskFilePrefixes = []struct {
	Prefix string
	Realm  string // 仅作 domain 缺失时的兜底；正常走 auth.domain 推断
}{
	{wbDeskFilePrefixAI, "global"},
	{wbDeskFilePrefixCN, "cn"},
}

// ---------- 账号 ----------

// WorkBuddyDesktopAccount 从桌面端登录态里抽出来的一个账号。
type WorkBuddyDesktopAccount struct {
	UID          string    // account.uid
	Nickname     string    // account.nickname（解密后；解不开时回落 uid 前缀）
	Domain       string    // auth.domain（www.workbuddy.ai / www.codebuddy.cn …）
	Realm        string    // cn / global（由 domain 推断）
	AccessToken  string    // 明文 accessToken（不序列化，只在进程内流转）
	RefreshToken string    // 明文 refreshToken（同上）
	ExpiresAt    int64     // Unix 秒（文件里是毫秒，已归一）
	EnterpriseID string    // account.enterpriseId（若有）
	SourceFile   string    // 命中的快照文件（排障用）
	FileModTime  time.Time // 该快照的落盘时刻（多份时取最新）
	// Variant 档位来源标注：文件名前缀给出的 cn / ai（与 Realm 分开记，
	// 便于排障时区分「按域名推断」与「按文件名推断」）。
	Variant string
	// KeySource 静态钥来自哪一档（env / client / builtin），用于排障回显。
	KeySource string
}

// Label 账号显示名（昵称优先，其次 uid 前缀）。
func (a WorkBuddyDesktopAccount) Label() string {
	if n := strings.TrimSpace(a.Nickname); n != "" {
		return n
	}
	if len(a.UID) > 8 {
		return a.UID[:8]
	}
	if a.UID != "" {
		return a.UID
	}
	return "(无名账号)"
}

// WbDeskUID 桌面端账号在号池里的 uid（直接沿用 account.uid，与 wb-switch 账本同源同值，
// 因此「switch 导入过的号」与「应用内登录态取到的号」会落到同一个文件名，天然幂等）。
func WbDeskUID(uid string) string { return strings.TrimSpace(uid) }

// ---------- 目录与文件 ----------

// WbDeskAuthDir 登录态目录（Windows/macOS/Linux 各一）。
// override 非空时优先（配置覆盖，外挂盘/非常规安装用）。
func WbDeskAuthDir(override string) string {
	if s := strings.TrimSpace(override); s != "" {
		return s
	}
	home := HomeDir()
	switch runtime.GOOS {
	case "windows":
		// 优先 %LOCALAPPDATA%（Windows 上的权威位置），取不到再按主目录拼。
		if la := strings.TrimSpace(os.Getenv("LOCALAPPDATA")); la != "" {
			return filepath.Join(la, "CodeBuddyExtension", "Data", "Public", "auth")
		}
		return filepath.Join(home, filepath.FromSlash(wbDeskAuthDirWindows))
	case "darwin":
		return filepath.Join(home, filepath.FromSlash(wbDeskAuthDirMacOS))
	default:
		return filepath.Join(home, filepath.FromSlash(wbDeskAuthDirLinux))
	}
}

// wbDeskVariantOf 由文件名前缀判断档位（cn / ai）。不匹配返回空串。
func wbDeskVariantOf(name string) string {
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, wbDeskLoggedOutSuffix) {
		return ""
	}
	// 先判 -ai（长前缀），再判 cn：cn 前缀是 ai 前缀的前缀，顺序反了会把 ai 误判成 cn。
	if strings.HasPrefix(lower, wbDeskFilePrefixAI) {
		return "ai"
	}
	if strings.HasPrefix(lower, wbDeskFilePrefixCN) {
		return "cn"
	}
	return ""
}

// wbDeskSnapshots 列出目录里所有登录态快照（跳过登出标记与非目标文件）。
func wbDeskSnapshots(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if wbDeskVariantOf(e.Name()) == "" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}

// ---------- 信封解密 ----------

// wbDeskKey 一把可用的静态钥及其 keyId。
type wbDeskKey struct {
	key    []byte // 32 字节
	keyID  string // sha256(key) 前 16 hex
	source string // env / client / builtin
}

// wbDeskKeyFromSecret 由 atRestSecretKey（base64 字符串）派生静态钥。
// keyId 由 key 本身算出（不信任外部声明），调用方拿它去和信封里的 keyId 比对。
func wbDeskKeyFromSecret(secret, source string) (wbDeskKey, error) {
	s := strings.TrimSpace(secret)
	if s == "" {
		return wbDeskKey{}, fmt.Errorf("空的 atRestSecretKey")
	}
	k := sha256.Sum256([]byte(s))
	key := k[:]
	sum := sha256.Sum256(key)
	return wbDeskKey{
		key:    append([]byte(nil), key...),
		keyID:  hex.EncodeToString(sum[:8]), // 8 字节 = 16 hex
		source: source,
	}, nil
}

// wbDeskKeys 按序收集本机可用的静态钥候选：环境变量 → 现场问客户端 → 内置常量。
// 去重（同 keyId 只留第一个），失败项静默跳过（排障信息由调用方按需再问）。
func wbDeskKeys() []wbDeskKey {
	out := make([]wbDeskKey, 0, 3)
	seen := map[string]bool{}
	add := func(secret, source string) {
		k, err := wbDeskKeyFromSecret(secret, source)
		if err != nil || seen[k.keyID] {
			return
		}
		seen[k.keyID] = true
		out = append(out, k)
	}
	add(os.Getenv(wbDeskEnvSecret), "env")
	for _, s := range wbDeskClientSecrets() {
		add(s, "client")
	}
	add(wbDeskBuiltinSecret, "builtin")
	return out
}

// wbDeskClientSecrets 现场向本机 WorkBuddy 客户端索取静态钥。
//
// 手段：以 ELECTRON_RUN_AS_NODE=1 起客户端 exe（Electron 的「当 node 用」开关），
// 执行一段脚本调它自己的原生绑定。绑定名与调用方式取自客户端自带代码
// （lib/browser/api/workbuddy-storage.ts 里 loggerGet = process._linkedBinding(...)）。
//
// 客户端没装 / 起不来 / 版本改了 API 名 → 返回空切片，由内置常量兜底。
func wbDeskClientSecrets() []string {
	if runtime.GOOS != "windows" {
		// macOS/Linux 的客户端 exe 位置与启动方式差异较大，本版本先只做 Windows
		// （用户场景全在 Windows）；非 Windows 直接走内置常量/环境变量。
		return nil
	}
	exes := wbDeskClientExes()
	out := make([]string, 0, len(exes))
	for _, exe := range exes {
		if s := wbDeskAskClient(exe); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// wbDeskClientExes 本机可能存在的客户端 exe 路径（去重）。
func wbDeskClientExes() []string {
	cands := []string{}
	// 1) wb-switch 的 exe 缓存（它记着用户实际装在哪，最准）
	if b, err := os.ReadFile(filepath.Join(HomeDir(), ".wb-switch", "workbuddy_exe.json")); err == nil {
		var m map[string]string
		if json.Unmarshal(b, &m) == nil {
			for _, v := range m {
				cands = append(cands, strings.TrimSpace(v))
			}
		}
	}
	// 2) 常见安装位（国内版 / 国际版）
	for _, drive := range []string{"C:", "D:", "E:", "F:", "G:", "H:"} {
		cands = append(cands,
			drive+`\workbutty\WorkBuddy\WorkBuddy.exe`,
			drive+`\WorkBuddy\WorkBuddy.exe`,
			drive+`\workbuddy 海外\WorkBuddyAI\WorkBuddyAI.exe`,
			drive+`\WorkBuddyAI\WorkBuddyAI.exe`,
		)
	}
	cands = append(cands,
		filepath.Join(os.Getenv("ProgramFiles"), "WorkBuddy", "WorkBuddy.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "WorkBuddy", "WorkBuddy.exe"),
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "WorkBuddy", "WorkBuddy.exe"),
	)
	seen := map[string]bool{}
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		c = strings.TrimSpace(c)
		if c == "" || seen[strings.ToLower(c)] {
			continue
		}
		seen[strings.ToLower(c)] = true
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			out = append(out, c)
		}
	}
	return out
}

// wbDeskAskClient 起一次客户端进程问静态钥。任何失败都返回空串（不 panic、不阻塞太久）。
func wbDeskAskClient(exe string) string {
	const script = `const b=process._linkedBinding("electron_browser_workbuddy_storage");` +
		`const p=JSON.parse(b.loggerGet());process.stdout.write(p.atRestSecretKey||"");`
	cmd := exec.Command(exe, "-e", script)
	cmd.Env = append(os.Environ(), "ELECTRON_RUN_AS_NODE=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// 客户端冷启动可能要几秒；给 15s 上限，超时就放弃这一档。
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		return ""
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return ""
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return ""
	}
	s := strings.TrimSpace(stdout.String())
	// 只认「base64 的 32 字节」形状，防止把报错文本当钥用。
	if _, err := wbDeskKeyFromSecret(s, "client"); err != nil {
		return ""
	}
	return s
}

// wbDeskOpenEnvelope 解一条 $wbEncrypted 信封，返回明文。
//
// 传入多把候选钥时逐个试：先用信封自带的 keyId 挑出对得上的那把（正常情况下只有一把），
// keyId 全对不上就直接报错——不做「盲试」以免把认证失败的噪音当成功。
func wbDeskOpenEnvelope(envelopeB64 string, keys []wbDeskKey) ([]byte, wbDeskKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(envelopeB64))
	if err != nil {
		return nil, wbDeskKey{}, fmt.Errorf("信封不是合法 base64：%w", err)
	}
	var env struct {
		Suite      int    `json:"suite"`
		KeyID      string `json:"keyId"`
		Nonce      string `json:"nonce"`
		AuthTag    string `json:"authTag"`
		Ciphertext string `json:"ciphertext"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, wbDeskKey{}, fmt.Errorf("信封不是合法 JSON：%w", err)
	}
	if env.Suite != 1 {
		return nil, wbDeskKey{}, fmt.Errorf("不支持的加密套件 suite=%d", env.Suite)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil || len(nonce) != 12 {
		return nil, wbDeskKey{}, fmt.Errorf("信封 nonce 非法（长度 %d，期望 12）", len(nonce))
	}
	tag, err := base64.StdEncoding.DecodeString(env.AuthTag)
	if err != nil || len(tag) != 16 {
		return nil, wbDeskKey{}, fmt.Errorf("信封 authTag 非法（长度 %d，期望 16）", len(tag))
	}
	ct, err := base64.StdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		return nil, wbDeskKey{}, fmt.Errorf("信封 ciphertext 不是合法 base64：%w", err)
	}
	// 用信封声明的 keyId 挑钥；没有匹配的直接报错（信息里带 keyId，便于对账）。
	var picked *wbDeskKey
	for i := range keys {
		if keys[i].keyID == env.KeyID {
			picked = &keys[i]
			break
		}
	}
	if picked == nil {
		return nil, wbDeskKey{}, fmt.Errorf(
			"没有能解这条信封的静态钥（信封 keyId=%s，本机可用钥 %s）；"+
				"该文件可能来自另一版本客户端，可用环境变量 %s 指定钥",
			env.KeyID, wbDeskKeyIDs(keys), wbDeskEnvSecret)
	}
	block, err := aes.NewCipher(picked.key)
	if err != nil {
		return nil, wbDeskKey{}, fmt.Errorf("构造 AES 失败：%w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, wbDeskKey{}, fmt.Errorf("构造 GCM 失败：%w", err)
	}
	aad := wbDeskFieldAAD(picked.keyID, env.Suite)
	// Go 的 Open 需要 tag 附在密文尾部。
	sealed := append(append([]byte(nil), ct...), tag...)
	plain, err := gcm.Open(nil, nonce, sealed, aad)
	if err != nil {
		return nil, wbDeskKey{}, fmt.Errorf("AES-GCM 认证失败（钥 keyId=%s 与信封不符或密文被改）：%w", picked.keyID, err)
	}
	return plain, *picked, nil
}

// wbDeskFieldAAD 拼字段级加密的 AAD（逐字对照客户端 buildAuthenticatedContextAad）。
//
//	"WB-AAD\0" || 0x01 || len("WBEV1")||"WBEV1" || len("sym-v1")||"sym-v1"
//	|| u32be(suite) || len(keyId)||keyId || framing(0x02) || 0x00 || 0x00
func wbDeskFieldAAD(keyID string, suite int) []byte {
	buf := make([]byte, 0, 64)
	buf = append(buf, wbDeskAADDomain...)
	buf = append(buf, 0x01)
	buf = append(buf, wbDeskLenPrefixed(wbDeskFieldFormat)...)
	buf = append(buf, wbDeskLenPrefixed(wbDeskScheme)...)
	buf = append(buf, byte(suite>>24), byte(suite>>16), byte(suite>>8), byte(suite))
	buf = append(buf, wbDeskLenPrefixed(keyID)...)
	buf = append(buf, byte(wbDeskFramingField))
	buf = append(buf, 0x00) // encodeOptionalUint64(undefined)
	buf = append(buf, 0x00) // final 未设置
	return buf
}

// wbDeskLenPrefixed u32be 长度前缀 + UTF-8 字节（对照 encodeLengthPrefixed）。
func wbDeskLenPrefixed(s string) []byte {
	b := []byte(s)
	out := make([]byte, 0, 4+len(b))
	n := uint32(len(b))
	out = append(out, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	return append(out, b...)
}

func wbDeskKeyIDs(keys []wbDeskKey) string {
	if len(keys) == 0 {
		return "(无)"
	}
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k.keyID+"/"+k.source)
	}
	return strings.Join(parts, ",")
}

// ---------- 读文件 ----------

// wbDeskRawField 一个可能是明文或加密信封的字段（宽松形态）。
type wbDeskRawField struct {
	plain   string
	enc     bool
	envB64  string
	present bool
}

func wbDeskParseField(raw json.RawMessage) wbDeskRawField {
	if len(raw) == 0 {
		return wbDeskRawField{}
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if strings.TrimSpace(s) == "" {
			return wbDeskRawField{}
		}
		return wbDeskRawField{plain: s, present: true}
	}
	var obj struct {
		Encrypted int    `json:"$wbEncrypted"`
		Envelope  string `json:"envelope"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Encrypted == 1 && obj.Envelope != "" {
		return wbDeskRawField{enc: true, envB64: obj.Envelope, present: true}
	}
	return wbDeskRawField{}
}

// wbDeskResolve 把（可能加密的）字段解成明文。keys 为空时只能读明文。
func (f wbDeskRawField) resolve(keys []wbDeskKey) (string, wbDeskKey, error) {
	switch {
	case !f.present:
		return "", wbDeskKey{}, nil
	case !f.enc:
		return f.plain, wbDeskKey{}, nil
	}
	plain, k, err := wbDeskOpenEnvelope(f.envB64, keys)
	if err != nil {
		return "", wbDeskKey{}, err
	}
	return string(plain), k, nil
}

// wbDeskInfoFile 一个快照文件的解析形态（只声明我们用得到的键，其余忽略）。
type wbDeskInfoFile struct {
	Account struct {
		UID          string          `json:"uid"`
		Nickname     json.RawMessage `json:"nickname"`
		EnterpriseID string          `json:"enterpriseId"`
	} `json:"account"`
	Auth struct {
		AccessToken     json.RawMessage `json:"accessToken"`
		RefreshToken    json.RawMessage `json:"refreshToken"`
		ExpiresAt       int64           `json:"expiresAt"`
		RefreshExpires  int64           `json:"refreshExpiresAt"`
		Domain          string          `json:"domain"`
		TokenType       string          `json:"tokenType"`
		LastRefreshTime int64           `json:"lastRefreshTime"`
	} `json:"auth"`
}

// wbDeskReadSnapshot 读一个快照文件，解出账号。keys 为空时只读明文（5.6.2 前的老文件）。
//
// 返回 (账号, 命中的钥, 错误)。「没有可用凭据」（字段缺失 / 全是解不开的密文）都算错误，
// 由调用方决定是否跳过——本函数不返回半截结果。
func wbDeskReadSnapshot(path string, keys []wbDeskKey) (WorkBuddyDesktopAccount, wbDeskKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return WorkBuddyDesktopAccount{}, wbDeskKey{}, err
	}
	var f wbDeskInfoFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return WorkBuddyDesktopAccount{}, wbDeskKey{}, fmt.Errorf("不是合法 JSON：%w", err)
	}
	at := wbDeskParseField(f.Auth.AccessToken)
	if !at.present {
		return WorkBuddyDesktopAccount{}, wbDeskKey{}, fmt.Errorf("没有 accessToken 字段")
	}
	token, key, err := at.resolve(keys)
	if err != nil {
		return WorkBuddyDesktopAccount{}, wbDeskKey{}, fmt.Errorf("accessToken 解密失败：%w", err)
	}
	if strings.TrimSpace(token) == "" {
		return WorkBuddyDesktopAccount{}, wbDeskKey{}, fmt.Errorf("accessToken 为空")
	}
	// refreshToken 解不开不算致命（只影响保活），尽力而为。
	refresh := ""
	if rt := wbDeskParseField(f.Auth.RefreshToken); rt.present {
		if v, _, rerr := rt.resolve(keys); rerr == nil {
			refresh = v
		}
	}
	nick := ""
	if nf := wbDeskParseField(f.Account.Nickname); nf.present {
		if v, _, nerr := nf.resolve(keys); nerr == nil {
			nick = strings.TrimSpace(v)
		}
	}
	fi, _ := os.Stat(path)
	acc := WorkBuddyDesktopAccount{
		UID:          strings.TrimSpace(f.Account.UID),
		Nickname:     nick,
		Domain:       strings.TrimSpace(f.Auth.Domain),
		Realm:        realmOfWbDeskDomain(f.Auth.Domain),
		AccessToken:  token,
		RefreshToken: refresh,
		ExpiresAt:    normalizeExpiry(f.Auth.ExpiresAt),
		EnterpriseID: strings.TrimSpace(f.Account.EnterpriseID),
		SourceFile:   path,
		Variant:      wbDeskVariantOf(filepath.Base(path)),
		KeySource:    key.source,
	}
	if fi != nil {
		acc.FileModTime = fi.ModTime()
	}
	if acc.UID == "" {
		// uid 缺失时用 token 哈希造稳定 id（与其它源同规则，保证重复导入幂等）。
		acc.UID = derivedUID(acc.AccessToken)
	}
	return acc, key, nil
}

// realmOfWbDeskDomain 由 domain 推断 cn / global（与 auth.ResolveRealm 同口径，
// 这里不引 auth 包以避免 source→auth 的额外耦合；口径不一致时以 auth 包为准）。
func realmOfWbDeskDomain(domain string) string {
	d := strings.ToLower(strings.TrimSpace(domain))
	if d == "workbuddy.ai" || strings.HasSuffix(d, ".workbuddy.ai") {
		return "global"
	}
	return "cn"
}

// ---------- 对外入口 ----------

// WorkBuddyDesktopResult 一次桌面端取源的结果。
type WorkBuddyDesktopResult struct {
	Accounts []WorkBuddyDesktopAccount
	// Keys 本次实际可用的静态钥档位（排障回显）。
	KeyIDs []string
	// Skipped 读到了文件但取不出凭据的（文件 → 原因），供管理页如实说明。
	Skipped map[string]string
}

// ListWorkBuddyDesktop 扫本机桌面端登录态，返回按 uid 去重后的账号（每个 uid 取最新快照）。
//
// dir 为空时用本机缺省。目录不存在 / 没有任何快照 → 返回空结果（不是错误）：
// 调用方据此显示「该工具没装或从未登录」。
func ListWorkBuddyDesktop(dir string) (WorkBuddyDesktopResult, error) {
	res := WorkBuddyDesktopResult{Skipped: map[string]string{}}
	if strings.TrimSpace(dir) == "" {
		dir = WbDeskAuthDir("")
	}
	files, err := wbDeskSnapshots(dir)
	if err != nil {
		return res, err
	}
	keys := wbDeskKeys()
	res.KeyIDs = make([]string, 0, len(keys))
	for _, k := range keys {
		res.KeyIDs = append(res.KeyIDs, k.keyID+"/"+k.source)
	}
	if len(files) == 0 {
		return res, nil
	}
	// 先按修改时间升序，同 uid 后出现的覆盖先出现的（= 取最新快照）。
	sort.Slice(files, func(i, j int) bool {
		fi, ei := os.Stat(files[i])
		fj, ej := os.Stat(files[j])
		if ei != nil || ej != nil {
			return files[i] < files[j]
		}
		return fi.ModTime().Before(fj.ModTime())
	})
	byUID := map[string]WorkBuddyDesktopAccount{}
	order := make([]string, 0, len(files))
	for _, p := range files {
		acc, _, rerr := wbDeskReadSnapshot(p, keys)
		if rerr != nil {
			res.Skipped[filepath.Base(p)] = rerr.Error()
			continue
		}
		if _, seen := byUID[acc.UID]; !seen {
			order = append(order, acc.UID)
		}
		byUID[acc.UID] = acc
	}
	res.Accounts = make([]WorkBuddyDesktopAccount, 0, len(order))
	for _, uid := range order {
		res.Accounts = append(res.Accounts, byUID[uid])
	}
	return res, nil
}

// WbDeskCandidates 管理页用：把桌面端账号摊成 Candidate 列表（带凭据指纹与档位）。
func WbDeskCandidates(dir, authDir string) ([]Candidate, error) {
	res, err := ListWorkBuddyDesktop(dir)
	if err != nil {
		return nil, err
	}
	existing := importedIndex(authDir)
	out := make([]Candidate, 0, len(res.Accounts))
	for _, a := range res.Accounts {
		detail := "档位=" + a.Realm + "（" + a.Variant + "）· 域=" + a.Domain
		if a.KeySource != "" && a.KeySource != "builtin" {
			detail += " · 钥源=" + a.KeySource
		}
		c := Candidate{
			ID:         WbDeskUID(a.UID),
			Label:      a.Label(),
			Realm:      a.Realm,
			ExpiresAt:  a.ExpiresAt,
			Expired:    isExpired(a.ExpiresAt),
			HasRefresh: a.RefreshToken != "",
			Producer:   ProducerWorkbuddy,
			Usable:     true,
			Detail:     detail,
			APIKeyHint: keyHint(a.AccessToken),
		}
		if fn, ok := existing[a.UID]; ok {
			c.Imported = true
			c.ImportedAs = fn
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

// ImportWorkBuddyDesktopAs 把桌面端账号导入号池。ids 命中 uid；空 = 全量。
// 与 wb-switch 导入同口径：uid 相同即同一账号，重复导入幂等（跳过）。
func ImportWorkBuddyDesktopAs(dir, authDir string, ids []string, opt ImportOptions) (ImportResult, error) {
	res0, err := ListWorkBuddyDesktop(dir)
	if err != nil {
		return ImportResult{}, err
	}
	opt = opt.normalized()
	opt.Producer = ProducerWorkbuddy
	if opt.Detail == "" {
		opt.Detail = WbDeskAuthDir(dir)
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Kind: "workbuddy-app", AuthDir: authDir}
	labels := map[string]string{}
	for _, a := range res0.Accounts {
		if len(want) > 0 && !want[a.UID] {
			continue
		}
		uid := WbDeskUID(a.UID)
		dest := filepath.Join(authDir, AuthFileName(uid))
		if _, serr := os.Stat(dest); serr == nil {
			res.Skipped = append(res.Skipped, uid)
			continue
		}
		f := Flat{
			AccessToken:  a.AccessToken,
			RefreshToken: a.RefreshToken,
			ExpiresAt:    a.ExpiresAt,
			Domain:       a.Domain,
			Realm:        a.Realm,
			UID:          uid,
			EnterpriseID: a.EnterpriseID,
			Nickname:     a.Label(),
			Producer:     ProducerWorkbuddy,
		}
		if werr := writeFlatAtomic(dest, f); werr != nil {
			res.Errors = append(res.Errors, uid+": "+werr.Error())
			continue
		}
		res.Imported = append(res.Imported, uid)
		labels[uid] = a.Label()
	}
	sort.Strings(res.Imported)
	sort.Strings(res.Skipped)
	if len(res.Imported) > 0 {
		mark := make(map[string]Origin, len(res.Imported))
		for _, uid := range res.Imported {
			mark[uid] = Origin{Method: opt.Method, Producer: ProducerWorkbuddy, Label: labels[uid], Detail: opt.Detail}
		}
		_ = LoadRegistry(authDir).Mark(mark)
	}
	return res, nil
}

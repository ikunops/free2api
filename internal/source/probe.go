// probe.go 「应用内登录态」探查：本机各 AI 工具**自己**的登录态在哪、能不能直接取。
//
// 与「switch 账本」的分工：账本（wb-switch / zcode-switch）是第三方工具帮你聚合好的
// 多账号清单，读它就够；应用内登录态是工具本体那一个当前登录，散在各家私有目录里
// （Electron 的 leveldb / SQLite / 私有 json），形状与加密方式各家不同。
//
// 本包只做「探查 + 如实上报」：能读的（明文 json / 已实现解密的加密字段）给出路径与
// 账号标识；读不了的（enc:v1 加密 / leveldb 二进制 / sqlite）明确标注原因。**不用猜出来的
// 解析糊弄**——猜错的 token 比没有 token 更坏事（拿错凭证去打上游 = 直接触发风控）。
//
// 特例：WorkBuddy 桌面端 5.6.2 起把登录态字段做了静态加密（$wbEncrypted），但它用的是
// 编译期常量钥，本网关已实现解密（见 workbuddy_desktop.go），所以这一条**不再是只发现**，
// 而是真能读出账号——这是「不装 switch 也能取本机登录态」的落点。
package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// 探查结论的几档「能不能用」。
const (
	KindNone      = "none"       // 目标不存在
	KindPlainJSON = "plain-json" // 明文 json，可以直接读成凭证
	KindEncJSON   = "enc-json"   // json 但在字段级加密（enc:v1），本网关读不了
	KindLevelDB   = "leveldb"    // Electron local_storage，二进制，需要专用解析
	KindSQLite    = "sqlite"     // SQLite 库，需要专用解析
	KindUnknown   = "unknown"
)

// ProbeTarget 一个候选探查目标及其结论。
type ProbeTarget struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Producer string `json:"producer"`
	Path     string `json:"path"`
	Exists   bool   `json:"exists"`
	Kind     string `json:"kind"`
	Readable bool   `json:"readable"` // 能否被本网关直接读成可用凭证
	Accounts int    `json:"accounts"` // 探测到的账号数（读不了的记 0，不编造）
	Note     string `json:"note"`
}

// probeSpec 探查目标的静态描述。
type probeSpec struct {
	id, name, producer, path, hint string
	// special 非空时走专用解析器：该工具的凭证文件形态是通用 classifyJSON 认不出来的。
	//   - ""：默认，按 token/credential 键名数明文/密文字段
	//   - "opencode"：OpenCode 的 auth.json，形如 {providerID: {type:"api", key:"..."}}，
	//     键名既不含 token 也不含 credential，必须按「对象里有 key 字段」判定
	//   - "workbuddy-desktop"：WorkBuddy 客户端登录态目录（多个 .info 快照 + 字段级
	//     加密），走 workbuddy_desktop.go 的专用解析（含解密），path 是 auth 目录本身
	special string
}

// Probe 扫本机已知的应用登录态目录，逐个给出「在不在 / 什么形态 / 能不能用」。
// 只读操作，不写任何东西。
func Probe() []ProbeTarget {
	home := HomeDir()
	specs := []probeSpec{
		// WorkBuddy 桌面端：凭证**不在** Electron 数据目录（~/.workbuddy*）里，而在客户端
		// 自己的登录态目录（CodeBuddyExtension/.../auth，CN 与国际版同目录、靠文件名区分）。
		// 5.6.2 起字段级加密，本网关已实现解密，所以这条能直接读出账号。
		{id: "workbuddy-desktop", name: "WorkBuddy 桌面端登录态（CN + 国际版）",
			producer: ProducerWorkbuddy, path: WbDeskAuthDir(""),
			hint: "客户端登录态目录（5.6.2 起字段级加密，本网关已能解）",
			special: "workbuddy-desktop"},
		{id: "zcode-app", name: "ZCode 应用当前登录", producer: ProducerZCode,
			path: filepath.Join(home, ".zcode", "v2", "credentials.json"), hint: "私有 json"},
		{id: "zcode-app-alt", name: "ZCode 应用（旧版 v2 目录）", producer: ProducerZCode,
			path: filepath.Join(home, ".zcode", "v2"), hint: "私有目录"},
		{id: "qoder-app", name: "Qoder 应用", producer: ProducerQoder,
			path: filepath.Join(home, ".qoder"), hint: "私有目录"},
		{id: "opencode-auth", name: "OpenCode CLI/TUI（OpenCode Zen）", producer: ProducerOpenCode,
			path: filepath.Join(home, ".local", "share", "opencode", "auth.json"),
			hint: "明文 json（providerID 到 {type,key} 的映射）", special: "opencode"},
		{id: "trae-app", name: "Trae 应用", producer: "trae",
			path: filepath.Join(home, ".trae"), hint: "VS Code 派生（state.vscdb）"},
		{id: "cursor-app", name: "Cursor 应用", producer: "cursor",
			path: filepath.Join(home, ".cursor"), hint: "VS Code 派生（state.vscdb）"},
	}
	out := make([]ProbeTarget, 0, len(specs))
	for _, s := range specs {
		out = append(out, probeOne(s))
	}
	return out
}

func probeOne(s probeSpec) ProbeTarget {
	t := ProbeTarget{ID: s.id, Name: s.name, Producer: s.producer, Path: s.path}
	if s.special == "workbuddy-desktop" {
		return probeWorkBuddyDesktop(t)
	}
	info, err := os.Stat(s.path)
	if err != nil {
		t.Kind = KindNone
		t.Note = "本机没有这个路径（该工具没装，或从未登录）"
		return t
	}
	t.Exists = true
	if !info.IsDir() {
		return probeFile(t, s)
	}
	return probeDir(t, s)
}

// probeFile 单文件形态：只有 json 需要细看，其余按未知处理。
func probeFile(t ProbeTarget, s probeSpec) ProbeTarget {
	if !strings.HasSuffix(strings.ToLower(t.Path), ".json") {
		t.Kind = KindUnknown
		t.Note = s.hint + "：非 json，未识别"
		return t
	}
	raw, err := os.ReadFile(t.Path)
	if err != nil {
		t.Kind = KindUnknown
		t.Note = "文件存在但读取失败：" + err.Error()
		return t
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Kind = KindUnknown
		t.Note = "文件不是合法 json"
		return t
	}
	enc, plain, credKeys := classifyJSON(obj)
	if s.special == "opencode" {
		enc, plain, credKeys = classifyOpenCodeAuth(obj)
	}
	switch {
	case enc > 0 && plain == 0:
		t.Kind = KindEncJSON
		// 密钥按「机器 + 用户名」派生（zcode 侧的 fallback 规则），所以这串密文只在**原机**能解：
		// 拷贝到另一台机器上解不开，也就没法直接搬走。不要写成「密钥只在该工具手里」——那是错的。
		t.Note = "凭证是 " + "enc:v1" + " 字段级加密（密钥按机器 + 用户名派生，换机器解不开）；" +
			"发现 " + itoa(enc) + " 个加密凭证字段（" + strings.Join(credKeys, ", ") + "）"
	case plain > 0:
		t.Kind = KindPlainJSON
		t.Readable = true
		t.Accounts = plain
		t.Note = "明文凭证字段 " + itoa(plain) + " 个（" + strings.Join(credKeys, ", ") + "），可直接导入"
	case len(obj) == 0:
		t.Kind = KindUnknown
		t.Note = "空 json"
	default:
		t.Kind = KindUnknown
		t.Note = "json 里没有识别到凭证字段（" + itoa(len(obj)) + " 个键）"
	}
	return t
}

// probeWorkBuddyDesktop WorkBuddy 桌面端登录态：走专用解析（含字段解密）。
//
// 与其他目标的区别：这里**不是**「看一眼能不能读」，而是真去读一遍——因为
// 「能读几个账号」正是用户要的答案，而它取决于本机有没有可用的静态钥
// （现场问客户端 / 内置常量 / 环境变量）。读出来的结果与「看候选」用的是同一套代码，
// 所以卡片上的数字与实际能导入的数量永远一致。
func probeWorkBuddyDesktop(t ProbeTarget) ProbeTarget {
	if _, err := os.Stat(t.Path); err != nil {
		t.Kind = KindNone
		t.Note = "本机没有这个路径（该工具没装，或从未登录）"
		return t
	}
	t.Exists = true
	res, err := ListWorkBuddyDesktop(t.Path)
	if err != nil {
		t.Kind = KindUnknown
		t.Note = "目录存在但读取失败：" + err.Error()
		return t
	}
	if len(res.Accounts) == 0 {
		t.Kind = KindEncJSON
		t.Note = "登录态目录存在，但没有解析出可用账号"
		if n := len(res.Skipped); n > 0 {
			t.Note += "（" + itoa(n) + " 个快照取不出凭据：钥不匹配或字段缺失）"
		}
		return t
	}
	t.Kind = KindPlainJSON
	t.Readable = true
	t.Accounts = len(res.Accounts)
	t.Note = "可直接读取 " + itoa(len(res.Accounts)) + " 个账号（含字段级解密）"
	if n := len(res.Skipped); n > 0 {
		t.Note += "；另有 " + itoa(n) + " 个快照取不出凭据"
	}
	if len(res.KeyIDs) > 0 {
		t.Note += " · 可用钥 " + strings.Join(res.KeyIDs, ",")
	}
	return t
}

// probeDir 目录形态：按目录里的标志性文件判定存储引擎。
func probeDir(t ProbeTarget, s probeSpec) ProbeTarget {
	// sqlite：根目录下直接躺着 .db / .sqlite（如 ~/.workbuddy/workbuddy.db）
	if hasFileWithSuffix(t.Path, ".db") || hasFileWithSuffix(t.Path, ".sqlite") {
		t.Kind = KindSQLite
		t.Note = s.hint + "：凭证落在 SQLite 库里，需要专用解析（本版本只做发现）"
		return t
	}
	// electron leveldb：local_storage 子目录里有 *.info / CURRENT / MANIFEST
	ls := filepath.Join(t.Path, "local_storage")
	if entries, err := os.ReadDir(ls); err == nil && len(entries) > 0 {
		n := 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".info") {
				n++
			}
		}
		if n > 0 {
			t.Kind = KindLevelDB
			t.Note = s.hint + "：登录态在 Electron local_storage（leveldb，" + itoa(n) +
				" 个 .info 段），二进制格式需要专用解析；本机这条链路建议走 switch 账本"
			return t
		}
	}
	// 目录里直接有明文 json 凭证的（少见，但真有工具这么干）
	if f := findFirstJSON(t.Path); f != "" {
		sub := t
		sub.Path = f
		return probeFile(sub, s)
	}
	t.Kind = KindUnknown
	t.Note = s.hint + "：目录存在，但没识别出已知的凭证形态（" + itoa(countEntries(t.Path)) + " 个条目）"
	return t
}

// classifyJSON 数一份 json 里的凭证字段：加密几个、明文几个，以及命中的键名。
// 判定口径刻意保守——只认名字里带 token/credential 且值非空的键。
func classifyJSON(obj map[string]any) (enc, plain int, keys []string) {
	for k, v := range obj {
		lk := strings.ToLower(k)
		if !strings.Contains(lk, "token") && !strings.Contains(lk, "credential") {
			continue
		}
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			// 嵌套对象（如 oauth:{access_token:...}）再下一层看
			if sub, ok2 := v.(map[string]any); ok2 {
				e2, p2, _ := classifyJSON(sub)
				enc += e2
				plain += p2
				if e2+p2 > 0 {
					keys = append(keys, k)
				}
			}
			continue
		}
		keys = append(keys, k)
		if strings.HasPrefix(s, "enc:v1:") {
			enc++
		} else {
			plain++
		}
	}
	return enc, plain, keys
}

// classifyOpenCodeAuth 数 OpenCode auth.json 里的可用凭据。
//
// 形态（实测 2026-09，730 字节）：顶层键是 providerID，值是 {type:"api", key:"..."}。
// 键名是 "openrouter" / "opencode" / "zhipuai-coding-plan" 这种，既不含 token 也不含
// credential，通用 classifyJSON 一个都数不出来——所以这里单写一条：对象里带非空
// 字符串 key 字段的算一条明文凭据。
// 只数**本网关真的会取用**的凭据（OpenCode 自家 provider：opencode=Zen / opencode-go）——
// 与「获取源」卡片、候选列表、导入三处口径一致。用户自己在 OpenCode 里配的第三方
// provider（openrouter / deepseek / zhipuai-coding-plan …）是「agent 上的模型」，
// 本网关不取，就不该算进「可直接读取的账号数」，否则卡片会报 6 个而可取用只有 2 个。
func classifyOpenCodeAuth(obj map[string]any) (enc, plain int, keys []string) {
	for k, v := range obj {
		if !isOpenCodeOwnProvider(k) {
			continue
		}
		sub, ok := v.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := sub["key"].(string)
		if !ok || strings.TrimSpace(raw) == "" {
			continue
		}
		keys = append(keys, k)
		if strings.HasPrefix(raw, "enc:v1:") {
			enc++
		} else {
			plain++
		}
	}
	return enc, plain, keys
}

func hasFileWithSuffix(dir, suffix string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), suffix) {
			return true
		}
	}
	return false
}

func findFirstJSON(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

func countEntries(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	return len(entries)
}

// itoa 局部小工具：避免为几处计数再引 strconv（本包已在别处用 fmt）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

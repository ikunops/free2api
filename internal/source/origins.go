// origins.go 账号来源归属（provenance）：auths 目录里每个账号「从哪来」。
//
// 存在的理由：三种获取方式（应用内登录态 / switch 账本 / 外部导入文件）导进来的账号
// 进的是同一个 auths 目录，光看凭证文件分不出谁是谁。来源单独记一份台账，管理页才能
// 按来源分组、按来源筛选、整体迁移时来源跟着走。
//
// 落盘位置：<authDir>/.origins.json —— 选在 auths 目录内、点号开头：
// auth.LoadDir 的 glob 是 workbuddy*.json，台账不会被当凭证误读；整个 auths 目录
// 拷到另一台机器，来源信息一起走（单二进制整体迁移的语义）。
package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// 三种「来源」（获取方式）。产品里对应三个入口，常量顺序即用户描述的顺序。
const (
	// MethodApp 应用内登录态：本机工具**自己**的当前登录，靠探查拿到。
	MethodApp = "app"
	// MethodSwitch switch 账本：账本里已经聚合好的多个账号，读账本即可。
	MethodSwitch = "switch"
	// MethodFile 其他机器导出的文件 / 粘贴的 JSON 快照。
	MethodFile = "file"
)

// 生产者：账号归属哪个 AI 工具。当前只有 workbuddy 走通了端到端反代。
const (
	ProducerWorkbuddy = "workbuddy"
	ProducerZCode     = "zcode"
	ProducerQoder     = "qoder"
	// ProducerOpenCode OpenCode?CLI/TUI????????????OpenCode ???
	// ????????? zen ???? internal/upstream/opencode.go ?????
	// ????? `opencode serve` ????????????????????
	ProducerOpenCode = "opencode"
)

// MethodOrder 三种来源的展示顺序（管理页按这个顺序排，后端定死避免前后端各写一份）。
var MethodOrder = []string{MethodApp, MethodSwitch, MethodFile}

// MethodLabel 来源方式的中文名。
func MethodLabel(m string) string {
	switch m {
	case MethodApp:
		return "应用内登录态"
	case MethodSwitch:
		return "switch 账本"
	case MethodFile:
		return "导入文件"
	}
	if m == "" {
		return "未标注"
	}
	return m
}

// ProducerLabel 生产者的显示名。
func ProducerLabel(p string) string {
	switch p {
	case ProducerWorkbuddy:
		return "WorkBuddy"
	case ProducerZCode:
		return "ZCode"
	case ProducerQoder:
		return "Qoder"
	case ProducerOpenCode:
		return "OpenCode"
	}
	if p == "" {
		return "未知"
	}
	return p
}

// Origin 一条来源归属记录。
type Origin struct {
	Method   string `json:"method"`             // app / switch / file
	Producer string `json:"producer"`           // workbuddy / zcode / qoder
	Label    string `json:"label,omitempty"`    // 导入时的账号名（凭证刷新后仍能对账）
	Detail   string `json:"detail,omitempty"`   // 来源细节：账本路径 / 文件名
	AddedAt  int64  `json:"added_at,omitempty"` // 导入时刻（Unix 秒）
}

// Registry 来源台账（uid → Origin）。进程内并发安全，写盘原子替换。
type Registry struct {
	mu       sync.Mutex
	path     string
	accounts map[string]Origin
	// modTime/size 是本实例「上次读到的落盘形态」，用于跨实例变更检测：
	// /admin/sources 与导入路径各自 LoadRegistry 出独立实例（导入时写盘），而
	// 网关选号侧只 LoadRegistry 一次并长期持有——不重读就会出现「导入成功了、
	// /status 里 producer 还是旧的」。Get/All/Counts 前按需重读（见 refreshLocked）。
	modTime time.Time
	size    int64
	// checkedAt 上次 stat 的时刻：热路径（选号/粘性）每个账号一次 Get，不加节流
	// 就是每请求 N 次 stat。1s 节流下导入结果的可见延迟上限 1 秒，代价可忽略。
	checkedAt time.Time
}

// RegistryPath 台账文件路径。
func RegistryPath(authDir string) string { return filepath.Join(authDir, ".origins.json") }

// LoadRegistry 读台账。文件缺失/损坏一律回落空台账：来源只是标注，
// 读不出来不该把管理页拖垮。
func LoadRegistry(authDir string) *Registry {
	r := &Registry{path: RegistryPath(authDir), accounts: map[string]Origin{}}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		return r
	}
	var f struct {
		Accounts map[string]Origin `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || f.Accounts == nil {
		return r
	}
	r.accounts = f.Accounts
	r.rememberStat()
	return r
}

// rememberStat 记下当前落盘形态（mtime + size），供 refreshLocked 比对。
func (r *Registry) rememberStat() {
	fi, err := os.Stat(r.path)
	if err != nil {
		return
	}
	r.modTime, r.size = fi.ModTime(), fi.Size()
}

// refreshLocked 落盘形态变了就重读。调用方必须持 r.mu。
// 变更来源是**别的实例**：导入路径 LoadRegistry 出自己的实例、Mark 后写盘，
// 本实例（网关选号侧长期持有的那个）靠这里跟上。文件缺失/解析失败保留现状——
// 台账只是标注，读不出来不该把调用方拖垮。
func (r *Registry) refreshLocked() {
	now := time.Now()
	if now.Sub(r.checkedAt) < time.Second {
		return
	}
	r.checkedAt = now
	fi, err := os.Stat(r.path)
	if err != nil {
		return
	}
	if fi.ModTime().Equal(r.modTime) && fi.Size() == r.size {
		return
	}
	raw, rerr := os.ReadFile(r.path)
	if rerr != nil {
		return
	}
	var f struct {
		Accounts map[string]Origin `json:"accounts"`
	}
	if jerr := json.Unmarshal(raw, &f); jerr != nil || f.Accounts == nil {
		return
	}
	r.accounts = f.Accounts
	r.modTime, r.size = fi.ModTime(), fi.Size()
}

// Path 台账文件路径（回显用）。
func (r *Registry) Path() string { return r.path }

// Get 取一条来源归属。
func (r *Registry) Get(uid string) (Origin, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	o, ok := r.accounts[uid]
	return o, ok
}

// All 返回台账副本（外部随便改，不影响内部状态）。
func (r *Registry) All() map[string]Origin {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	out := make(map[string]Origin, len(r.accounts))
	for k, v := range r.accounts {
		out[k] = v
	}
	return out
}

// Counts 按来源方式计数（三种来源各有多少号）。
func (r *Registry) Counts() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	out := map[string]int{MethodApp: 0, MethodSwitch: 0, MethodFile: 0}
	for _, o := range r.accounts {
		if o.Method == "" {
			continue
		}
		out[o.Method]++
	}
	return out
}

// Mark 记一批来源归属并落盘。key 是 uid，value 是该账号的来源；
// AddedAt 为零值时自动填当前时刻。重复导入同一 uid 视为来源更新（覆盖）。
func (r *Registry) Mark(byUID map[string]Origin) error {
	if len(byUID) == 0 {
		return nil
	}
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for uid, o := range byUID {
		if uid == "" {
			continue
		}
		if o.AddedAt == 0 {
			o.AddedAt = now
		}
		r.accounts[uid] = o
	}
	return r.saveLocked()
}

// Prune 删掉不在 keep 里的 uid（凭证文件被删后台账不留孤儿）。
func (r *Registry) Prune(keep []string) error {
	alive := make(map[string]bool, len(keep))
	for _, uid := range keep {
		alive[uid] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for uid := range r.accounts {
		if !alive[uid] {
			delete(r.accounts, uid)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return r.saveLocked()
}

// saveLocked 原子落盘（tmp + rename，权限 0600）。
// encoding/json 对 map 键天然按字典序输出，落盘 diff 可读，不必额外排序。
func (r *Registry) saveLocked() error {
	body := struct {
		Version  int               `json:"version"`
		Accounts map[string]Origin `json:"accounts"`
	}{Version: 1, Accounts: r.accounts}
	buf, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, r.path); err != nil {
		return err
	}
	r.rememberStat()
	return nil
}

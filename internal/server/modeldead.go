package server

// 模型可服务性负缓存（池级 dead-marking）。
//
// 背景：上游目录（/models）是「厂商全家桶」，不等于号池里的号真能调——实测 72 个
// 目录模型里 20 个打不通（上游下架的老款 / 账号套餐够不着的昂贵档 / 非对话模型）。
// 目录若只跟上游走，客户端（Codex/ZCode）就会选到必失败的模型，换号重试也救不了
// （错误语义是「模型不在后端」，不是账号故障）。所以目录里「列什么」除了发布清单
// （用户显式意图）之外，还要过一道池级可服务性判定。
//
// 判定来源（自动）：请求在号池轮转耗尽后，末态上游错误为 ErrModelBlocked
// （11102「该后端无此模型」/ 1211「模型不存在」——权威性答复，语义是「模型不在
// 后端」）→ 池级标记 realm/producer/裸名 不可用（按裸名标记：倍率后缀只是计费档，
// 模型本身不在，整个裸名下的变体一起剔除）。
// 参数类错误（11133 model_param_invalid）可能是请求侧问题（带图/历史形态等，
// 见 responses 翻译层），不自动标记——宁可多列，不误杀。
//
// 复活：TTL 到期自动回到目录与路由（上游可能恢复供给、新套餐号入池）；期间首个
// 请求若仍打不通会在轮转耗尽后再次标记。收敛代价：每个死模型每天至多一次失败请求。
// 持久化：data/modeldead.json（路径由 AuthDir 同级 data/ 派生，与 state.json 同目录），
// 重启不丢；/admin/modeldead 可查看，DELETE 可手动清除（用户判定标记有误时）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// modelDeadTTL 死标记存活时长。到期不续期：复活后是否再次死亡由真实请求决定。
const modelDeadTTL = 24 * time.Hour

// modelDeadEntry 一条死标记。Until 为过期时刻（此后视为可用）；Reason 记录末态
// 上游错误摘要，供 /admin/modeldead 排查。
type modelDeadEntry struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

// modelDeadList 池级死模型集合。锁内完成读改写与落盘（文件很小、写频率极低：
// 只在标记/清除时写，读路径纯内存）。
type modelDeadList struct {
	mu      sync.Mutex
	path    string
	entries map[string]modelDeadEntry
}

// newModelDeadList 从 path 加载（无文件/损坏 → 空表，不报错：负缓存是优化不是事实源）。
// 单条坏记录（如 until 时间格式不对）只跳过该条，不拖垮整表——这个文件允许手工编辑。
func newModelDeadList(path string) *modelDeadList {
	m := &modelDeadList{path: path, entries: map[string]modelDeadEntry{}}
	raw, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	if err := json.Unmarshal(raw, &m.entries); err == nil {
		return m
	}
	var loose map[string]map[string]any
	if err := json.Unmarshal(raw, &loose); err != nil {
		return m
	}
	for k, v := range loose {
		e := modelDeadEntry{}
		if s, ok := v["until"].(string); ok {
			if t, perr := time.Parse(time.RFC3339, s); perr == nil {
				e.Until = t
			}
		}
		if s, ok := v["reason"].(string); ok {
			e.Reason = s
		}
		if !e.Until.IsZero() {
			m.entries[k] = e
		}
	}
	return m
}

// deadKey 归一化键：producer 空串归一为 workbuddy（与路由层口径一致——空串在网关
// 里就是「CN 默认那家」）。分隔符用 "/"：模型裸名不含 "/"（realm/producer 是枚举值）。
func deadKey(realm, producer, bare string) string {
	if producer == "" {
		producer = "workbuddy"
	}
	return realm + "/" + producer + "/" + bare
}

// mark 标记一个模型不可用，TTL 从现在起算；重复标记刷新 Until（取最新观测）。
// 落盘失败只降级为进程内标记（下次启动重新收敛），不向调用方报错——调用方在
// 错误透传路径上，没有更好的处理办法。
func (m *modelDeadList) mark(realm, producer, bare, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries[deadKey(realm, producer, bare)] = modelDeadEntry{
		Until:  time.Now().Add(modelDeadTTL),
		Reason: reason,
	}
	m.pruneAndSaveLocked()
}

// active 该模型当前是否处于死标记期（含 TTL 判定；过期条目惰性清除）。
func (m *modelDeadList) active(realm, producer, bare string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[deadKey(realm, producer, bare)]
	if !ok {
		return false
	}
	if time.Now().After(e.Until) {
		delete(m.entries, deadKey(realm, producer, bare))
		m.pruneAndSaveLocked()
		return false
	}
	return true
}

// snapshot 管理端视图：键 + 条目（含已过期未清理的，调用方可感知全量）。
func (m *modelDeadList) snapshot() map[string]modelDeadEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]modelDeadEntry, len(m.entries))
	for k, v := range m.entries {
		out[k] = v
	}
	return out
}

// clear 手动清除全部标记（用户判定误杀时）；返回清除条数。
func (m *modelDeadList) clear() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.entries)
	m.entries = map[string]modelDeadEntry{}
	m.pruneAndSaveLocked()
	return n
}

// pruneAndSaveLocked 清过期条目后落盘。调用方须持锁。
func (m *modelDeadList) pruneAndSaveLocked() {
	now := time.Now()
	for k, e := range m.entries {
		if now.After(e.Until) {
			delete(m.entries, k)
		}
	}
	if len(m.entries) == 0 {
		// 空表不落盘也不强删旧文件：留着无碍，删了反而多一次失败面。
		return
	}
	if raw, err := json.MarshalIndent(m.entries, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(m.path), 0o755)
		_ = os.WriteFile(m.path, raw, 0o644)
	}
}

// adminModelDeadList GET /admin/modeldead：死标记全量视图（键 → {until, reason}）。
func (h *Handler) adminModelDeadList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"entries": h.modelDead.snapshot()})
}

// adminModelDeadClear DELETE /admin/modeldead：手动清除全部死标记（误杀兜底）。
func (h *Handler) adminModelDeadClear(w http.ResponseWriter, r *http.Request) {
	n := h.modelDead.clear()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": n})
}

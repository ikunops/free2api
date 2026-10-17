// model_catalog.go context_length / max_output_tokens 四级查找链 + model.json
// 本地缓存（任务书 model-json-dynamic）。
//
// 查找链（32a3c13 三级 → 四级，上游动态值永远权威不变）：
//  1. 上游动态值（ModelInfo.ContextWindow/MaxTokens）——权威，永远压过 model.json
//     （即使后者更新：上游才是权威，任务书 §清理）；
//  2. 静态种子表（context_catalog.go 的 contextCapFallback，编译期兜底）；
//  3. model.json 本地缓存（数据目录，含运行时从 models.dev 补的值 + 仓库种子
//     embed 的初值）；损坏 → 降级种子并 WARN 不崩溃（手动维护入口的容错）；
//  4. 触发 models.dev 按需拉取（异步，不阻塞本次响应）→ 值写入 model.json →
//     本次先落 1M/省略，下次命中缓存；负缓存 24h。
//
// model.json 读写并发安全（单 sync.Mutex 全程持锁 + 原子落盘 tmp+rename，
// 多请求同时 miss 同一模型只写一次）；文件损坏/不可写均静默降级（种子表→1M）。
//
// 状态归属：包级 catalogState 单例（与 modelsDev fetcher 同模式，进程一份）。
// 测试用 resetModelCatalog / loadModelCatalogAt 隔离。
package upstream

import (
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// modelSeedFS 仓库种子版 model.json（context_catalog 静态表 27 值迁移，source=seed）。
// 只作 model.json 缺失时的初值来源；运行目录的 model.json 一旦存在则以它为准
// （用户手动维护入口——直接编辑文件，格式容错由 loadModelCatalog 的校验兜底）。
//
//go:embed model.json
var modelSeedFS embed.FS

// ModelCapEntry model.json 单条目（与静态表同字段口径 + 来源与抓取时间）。
// Source：seed（仓库种子迁移）/ modelsdev（运行时按需拉取）/ manual（用户手编
// ——无法区分手编与 seed，手编条目保留其原 source 字符串，语义等同「非拉取」）。
// Context 必须 >0（零/负条目校验拒绝）；MaxOutput 0 = 输出上限未知 → 省略字段。
type ModelCapEntry struct {
	ContextLength   int64  `json:"context_length"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
	FetchedAt       string `json:"fetched_at,omitempty"` // RFC3339；seed 条目为空
	Source          string `json:"source"`
	Attachment      bool   `json:"attachment,omitempty"`
	Reasoning       bool   `json:"reasoning,omitempty"`
	ToolCall        bool   `json:"tool_call,omitempty"`
}

// ModelCatalogTTL models.dev 补值的最长可信时间。旧缓存无 TTL 会让一次脏值
// 永久压住后续探测；24h 后重新从当次文档取值/重新回填。
const ModelCatalogTTL = 24 * time.Hour

// modelCatalog model.json 缓存状态机（包级 catalogState 单例的字段载体）。
type modelCatalog struct {
	mu sync.Mutex

	path    string // 落盘路径（空 = 禁用持久化：纯内存 + 种子）
	entries map[string]ModelCapEntry
	loaded  bool // entries 已初始化（含损坏降级形态）
}

// catalogState 包级单例：全进程一份 model.json 状态。
var catalogState = &modelCatalog{}

// initModelCatalogLocked 确保 entries 已加载（持锁调用）：
//   - 运行目录 model.json 存在且合法 → 全量加载（校验失败的条目剔除并 WARN）；
//   - 文件不存在 / 整体损坏（非法 JSON）→ 种子 embed 初值（不落盘：保持「用户
//     尚无缓存」状态，首次拉取成功后再落盘）；
//   - path 为空（未接线，如测试/工具进程）→ 种子初值。
func (c *modelCatalog) initLocked() {
	if c.loaded {
		return
	}
	c.entries = map[string]ModelCapEntry{}
	c.loaded = true
	if c.path == "" {
		c.loadSeedLocked()
		return
	}
	raw, err := os.ReadFile(c.path)
	if err != nil {
		// 不存在 / 不可读 → 种子（首次启动形态）。
		c.loadSeedLocked()
		return
	}
	var file map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &file); err != nil {
		// 整体损坏：降级种子 + WARN 不崩溃（任务书 §手动维护入口容错）。
		log.Printf("WARN: [upstream] model.json 损坏（降级内置种子表）: path=%s err=%v", c.path, err)
		c.loadSeedLocked()
		return
	}
	for id, e := range file {
		if !validCapEntry(e) {
			log.Printf("WARN: [upstream] model.json 条目非法剔除: model=%s entry=%+v", id, e)
			continue
		}
		c.entries[id] = e
	}
}

// loadSeedLocked 把仓库种子 model.json 灌入 entries（持锁调用）。
func (c *modelCatalog) loadSeedLocked() {
	raw, err := modelSeedFS.ReadFile("model.json")
	if err != nil {
		// embed 编译期保证存在，理论不可达；防御性兜底走静态表（initLocked 调用方
		// 查找链第 2 级本来就会兜，这里只需保持 entries 为空）。
		return
	}
	var seed map[string]ModelCapEntry
	if err := json.Unmarshal(raw, &seed); err != nil {
		return
	}
	for id, e := range seed {
		if validCapEntry(e) {
			c.entries[id] = e
		}
	}
}

// validCapEntry 条目级校验：context 正数 + 输出非负（任务书 §值校验的写入侧）。
func validCapEntry(e ModelCapEntry) bool {
	return e.ContextLength > 0 && e.MaxOutputTokens >= 0
}

func capEntryFresh(e ModelCapEntry) bool {
	if e.Source != "modelsdev" {
		return true // seed/manual 没有抓取时间，保持永久有效
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(e.FetchedAt))
	if err != nil {
		return false
	}
	return time.Since(t) < ModelCatalogTTL
}

// get 查 model.json 缓存（第 3 级）。返回条目与是否命中。
// 只读内存，不发网络；加载与网络动作由 ensure/触发侧负责。
func (c *modelCatalog) get(model string) (ModelCapEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	e, ok := c.entries[model]
	if !ok || !validCapEntry(e) || !capEntryFresh(e) {
		return ModelCapEntry{}, false
	}
	return e, true
}

// getFor 渠道感知读取：devBase 已知时只读精确键，绝不回落裸 model 键。
// 裸键是同名模型跨渠道的历史污染点（opencode 免费层 200K 被聚合网关 1M 覆盖）；
// 只有调用方本来就不知道渠道（devBase 为空）时才使用裸键。
func (c *modelCatalog) getFor(devBase, model string) (ModelCapEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	for _, key := range capKeys(devBase, model) {
		if e, ok := c.entries[key]; ok && validCapEntry(e) && capEntryFresh(e) {
			return e, true
		}
	}
	return ModelCapEntry{}, false
}

func capKeys(devBase, model string) []string {
	if strings.TrimSpace(devBase) == "" {
		return []string{model}
	}
	return []string{model + "@" + devBaseKey(devBase)}
}

// put 写一条缓存（第 4 级拉取成功后调用）并落盘。同模型已存在（用户手动维护过）
// 仍覆盖——运行时拉取值采信 models.dev 官方源优先口径，比手编更可信；
// 若不希望覆盖，删掉 model.json 里对应条目即可（加载后手编值只在本次进程生效）。
func (c *modelCatalog) put(model string, e ModelCapEntry) {
	if model == "" || !validCapEntry(e) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	c.entries[model] = e
	c.saveLocked()
}

// invalidateExact 让某个渠道的精确缓存立即失效（不是“等 24h 后自然过期”）。
// 管理面点击“刷新”时用：上游刚恢复/刚改了窗口，用户期望这一轮就看到新值。
// 只删除该渠道键；裸键与其他渠道互不影响。返回是否真的删了条目。
func (c *modelCatalog) invalidateExact(devBase, model string) bool {
	if strings.TrimSpace(devBase) == "" || strings.TrimSpace(model) == "" {
		return false
	}
	key := model + "@" + devBaseKey(devBase)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	if _, ok := c.entries[key]; !ok {
		return false
	}
	delete(c.entries, key)
	c.saveLocked()
	return true
}

// InvalidateModelCapForRefresh 管理面强制刷新某个渠道模型的缓存。
// 返回 true 表示确有旧条目被移除；调用方可据此记录刷新动作。
func InvalidateModelCapForRefresh(devBase, model string) bool {
	return catalogState.invalidateExact(devBase, model)
}

// InvalidateModelsDevCapsForRefresh 清掉所有运行时从 models.dev 拉取的缓存条目
// （裸键与精确键都算），保留 seed/manual。管理面强制刷新用：不依赖调用方枚举
// 当前有哪些渠道/模型，后续新增的来源也自然覆盖到。
func InvalidateModelsDevCapsForRefresh() int {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.initLocked()
	n := 0
	for key, e := range c.entries {
		if strings.EqualFold(strings.TrimSpace(e.Source), "modelsdev") {
			delete(c.entries, key)
			n++
		}
	}
	if n > 0 {
		c.saveLocked()
	}
	return n
}

// saveLocked 原子落盘（tmp + rename，pool state.json 同模式）。持锁调用。
// path 为空 / 目录不可写 / 序列化失败 → 静默（内存缓存仍生效，下次进程重拉）。
func (c *modelCatalog) saveLocked() {
	if c.path == "" {
		return
	}
	raw, err := json.MarshalIndent(c.entries, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("WARN: [upstream] model.json 落盘失败（内存缓存仍生效）: path=%s err=%v", c.path, err)
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		log.Printf("WARN: [upstream] model.json 落盘改名失败: path=%s err=%v", c.path, err)
	}
}

// ---- 包级 API（查找链 3/4 级 + 接线）----

// SetModelCatalogPath 接线 model.json 落盘路径（cmd/server 启动时调用，
// 数据目录与 state.json 同风格）。首次调用生效；后续调用在已加载后仅更新路径
// （不重载——进程内以内存 entries 为准）。
func SetModelCatalogPath(path string) {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.path = path
}

// modelCatalogGet 第 3 级：model.json 缓存命中（内含种子初值与损坏降级）。
func modelCatalogGet(model string) (ModelCapEntry, bool) {
	return catalogState.get(model)
}

func modelCatalogGetFor(devBase, model string) (ModelCapEntry, bool) {
	return catalogState.getFor(devBase, model)
}

// modelCatalogPut 第 4 级写入：models.dev 拉到的值落缓存（含落盘）。
func modelCatalogPut(model string, context, maxOutput int64) {
	modelCatalogPutSourced(model, context, maxOutput, "modelsdev")
}

// modelCatalogPutSourced 写入指定来源的条目（测试可注入 fetched_at 检查落盘格式）。
func modelCatalogPutSourced(model string, context, maxOutput int64, source string) {
	catalogState.put(model, ModelCapEntry{
		ContextLength:   context,
		MaxOutputTokens: maxOutput,
		FetchedAt:       time.Now().UTC().Format(time.RFC3339),
		Source:          source,
	})
}

func modelCatalogPutFor(devBase, model string, context, maxOutput int64) {
	modelCatalogPutEntry(devBase, model, ModelCapEntry{ContextLength: context, MaxOutputTokens: maxOutput, Source: "modelsdev", FetchedAt: time.Now().UTC().Format(time.RFC3339)})
}

func modelCatalogPutEntry(devBase, model string, e ModelCapEntry) {
	if strings.TrimSpace(e.Source) == "" {
		e.Source = "modelsdev"
	}
	if strings.TrimSpace(e.FetchedAt) == "" {
		e.FetchedAt = time.Now().UTC().Format(time.RFC3339)
	}
	key := model
	if strings.TrimSpace(devBase) != "" {
		key = model + "@" + devBaseKey(devBase)
	}
	catalogState.put(key, e)
}

// resetModelCatalog 测试隔离：清空单例状态（entries/path/loaded）。
func resetModelCatalog() {
	c := catalogState
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = nil
	c.path = ""
	c.loaded = false
}

// loadModelCatalogAt 测试接线：指向指定路径后立即触发一次加载（同步，可断言文件
// 解析行为）。生产路径用 SetModelCatalogPath（惰性首次 get 触发加载）。
func loadModelCatalogAt(path string) {
	SetModelCatalogPath(path)
	catalogState.mu.Lock()
	defer catalogState.mu.Unlock()
	catalogState.initLocked()
}

// ResetLookupChainForTest 跨包测试钩子：清空 model.json 缓存与 models.dev fetcher
// 的全部包级单例状态（含在途拉取冷却——防 server 包测试末尾的异步 goroutine 打
// 真网、防跨测试缓存污染）。仅测试引用（upstream 包内用 resetModelsDev /
// resetModelCatalog 等价内联）。
func ResetLookupChainForTest() {
	resetModelCatalog()
	resetModelsDev()
}

// ---- 四级查找链（对 handler 暴露的入口，签名与 32a3c13 三级版兼容）----

// ContextWindowListingV4 四级查找链的 context_length 决策：
//  1. remote>0 权威透出（上游动态值永远压过 model.json，任务书 §清理）；
//  2. 静态种子表（contextCapFallback）；
//  3. model.json 缓存（内含种子初值 / models.dev 运行时补充值）；
//  4. 全链 miss 且非负缓存 → 异步触发 models.dev 拉取（本次返回 DefaultContextWindow
//     1M，不阻塞；拉到后写 model.json 供下次命中）。
func ContextWindowListingV4(model string, remote int64, client *http.Client) int64 {
	return ContextWindowListingV4For(model, "", remote, client)
}

// ContextWindowListingV4Known 渠道感知查找的公开形态：第二个返回值表示**这条
// 记录是否有可信来源**。渠道已知时未命中 models.dev / 本地精确缓存 → ok=false，
// 调用方应省略 context_length，而不是写一个 1M 的猜测。
func ContextWindowListingV4Known(model, devBase string, remote int64, client *http.Client) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if model == "" {
		return 0, false
	}
	if strings.TrimSpace(devBase) != "" {
		if e, ok := modelCatalogGetFor(devBase, model); ok && e.ContextLength > 0 {
			return e.ContextLength, true
		}
		if !modelsDev.negativeFreshFor(model, devBase) {
			if e, ok := modelsDev.lookup(model, devBase); ok {
				modelCatalogPutEntry(devBase, model, ModelCapEntry{ContextLength: e.Context, MaxOutputTokens: e.Output, Attachment: e.Attachment, Reasoning: e.Reasoning, ToolCall: e.ToolCall})
				return e.Context, true
			}
			modelsDev.ensureDocAsync(client, "")
		}
		// 渠道已识别但精确条目缺失/尚未拉到：这是“未知”，不是 1M 的许可证。
		// 静态表是裸名口径，不能拿来覆盖已知渠道。
		return 0, false
	}
	if cap, ok := contextCapFallback[model]; ok && cap.context > 0 {
		return cap.context, true
	}
	if e, ok := modelCatalogGetFor(devBase, model); ok && e.ContextLength > 0 {
		return e.ContextLength, true
	}
	if !modelsDev.negativeFreshFor(model, devBase) {
		if e, ok := modelsDev.lookup(model, devBase); ok {
			modelCatalogPutEntry(devBase, model, ModelCapEntry{ContextLength: e.Context, MaxOutputTokens: e.Output, Attachment: e.Attachment, Reasoning: e.Reasoning, ToolCall: e.ToolCall})
			return e.Context, true
		}
		modelsDev.ensureDocAsync(client, "")
	}
	return 0, false
}

// ContextWindowListingV4For 是 ContextWindowListingV4 的渠道感知版本：devBase 是
// 模型实际出站的上游 base。调用方必须优先用它——同名模型在免费层/付费层/聚合
// 网关上的 context 上限不同，裸 id 查找只应作为不知道渠道时的兜底。
//
// 渠道已知时顺序：remote > 精确 model.json(models.dev) > 静态表 > 裸缓存/兜底。
// 这是动态数据优先，静态表只补真正没有渠道记录的情况。
func ContextWindowListingV4For(model, devBase string, remote int64, client *http.Client) int64 {
	if ctx, ok := ContextWindowListingV4Known(model, devBase, remote, client); ok {
		return ctx
	}
	return DefaultContextWindow
}

// MaxOutputTokensListingV4 四级查找链的 max_output_tokens 决策（与 context 口径
// 刻意不同：未知 → 省略，无「宁可高估」安全侧）。
func MaxOutputTokensListingV4(model string, remote int64, client *http.Client) (int64, bool) {
	return MaxOutputTokensListingV4For(model, "", remote, client)
}

// MaxOutputTokensListingV4For 渠道感知版，口径同 ContextWindowListingV4For。
func MaxOutputTokensListingV4For(model, devBase string, remote int64, client *http.Client) (int64, bool) {
	if remote > 0 {
		return remote, true
	}
	if model == "" {
		return 0, false
	}
	if strings.TrimSpace(devBase) != "" {
		if e, ok := modelCatalogGetFor(devBase, model); ok && e.MaxOutputTokens > 0 {
			return e.MaxOutputTokens, true
		}
		if !modelsDev.negativeFreshFor(model, devBase) {
			if e, ok := modelsDev.lookup(model, devBase); ok {
				modelCatalogPutEntry(devBase, model, ModelCapEntry{ContextLength: e.Context, MaxOutputTokens: e.Output, Attachment: e.Attachment, Reasoning: e.Reasoning, ToolCall: e.ToolCall})
				return e.Output, e.Output > 0
			}
			modelsDev.ensureDocAsync(client, "")
		}
		// channel known but exact cap missing: max_output must stay unknown too
		return 0, false
	}
	if cap, ok := contextCapFallback[model]; ok && cap.maxOutput > 0 {
		return cap.maxOutput, true // 第 2 级
	}
	if e, ok := modelCatalogGetFor(devBase, model); ok && e.MaxOutputTokens > 0 {
		return e.MaxOutputTokens, true // 第 3 级
	}
	if !modelsDev.negativeFreshFor(model, devBase) {
		// 与 ContextWindowListingV4 同触发：lookup 命中先入缓存再返回
		// （两条查找链并发 miss 同一模型时，第二调用方直接拿到刚写入的值）。
		if e, ok := modelsDev.lookup(model, devBase); ok {
			modelCatalogPutEntry(devBase, model, ModelCapEntry{ContextLength: e.Context, MaxOutputTokens: e.Output, Attachment: e.Attachment, Reasoning: e.Reasoning, ToolCall: e.ToolCall})
			return e.Output, e.Output > 0
		}
		modelsDev.ensureDocAsync(client, "")
	}
	return 0, false // 第 4 级兜底：省略（输出上限不编造）
}

// ---- 第 4 级拉取值的回流（fetchDoc 成功后调用，写 model.json）----
// ModelsDevCapsFor 返回 models.dev 对“当前渠道 + 模型”声明的能力旗标。
// ok=false 表示没有可靠记录（未知），调用方应保持字段省略而不是写 false。
func ModelsDevCapsFor(model, devBase string) (ModelCapEntry, bool) {
	if e, ok := modelsDev.peek(model, devBase); ok {
		return ModelCapEntry{ContextLength: e.Context, MaxOutputTokens: e.Output, Attachment: e.Attachment, Reasoning: e.Reasoning, ToolCall: e.ToolCall}, true
	}
	// 能力也可能已经由上一次 context/output 查询写进 model.json；这里同样按
	// 渠道精确键读取，避免文档尚未加载时常驻能力缺失。
	if e, ok := modelCatalogGetFor(devBase, model); ok {
		return e, true
	}
	return ModelCapEntry{}, false
}

// noteModelsDevMiss 查询未命中 models.dev 索引 → 负缓存已由 lookup 记录。
// 本函数是 lookup + 写缓存的粘合层：fetchDoc 拉到文档后对「曾 miss 过的模型」
// 重查一次并写入 model.json（下次 /v1/models 直接命中第 3 级）。
func (f *modelsDevFetcher) backfillMisses() {
	f.mu.Lock()
	doc := f.doc
	missed := make([]string, 0, len(f.negatives))
	for m := range f.negatives {
		missed = append(missed, m)
	}
	f.mu.Unlock()
	if doc == nil {
		return
	}
	for _, m := range missed {
		// 负缓存 key 形如 "devBase|model"：按触发时记录的渠道精确回流，避免
		// 同一个裸 model 名被另一个 provider 的值污染。
		devBase, model := splitLookupKey(m)
		e, ok := doc.lookup(model, devBase)
		if !ok {
			continue // 仍查不到：负缓存 24h 生效，不写
		}
		modelCatalogPutEntry(devBase, model, ModelCapEntry{
			ContextLength:   e.Context,
			MaxOutputTokens: e.Output,
			Attachment:      e.Attachment,
			Reasoning:       e.Reasoning,
			ToolCall:        e.ToolCall,
			Source:          "modelsdev",
			FetchedAt:       time.Now().UTC().Format(time.RFC3339),
		})
		// 回流成功：清除负缓存条目（该模型已有值，后续走第 3 级缓存，
		// 不再进本清单）。
		f.mu.Lock()
		delete(f.negatives, m)
		f.mu.Unlock()
	}
}

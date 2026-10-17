// modelsdev.go models.dev 按需兜底（context_length 四级查找链的第 4 级）。
//
// 定位：只对「上游动态值缺失 + 精确渠道缓存未命中」的模型查 models.dev，是
// 兜底的兜底——超时短（默认 5s）、失败静默降级：渠道已知时省略 context_length，
// 渠道未知时才用历史 1M 兼容兜底。绝不阻塞 /v1/models 主路径（查找异步化，
// 拉到后写 model.json 供下次命中）。
//
// 数据源（2026-09-16 逆向，结论详见 .claude/reports/model-json-dynamic.md）：
//   - 官方聚合 JSON 端点 https://models.dev/api.json：~4.7MB 单文档、217 provider、
//     免鉴权、Cloudflare 托管静态站；
//   - 无按模型/按 provider 子端点（/z-ai.json 等 302 回 /），「按需」的实现是
//     单次拉全量文档 + 建裸 id 索引（拉一次只发生一次，此后进程内复用索引）；
//   - schema：{ "<provider>": { "models": { "<id>": { "limit": {"context": N,
//     "output": N} } } } }，模型 id 有裸名（glm-5.2）与带命名空间（openai/gpt-5.5）
//     两种形态，均取尾段做索引 key；
//   - 多 provider 同名值会分歧（聚合网关常自报改动）：vendor 官方源（zai/
//     moonshotai/openai/google/deepseek/minimax）优先，其余取众数（共识值）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ModelsDevURL models.dev 官方聚合 JSON 端点（唯一端点，见文件头逆向结论）。
const ModelsDevURL = "https://models.dev/api.json"

// modelsDevTimeout 单次拉取超时：兜底的兜底，不值得等（任务书 §2：如 5s）。
const modelsDevTimeout = 5 * time.Second

// modelsDevFetchCooldown 拉取节流（进程级）：文档是全量聚合体，5min 内不重拉
// （同模型 24h 负缓存之外的整体节流，防短窗反复打 models.dev）。
const modelsDevFetchCooldown = 5 * time.Minute

// modelsDevNegativeTTL 同模型负缓存：查不到的模型 24h 内不重查
// （任务书 §2：如同模型 24h 内不重查，查不到的模型负缓存防反复打）。
const modelsDevNegativeTTL = 24 * time.Hour

// modelsDevNegativesSoftCap 负缓存 map 的软上限：规模超过它时才做一次过期条目淘汰
// 扫描。分批摊销是为避免每次 lookup 都做 O(n) 全扫——第 4 级触发点在 /v1/models 里
// 每个模型各调一次（handler 遍历模型列表逐条 V4），n 大时全扫会被请求数放大成 CPU
// 开销。淘汰只针对 TTL 已过期的条目，故低于上限时不扫也不会让任何有效条目过期失效。
//
// 注意「只扫过期条目」不足以保证有界：TTL 从**首次未命中**起算（见 lookup 的写回
// 判断），若模型名持续出现在 /v1/models 名单里，一个 TTL 窗口内所有条目都会被反复
// 盖章成 fresh，扫描一条也删不掉，规模只增不减。因此超过**硬上限**（两倍软上限）
// 时额外丢弃最旧的条目——负缓存是纯性能优化（miss 时多查一次进程内索引），丢条目
// 只可能让某个模型重新进一次扫描，不改变任何对外语义。
const modelsDevNegativesSoftCap = 1024

// modelsDevNegativesHardCap 负缓存 map 的硬上限：超过即按时间序丢弃最旧条目
// （见 modelsDevNegativesSoftCap 注释）。取两倍软上限，给「一轮 /v1/models 新增」
// 留出余量，正常规模远达不到。
const modelsDevNegativesHardCap = 2 * modelsDevNegativesSoftCap

// modelsDevMaxBody 拉取响应体上限（文档实测 ~4.7MB，留余量；防异常大响应拖死）。
const modelsDevMaxBody = 32 << 20

// modelsDevValueMax 值校验上限：context/output 超过 1e9 视为脏数据拒绝
// （量级上限校验，任务书 §2 值校验；正数下界在 catalog 写入侧兜底）。
const modelsDevValueMax = int64(1e9)

// modelsDevVendorSources 官方 vendor provider 优先名单：models.dev 收录 217 个
// provider，聚合网关（merge-gateway/nano-gpt 等）自报的 limit 常与官方源分歧，
// 采值优先级 = 本名单命中 > 众数共识。
var modelsDevVendorSources = map[string]bool{
	"zai":           true, // Z.AI（glm 家族官方）
	"moonshotai":    true, // Moonshot AI（kimi 家族官方，国际版）
	"moonshotai-cn": true, // Moonshot AI 中国版
	"openai":        true,
	"google":        true,
	"deepseek":      true,
	"minimax":       true,
}

// modelsDevAggregators models.dev 里常见的聚合网关/转售方。它们的 limit 是
// “这个转售方当前愿意放多少”，不是模型本身能力；只在无法识别真实渠道时才允许
// 用它们的裸名共识，且不能因为票数多就盖过官方源。
var modelsDevAggregators = map[string]bool{
	"openrouter":    true,
	"orcarouter":    true,
	"nano-gpt":      true,
	"merge-gateway": true,
	"vivgrid":       true,
	"requesty":      true,
	"cloudflare":    true,
	"vercel":        true,
	"huggingface":   true,
	"together":      true,
	"groq":          true,
	"fireworks-ai":  true,
	"targon":        true,
	"chutes":        true,
	"kluster":       true,
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

// modelsDevEntry models.dev 单模型采值结果（limit + 能力旗标）。
type modelsDevEntry struct {
	Context    int64
	Output     int64
	Attachment bool
	Reasoning  bool
	ToolCall   bool
}

// modelsDevDoc 是 models.dev 解析后的双索引：
//   - byProvider[provider][fullID]：**精确渠道**值（opencode 免费层与付费层因此不会串值）；
//   - byBare[bareID]：跨 provider 的保守共识值，只作为无法识别实际通道时的兜底；
//   - providerAPI[provider]：provider 声明 api，用于按“实际出站 base”动态匹配渠道。
type modelsDevDoc struct {
	byProvider  map[string]map[string]modelsDevEntry
	byBare      map[string]modelsDevEntry
	providerAPI map[string]string
}

// devBaseKey 将实际出站 base 规范化成渠道缓存键：同一 host/path 的大小写、尾斜杠
// 不应制造两份缓存，否则同一渠道会重复探测、重复落 model.json。
func devBaseKey(devBase string) string {
	raw := strings.TrimSpace(devBase)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return strings.TrimRight(raw, "/")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	u.Fragment = ""
	return u.String()
}

// lookup 先按实际出站 base 对齐 models.dev 的 provider 精确条目；无法对齐时再
// 回落裸 id 共识。精确匹配优先是修复“opencode 免费层 200K 被聚合网关 1M 覆盖”
// 的关键：同名并不意味着同渠道、同额度。
func (d *modelsDevDoc) lookup(model, devBase string) (modelsDevEntry, bool) {
	if d == nil {
		return modelsDevEntry{}, false
	}
	id := modelIDTail(model)
	if provider := d.matchProvider(devBase); provider != "" {
		if models := d.byProvider[provider]; models != nil {
			if e, ok := models[model]; ok {
				return e, true
			}
			if e, ok := models[id]; ok {
				return e, true
			}
			for fullID, e := range models {
				if modelIDTail(fullID) == id {
					return e, true
				}
			}
		}
		// 渠道已明确匹配到 models.dev provider，但该 provider 没有这个模型：
		// 不能回落到跨渠道裸名共识。否则 opencode 缺少某模型时会误拿聚合网关
		// （例如 orcarouter）的同名 limit，正是 200K 免费层被 1M 覆盖的根因。
		return modelsDevEntry{}, false
	}
	// devBase 的 host 在文档里出现过，只是路径没有对齐（例如镜像的 /some-prefix）。
	// 这也是“渠道已知”的一种：绝不能把其他 provider 的裸名共识当成它。
	if strings.TrimSpace(devBase) != "" && d.hasProviderFor(devBase) {
		return modelsDevEntry{}, false
	}
	if strings.TrimSpace(devBase) != "" {
		// 只要调用方给了 devBase，就视为“渠道已知”。即使 models.dev 文档里没有
		// 这个 host（新上游/新镜像），也不能回落到跨 provider 的裸名共识——那会把
		// 别家的上下文/能力冒充成这条渠道的事实。未知就省略字段，等上游/目录更新。
		return modelsDevEntry{}, false
	}
	e, ok := d.byBare[id]
	return e, ok
}

// matchProvider 用实际出站 base 的 host + 路径对齐 docs 中的 provider.api。
// host 必须相同；路径按前缀匹配，取最长匹配（opencode-go 的 /go/ 会胜过同一
// host 的 opencode）。base 为空 / 自建镜像 / docs 未加载 → 返回空。
func (d *modelsDevDoc) matchProvider(devBase string) string {
	u, err := url.Parse(strings.TrimSpace(devBase))
	if err != nil || u.Hostname() == "" {
		return ""
	}
	wantHost := strings.ToLower(u.Hostname())
	wantPath := strings.TrimRight(u.EscapedPath(), "/")
	best, bestLen := "", -1
	for provider, api := range d.providerAPI {
		pu, err := url.Parse(api)
		if err != nil || !strings.EqualFold(pu.Hostname(), wantHost) {
			continue
		}
		apiPath := strings.TrimRight(pu.EscapedPath(), "/")
		if wantPath != "" && apiPath != "" && !strings.HasPrefix(wantPath, apiPath) && !strings.HasPrefix(apiPath, wantPath) {
			continue
		}
		n := len(apiPath)
		if n > bestLen {
			best, bestLen = provider, n
		}
	}
	return best
}

// hasProviderFor 判断 devBase 的 host 是否出现在任何 provider.api 中。路径不参与：
// 这只是一道“渠道是否被文档认识”的保守闸门；真正的模型命中仍走 matchProvider 的
// host+最长路径精确规则。
func (d *modelsDevDoc) hasProviderFor(devBase string) bool {
	u, err := url.Parse(strings.TrimSpace(devBase))
	if err != nil || u.Hostname() == "" {
		return false
	}
	wantHost := strings.ToLower(u.Hostname())
	for _, api := range d.providerAPI {
		pu, err := url.Parse(api)
		if err == nil && strings.EqualFold(pu.Hostname(), wantHost) {
			return true
		}
	}
	return false
}

func modelIDTail(id string) string {
	if i := strings.LastIndex(id, "/"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// modelsDevFetcher models.dev 按需拉取器：进程级单例语义（包级变量 modelsDev），
// 拉取节流 + 裸 id 索引缓存 + 同模型负缓存。测试用 resetModelsDev / 独立 base URL
// 注入隔离（newModelsDevForTest）。
type modelsDevFetcher struct {
	mu sync.Mutex

	// doc 文档解析后的双索引（provider 精确值 + 裸 id 共识兜底）。
	// nil = 未拉取；空 map（非 nil）= 拉取过但索引为空（视作失败冷却）。
	doc *modelsDevDoc

	lastFetch time.Time // 最近一次拉取尝试（成功与失败都算，冷却节流）
	fetched   bool      // 是否已拉取过（doc 字段区分成败）

	negatives map[string]time.Time // 查询未命中的模型 → 记录时间（24h 负缓存）
}

// modelsDev 包级拉取器实例（单例：全进程共享一份文档索引与节流状态）。
var modelsDev = &modelsDevFetcher{}

// resetModelsDev 测试隔离：清空单例状态（doc/fetched/lastFetch/negatives）。
func resetModelsDev() {
	modelsDev.mu.Lock()
	modelsDev.doc = nil
	modelsDev.fetched = false
	modelsDev.lastFetch = time.Time{}
	modelsDev.negatives = nil
	modelsDev.mu.Unlock()
}

// ResetModelsDevDocumentCache 管理面强制刷新入口：只清文档与负缓存，不清
// model.json。已写入的 24h 有效缓存仍可复用；超过 TTL 的脏值会自动失效。
//
// Deprecated: 管理面刷新应使用 RefreshModelsDevDocumentNow；先清再异步拉会让
// 刷新响应第一帧短暂失去 context/能力，前端看起来像数据被擦掉。
func ResetModelsDevDocumentCache() {
	modelsDev.mu.Lock()
	modelsDev.doc = nil
	modelsDev.fetched = false
	modelsDev.lastFetch = time.Time{}
	modelsDev.negatives = nil
	modelsDev.mu.Unlock()
}

// RefreshModelsDevDocumentNow 同步拉取并替换 models.dev 文档。成功返回 true；
// 网络/状态码/解析失败返回 false，且保留旧文档，避免管理面刷新把可用能力打空。
// 仅由用户显式刷新调用，常规 /v1/models 仍走 ensureDocAsync，不阻塞主路径。
func RefreshModelsDevDocumentNow(client *http.Client) bool {
	if client == nil {
		return false
	}
	doc, err := fetchModelsDevDoc(client, "")
	if err != nil {
		log.Printf("WARN: [upstream] models.dev sync refresh failed (keeping previous document): %v", err)
		return false
	}
	modelsDev.mu.Lock()
	modelsDev.doc = doc
	modelsDev.fetched = true
	modelsDev.lastFetch = time.Now()
	modelsDev.negatives = make(map[string]time.Time)
	modelsDev.mu.Unlock()
	modelsDev.backfillMisses()
	return true
}

// lookup 查询一个模型的 (context, output, found)：
//   - 索引命中且值合法 → found=true；
//   - 索引未命中（含索引尚未就绪）→ 记入 negatives（既是 24h 负缓存，也是
//     fetchDoc 成功后 backfillMisses 的回流清单——「曾 miss 过的模型」），found=false。
//
// 只读内存索引，不发网络请求；网络动作由 ensureDocAsync（goroutine 内）负责。
// 注意：doc 就绪前的 miss 也记 negatives——backfillMisses 回流时查到即写
// model.json 并清除负缓存条目（freshLookup），查不到的保持 24h 负缓存。
func (f *modelsDevFetcher) lookup(model string, devBase string) (modelsDevEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doc != nil {
		if e, ok := f.doc.lookup(model, devBase); ok {
			return e, true
		}
	}
	key := lookupKey(devBase, model)
	// 未命中（索引在但模型不在，或索引尚未就绪）：记 miss。
	if f.negatives == nil {
		f.negatives = make(map[string]time.Time)
	}
	now := time.Now()
	// 未命中「重复查询」不再刷新记录时刻：TTL 只从**首次未命中**起算。
	// 否则覆盖写等于每次查询都把条目续期——一个持续出现在 /v1/models 名单里的
	// 未知模型（每条 listing 经 ContextWindowListingV4 + MaxOutputTokensListingV4
	// 各查一次，即每请求 2 次写回）其条目永远 fresh，下面的惰性淘汰一条也扫不掉；
	// 又因淘汰只在超软上限时才扫，规模超限后只增不减（进程生命周期内无界增长，
	// 与 #121 同一类泄漏，仅「不再被查询」的那部分被 #121 覆盖）。
	// 续期对行为零影响：negativeFresh 只判是否存在 + within TTL，未过 TTL 的条目
	// 无论是否续期都同样短路第 4 级触发。
	if _, seen := f.negatives[key]; !seen {
		f.negatives[key] = now
	}
	// 惰性淘汰已过期的负缓存条目：TTL 到期后 negativeFresh 本就判 false（等效不存在），
	// 条目继续留着只是内存泄漏——models.dev 永不收录的模型名（model 由客户端任意指定）
	// 查一次就永久驻留，而全库唯一的删除点 backfillMisses 只删「文档里查到」的模型，
	// 永远不会回收这些条目，map 在进程生命周期内无界增长。
	// 仅在规模超软上限时才扫一遍（摊销 O(1)，理由见 modelsDevNegativesSoftCap 注释）。
	if len(f.negatives) > modelsDevNegativesSoftCap {
		for m, t := range f.negatives {
			if now.Sub(t) >= modelsDevNegativeTTL {
				delete(f.negatives, m)
			}
		}
	}
	// 硬上限兜底：TTL 未到期的条目本就无可淘汰（上一轮扫描已删净过期项），若规模
	// 仍超硬上限说明输入模型名太多，按时间序丢弃最旧条目、削回软上限
	// （纯性能优化：负缓存 miss 只会多查一次进程内索引，丢条目无语义影响）。
	if len(f.negatives) > modelsDevNegativesHardCap {
		cut := len(f.negatives) - modelsDevNegativesSoftCap
		oldest := make([]string, 0, len(f.negatives))
		for m := range f.negatives {
			oldest = append(oldest, m)
		}
		sort.Slice(oldest, func(i, j int) bool { return f.negatives[oldest[i]].Before(f.negatives[oldest[j]]) })
		for _, m := range oldest[:cut] {
			delete(f.negatives, m)
		}
	}
	return modelsDevEntry{}, false
}

// peek 只读查询：用于能力补全等旁路场景，命中返回，未命中不写负缓存，
// 避免一次 /admin/output 渲染就把文档未就绪误记为“24h 不存在”。
func (f *modelsDevFetcher) peek(model, devBase string) (modelsDevEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.doc == nil {
		return modelsDevEntry{}, false
	}
	return f.doc.lookup(model, devBase)
}

// negativeFresh 模型是否在负缓存有效期内（供查找链短路第 4 级触发）。
func (f *modelsDevFetcher) negativeFresh(model string) bool {
	return f.negativeFreshFor(model, "")
}

func (f *modelsDevFetcher) negativeFreshFor(model, devBase string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.negatives[lookupKey(devBase, model)]
	return ok && time.Since(t) < modelsDevNegativeTTL
}

// lookupKey 把渠道与模型名绑定成一个负缓存 key。devBase 空时是 "|model"，
// 与旧裸 model 行为等价但不会和真实渠道 key 冲突。
func lookupKey(devBase, model string) string {
	return devBaseKey(devBase) + "|" + model
}

func splitLookupKey(key string) (devBase, model string) {
	if i := strings.IndexByte(key, 124); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

// ensureDocAsync 确保 models.dev 文档索引可用（异步，不阻塞调用方）：
// 已有索引 / 拉取冷却期内 / 已有人在拉（in-flight 去重）→ 直接返回。
// 否则起 goroutine 拉取解析，完成后落 f.doc（失败静默：只刷新 lastFetch 冷却，
// 下次查找仍按未知口径处理，不重试风暴）。
func (f *modelsDevFetcher) ensureDocAsync(client *http.Client, baseOverride string) {
	f.mu.Lock()
	if f.doc != nil {
		f.mu.Unlock()
		return
	}
	if f.fetched && time.Since(f.lastFetch) < modelsDevFetchCooldown {
		// 拉取过（成功或失败）且冷却期内：不再打 models.dev。
		f.mu.Unlock()
		return
	}
	// in-flight 去重：把 fetched/lastFetch 先置为「本次进行中」，
	// 后续并发调用在冷却窗口内直接返回，不重复起拉取。
	f.fetched = true
	f.lastFetch = time.Now()
	f.mu.Unlock()

	go f.fetchDoc(client, baseOverride)
}

// EnsureModelsDevDocument 冷启动首次目录的有界等待入口。
//
// 为什么不能只靠 ensureDocAsync：/v1/models 是客户端第一次接触网关的请求，若
// models.dev 文档尚未就绪，第一条 OpenCode 记录只能省略 context_length/能力，
// 用户看到的就是「第一遍 200K 空白、刷新后才正常」。这里把首次拉取从后台
// fire-and-forget 改成最多等 modelsDevTimeout（5s）的同步动作；已有文档、
// 冷却期失败、nil client 都立即返回，不把后续请求拖慢。调用方只在目录里
// 确实有 DevBase 模型时调用一次。
func EnsureModelsDevDocument(client *http.Client) {
	if client == nil {
		return
	}
	modelsDev.mu.Lock()
	if modelsDev.doc != nil {
		modelsDev.mu.Unlock()
		return
	}
	if modelsDev.fetched && time.Since(modelsDev.lastFetch) < modelsDevFetchCooldown {
		modelsDev.mu.Unlock()
		return
	}
	modelsDev.fetched = true
	modelsDev.lastFetch = time.Now()
	modelsDev.mu.Unlock()

	doc, err := fetchModelsDevDoc(client, "")
	if err != nil {
		log.Printf("WARN: [upstream] models.dev cold-start fetch failed (capability stays unknown): %v", err)
		return
	}
	modelsDev.mu.Lock()
	modelsDev.doc = doc
	modelsDev.lastFetch = time.Now()
	modelsDev.mu.Unlock()
	modelsDev.backfillMisses()
}

// fetchDoc 拉取并解析 models.dev 文档，建裸 id 索引（goroutine 内执行，永不 panic
// 上抛：任何失败只静默冷却）。
// 拒绝 nil client（不回落 http.DefaultClient）：生产调用方恒传非 nil，nil 只意味着
// 测试疏漏——DefaultClient 无超时（挂起隐患）且会打真网（测试污染 + 不确定延迟），
// 静默 WARN + 返回（能力保持未知，不编造 1M）让疏漏显式化。
func (f *modelsDevFetcher) fetchDoc(client *http.Client, baseOverride string) {
	if client == nil {
		log.Printf("WARN: [upstream] models.dev fetch: nil client rejected (no DefaultClient fallback; capability stays unknown)")
		return
	}
	doc, err := fetchModelsDevDoc(client, baseOverride)
	if err != nil {
		log.Printf("WARN: [upstream] models.dev fetch failed (capability stays unknown): %v", err)
		return
	}
	f.mu.Lock()
	f.doc = doc
	f.mu.Unlock()
	// 文档就绪后把「曾 miss 过的模型」回流 model.json（第 4 级 → 第 3 级，
	// 下次 /v1/models 直接命中缓存）。仍查不到的保持负缓存。
	f.backfillMisses()
}

// fetchModelsDevDoc 是拉取与解析的纯函数部分，供异步与同步刷新共用。
func fetchModelsDevDoc(client *http.Client, baseOverride string) (*modelsDevDoc, error) {
	if client == nil {
		return nil, fmt.Errorf("nil client")
	}
	url := ModelsDevURL
	if baseOverride != "" {
		url = baseOverride
	}
	ctx, cancel := context.WithTimeout(context.Background(), modelsDevTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, modelsDevMaxBody))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	doc, err := parseModelsDevDoc(raw)
	if err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	return doc, nil
}

// parseModelsDevDoc 解析 models.dev api.json：{provider:{models:{id:{limit:{context,
// output}}}}} → 裸 id 索引。同名多 provider 采值优先级四级：官方 vendor 源
// （modelsDevVendorSources）> 票数众数 > provider 字典序 > 先出现。
// 第 3 级 provider 字典序是确定性 tie-break：聚合时维护候选的最小 provider 名
// （minProvider，同 doc 稳定的选择器身份），消灭 map 迭代序随机化导致的
// 「同票先到先得」值抖动（同 binary 两次拉取同一文档可能落不同的值进 model.json，
// /v1/models 的 context_length 不可复现）。不引入「值字典序」——那会把
// 「选谁」变成「选什么值」的启发式，语义不如 provider 名干净。
func parseModelsDevDoc(raw []byte) (*modelsDevDoc, error) {
	var doc map[string]struct {
		API    string `json:"api"`
		Models map[string]struct {
			Limit *struct {
				Context int64 `json:"context"`
				Output  int64 `json:"output"`
			} `json:"limit"`
			Attachment bool `json:"attachment"`
			Reasoning  bool `json:"reasoning"`
			ToolCall   bool `json:"tool_call"`
		} `json:"models"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("models.dev doc: %w", err)
	}
	exact := make(map[string]map[string]modelsDevEntry, len(doc))
	apis := make(map[string]string, len(doc))
	providerIsOfficial := make(map[string]bool, len(doc))
	// 同名 id 的候选值收集：vendorOfficial 标记官方源，votes 计众数，
	// minProvider 维护该候选已见的最小 provider 名（tie-break 用）。
	type candidate struct {
		entry       modelsDevEntry
		vendor      bool
		votes       int
		aggKey      string // 去重聚合 key（同值多 provider 只计票不重复存）
		minProvider string
	}
	byModel := map[string][]candidate{}
	for provider, pv := range doc {
		apis[provider] = strings.TrimSpace(pv.API)
		providerIsOfficial[provider] = modelsDevVendorSources[provider] && !modelsDevAggregators[provider]
		exact[provider] = make(map[string]modelsDevEntry, len(pv.Models))
		for fullID, mv := range pv.Models {
			if mv.Limit == nil {
				continue
			}
			id := fullID
			if i := strings.LastIndex(fullID, "/"); i >= 0 {
				id = fullID[i+1:]
			}
			if id == "" {
				continue
			}
			// 值校验（任务书 §2）：正数 + 量级上限，脏值不进索引。
			ctx, out := mv.Limit.Context, mv.Limit.Output
			if ctx <= 0 || ctx > modelsDevValueMax {
				continue
			}
			if out < 0 || out > modelsDevValueMax {
				continue
			}
			key := fmt.Sprintf("%d/%d", ctx, out)
			entry := modelsDevEntry{
				Context: ctx, Output: out,
				Attachment: mv.Attachment, Reasoning: mv.Reasoning, ToolCall: mv.ToolCall,
			}
			exact[provider][fullID] = entry
			exact[provider][id] = entry
			cs := byModel[id]
			dup := false
			for i := range cs {
				if cs[i].aggKey == key {
					// 聚合网关不参与“官方源”判定；但票数仍是裸名共识的
					// 最后兜底，只统计非聚合 provider，避免转售商刷票。
					if !modelsDevAggregators[provider] {
						cs[i].votes++
					}
					if providerIsOfficial[provider] {
						cs[i].vendor = true
					}
					if provider < cs[i].minProvider {
						cs[i].minProvider = provider
					}
					dup = true
					break
				}
			}
			if !dup {
				byModel[id] = append(cs, candidate{
					entry:       entry,
					vendor:      providerIsOfficial[provider],
					votes:       boolToInt(!modelsDevAggregators[provider]),
					aggKey:      key,
					minProvider: provider,
				})
			}
		}
	}
	out := &modelsDevDoc{byProvider: exact, byBare: make(map[string]modelsDevEntry, len(byModel)), providerAPI: apis}
	for id, cs := range byModel {
		best := 0
		for i, c := range cs {
			// 优先级：官方 vendor 源 > 票数众数 > provider 字典序（tie-break 确定性）。
			cur := cs[best]
			better := false
			if c.vendor && !cur.vendor {
				better = true
			} else if c.vendor == cur.vendor && c.votes > cur.votes {
				better = true
			} else if c.vendor == cur.vendor && c.votes == cur.votes && c.minProvider < cur.minProvider {
				better = true
			}
			if better {
				best = i
			}
		}
		out.byBare[id] = cs[best].entry
	}
	return out, nil
}

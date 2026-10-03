// logging.go 请求级表格日志：每个 /v1/chat/completions 请求结束后打印一行到 stdout。
package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"free2api/internal/logfmt"
)

// chatSeq 进程级请求序号。
var chatSeq atomic.Int64

// chatLogEnabled 聊天表格日志总开关。生产恒 true；
// 测试包经 TestMain 置 false 关闭 stdout 噪音，需要断言行输出的测试用 withChatLog 临时开启（R5）。
var chatLogEnabled = true

// chatStat 单个 chat 请求的日志统计；handler 挂 defer，请求出口后落一行。
type chatStat struct {
	start time.Time
	model string
	// producer 本次请求路由到的来源（workbuddy / zcode / …）。/v1/stats 的「按来源」
	// 维度就靠它；空串在聚合侧归一为 workbuddy（与路由层同口径）。
	producer string
	mode     string // "stream" | "sync"
	uid      string // 完整 uid，展示时只取前 8 位
	nick     string // 账号昵称（auth.Auth.Nickname，登录时落盘）；空则只显示 uid8
	ttfb     time.Duration
	toks     int // <0 表示 usage 缺失 → 显示 "-"
	status   int

	// metrics 采集字段（供 /v1/stats 聚合）：全部来自上游 usage，缺失时保持零值
	// 并由 hasUsage 区分「缺观测」与「显式 0」——与成本账本同一纪律。
	hasUsage  bool
	prompt    int
	cacheHit  int
	cacheMiss int
	cacheWr   int
	credit    float64
	hasCredit bool

	logged bool
	// liveModel 本次请求是否已登记进 globalLiveModels（done 时据此销账）。
	// 不看 s.model != "" 判定——那样和登记条件耦合，一改就漏销或多销。
	liveModel bool
}

// ---------- 在飞模型计数 ----------
//
// 「此刻正在跑哪个模型」是概览实时区块要答的问题。账号维度的 in_flight 早就有，
// 但那是「哪个号在忙」，答不了「在跑什么」。这里按模型名做增减计数：
// 请求进 handler +1，done() -1。计数器自带 clamp，异常路径也不会变负。
//
// 零值不可用，必须 newModelCounter()。

type modelCounter struct {
	mu sync.Mutex
	m  map[string]int
}

func newModelCounter() *modelCounter { return &modelCounter{m: map[string]int{}} }

// add delta=+1 登记、-1 销账。计数下限 0。
func (c *modelCounter) add(model string, delta int) {
	if c == nil || model == "" || delta == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]int{}
	}
	n := c.m[model] + delta
	if n <= 0 {
		delete(c.m, model)
		return
	}
	c.m[model] = n
}

// snapshot 返回 model → 在飞数的拷贝（按值传出，调用方随便遍历都不影响计数）。
func (c *modelCounter) snapshot() map[string]int {
	out := map[string]int{}
	if c == nil {
		return out
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if v > 0 {
			out[k] = v
		}
	}
	return out
}

var globalLiveModels = newModelCounter()

// newChatStat 以请求进入 handler 的时刻为起点构造统计对象；toks 默认 -1（usage 缺失）。
func newChatStat(now time.Time, body []byte, stream bool) *chatStat {
	mode := "sync"
	if stream {
		mode = "stream"
	}
	st := &chatStat{start: now, model: parseModelFromBody(body), mode: mode, toks: -1}
	// 在飞模型计数：进入 handler 就登记，done() 时销账。
	// 这是「此刻正在跑哪个模型」的唯一数据源——账号维度的 in_flight 早就有
	// （/status accounts[].in_flight），但没有模型维度，页面答不了这个问题。
	if st.model != "" {
		globalLiveModels.add(st.model, 1)
		st.liveModel = true
	}
	return st
}

// done 幂等落一行表格日志，并把本次请求记入 metrics 聚合（/v1/stats 数据源）。
//
// 单一埋点：流式 / 非流式 / 各类错误路径最终都汇到此处，故 metrics 天然覆盖全路径，
// 不需要在每个 return 前重复记账（重复记账反而会漏分支或双计）。
func (s *chatStat) done() {
	if s.logged {
		return
	}
	s.logged = true
	total := time.Since(s.start)
	// prompt 侧只在有 usage 观测时落盘；缺失写 -1（哨兵），
	// 回填时才能区分「显式 0」与「没观测到」。
	prompt := -1
	if s.hasUsage {
		prompt = s.prompt
	}
	// 销账：与 newChatStat 里的登记配对。模型名可能为空（无 liveModel 标记），
	// 或者请求跨过进程重启（此时计数已随进程清空，多减一次会减成负数——
	// 所以 clamp 到 0，绝不让计数变负）。
	if s.liveModel {
		globalLiveModels.add(s.model, -1)
	}
	logChatRow(s.ttfb, total, s.model, s.mode, s.uid, s.nick, s.status, s.toks, prompt)
	recordChatMetric(s, total)
}

// chatStatsReader 在流式透传时抓取 SSE 末帧的 usage.completion_tokens 精确值，
// 并记录首个 data 帧的 TTFB；原始字节原样返回给下游透传。
// 注意：不做 rune 估算，token 数一律采信上游 usage。
type chatStatsReader struct {
	br        *bufio.Reader
	start     time.Time
	ttfb      time.Duration
	seen      bool // 已见过首个 data 帧（TTFB 只记一次）
	hasUsage  bool // 末帧是否带 usage
	hasCredit bool // 是否出现过带 credit 的 usage（缺失≠0，见 Credit() 注释）
	tokens    int
	credit    float64 // 末帧 usage.credit（本次真实扣费，供成本账本）
	prompt    int     // 末帧 usage.prompt_tokens（与 completion 合计折算单价）
	cacheHit  int     // 末帧 usage.prompt_cache_hit_tokens（供 /v1/stats）
	cacheMiss int     // 末帧 usage.prompt_cache_miss_tokens
	cacheWr   int     // 末帧 usage.prompt_cache_write_tokens
	pend      []byte  // 已读未返回的行缓存
}

// newChatStatsReaderSince 以 since 为 TTFB 计时起点（通常是请求进入 handler 的时刻）。
func newChatStatsReaderSince(r io.Reader, since time.Time) *chatStatsReader {
	return &chatStatsReader{br: bufio.NewReaderSize(r, 64*1024), start: since}
}

// TTFB 返回首个 data 帧到达耗时；无帧时为 0。
func (s *chatStatsReader) TTFB() time.Duration { return s.ttfb }

// Tokens 返回末帧 usage.completion_tokens 与是否缺失；无 usage 时 ok=false。
func (s *chatStatsReader) Tokens() (int, bool) { return s.tokens, s.hasUsage }

// Credit 返回末帧 usage.credit（本次真实扣费）。ok=true 要求 usage 存在**且** credit
// 字段显式出现——字段缺失时 ok=false（缺失≠0：不能把"缺观测"当"0 成本"写入账本，
// 否则收费的号可能被误判 tier0 免费层）。显式 credit:0 仍是合法免费观测（ok=true）。
func (s *chatStatsReader) Credit() (float64, bool) { return s.credit, s.hasUsage && s.hasCredit }

// TotalTokens 返回本次请求总 token 数（prompt + completion），供成本单价折算。
func (s *chatStatsReader) TotalTokens() int { return s.prompt + s.tokens }

// PromptTokens 返回末帧 usage.prompt_tokens（供 /v1/stats 输入侧统计）。
func (s *chatStatsReader) PromptTokens() int { return s.prompt }

// CacheTokens 返回缓存三段计数（命中 / 未命中 / 写入），供 /v1/stats 的命中率聚合。
func (s *chatStatsReader) CacheTokens() (hit, miss, write int) {
	return s.cacheHit, s.cacheMiss, s.cacheWr
}

// parseSSELine 解析一行 "data: {...}"：首帧记 TTFB，含 usage 时采信精确 completion_tokens。
func (s *chatStatsReader) parseSSELine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "data: ") {
		return
	}
	payload := strings.TrimPrefix(line, "data: ")
	if payload == "[DONE]" {
		return
	}
	if !s.seen {
		s.seen = true
		s.ttfb = time.Since(s.start)
	}
	var chunk struct {
		Usage *struct {
			CompletionTokens int      `json:"completion_tokens"`
			PromptTokens     int      `json:"prompt_tokens"`
			Credit           *float64 `json:"credit"` // 指针区分「缺失」与「显式 0」
			// 缓存三段（上游实测字段名，见 /v1/stats 的 cache_* 口径）。
			PromptCacheHitTokens   int `json:"prompt_cache_hit_tokens"`
			PromptCacheMissTokens  int `json:"prompt_cache_miss_tokens"`
			PromptCacheWriteTokens int `json:"prompt_cache_write_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal([]byte(payload), &chunk) != nil || chunk.Usage == nil {
		return
	}
	s.hasUsage = true
	s.tokens = chunk.Usage.CompletionTokens
	s.prompt = chunk.Usage.PromptTokens
	s.cacheHit = chunk.Usage.PromptCacheHitTokens
	s.cacheMiss = chunk.Usage.PromptCacheMissTokens
	s.cacheWr = chunk.Usage.PromptCacheWriteTokens
	if chunk.Usage.Credit != nil {
		s.hasCredit = true
		s.credit = *chunk.Usage.Credit
	}
}

// Read 返回原始数据，同时解析统计 TTFB/token。
func (s *chatStatsReader) Read(p []byte) (int, error) {
	if len(s.pend) > 0 {
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	line, err := s.br.ReadString('\n')
	if line != "" {
		s.parseSSELine(line)
		s.pend = []byte(line)
		n := copy(p, s.pend)
		s.pend = s.pend[n:]
		return n, nil
	}
	return 0, err
}

// parseModelFromBody 从请求 JSON 取 model 字段，缺省标 "-"。
func parseModelFromBody(body []byte) string {
	var obj struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &obj); err != nil || obj.Model == "" {
		return "-"
	}
	return obj.Model
}

// completionTokens 从 Aggregate 返回的响应中提取 usage.completion_tokens；缺失返回 -1。
func completionTokens(resp map[string]any) int {
	u, ok := resp["usage"].(map[string]any)
	if !ok {
		return -1
	}
	v, ok := u["completion_tokens"].(float64)
	if !ok {
		return -1
	}
	return int(v)
}

// usageCreditTotal 从聚合响应提取本次真实扣费与总 token 数（供成本账本）。
// ok=false 表示 usage 缺失或字段类型不符——此时不记录观测，避免污染账本。
func usageCreditTotal(resp map[string]any) (credit float64, total int, ok bool) {
	u, isMap := resp["usage"].(map[string]any)
	if !isMap {
		return 0, 0, false
	}
	c, hasCredit := u["credit"].(float64)
	pt, hasPrompt := u["prompt_tokens"].(float64)
	ct, hasCompletion := u["completion_tokens"].(float64)
	if !hasCredit || (!hasPrompt && !hasCompletion) {
		return 0, 0, false
	}
	return c, int(pt) + int(ct), true
}

// uidPrefix 只显示 uid 前 8 位；空 uid 显示 "-"。
//
// 保留本函数是因为 logging_test.go 直接断言它；实现委托 logfmt.UID8，避免
// "截 8 位" 的规则在 server 与 logfmt 两处各写一份而走样。
func uidPrefix(uid string) string {
	return logfmt.UID8(uid)
}

// 请求流水行的固定列宽（显示列宽，非字节）。取固定宽度而不是让内容自然长度撑开，
// 是为了让 stdout 里成百上千行能竖着扫——否则模型名长短不一、中文昵称按字节补空格
// 错位，根本没法用肉眼对齐着一列列看（这正是上一版 11 字节硬截断要解决的问题）。
const (
	// chatModelWidth 覆盖 realm 前缀 + 最长模型名。取 40 是给「realm: (7) + 上游最长
	// 模型名 (31) = 38」留出余量：目录里 muse-spark-1.2-contributor-free 这类 31 字符
	// 的裸名，加前缀后正好 38。旧的 11 字节截断会把 "cn:deepseek-v4-flash" 切成
	// "cn:deepseek"；26 列也仍会截掉上面那种长名（两个不同的 muse-spark 会合并成
	// 同一行），所以放宽到 40。
	chatModelWidth = 40
	// chatAcctWidth 容纳 "昵称(uid8)"：中文昵称按 2 列/字算，5 字中文 + "(xxxxxxxx)" = 20 列。
	chatAcctWidth = 22
	chatTTFBWidth = 8
	chatTokWidth  = 6
	// chatPromptWidth 输入 token 列：最长见过 8 位（30538665），留 9 列余量。
	chatPromptWidth = 9
	chatRateWidth   = 11 // 形如 "183.6tok/s"
)

// logChatRow 打印一行请求级表格日志（直接输出 stdout，无 log 时间戳前缀）。
//
// 参数：
//   - model：模型名（含 realm 前缀），超 chatModelWidth 截断（模型名是 ASCII，字节截即列宽）。
//     截断会合并不同模型（回填统计时尤其致命），故宽度按「realm 前缀 + 最长目录名」取足；
//   - uid/nick：完整 uid 与账号昵称，经 logfmt.Label 拼成 "昵称(uid8)" 展示——只有
//     uid8 时人眼无法判断是哪个号，要辨认必须再查 auths/，排障多一跳；
//   - toks<0 表示 usage 缺失，显示 "-"。
//   - prompt<0 表示这行没有输入 token 观测（旧格式流水行），显示 "-"。
func logChatRow(ttfb, total time.Duration, model, mode, uid, nick string, status int, toks, prompt int) {
	if !chatLogEnabled {
		return
	}
	seq := chatSeq.Add(1)
	model = logfmt.Pad(logfmt.Truncate(model, chatModelWidth), chatModelWidth)
	// 账号标签只补不截：超宽时宁可让该行变宽，也不丢昵称信息（昵称是排查的主线索）。
	acct := logfmt.Pad(logfmt.Label(uid, nick), chatAcctWidth)
	tokField := "-"
	tokpsField := "-"
	if toks >= 0 {
		tokField = fmt.Sprintf("%d", toks)
		if total > 0 {
			tokpsField = fmt.Sprintf("%.1ftok/s", float64(toks)/total.Seconds())
		} else {
			tokpsField = "0.0tok/s"
		}
	}
	// prompt 侧：<0 是「没观测到 usage」的哨兵，回填时靠它区分缺失与显式 0。
	promptField := "-"
	if prompt >= 0 {
		promptField = fmt.Sprintf("%d", prompt)
	}
	ttfbMS := "-"
	if ttfb > 0 {
		ttfbMS = fmt.Sprintf("%dms", ttfb.Milliseconds())
	}
	fmt.Fprintf(os.Stdout, "| #%03d | %s | %s | %s | %d | %s | TTFB=%s | tok=%s | ptok=%s | %s | total=%.1fs |\n",
		seq,
		time.Now().Format("15:04:05"),
		model,
		mode,
		status,
		acct,
		logfmt.Pad(ttfbMS, chatTTFBWidth),
		logfmt.Pad(tokField, chatTokWidth),
		logfmt.Pad(promptField, chatPromptWidth),
		logfmt.Pad(tokpsField, chatRateWidth),
		total.Seconds(),
	)
}

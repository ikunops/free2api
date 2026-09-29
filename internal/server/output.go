// output.go 「输出」侧配置与端点：网关对外那层 API 长什么样，由这里决定。
//
// 与 config.json 的分工：config.json 只放**启动期**的东西（监听地址、账号目录、上游 UA…）；
// 输出侧是运行期可调项（模型前缀、费率提示），单独存 <state 同目录>/output.json，由本包
// 独占读写。好处有二：改前缀/费率不用重启；不必去重写用户的 config.json（重写会把键序
// 打乱、把注释过的痕迹抹掉，属于无谓破坏）。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// 已跑通的输出协议。anthropic / gemini 仍在 /admin/output 里**列出来但不给选**：
// 列出来是为了让「还缺什么」写在明处，不给选是因为没实现的开关点了只会骗人。
const (
	// FormatOpenAI OpenAI Chat Completions（/v1/chat/completions，SSE 流式）。
	FormatOpenAI = "openai"
	// FormatResponses OpenAI Responses（/v1/responses）。Codex CLI 的
	// wire_api="responses" 就落在这里；实现见 internal/responses（请求/响应/事件流翻译）。
	FormatResponses = "responses"
)

// formatSupported 该输出格式是否已实现（PUT 校验与 UI 可选项共用同一判据）。
func formatSupported(f string) bool {
	switch f {
	case FormatOpenAI, FormatResponses:
		return true
	}
	return false
}

// 费率提示档位：决定对外**模型名尾巴**上带什么费率后缀（不是描述前缀）。
//   - credit（默认）：按模型自身的倍率原文逐个加——x0.05 → "-x0.05"，x0.00 → "-free"，
//     上游没给倍率 → 不加。客户端在模型选择器里看名字就知道免费还是扣积分。
//   - free：忽略真实倍率，所有模型统一加 "-free"（整批号都走免费额度时用）。
//   - off：不加后缀。
const (
	RateCredit = "credit" // 逐模型后缀：-x0.05 / -free
	RateFree   = "free"   // 统一后缀：-free
	RateOff    = "off"    // 不加后缀
)

// 后缀的两个可辨识形状：免费是 "-free"，收费是 "-x<数字>"（与上游 "x0.05" 写法一致）。
const (
	rateFreeSuffix = "-free"
	rateCreditMark = "-x"
)

// RateSuffix 依费率档位 + 模型自身倍率原文算出模型名后缀。上游倍率格式不统一
// （"x0.05 credits" / "x0.29" / 空），统一成 "-x0.05" 这种形态；零倍率当免费。
//
// 这是 modelIDFor（出站拼名）与 StripRateSuffix（入站剥名）共用的唯一口径，
// 两侧必须成对改，否则客户端拿到的名字回传时路由不到。
func RateSuffix(rateHint, credits string) string {
	switch rateHint {
	case RateOff:
		return ""
	case RateFree:
		return rateFreeSuffix
	default:
		v := creditMultiplier(credits)
		if v == "" {
			return ""
		}
		if creditIsZero(v) {
			return rateFreeSuffix
		}
		return rateCreditMark + strings.TrimPrefix(v, "x")
	}
}

// creditMultiplier 从上游 credits 原文提取倍率，统一成 "x0.05" 形态；提取不到返回 ""。
// 认得的形态："x0.05 credits" / "x0.29" / "0.29"（没带 x 的补上）。
func creditMultiplier(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "credits")
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.HasPrefix(s, "x") {
		if !isCreditNumber(s) {
			return ""
		}
		return "x" + s
	}
	if !isCreditNumber(s[1:]) {
		return ""
	}
	return s
}

// creditIsZero 倍率是否为零（x0 / x0.00 → 免费）。
func creditIsZero(v string) bool {
	n, err := strconv.ParseFloat(strings.TrimPrefix(v, "x"), 64)
	return err == nil && n == 0
}

// isCreditNumber 纯数字倍率体（"0.05" / "0.29" / "0.00"）：至少一位数字，最多一个小数点。
func isCreditNumber(s string) bool {
	if s == "" {
		return false
	}
	dot := false
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r == '.':
			if dot {
				return false
			}
			dot = true
		default:
			return false
		}
	}
	return true
}

// StripRateSuffix 去掉 RateSuffix 加上的尾巴。入站解析必须在「裸名比对/路由」之前
// 调用，否则带后缀的名字会被当成裸名整串透传给上游。只认两种形状：结尾 "-free"，
// 或结尾 "-x<数字>"；其余原样返回（不做模糊猜测，避免误剥真实模型名的尾巴）。
func StripRateSuffix(id string) string {
	if s, ok := strings.CutSuffix(id, rateFreeSuffix); ok && s != "" {
		return s
	}
	i := strings.LastIndex(id, rateCreditMark)
	if i <= 0 {
		return id
	}
	if !isCreditNumber(id[i+len(rateCreditMark):]) {
		return id
	}
	return id[:i]
}

// OutputConfig 「输出」侧配置。
type OutputConfig struct {
	Format      string `json:"format"`       // openai（其余协议待接入）
	ModelPrefix string `json:"model_prefix"` // 模型前缀：便于客户端区分模型来自哪个网关
	RateHint    string `json:"rate_hint"`    // credit / free / off
	// Models 对外发布的模型白名单（**裸 id**，不含 realm 前缀与输出前缀）。
	// 空 = 全放（与接入本功能之前一致）。这是「输出侧等用户来生成」的落点：
	// 账号能跑什么由上游决定（账号携带），网关对外吐什么由用户勾选决定，两者不混。
	Models []string `json:"models"`
	// ModelsRealmScoped 发布清单是否「按域分开控制」：true = CN 版与国际版（wba）各用一把
	// 开关（国际版在清单里的键是 "global:<裸 id>"，裸 id 只管 CN）；false/缺省 = 旧清单
	// 语义，裸 id 同时管两域（加国际版分组之前的存档，见 publishAllows）。新面板保存时置 true。
	ModelsRealmScoped bool `json:"models_realm_scoped,omitempty"`
	// Channels 额外的输出通道：每个通道一个自己的监听端口 + 自己的输出参数 +
	// 自己的客户端范围。空 = 只有主口这一个出口（默认，与接入前完全一致）。
	// 主口本身不在这里：它的监听地址在 config.json 的 listen，改动需要重启。
	Channels []OutputChannel `json:"channels,omitempty"`
}

// OutputChannel 一个额外出口。「一个出口服务哪些客户端」是用户的选择：
//   - 全放（Producers 空）= 一个口服务全部来源，内部按模型名自动路由；
//   - 指定 producers（如 ["zcode"]）= 这个口只服务这些客户端，别的模型的请求直接 404。
type OutputChannel struct {
	// ID 通道标识（小写字母数字与 -_），配置与运行状态都用它对齐。
	ID string `json:"id"`
	// Name 展示名（如「ZCode 出口」），空则用 ID。
	Name string `json:"name,omitempty"`
	// Listen 该通道的监听地址（形如 127.0.0.1:7870；只写端口按 :port 处理）。
	Listen string `json:"listen"`
	// Format 输出格式（openai / responses 可用，其余列出但不可选）。
	Format string `json:"format,omitempty"`
	// ModelPrefix / RateHint / Models 语义与主口同名键完全一致，只是作用在本通道。
	ModelPrefix string   `json:"model_prefix,omitempty"`
	RateHint    string   `json:"rate_hint,omitempty"`
	Producers   []string `json:"producers,omitempty"`
	Models      []string `json:"models,omitempty"`
}

// maxOutputModels 白名单长度上限：防手写 PUT 塞进超大数组把 output.json 撑爆。
const maxOutputModels = 4096

// normalizeModelList 归一化模型白名单：去空白、去重、排序。空/全空白 → nil（= 全放）。
// 不做「滤掉当前不存在的模型」：上游拉取失败时模型表为空，按存在性过滤会把用户
// 勾好的清单清空——宁可选了但暂时没有，也绝不静默丢选择。
func normalizeModelList(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
		if len(out) >= maxOutputModels {
			break
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// sameModelList 两个已归一化（排序 + 去重）的模型清单是否完全一致。
func sameModelList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// DefaultOutputConfig 缺省输出配置（与接入本功能之前的行为一致：无前缀 + 显示积分倍率）。
func DefaultOutputConfig() OutputConfig {
	return OutputConfig{Format: FormatOpenAI, ModelPrefix: "", RateHint: RateCredit}
}

// normalizeOutput 归一化并校验。ModelPrefix 里的冒号会被剔除：
// 冒号是 realm 路由分隔符（"cn:xxx"），前缀混进冒号会破坏 resolveModel 的解析。
func normalizeOutput(c OutputConfig) (OutputConfig, error) {
	c.Format = strings.ToLower(strings.TrimSpace(c.Format))
	if c.Format == "" {
		c.Format = FormatOpenAI
	}
	if !formatSupported(c.Format) {
		return c, fmt.Errorf("暂不支持输出格式 %q：当前可用 openai（Chat Completions）与 responses（OpenAI Responses）", c.Format)
	}
	switch strings.ToLower(strings.TrimSpace(c.RateHint)) {
	case "", RateCredit:
		c.RateHint = RateCredit
	case RateFree:
		c.RateHint = RateFree
	case RateOff:
		c.RateHint = RateOff
	default:
		return c, fmt.Errorf("rate_hint 只能是 credit / free / off，收到 %q", c.RateHint)
	}
	c.ModelPrefix = strings.ReplaceAll(strings.TrimSpace(c.ModelPrefix), ":", "")
	c.Models = normalizeModelList(c.Models)
	ch, err := normalizeChannels(c.Channels)
	if err != nil {
		return c, err
	}
	// 通道字段与主口取值相同 = 当初并没有特意覆盖它（早期配置里会留着一份冗余的同值）。
	// 折成空串，「继承主口」才真正成立：主口之后改格式/前缀/费率，这些出口才跟得上。
	// 用户真要固定某个出口，把主口改成别的值再单独选即可——那时不相等，不会被折掉。
	for i := range ch {
		if strings.EqualFold(strings.TrimSpace(ch[i].Format), c.Format) {
			ch[i].Format = ""
		}
		if strings.TrimSpace(ch[i].ModelPrefix) == strings.TrimSpace(c.ModelPrefix) {
			ch[i].ModelPrefix = ""
		}
		if strings.EqualFold(strings.TrimSpace(ch[i].RateHint), c.RateHint) {
			ch[i].RateHint = ""
		}
		if len(ch[i].Models) > 0 && sameModelList(ch[i].Models, c.Models) {
			ch[i].Models = nil
		}
	}
	c.Channels = ch
	return c, nil
}

// normalizeChannels 校验并归一化通道列表。规则（全部第一次就写死，避免运行时才发现）：
//   - id 必须是小写 slug（字母数字 - _），且**全局唯一**——它是配置与运行状态的键；
//   - listen 必须合法且**互相不重复**（同一个 socket 起两次必然失败）；
//   - format 只允许当前真能跑的 openai（其余协议在 UI 里列出但不可选，见 formats）；
//   - producers 去空去重；空 = 全放（该口服务所有客户端）。
//
// 不在这里校验「与主口 listen 相同」：主口地址在 config.json 里，本包不持有它；
// 那一条由 /admin/output 的 PUT 校验（那里能取到 configuredListen）。
func normalizeChannels(in []OutputChannel) ([]OutputChannel, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seenID := map[string]bool{}
	var seenListen []string
	out := make([]OutputChannel, 0, len(in))
	for i, ch := range in {
		ch.ID = strings.ToLower(strings.TrimSpace(ch.ID))
		if !validChannelID(ch.ID) {
			return nil, fmt.Errorf("通道 #%d 的 id %q 不合法：只能用小写字母/数字/-/_，且以字母数字开头", i+1, ch.ID)
		}
		if seenID[ch.ID] {
			return nil, fmt.Errorf("通道 id %q 重复了", ch.ID)
		}
		seenID[ch.ID] = true
		addr, err := normalizeListen(ch.Listen)
		if err != nil {
			return nil, fmt.Errorf("通道 %s：%v", ch.ID, err)
		}
		for _, other := range seenListen {
			if listenConflicts(addr, other) {
				return nil, fmt.Errorf("通道 %s 的监听地址 %s 与另一个通道（%s）重复", ch.ID, addr, other)
			}
		}
		seenListen = append(seenListen, addr)
		ch.Listen = addr
		if ch.Format = strings.ToLower(strings.TrimSpace(ch.Format)); ch.Format != "" && !formatSupported(ch.Format) {
			return nil, fmt.Errorf("通道 %s 的格式 %q 暂不支持：当前可用 openai 与 responses", ch.ID, ch.Format)
		}
		switch strings.ToLower(strings.TrimSpace(ch.RateHint)) {
		case "", RateCredit, RateFree, RateOff:
		default:
			return nil, fmt.Errorf("通道 %s 的 rate_hint 只能是 credit / free / off", ch.ID)
		}
		ch.ModelPrefix = strings.ReplaceAll(strings.TrimSpace(ch.ModelPrefix), ":", "")
		ch.Producers = normalizeProducers(ch.Producers)
		ch.Models = normalizeModelList(ch.Models)
		out = append(out, ch)
	}
	return out, nil
}

// listenConflicts 两个监听地址是否会抢同一个 socket。空 host / 0.0.0.0 / :: 是通配：
// 同一端口上「通配」与任何 host 都冲突（:7870 与 127.0.0.1:7870 抢的是同一个口）。
// 两个都是具体 host 时按 host+port 比（等长大小写不敏感，IPv6 由 SplitHostPort 归一）。
func listenConflicts(a, b string) bool {
	ah, ap, aerr := net.SplitHostPort(a)
	bh, bp, berr := net.SplitHostPort(b)
	if aerr != nil || berr != nil {
		return a == b
	}
	if ap != bp {
		return false
	}
	if isWildcardHost(ah) || isWildcardHost(bh) {
		return true
	}
	return strings.EqualFold(ah, bh)
}

func isWildcardHost(h string) bool {
	switch h {
	case "", "*", "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// validChannelID 通道 id 的字符集：小写字母数字与 -_，首字符必须是字母数字。
// 限制字符集不是洁癖：id 会出现在 URL/文件/日志里，放开等于把转义问题留给下游。
func validChannelID(id string) bool {
	if id == "" || len(id) > 32 {
		return false
	}
	for i, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case (r == '-' || r == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}

// normalizeProducers 客户端范围：去空白、去重、保序（用户填的顺序有意义：面板按它显示）。
func normalizeProducers(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, p := range in {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// OutputStore 输出配置的运行时容器：读多写少，写的时候落盘。
// path 为空 = 纯内存（嵌入/测试形态），改了不持久化但本次运行内生效。
type OutputStore struct {
	mu   sync.RWMutex
	cur  OutputConfig
	path string
	// onChange 配置变更回调（进程内唯一订阅者：通道监听管理器）。
	// 存在的原因：通道要真的**绑定/解绑 socket**，那是进程生命周期的事，
	// 不是配置结构自己能做的；配置层只负责「变了就喊一声」。
	onChange func(OutputConfig)
}

// NewOutputStore 建容器并尝试从 path 读入既有配置。
// 文件缺失/损坏一律回落默认值：输出配置丢了是小事，起不来是大事。
func NewOutputStore(path string) *OutputStore {
	s := &OutputStore{path: path, cur: DefaultOutputConfig()}
	if path == "" {
		return s
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var c OutputConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return s
	}
	if norm, nerr := normalizeOutput(c); nerr == nil {
		s.cur = norm
		// 归一化可能改动了磁盘上的旧写法（早期配置在通道里留着一份与主口同值的冗余字段）。
		// 落一次盘把它折成「继承」形态，下次重启读到的就是干净的配置。
		if before, berr := json.Marshal(c); berr == nil {
			if after, aerr := json.Marshal(norm); aerr == nil && string(before) != string(after) {
				_ = s.save(path, norm)
			}
		}
	}
	return s
}

// Get 取当前配置副本。
func (s *OutputStore) Get() OutputConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur
}

// Prefix 取当前模型前缀（wiring.go 的热路径调用，单独开一个短方法避免拷贝整个结构体）。
func (s *OutputStore) Prefix() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur.ModelPrefix
}

// Path 配置文件路径（空 = 不落盘）。
func (s *OutputStore) Path() string { return s.path }

// OnChange 注册变更回调（重复注册覆盖前一个：进程内只需要一个监听管理器）。
// 回调在**释放锁之后**调用，回调里可以安全地读 Get()。
func (s *OutputStore) OnChange(fn func(OutputConfig)) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// ModelSet 当前白名单的集合视图；nil = 全放（空名单）。活路径按 id 查一次，
// 返回 map 让 /v1/models 的过滤是 O(1) 而不是每条都扫一遍白名单。
func (s *OutputStore) ModelSet() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return modelSetOf(s.cur.Models)
}

// modelSetOf 把白名单切片转集合；空 → nil（= 全放，调用方按 nil 判「不过滤」）。
func modelSetOf(list []string) map[string]bool {
	if len(list) == 0 {
		return nil
	}
	out := make(map[string]bool, len(list))
	for _, m := range list {
		out[m] = true
	}
	return out
}

// Set 校验并保存；返回归一化之后的实际生效值。
func (s *OutputStore) Set(c OutputConfig) (OutputConfig, error) {
	norm, err := normalizeOutput(c)
	if err != nil {
		return s.Get(), err
	}
	s.mu.Lock()
	s.cur = norm
	path := s.path
	hook := s.onChange
	s.mu.Unlock()
	var saveErr error
	if path != "" {
		if werr := s.save(path, norm); werr != nil {
			saveErr = fmt.Errorf("已生效但落盘失败（重启会丢）：%w", werr)
		}
	}
	// 先落盘再调和监听器：起完新端口立刻崩也不会留下「配置里没有但端口开着」的错位。
	if hook != nil {
		hook(norm)
	}
	if saveErr != nil {
		return norm, saveErr
	}
	return norm, nil
}

// save 原子落盘。
func (s *OutputStore) save(path string, c OutputConfig) error {
	buf, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- 端点 ----------

// adminOutputGet 返回输出配置 + 可选档位（前端照着渲染，不在前端各写一份枚举）。
func (h *Handler) adminOutputGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.outputView())
}

// adminOutputPut 局部更新输出配置（只传要改的字段）。
func (h *Handler) adminOutputPut(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Output == nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "output_readonly", "本次启动未挂载输出配置存储",
			"正常启动的网关（cmd/server）都带存储；嵌入或测试形态下输出配置是只读的")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var body struct {
		Format      *string `json:"format"`
		ModelPrefix *string `json:"model_prefix"`
		RateHint    *string `json:"rate_hint"`
		// Models 输出白名单：传了才改（空数组 = 恢复全放）。指针区分「没传」与「清空」。
		Models *[]string `json:"models"`
		// ModelsRealmScoped 白名单是否按域分开（true = 国际版用 "global:<裸 id>" 自己的开关，
		// 裸 id 只管 CN）。面板保存时置 true；不带这个键的历史清单保持旧语义（见 publishAllows）。
		ModelsRealmScoped *bool `json:"models_realm_scoped"`
		// Listen 改监听端口：只改 config.json 的 listen 键，**需要重启才换端口**
		// （监听 socket 在启动时就绑好了，进程内换端口等于重启 HTTP 服务）。
		// 注意：这是**主口**。额外出口请用 channels（那些是进程内即时绑定/解绑的）。
		Listen *string `json:"listen"`
		// Channels 额外输出通道（整份替换：传了就按这个列表调和监听器）。
		Channels *[]OutputChannel `json:"channels"`
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "body must be JSON: "+err.Error())
			return
		}
	}
	next := h.cfg.Output.Get()
	if body.Format != nil {
		next.Format = *body.Format
	}
	if body.ModelPrefix != nil {
		next.ModelPrefix = *body.ModelPrefix
	}
	if body.RateHint != nil {
		next.RateHint = *body.RateHint
	}
	if body.Models != nil {
		next.Models = *body.Models
	}
	if body.ModelsRealmScoped != nil {
		next.ModelsRealmScoped = *body.ModelsRealmScoped
	}
	primary := h.configuredListen()
	if body.Channels != nil {
		next.Channels = *body.Channels
		// 与主口撞地址是必错项：先在这里挡掉，不然表现是「通道起不来」而不是「配置写错了」。
		for _, ch := range next.Channels {
			if addr, aerr := normalizeListen(ch.Listen); aerr == nil && listenConflicts(addr, primary) {
				writeOpenAIErrorHint(w, http.StatusBadRequest, "listen_conflict",
					"通道 "+ch.ID+" 的端口与主口相同（"+primary+"）",
					"主口服务全部来源；要给某个客户端单独一个口，请换一个端口，例如 127.0.0.1:7870")
				return
			}
		}
	}
	restart := false
	if body.Listen != nil {
		addr, aerr := normalizeListen(*body.Listen)
		if aerr != nil {
			writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_listen", aerr.Error(),
				"形如 127.0.0.1:7863（只本机）或 :7863（所有网卡）；端口 1-65535")
			return
		}
		if h.cfg.ConfigPath == "" {
			writeOpenAIErrorHint(w, http.StatusBadRequest, "config_readonly", "本次启动未记录 config.json 路径",
				"嵌入/测试形态下改不了端口；正常启动的网关可以")
			return
		}
		if h.configuredListen() != addr {
			if werr := h.writeConfigListen(addr); werr != nil {
				writeOpenAIError(w, http.StatusBadRequest, "config_write_failed", werr.Error())
				return
			}
			restart = true
		}
	}
	if _, serr := h.cfg.Output.Set(next); serr != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_output", serr.Error())
		return
	}
	view := h.outputView()
	view["restart_required"] = restart
	if restart {
		view["restart_note"] = "listen 已写入 config.json，重启网关后生效（当前进程仍听在原端口）"
	}
	writeJSON(w, http.StatusOK, view)
}

// outputCfg 当前生效的输出配置（没挂 store 时用 Config 里的静态值兜底）。
func (h *Handler) outputCfg() OutputConfig {
	if h.cfg.Output != nil {
		return h.cfg.Output.Get()
	}
	c, err := normalizeOutput(OutputConfig{Format: FormatOpenAI, ModelPrefix: h.cfg.ModelPrefix, RateHint: h.cfg.RateHint})
	if err != nil {
		return DefaultOutputConfig()
	}
	return c
}

// outputView /admin/output 的响应体：当前值 + 可选项 + 落盘位置。
func (h *Handler) outputView() map[string]any {
	path := ""
	if h.cfg.Output != nil {
		path = h.cfg.Output.Path()
	}
	return map[string]any{
		"output": h.outputCfg(),
		// listen 从 config.json 现读（不是启动快照）：页面回显必须反映「文件里现在写的是什么」，
		// 否则改完端口刷新一下就退回旧值，看着像没改成。
		"listen":      h.configuredListen(),
		"config_path": h.cfg.ConfigPath,
		"formats": []map[string]any{
			{"id": "openai", "name": "OpenAI 兼容", "available": true,
				"chat_path": "/v1/chat/completions", "models_path": "/v1/models",
				"note": "已可用：Chat Completions（含 SSE 流式）+ 模型列表"},
			{"id": "anthropic", "name": "Anthropic Messages", "available": false,
				"chat_path": "/v1/messages",
				"note":      "待接入：需要把请求体与流式事件双向翻译成 Messages 协议"},
			{"id": "gemini", "name": "Gemini generateContent", "available": false,
				"chat_path": "/v1beta/models/{model}:generateContent",
				"note":      "待接入：需要一套 generateContent 的请求/响应结构"},
			{"id": "responses", "name": "OpenAI Responses", "available": true,
				"chat_path": "/v1/responses", "models_path": "/v1/models",
				"note": "已可用：Codex 等 wire_api=responses 客户端直连；与 Chat 出口共用同一号池与轮转"},
		},
		"rate_hints": []map[string]any{
			{"id": "credit", "name": "显示费率（按模型）", "note": "模型名后缀按各自倍率：免费 -free，收费 -x0.11"},
			{"id": "free", "name": "统一标记免费", "note": "忽略真实倍率，所有模型名一律加 -free"},
			{"id": "off", "name": "不显示", "note": "模型名不带任何费率后缀"},
		},
		// available_models 输出侧可勾选清单：上游当前下发的模型（裸 id）+ 是否已选中。
		// 只读 fetchDynamicModels 的 1h 缓存，不额外打上游；上游没号/拉失败 → 空数组。
		"available_models": h.availableOutputModels(),
		"persisted":        path != "",
		// 额外出口：配置 + 运行时状态分开给（配置是用户写的，status 是真实监听情况）。
		"channels":       h.outputCfg().Channels,
		"channel_status": h.channelStatus(),
		// 主口也当成一个「通道」展示，面板上就不必分两套渲染逻辑。
		// 它服务全部来源、监听地址来自 config.json（改它要重启）。
		"channel_default": map[string]any{
			"id": "default", "name": "主口（全部来源）", "listen": h.configuredListen(),
			"format": h.outputCfg().Format, "model_prefix": h.outputCfg().ModelPrefix,
			"rate_hint": h.outputCfg().RateHint,
			"base_url":  baseURLOf(h.configuredListen()),
			"running":   true, "restart_to_change": true,
		},
	}
}

// channelStatus 通道运行时状态（没挂监听管理器时返回空数组——嵌入/测试形态）。
func (h *Handler) channelStatus() []ChannelStatus {
	if h.cfg.ChannelStatus == nil {
		return []ChannelStatus{}
	}
	return h.cfg.ChannelStatus()
}

// normalizeListen 归一化监听地址：缺冒号的当端口补成 ":port"（与 config.applyEnv 同口径），
// 端口必须是 1-65535 的纯数字。归一化后过一遍 SplitHostPort，把 "1:2:3" 这类打回。
func normalizeListen(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("监听地址不能为空")
	}
	if !strings.HasPrefix(s, ":") && !strings.Contains(s, ":") {
		s = ":" + s
	}
	_, port, err := net.SplitHostPort(s)
	if err != nil {
		return "", fmt.Errorf("监听地址 %q 不合法：%v", raw, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("端口 %q 不合法：应为 1-65535 的整数", port)
	}
	return s, nil
}

// configuredListen 当前 config.json 里写的 listen；读不到就回落到启动时的值。
func (h *Handler) configuredListen() string {
	if h.cfg.ConfigPath == "" {
		return h.cfg.Listen
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return h.cfg.Listen
	}
	var f struct {
		Listen string `json:"listen"`
	}
	if err := json.Unmarshal(raw, &f); err != nil || strings.TrimSpace(f.Listen) == "" {
		return h.cfg.Listen
	}
	return f.Listen
}

// writeConfigListen 只替换 config.json 的 listen 键，其余键**原样回写**。
//
// 走 map[string]json.RawMessage 而不是「结构体 → 再序列化」：结构体序列化会丢掉
// 我们不认识的键（用户自己加的实验字段），把配置写残。RawMessage 保留每个键的原始
// 字节，只动 listen 一个。代价是键序变字典序，属于可接受的外观变化。
func (h *Handler) writeConfigListen(listen string) error {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return fmt.Errorf("读取 %s 失败：%w", h.cfg.ConfigPath, err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return fmt.Errorf("%s 不是合法 JSON：%w", h.cfg.ConfigPath, err)
	}
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	enc, err := json.Marshal(listen)
	if err != nil {
		return err
	}
	all["listen"] = enc
	buf, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := h.cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return fmt.Errorf("写入临时文件失败：%w", err)
	}
	if err := os.Rename(tmp, h.cfg.ConfigPath); err != nil {
		return fmt.Errorf("替换 %s 失败：%w", h.cfg.ConfigPath, err)
	}
	return nil
}

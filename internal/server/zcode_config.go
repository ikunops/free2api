package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"free2api/internal/source"
)

// —— ZCode 一键接入（上下文窗口同步） ——
//
// 为什么需要：ZCode 拉不读 /v1/models 的上下文字段。抓包实证——它一次都没请求过本网关的
// /v1/models；它显示的上下文窗口来自 ~/.zcode/v2/provider_config.json 里
// config.modelConfigRules.providerModelRules[].config.properties.contextWindow。
// 抓包里 cn:deepseek-v4.1-flash-x0.11 显示 1M 是**蒙对的**：ZCode 内置正则
// `.*deepseek-v4-flash(?:[.\-:/\[].*)?` 命中了它，走自己那份 models.dev 快照；
// 其余自建 provider 的规则全都没配 contextWindow，于是落进 200000 兜底。
//
// 所以网关代它写。三个要点：
//
//  1. **认 baseUrl，不认 provider 名字**。用户可以手建任意名字的 provider
//     （实测有 "wbhub 聚合"、"新供应商 4" 两个都指向 7864），按名字匹配必然漏。
//     只认 baseUrl 指向本网关的 provider，直连官方上游的（ocz / OpenCode Zen /
//     Kilo / OpenRouter）一律不碰。
//  2. **按 ZCode 的字段白名单写**。反编译 asar 得到它持久化的 pick 白名单是
//     properties.{contextWindow, supportsJsonSchemaOutput, supportsNativeWebSearch,
//     supportsMidConversationSystem, inputFormat.{supportsImage,supportsVideo,supportsPdf}}
//     与 optionSpecs.{reasoningLevel, maxOutputTokens}——maxOutputTokens 在
//     optionSpecs.maxOutputTokens.max，**不在 properties 里**。写错位置会被丢掉。
//  3. **JSON 走 map[string]any 而不是强类型 struct**。ZCode 每次启动都会重写这张表
//     （实测 127 条规范化回 126 条），强类型回写会顺手抹掉它新加的字段；用泛型
//     map 增删改，未知字段原样保留。
//
// 已实证：ZCode 启动时重建规则表但**不覆盖已存在的规则**，所以写进去就能留住。

// ZCodeConfigPath 解析 ZCode 的 provider_config.json 路径。
// 优先级：显式覆盖 > ZCODE_HOME > ~/.zcode（Windows 与 Unix 同口径）。
func ZCodeConfigPath(override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("ZCODE_HOME")); v != "" {
		return filepath.Join(v, "v2", "provider_config.json")
	}
	return filepath.Join(source.HomeDir(), ".zcode", "v2", "provider_config.json")
}

// ZCodeModelMeta 一条模型要写进 ZCode 的上下文 / 输出上限。
type ZCodeModelMeta struct {
	ContextWindow   int64 `json:"context_window"`
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty"`
}

// ZCodeProviderInfo 扫出来的一个 provider，以及它是不是指向本网关。
type ZCodeProviderInfo struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	BaseURL   string `json:"base_url"`
	Matched   bool   `json:"matched"`
	ModelNums int    `json:"model_count"`
}

// ZCodeChange 一处将要发生的改动（给人看的 diff，不是整文件对拍）。
type ZCodeChange struct {
	ProviderID   string `json:"provider_id"`
	ProviderName string `json:"provider_name"`
	ModelID      string `json:"model_id"`
	// Action update = 已有规则改字段；create = 该 provider 选了但没有对应规则，补一条。
	Action string `json:"action"`
	// Field context_window / max_output_tokens。
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
}

// ZCodePlan 一次同步的完整计划（preview 与 apply 共用）。
type ZCodePlan struct {
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	BaseURL string `json:"base_url"`
	// Providers 全部 provider 现状（Matched=true 的会被改）。
	Providers []ZCodeProviderInfo `json:"providers"`
	// Changes 逐条改动；空 = 已是最新。
	Changes []ZCodeChange `json:"changes"`
	// MatchedProviders / TotalRules 让前端一句话说清覆盖率。
	MatchedProviders int `json:"matched_providers"`
	MatchedModels    int `json:"matched_models"`
	TotalModels      int `json:"total_models"`
	// MissingModels 本网关有元数据、但该 provider 没选的模型（只提示，不改）。
	MissingModels []string `json:"missing_models,omitempty"`
	// Before / After 全文（apply 用 After 落盘）。
	Before  string   `json:"before"`
	After   string   `json:"after"`
	Changed bool     `json:"changed"`
	Backup  string   `json:"backup_path,omitempty"`
	ReadErr string   `json:"read_error,omitempty"`
	Notes   []string `json:"notes,omitempty"`
}

// normalizeGatewayURL 把 baseUrl 归一化成可比对的 host:port 形态。
// 去掉末尾 /、去掉末尾 /v1、转小写。":7864" 这种只写端口的补 127.0.0.1。
func normalizeGatewayURL(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	s = strings.TrimRight(s, "/")
	s = strings.TrimSuffix(s, "/v1")
	s = strings.TrimRight(s, "/")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "https://")
	// ":7864" / "7864" 这种**只写端口**的形态补 127.0.0.1。判据是「没有冒号也没有斜杠」——
	// 只判斜杠会把 192.168.1.5:7864 误当成裸端口，补成 127.0.0.1:192.168.1.5:7864。
	if !strings.ContainsAny(s, ":/") {
		s = "127.0.0.1:" + s
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// sameGateway 判断两个 baseUrl 是否指的是同一个网关。
// 端口相同且主机都是本机回环（127.0.0.1 / 0.0.0.0 / localhost / ::1）也算同一个——
// 用户在 ZCode 里手打 0.0.0.0 而网关听 127.0.0.1 是常事。
func sameGateway(a, b string) bool {
	na, nb := normalizeGatewayURL(a), normalizeGatewayURL(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	ha, pa, oka := splitHostPortLoose(na)
	hb, pb, okb := splitHostPortLoose(nb)
	if !oka || !okb || pa != pb {
		return false
	}
	return isLoopbackHost(ha) && isLoopbackHost(hb)
}

func splitHostPortLoose(s string) (string, string, bool) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

func isLoopbackHost(h string) bool {
	switch h {
	case "127.0.0.1", "0.0.0.0", "localhost", "::1", "[::1]":
		return true
	}
	return strings.HasPrefix(h, "127.")
}

// PlanZCode 算出同步计划，**不落盘**。
// meta 是「对外模型名 → 上下文元数据」的映射（来自 modelList，与 /v1/models 同源）。
func PlanZCode(path, baseURL string, meta map[string]ZCodeModelMeta) (*ZCodePlan, error) {
	p := &ZCodePlan{Path: path, BaseURL: baseURL}

	before := ""
	if b, err := os.ReadFile(path); err == nil {
		before, p.Exists = string(b), true
	} else if !os.IsNotExist(err) {
		p.ReadErr = err.Error()
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}
	if before == "" {
		p.ReadErr = "文件不存在或为空"
		return p, nil
	}

	// 泛型解码：保留 ZCode 自己写的一切未知字段。
	var root map[string]any
	if err := json.Unmarshal([]byte(before), &root); err != nil {
		p.ReadErr = "JSON 解析失败：" + err.Error()
		return p, nil
	}
	cfg, _ := root["config"].(map[string]any)
	if cfg == nil {
		p.ReadErr = "缺少 config 段"
		return p, nil
	}
	pcr, _ := cfg["providerConfigRules"].(map[string]any)
	provsAny, _ := pcr["providerRules"].([]any)
	mcr, _ := cfg["modelConfigRules"].(map[string]any)
	rulesAny, _ := mcr["providerModelRules"].([]any)

	// 1) 找出指向本网关的 provider
	matched := map[string]string{} // providerId → name
	allProviders := map[string]string{}
	provModels := map[string][]string{} // providerId → personalModelIds
	for _, it := range provsAny {
		pr, ok := it.(map[string]any)
		if !ok {
			continue
		}
		id, _ := pr["providerId"].(string)
		if id == "" {
			continue
		}
		name, _ := pr["providerName"].(string)
		c, _ := pr["config"].(map[string]any)
		api, _ := c["api"].(map[string]any)
		bu, _ := api["baseUrl"].(string)
		ids := strList(c["personalModelIds"])
		allProviders[id] = name
		provModels[id] = ids
		hit := sameGateway(bu, baseURL)
		if hit {
			matched[id] = name
		}
		p.Providers = append(p.Providers, ZCodeProviderInfo{
			ID: id, Name: name, BaseURL: bu, Matched: hit, ModelNums: len(ids),
		})
	}
	p.MatchedProviders = len(matched)

	// 2) 现有规则索引：(providerId, modelId) → 该规则 map
	ruleIdx := map[string]map[string]any{}
	rulePos := map[string]int{}
	for i, it := range rulesAny {
		r, ok := it.(map[string]any)
		if !ok {
			continue
		}
		pid, _ := r["providerId"].(string)
		mid, _ := r["modelId"].(string)
		if pid == "" || mid == "" {
			continue
		}
		ruleIdx[pid+"\x00"+mid] = r
		rulePos[pid+"\x00"+mid] = i
	}

	// 3) 逐 provider × 逐模型算改动
	// 覆盖范围 = personalModelIds ∪ modelOrder ∪ 已有规则里的 modelId：
	// 三者都是「用户在 ZCode 里为这个 provider 选过的模型」，任一来源都算。
	touched := map[string]bool{}
	for pid := range matched {
		wanted := map[string]bool{}
		for _, m := range provModels[pid] {
			wanted[m] = true
		}
		// 已有规则也算（用户可能手工加过规则但没进 personalModelIds）
		for i, it := range rulesAny {
			r, _ := it.(map[string]any)
			if r == nil {
				continue
			}
			if rp, _ := r["providerId"].(string); rp == pid {
				if mid, _ := r["modelId"].(string); mid != "" {
					wanted[mid] = true
				}
			}
			_ = i
		}
		pname := matched[pid]
		mids := make([]string, 0, len(wanted))
		for m := range wanted {
			mids = append(mids, m)
		}
		sortStrings(mids)
		for _, mid := range mids {
			mm, ok := meta[mid]
			if !ok {
				continue // 本网关当前目录里没这个模型（未发布/未授权），不动它
			}
			p.TotalModels++
			key := pid + "\x00" + mid
			rule, has := ruleIdx[key]
			if !has {
				// 补一条规则（enabled + 上下文字段）
				rule = map[string]any{
					"providerId": pid,
					"modelId":    mid,
					"config":     map[string]any{"enabled": true},
				}
				rulesAny = append(rulesAny, rule)
				ruleIdx[key] = rule
				rulePos[key] = len(rulesAny) - 1
			}
			rc, _ := rule["config"].(map[string]any)
			if rc == nil {
				rc = map[string]any{}
				rule["config"] = rc
			}
			if _, ok := rc["enabled"]; !ok {
				rc["enabled"] = true
			}
			props, _ := rc["properties"].(map[string]any)
			if props == nil {
				props = map[string]any{}
				rc["properties"] = props
			}
			if mm.ContextWindow > 0 {
				cur := numOf(props["contextWindow"])
				if cur != mm.ContextWindow {
					p.Changes = append(p.Changes, ZCodeChange{
						ProviderID: pid, ProviderName: pname, ModelID: mid,
						Action: actionOf(has), Field: "context_window",
						Before: anyNilZero(cur), After: mm.ContextWindow,
					})
					props["contextWindow"] = mm.ContextWindow
					touched[mid] = true
				} else {
					touched[mid] = true
				}
			}
			if mm.MaxOutputTokens > 0 {
				specs, _ := rc["optionSpecs"].(map[string]any)
				if specs == nil {
					specs = map[string]any{}
					rc["optionSpecs"] = specs
				}
				mo, _ := specs["maxOutputTokens"].(map[string]any)
				if mo == nil {
					mo = map[string]any{}
					specs["maxOutputTokens"] = mo
				}
				cur := numOf(mo["max"])
				if cur != mm.MaxOutputTokens {
					p.Changes = append(p.Changes, ZCodeChange{
						ProviderID: pid, ProviderName: pname, ModelID: mid,
						Action: actionOf(has), Field: "max_output_tokens",
						Before: anyNilZero(cur), After: mm.MaxOutputTokens,
					})
					mo["max"] = mm.MaxOutputTokens
					touched[mid] = true
				} else {
					touched[mid] = true
				}
			}
		}
	}
	for range touched {
		p.MatchedModels++
	}

	// MissingModels：本网关有元数据、但没有任何指向本网关的 provider 选了它。
	var missing []string
	for m := range meta {
		if !touched[m] {
			missing = append(missing, m)
		}
	}
	sortStrings(missing)
	p.MissingModels = missing

	if len(p.Changes) == 0 {
		p.After = before
		p.Changed = false
		return p, nil
	}

	mcr["providerModelRules"] = rulesAny
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("序列化失败：%w", err)
	}
	after := string(out) + "\n"
	p.After = after
	p.Changed = after != before
	if p.Exists {
		p.Backup = fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	}
	p.Notes = append(p.Notes,
		"ZCode 启动时会重写这张表，但不会覆盖已存在的规则——实测写入后重启仍在。",
		"改完需要重启 ZCode（或在设置页重新打开该模型）才会看到新的上下文窗口。",
		fmt.Sprintf("只改 baseUrl 指向 %s 的 %d 个 provider，直连官方上游的（ocz / OpenCode Zen / Kilo / OpenRouter 等）不碰。", baseURL, p.MatchedProviders),
	)
	return p, nil
}

func actionOf(hasRule bool) string {
	if hasRule {
		return "update"
	}
	return "create"
}

func anyNilZero(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

func numOf(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case int:
		return int64(t)
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

func strList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// sortStrings 极简排序（避免为 3 行代码引入 sort 的心智负担时也保确定序）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ApplyZCode 落盘：先备份（若原文件存在）再原子写。
func ApplyZCode(p *ZCodePlan) error {
	if p == nil {
		return fmt.Errorf("计划为空")
	}
	if !p.Changed {
		return nil
	}
	if p.Exists {
		orig, err := os.ReadFile(p.Path)
		if err != nil {
			return fmt.Errorf("读原文件失败：%w", err)
		}
		if p.Backup == "" {
			p.Backup = fmt.Sprintf("%s.bak-%s", p.Path, time.Now().Format("20060102-150405"))
		}
		if err := os.WriteFile(p.Backup, orig, 0o600); err != nil {
			return fmt.Errorf("写备份失败：%w", err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.Path), 0o755); err != nil {
		return fmt.Errorf("建目录失败：%w", err)
	}
	tmp := p.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(p.After), 0o600); err != nil {
		return fmt.Errorf("写临时文件失败：%w", err)
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		return fmt.Errorf("替换 %s 失败：%w", p.Path, err)
	}
	return nil
}

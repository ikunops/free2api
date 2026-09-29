package server

import "strings"

// —— Codex 模型目录分支 ——
//
// Codex 桌面端 / CLI 拉模型列表时固定发 GET {base_url}/models?client_version=<ver>，
// 而且只认 Codex 自己的目录格式 {"models":[{slug, display_name, ...}]}。上游回通用
// OpenAI 格式 {"data":[...]} 时它解析不出来，静默退回内置的 8 个模型——实测：把 Codex
// 指到只有通用格式的口，选择器里出现的是 gpt-5.6-sol 那 8 个内置模型，不是号池里的。
//
// 所以同一个 /v1/models 按 client_version 分流：带了走 Codex 目录，不带走通用格式。
// 两者共用 modelList()，发布白名单 / 出口来源范围 / zcode 额度收敛三处口径完全同源，
// 不会出现「Codex 列表里看得见、一调就 404」。

// codexBaseInstructions Codex 目录里每个模型都要带的系统提示。取 Codex 自己的默认值：
// 客户端会用这份文本覆盖本地默认，换成别的话等于改掉它的行为。
const codexBaseInstructions = "You are Codex, a coding agent. You and the user share one workspace."

// codexReasoningLevelDesc 推理档位的说明文案（Codex 目录 supported_reasoning_levels
// 里的 description）。查不到的档位退回 "Reasoning effort <x>"，不编造描述。
var codexReasoningLevelDesc = map[string]string{
	"none":    "No reasoning",
	"minimal": "Minimal reasoning",
	"low":     "Fast responses with lighter reasoning",
	"medium":  "Balances speed and reasoning depth for everyday tasks",
	"high":    "Greater reasoning depth for complex problems",
	"xhigh":   "Extra high reasoning depth for complex problems",
	"max":     "Maximum reasoning depth for the hardest problems",
	"ultra":   "Maximum reasoning with automatic task delegation",
}

// codexFallbackReasoningLevels 上游没声明档位时给的默认三档（与 Codex 内置模型一致）。
var codexFallbackReasoningLevels = []string{"low", "medium", "high"}

// codexServiceTierPriority Codex 目录里的「速度档位」广告。
//
// 客户端配了 service_tier = "priority"（桌面端「Fast」档）时，Codex 会先看目录有没有广告
// 这个档位：没广告就每次请求打一条 warning（"Configured service tier `priority` is not
// advertised as supported ... and will be omitted"），再把该字段丢掉。
// 上游（腾讯侧）没有服务档概念，本网关在 Responses 入口做的是白名单翻译
// （responses.RequestToChat 只取已知字段），service_tier 本来就到不了上游——所以这里如实
// 广告「接受但不改变速度与计费」：消掉噪音，不谎报能力。
func codexServiceTierPriority() []map[string]any {
	return []map[string]any{{
		"id":          "priority",
		"name":        "Fast",
		"description": "本网关接受该档位但不改变速度与计费（上游不支持服务档）",
	}}
}

// codexModelMessages 目录里的 model_messages：Codex 找不到它时会按 personality 回落并打
// warning（"Model personality requested but model_messages is missing, falling back to
// base instructions"）。给出 instructions_template（= base_instructions）即消掉该 warning；
// instructions_variables / approvals 留字段位并置 null，与官方目录形状一致。
func codexModelMessages() map[string]any {
	return map[string]any{
		"instructions_template":  codexBaseInstructions,
		"instructions_variables": nil,
		"approvals":              nil,
	}
}

// codexModels /v1/models 的 Codex 分支：把通用清单翻译成 Codex 目录格式。
// 只写 Codex 认识的字段——它对自己缺的字段有默认值，但对多余字段宽不宽容没把握，
// 所以 max_output_tokens / credits 这些我们自己的字段一律不带。
func (h *Handler) codexModels() []map[string]any {
	list := h.modelList()
	out := make([]map[string]any, 0, len(list))
	for i, e := range list {
		id := entryStr(e, "id")
		if id == "" {
			continue
		}
		name := entryStr(e, "name")
		if name == "" {
			name = id
		}
		entry := map[string]any{
			"slug":                         id,
			"display_name":                 codexDisplayName(id, name),
			"base_instructions":            codexBaseInstructions,
			"experimental_supported_tools": []any{},
			"model_messages":               codexModelMessages(),
			"service_tiers":                codexServiceTierPriority(),
			"priority":                     i,
			"shell_type":                   "shell_command",
			"support_verbosity":            true,
			"supported_in_api":             true,
			"supported_reasoning_levels":   codexReasoningLevels(e),
			"supports_parallel_tool_calls": true,
			"supports_reasoning_summaries": true,
			"truncation_policy":            map[string]any{"mode": "tokens", "limit": 10000},
			"visibility":                   "list",
		}
		if d := entryStr(e, "description"); d != "" {
			entry["description"] = d
		}
		if cw := entryInt(e, "context_length"); cw > 0 {
			entry["context_window"] = cw
			entry["max_context_window"] = cw
		}
		out = append(out, entry)
	}
	return out
}

// codexDisplayName 模型在 Codex 选择器里显示的名字：上游中文名 + 「域 费率」标签，
// 例如 "快速 [CN x0.21]"、"Deepseek-V4.1-Flash [GLOBAL free]"。标签只给人看，
// slug 才是路由依据。
func codexDisplayName(id, name string) string {
	realm, rest := "", id
	switch {
	case strings.HasPrefix(rest, "cn:"):
		realm, rest = "CN", strings.TrimPrefix(rest, "cn:")
	case strings.HasPrefix(rest, "global:"):
		realm, rest = "GLOBAL", strings.TrimPrefix(rest, "global:")
	}
	rate := ""
	if strings.HasSuffix(rest, "-free") {
		rate = "free"
	} else if i := strings.LastIndex(rest, "-x"); i >= 0 && i+2 < len(rest) {
		rate = rest[i+1:]
	}
	tag := strings.TrimSpace(realm + " " + rate)
	if tag == "" {
		return name
	}
	return name + " [" + tag + "]"
}

// codexReasoningLevels 把通用条目里的 reasoning_supported_efforts 翻成 Codex 的档位
// 数组；上游没给就退回默认三档（宁可按默认放开，也不让客户端选不了档位）。
func codexReasoningLevels(e map[string]any) []map[string]any {
	efforts := entryStrSlice(e, "reasoning_supported_efforts")
	if len(efforts) == 0 {
		efforts = codexFallbackReasoningLevels
	}
	out := make([]map[string]any, 0, len(efforts))
	for _, ef := range efforts {
		desc, ok := codexReasoningLevelDesc[ef]
		if !ok {
			desc = "Reasoning effort " + ef
		}
		out = append(out, map[string]any{"effort": ef, "description": desc})
	}
	return out
}

// entryStr / entryInt / entryStrSlice 从 modelList() 的通用条目里取字段。
// 上游字段是「有才写、零值省略」，所以取不到就是零值，不编造。
func entryStr(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func entryInt(m map[string]any, k string) int64 {
	switch v := m[k].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	}
	return 0
}

func entryStrSlice(m map[string]any, k string) []string {
	switch v := m[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, it := range v {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

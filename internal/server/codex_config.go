package server

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"free2api/internal/source"
)

// —— Codex 一键接入 ——
//
// 为什么需要这个：Codex CLI / 桌面端**没有「自定义供应商」入口**，它只认
// ~/.codex/config.toml 里的 [model_providers.*] 段与顶层 model / model_provider。
// 其它客户端（ZCode / OpenCode / Qoder / Cherry Studio）都能在 UI 里手填 Base URL，
// 唯独 Codex 必须改文件。所以网关代它改，并且：
//
//   - **只动自己那一段**（[model_providers.<id>]）+ 顶层 model / model_provider 两行，
//     用户的其它 provider、注释、缩进一律原样保留；
//   - **改前先备份**（config.toml.bak-<时间戳>）；
//   - 提供 **preview**：把改前/改后全文给前端，用户确认了才落盘。
//
// 刻意不做「全量 TOML 解析再回写」：那会抹掉用户的注释与格式。这里用外科编辑——
// 只按段落边界切割，不做语义解析。

// codexProviderID 写进 config.toml 的 provider 段名（[model_providers.<id>]）。
// 固定值而非随机：用户重复点「写入」时应覆盖同一段，而不是每点一次多一段。
const codexProviderID = "free2api"

// codexEnvKey Codex 从这里读 API key 的环境变量名。网关不鉴权时该变量也要有值
// （Codex 要求 env_key 指向的变量存在），所以 UI 会一并给出设置命令。
const codexEnvKey = "WB_API_KEY"

// CodexConfigPath 解析 Codex 的 config.toml 路径。
// 优先级：显式覆盖 > CODEX_HOME > ~/.codex（Windows 与 Unix 同口径）。
func CodexConfigPath(override string) string {
	if v := strings.TrimSpace(override); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("CODEX_HOME")); v != "" {
		return filepath.Join(v, "config.toml")
	}
	return filepath.Join(source.HomeDir(), ".codex", "config.toml")
}

// codexSectionRe 匹配 [model_providers.<id>] 段头（含可能的前后空白）。
func codexSectionRe(id string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*\[model_providers\.` + regexp.QuoteMeta(id) + `\][ \t]*\r?\n`)
}

// codexSectionSpan 定位 [model_providers.<id>] 段的字节区间 [start, end)。
// end 是「下一个段头」或文件末尾。找不到段头时返回 (-1, -1)。
//
// 段头识别：以 "[" 开头且不是数组续行。这里用宽松判据——行首（可含空白）的 "["。
func codexSectionSpan(s, id string) (int, int) {
	re := codexSectionRe(id)
	loc := re.FindStringIndex(s)
	if loc == nil {
		return -1, -1
	}
	start := loc[0]
	rest := s[loc[1]:]
	// 在 rest 里找下一个段头
	if next := regexp.MustCompile(`(?m)^[ \t]*\[`).FindStringIndex(rest); next != nil {
		return start, loc[1] + next[0]
	}
	return start, len(s)
}

// codexTopKeyRe 匹配顶层键（不在任何 [section] 内）——用于替换 model / model_provider。
// 只在「第一个段头之前」的区间里搜，避免误改 provider 段内的同名键。
func codexTopKeyRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(key) + `[ \t]*=`)
}

// codexTopSpan 顶层区间的字节范围：从文件头到第一个段头（或末尾）。
func codexTopSpan(s string) (int, int) {
	if loc := regexp.MustCompile(`(?m)^[ \t]*\[`).FindStringIndex(s); loc != nil {
		return 0, loc[0]
	}
	return 0, len(s)
}

// buildCodexProviderBlock 生成 [model_providers.<id>] 段全文。
func buildCodexProviderBlock(baseURL, wireAPI string) string {
	var b strings.Builder
	b.WriteString("[model_providers." + codexProviderID + "]\n")
	b.WriteString(`name = "Free2API (local)"` + "\n")
	b.WriteString(`base_url = "` + baseURL + `"` + "\n")
	b.WriteString(`env_key = "` + codexEnvKey + `"` + "\n")
	b.WriteString(`wire_api = "` + wireAPI + `"` + "\n")
	return b.String()
}

// setTopKey 在顶层区间内替换或插入 key = "value"。
// 已有该键 → 原地替换（保留行尾注释之前的其它行不动）；
// 没有 → 插到顶层区间末尾（即第一个段头之前），保持 TOML 语义正确。
func setTopKey(s, key, value string) string {
	topStart, topEnd := codexTopSpan(s)
	top := s[topStart:topEnd]
	re := codexTopKeyRe(key)
	line := key + ` = "` + value + `"`
	if loc := re.FindStringIndex(top); loc != nil {
		// 找到该行结尾，整行替换
		lineEnd := strings.Index(top[loc[0]:], "\n")
		if lineEnd < 0 {
			top = top[:loc[0]] + line + "\n"
		} else {
			top = top[:loc[0]] + line + top[loc[0]+lineEnd:]
		}
	} else {
		// 插入到顶层末尾：确保前面有换行分隔
		if top != "" && !strings.HasSuffix(top, "\n") {
			top += "\n"
		}
		top += line + "\n"
	}
	return s[:topStart] + top + s[topEnd:]
}

// upsertCodexProvider 替换或追加 [model_providers.<id>] 段。
func upsertCodexProvider(s, block string) string {
	start, end := codexSectionSpan(s, codexProviderID)
	if start >= 0 {
		return s[:start] + block + s[end:]
	}
	// 追加到文件末尾：段前留一个空行
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	if strings.TrimSpace(s) != "" {
		s += "\n"
	}
	return s + block
}

// CodexPlan 一次「写入 Codex 配置」的完整计划（preview 与 apply 共用）。
type CodexPlan struct {
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	ProviderID string `json:"provider_id"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	EnvKey     string `json:"env_key"`
	// EnvValueHint 该设成什么（网关不鉴权时也要有值，Codex 要求变量存在）。
	EnvValueHint string `json:"env_value_hint"`
	// Before / After 改前改后全文（preview 给前端做 diff，apply 用它落盘）。
	Before string `json:"before"`
	After  string `json:"after"`
	// Changed 是否有实际改动（重复点写入且内容一致时为 false，前端可提示「已是最新」）。
	Changed bool `json:"changed"`
	// BackupPath 落盘时会写到的备份路径（apply 后才真实存在）。
	BackupPath string `json:"backup_path,omitempty"`
	// Notes 给用户看的注意事项（如 models_cache.json 需删除重拉）。
	Notes []string `json:"notes,omitempty"`
}

// PlanCodex 计算写入计划，**不落盘**。baseURL 是网关对外地址（如 http://127.0.0.1:7864/v1），
// model 是要设成默认的模型名（如 cn:auto）。wireAPI 固定 responses（Codex 只认它）。
func PlanCodex(path, baseURL, model, apiKey string) (*CodexPlan, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("base_url 为空：先在「输出 API」页确认监听地址")
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, fmt.Errorf("model 为空：从发布清单里选一个模型作为默认")
	}

	before := ""
	exists := false
	if b, err := os.ReadFile(path); err == nil {
		before, exists = string(b), true
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取 %s 失败：%w", path, err)
	}

	after := before
	// 1) 顶层 model / model_provider
	after = setTopKey(after, "model", model)
	after = setTopKey(after, "model_provider", codexProviderID)
	// 2) provider 段
	after = upsertCodexProvider(after, buildCodexProviderBlock(baseURL, "responses"))

	envHint := strings.TrimSpace(apiKey)
	if envHint == "" {
		// 网关不鉴权：Codex 仍要求 env_key 指向的变量存在，给一个占位值。
		envHint = "free2api-local"
	}

	p := &CodexPlan{
		Path: path, Exists: exists, ProviderID: codexProviderID,
		BaseURL: baseURL, Model: model, EnvKey: codexEnvKey, EnvValueHint: envHint,
		Before: before, After: after, Changed: after != before,
	}
	if exists {
		p.BackupPath = fmt.Sprintf("%s.bak-%s", path, time.Now().Format("20060102-150405"))
	}
	// models_cache.json 是 Codex 自己拉的模型目录缓存。里面存的是「上次从哪个
	// base_url 拉的」，换网关后旧缓存可能让选择器显示旧模型，所以提示删除重拉。
	cachePath := filepath.Join(filepath.Dir(path), "models_cache.json")
	if _, err := os.Stat(cachePath); err == nil {
		p.Notes = append(p.Notes, "检测到 "+cachePath+"：它是 Codex 的模型目录缓存，换网关后建议删除让 Codex 重新拉取（否则选择器可能还显示旧模型）。")
	}
	p.Notes = append(p.Notes,
		"Codex 从环境变量 "+codexEnvKey+" 读 API key。请确保它已设置：",
		"  PowerShell:  $env:"+codexEnvKey+` = "`+envHint+`"`,
		"  bash/zsh:    export "+codexEnvKey+"="+envHint,
	)
	return p, nil
}

// ApplyCodex 落盘：先备份（若原文件存在）再原子写。
func ApplyCodex(p *CodexPlan) error {
	if p == nil {
		return fmt.Errorf("计划为空")
	}
	if p.Exists {
		orig, err := os.ReadFile(p.Path)
		if err != nil {
			return fmt.Errorf("读原文件失败：%w", err)
		}
		if p.BackupPath == "" {
			p.BackupPath = fmt.Sprintf("%s.bak-%s", p.Path, time.Now().Format("20060102-150405"))
		}
		if err := os.WriteFile(p.BackupPath, orig, 0o600); err != nil {
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
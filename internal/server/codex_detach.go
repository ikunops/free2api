package server

// codex_detach.go —— Codex 一键接入的**反向操作**（卸载 / 还原官方默认）。
//
// 为什么需要：前端「输出 API」页能一键把 Codex 挂到本网关（写 config.toml），
// 但挂了之后想还原（走官方 provider）过去只能敲 cmd/attach detach。这里把同一套
// 外科编辑搬进 HTTP 面，页面加一个「还原官方默认」按钮即可，不用开终端。
//
// 语义与 cmd/attach detach 保持一致：
//   - 删顶层 model_provider（回到官方 provider 选择）；
//   - 顶层 model 若是网关模型名（带 realm 冒号前缀）也一并删，回落 Codex 默认模型；
//     若是官方模型名（gpt-5.x 之类，不含冒号）则**保留**——那是用户自己的选择。
//   - [model_providers.free2api] 段**保留**（不影响官方使用，下次 attach 直接覆盖）。
//
// 改前先备份（与 apply 同一套 ApplyCodex 落盘路径）。

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// removeTopKeyLine 删掉顶层区间里的 `key = ...` 整行；返回是否删到了。
// 只在第一个段头之前搜（与 setTopKey / codexTopKeyRe 同口径），不碰段内同名键。
func removeTopKeyLine(s, key string) (string, bool) {
	topStart, topEnd := codexTopSpan(s)
	top := s[topStart:topEnd]
	re := codexTopKeyRe(key)
	loc := re.FindStringIndex(top)
	if loc == nil {
		return s, false
	}
	lineEnd := strings.Index(top[loc[0]:], "\n")
	var newTop string
	if lineEnd < 0 {
		newTop = top[:loc[0]]
	} else {
		newTop = top[:loc[0]] + top[loc[0]+lineEnd+1:]
	}
	return s[:topStart] + newTop + s[topEnd:], true
}

// codexDetachPlan 「还原官方默认」的计划（preview 与落盘共用）。
type codexDetachPlan struct {
	Path            string `json:"path"`
	Exists          bool   `json:"exists"`
	CurrentModel    string `json:"current_model"`
	CurrentProvider string `json:"current_provider"`
	RemovedProvider bool   `json:"removed_provider"`
	RemovedModel    bool   `json:"removed_model"`
	// KeptModel 顶层 model 是官方名（不含冒号）而保留时的值，供页面提示。
	KeptModel string `json:"kept_model,omitempty"`
	// HasOurSection 是否还留着 [model_providers.free2api] 段（保留，随时可再 attach）。
	HasOurSection bool   `json:"has_our_section"`
	Before        string `json:"before"`
	After         string `json:"after"`
	Changed       bool   `json:"changed"`
	BackupPath    string `json:"backup_path,omitempty"`
}

// PlanCodexDetach 计算还原计划，**不落盘**。
func PlanCodexDetach(path string) (*codexDetachPlan, error) {
	before := ""
	exists := false
	if b, err := os.ReadFile(path); err == nil {
		before, exists = string(b), true
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	p := &codexDetachPlan{Path: path, Exists: exists, Before: before}
	if !exists {
		p.After = ""
		p.Changed = false
		return p, nil
	}
	p.CurrentModel = readTopString(before, "model")
	p.CurrentProvider = readTopString(before, "model_provider")
	after := before
	var removedProvider bool
	after, removedProvider = removeTopKeyLine(after, "model_provider")
	p.RemovedProvider = removedProvider
	// 只删「看起来是网关模型名」的 model（带 realm 冒号前缀）；官方名保留。
	if p.CurrentModel != "" {
		if strings.Contains(p.CurrentModel, ":") {
			var removedModel bool
			after, removedModel = removeTopKeyLine(after, "model")
			p.RemovedModel = removedModel
		} else {
			p.KeptModel = p.CurrentModel
		}
	}
	if start, _ := codexSectionSpan(after, codexProviderID); start >= 0 {
		p.HasOurSection = true
	}
	p.After = after
	p.Changed = after != before
	if p.Changed {
		p.BackupPath = path + ".bak-" + time.Now().Format("20060102-150405")
	}
	return p, nil
}

// applyCodexDetach 落盘：先备份再原子写（与 ApplyCodex 同纪律）。
func applyCodexDetach(p *codexDetachPlan) error {
	if p == nil || !p.Exists || !p.Changed {
		return nil
	}
	orig, err := os.ReadFile(p.Path)
	if err != nil {
		return err
	}
	if p.BackupPath == "" {
		p.BackupPath = p.Path + ".bak-" + time.Now().Format("20060102-150405")
	}
	if err := os.WriteFile(p.BackupPath, orig, 0o600); err != nil {
		return err
	}
	tmp := p.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(p.After), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.Path)
}

// adminCodexDetach POST /admin/codex/detach：还原官方默认。
// body {"apply":true} 落盘；缺省 / false 只回预览（与 codex preview/apply 同形）。
func (h *Handler) adminCodexDetach(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	apply := false
	if len(strings.TrimSpace(string(raw))) > 0 {
		var body struct {
			Apply bool `json:"apply"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON："+err.Error())
			return
		}
		apply = body.Apply
	}
	plan, err := PlanCodexDetach(CodexConfigPath(h.cfg.CodexConfigPath))
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "codex_detach_failed", err.Error(),
			"确认网关进程对 Codex config.toml 有读写权限")
		return
	}
	if apply {
		if !plan.Changed {
			writeJSON(w, http.StatusOK, map[string]any{
				"applied": false, "changed": false, "path": plan.Path,
				"message": "已经是官方默认形态，无需改动", "plan": plan,
			})
			return
		}
		if err := applyCodexDetach(plan); err != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "codex_detach_failed", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"applied": true, "changed": true, "path": plan.Path,
			"backup_path": plan.BackupPath, "message": "已还原官方默认，重启 Codex 后生效", "plan": plan,
		})
		return
	}
	writeJSON(w, http.StatusOK, plan)
}

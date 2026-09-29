package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// —— Codex 一键接入的 admin 接口 ——
//
// 三个端点：
//   GET  /admin/codex       现状（路径 / 是否已装 / 当前写的是什么）+ 建议值
//   POST /admin/codex/preview 给 base_url + model，回改前改后全文（不落盘）
//   POST /admin/codex/apply   同一份计划，落盘（先备份）
//
// 都用 withLocalOrAuth：本机免 key（改的是本机文件，本机用户本来就能改），
// 非本机仍校验 api_key。

// codexStateView GET /admin/codex 的返回。
type codexStateView struct {
	Path       string `json:"path"`
	Exists     bool   `json:"exists"`
	ProviderID string `json:"provider_id"`
	EnvKey     string `json:"env_key"`
	// CurrentModel / CurrentProvider 从现有 config.toml 里读到的顶层值（没有则空）。
	CurrentModel    string `json:"current_model"`
	CurrentProvider string `json:"current_provider"`
	// CurrentBaseURL 现有 [model_providers.free2api].base_url（没有则空）。
	CurrentBaseURL string `json:"current_base_url"`
	// HasOurSection 是否已经写过我们的 provider 段。
	HasOurSection bool `json:"has_our_section"`
	// SuggestedBaseURL / SuggestedModels 建议值（来自当前输出配置与发布清单）。
	SuggestedBaseURL string   `json:"suggested_base_url"`
	SuggestedModels  []string `json:"suggested_models"`
	// ModelsCachePath 模型目录缓存路径（存在时提示删除重拉）。
	ModelsCachePath string `json:"models_cache_path,omitempty"`
	// EnvKeySet 该环境变量当前进程里有没有值（只报真假，不回报值）。
	EnvKeySet bool `json:"env_key_set"`
	// ReadError 读 config.toml 失败时的原因（如权限）。
	ReadError string `json:"read_error,omitempty"`
}

// readTopString 从 TOML 文本里读顶层 key = "value"（只扫顶层区间，避免误读段内同名键）。
func readTopString(s, key string) string {
	start, end := codexTopSpan(s)
	top := s[start:end]
	re := codexTopKeyRe(key)
	loc := re.FindStringIndex(top)
	if loc == nil {
		return ""
	}
	rest := top[loc[1]:]
	nl := strings.Index(rest, "\n")
	if nl >= 0 {
		rest = rest[:nl]
	}
	// 取第一个引号对
	i := strings.Index(rest, `"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(rest[i+1:], `"`)
	if j < 0 {
		return ""
	}
	return rest[i+1 : i+1+j]
}

// readProviderBaseURL 读 [model_providers.<id>] 段里的 base_url。
func readProviderBaseURL(s, id string) string {
	start, end := codexSectionSpan(s, id)
	if start < 0 {
		return ""
	}
	body := s[start:end]
	re := regexp.MustCompile(`(?m)^[ \t]*base_url[ \t]*=[ \t]*"([^"]*)"`)
	if m := re.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

func (h *Handler) adminCodexGet(w http.ResponseWriter, r *http.Request) {
	path := CodexConfigPath(h.cfg.CodexConfigPath)
	view := codexStateView{
		Path: path, ProviderID: codexProviderID, EnvKey: codexEnvKey,
		SuggestedBaseURL: h.suggestedBaseURL(),
		SuggestedModels:  h.suggestedModels(),
	}
	if _, ok := os.LookupEnv(codexEnvKey); ok {
		view.EnvKeySet = true
	}
	if b, err := os.ReadFile(path); err == nil {
		s := string(b)
		view.Exists = true
		view.CurrentModel = readTopString(s, "model")
		view.CurrentProvider = readTopString(s, "model_provider")
		view.CurrentBaseURL = readProviderBaseURL(s, codexProviderID)
		start, _ := codexSectionSpan(s, codexProviderID)
		view.HasOurSection = start >= 0
	} else if !os.IsNotExist(err) {
		view.ReadError = err.Error()
	}
	cachePath := filepath.Join(filepath.Dir(path), "models_cache.json")
	if _, err := os.Stat(cachePath); err == nil {
		view.ModelsCachePath = cachePath
	}
	writeJSON(w, http.StatusOK, view)
}

// suggestedBaseURL 主口的对外 Base URL（含 /v1）。没有配置时回落到默认监听。
func (h *Handler) suggestedBaseURL() string {
	listen := h.configuredListen()
	if strings.TrimSpace(listen) == "" {
		listen = "127.0.0.1:7864"
	}
	return baseURLOf(listen)
}

// suggestedModels 建议的默认模型：发布清单里的第一个（没发布清单 = 取号池前几个）。
func (h *Handler) suggestedModels() []string {
	out := []string{}
	for _, m := range h.availableOutputModels() {
		if sel, _ := m["selected"].(bool); sel {
			if id, _ := m["id"].(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// codexPreviewReq preview / apply 共用的请求体。
type codexPreviewReq struct {
	BaseURL string `json:"base_url"`
	Model   string `json:"model"`
	// Apply true 时落盘（仅 /admin/codex/apply 用）。
	Apply bool `json:"apply"`
}

func (h *Handler) adminCodexPreview(w http.ResponseWriter, r *http.Request) {
	h.codexPlanHandler(w, r, false)
}

func (h *Handler) adminCodexApply(w http.ResponseWriter, r *http.Request) {
	h.codexPlanHandler(w, r, true)
}

func (h *Handler) codexPlanHandler(w http.ResponseWriter, r *http.Request, apply bool) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var req codexPreviewReq
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "请求体不是合法 JSON："+err.Error())
			return
		}
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" {
		baseURL = h.suggestedBaseURL()
	}
	model := strings.TrimSpace(req.Model)
	if model == "" {
		if ms := h.suggestedModels(); len(ms) > 0 {
			model = ms[0]
		}
	}
	path := CodexConfigPath(h.cfg.CodexConfigPath)
	plan, err := PlanCodex(path, baseURL, model, h.cfg.APIKey)
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "codex_plan_failed", err.Error(),
			"base_url 从「输出 API」页的监听地址派生；model 从发布清单里选一个")
		return
	}
	if apply {
		if !plan.Changed {
			writeJSON(w, http.StatusOK, map[string]any{
				"applied": false, "changed": false, "path": plan.Path,
				"message": "配置已是目标状态，无需写入", "plan": plan,
			})
			return
		}
		if err := ApplyCodex(plan); err != nil {
			writeOpenAIErrorHint(w, http.StatusInternalServerError, "codex_apply_failed", err.Error(),
				"确认网关进程对该文件有写权限；改前会先写 .bak-<时间戳> 备份")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"applied": true, "changed": true, "path": plan.Path,
			"backup_path": plan.BackupPath,
			"message":     "已写入，重启 Codex 后生效", "plan": plan,
		})
		return
	}
	writeJSON(w, http.StatusOK, plan)
}
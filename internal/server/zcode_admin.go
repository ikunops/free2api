package server

import (
	"net/http"
	"strings"
)

// —— ZCode 一键接入的 admin 接口 ——
//
// 三个端点，与 Codex 那套同构（读现状 / 预览 / 落盘），只是改的文件不同：
//
//	GET  /admin/zcode         现状：哪些 provider 指向本网关、缺多少条上下文
//	POST /admin/zcode/preview 算出逐条 diff，不落盘
//	POST /admin/zcode/apply   同一份计划落盘（先备份）
//
// 都走 withLocalOrAuth：改的是本机文件，本机用户本来就能改；非本机仍校验 api_key。

// zcodeStateView GET /admin/zcode 的返回。
type zcodeStateView struct {
	Path   string `json:"path"`
	Exists bool   `json:"exists"`
	// BaseURL 当前认为的本网关地址（用于前端显示匹配依据）。
	BaseURL string `json:"base_url"`
	// Providers 全部 provider 现状，Matched=true 的会被改。
	Providers []ZCodeProviderInfo `json:"providers"`
	// MatchedProviders 指向本网关的 provider 数。
	MatchedProviders int `json:"matched_providers"`
	// CatalogModels 网关当前对外发布的模型数。
	CatalogModels int `json:"catalog_models"`
	// MissingContext 有元数据但还没写进 ZCode 的模型名（还没选进任何本网关 provider）。
	MissingContext int `json:"missing_context"`
	// Catalog 供前端显示「我们会写什么值」，只含有元数据的模型。
	Catalog []zcodeCatalogItem `json:"catalog,omitempty"`
	ReadErr string             `json:"read_error,omitempty"`
}

// zcodeCatalogItem 一条模型的上下文 / 输出上限，供前端预览。
type zcodeCatalogItem struct {
	ModelID         string `json:"model_id"`
	ContextWindow   int64  `json:"context_window"`
	MaxOutputTokens int64  `json:"max_output_tokens,omitempty"`
}

// zcodeMetaOf 从 modelList()（与 /v1/models 同源）抽「对外模型名 → 上下文元数据」。
func (h *Handler) zcodeMetaOf() map[string]ZCodeModelMeta {
	out := map[string]ZCodeModelMeta{}
	for _, e := range h.modelList() {
		id := entryStr(e, "id")
		if id == "" {
			continue
		}
		m := ZCodeModelMeta{
			ContextWindow:   entryInt(e, "context_length"),
			MaxOutputTokens: entryInt(e, "max_output_tokens"),
		}
		if m.ContextWindow <= 0 {
			continue
		}
		out[id] = m
	}
	return out
}

func (h *Handler) adminZCodeGet(w http.ResponseWriter, r *http.Request) {
	path := ZCodeConfigPath(h.cfg.ZCodeConfigPath)
	base := h.suggestedBaseURL()
	meta := h.zcodeMetaOf()

	view := zcodeStateView{
		Path: path, BaseURL: base,
		CatalogModels: len(meta),
		Providers:     []ZCodeProviderInfo{},
	}

	// 复用 PlanZCode 拿现状（它只读不写），避免两处解析逻辑漂移。
	plan, err := PlanZCode(path, base, meta)
	if err != nil {
		view.ReadErr = err.Error()
		view.Exists = false
		view.Providers = []ZCodeProviderInfo{}
		view.Catalog = zcodeCatalogList(meta)
		writeJSON(w, http.StatusOK, view)
		return
	}
	view.Exists = plan.Exists
	view.Providers = plan.Providers
	view.MatchedProviders = plan.MatchedProviders
	view.MissingContext = len(plan.MissingModels)
	view.Catalog = zcodeCatalogList(meta)
	if plan.ReadErr != "" {
		view.ReadErr = plan.ReadErr
	}
	writeJSON(w, http.StatusOK, view)
}

func zcodeCatalogList(meta map[string]ZCodeModelMeta) []zcodeCatalogItem {
	out := make([]zcodeCatalogItem, 0, len(meta))
	for id, m := range meta {
		out = append(out, zcodeCatalogItem{
			ModelID: id, ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
		})
	}

	sortCatalogItems(out)
	return out
}

func sortCatalogItems(s []zcodeCatalogItem) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].ModelID < s[j-1].ModelID; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func (h *Handler) adminZCodePreview(w http.ResponseWriter, r *http.Request) {
	h.zcodePlanHandler(w, r, false)
}

func (h *Handler) adminZCodeApply(w http.ResponseWriter, r *http.Request) {
	h.zcodePlanHandler(w, r, true)
}

func (h *Handler) zcodePlanHandler(w http.ResponseWriter, r *http.Request, apply bool) {
	path := ZCodeConfigPath(h.cfg.ZCodeConfigPath)
	base := h.suggestedBaseURL()
	if v := strings.TrimSpace(r.URL.Query().Get("base_url")); v != "" {
		base = v
	}
	plan, err := PlanZCode(path, base, h.zcodeMetaOf())
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "zcode_plan_failed", err.Error(),
			"确认 ZCode 装在本机、配置文件可读；网关地址从「输出 API」页的监听地址派生")
		return
	}
	if !apply {
		writeJSON(w, http.StatusOK, plan)
		return
	}
	if !plan.Changed {
		writeJSON(w, http.StatusOK, map[string]any{
			"applied": false, "changed": false, "path": plan.Path,
			"message": "上下文已是最新，无需写入", "plan": plan,
		})
		return
	}
	if err := ApplyZCode(plan); err != nil {
		writeOpenAIErrorHint(w, http.StatusInternalServerError, "zcode_apply_failed", err.Error(),
			"确认网关进程对该文件有写权限；改前会先写 .bak-<时间戳> 备份")
		return
	}
	// 重算一遍再回：刚才那份 plan 是**写入前**算的，changed 仍是 true。前端拿它当
	// 「当前状态」渲染，用户点了写入却还看到「待确认写入」+「确认写入」按钮——只能
	// 硬刷新才知道其实已经写好了（和之前 codex 那套一样的刺头，这里一次性堵掉）。
	fresh, ferr := PlanZCode(path, base, h.zcodeMetaOf())
	if ferr != nil || fresh.Changed {
		// 重算失败或仍有残留改动：老实回传旧 plan，前端会继续显示待确认，用户再点一次即可。
		if fresh != nil {
			plan = fresh
		}
	} else {
		plan = fresh
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"applied": true, "changed": plan.Changed, "path": plan.Path,
		"backup_path": plan.Backup,
		"message":     "已写入，重启 ZCode 后生效", "plan": plan,
	})
}

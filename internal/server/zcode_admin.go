package server

import (
	"encoding/json"
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

	// Formats 网关输出格式 → ZCode 能不能接，供一键接入表单置灰。
	Formats []zcodeFormatView `json:"formats,omitempty"`
	ReadErr string            `json:"read_error,omitempty"`
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
	view.Formats = zcodeFormatViews(h)
	view.MissingContext = len(plan.MissingModels)
	view.Catalog = zcodeCatalogList(meta)
	if plan.ReadErr != "" {
		view.ReadErr = plan.ReadErr
	}
	writeJSON(w, http.StatusOK, view)
}

// zcodeFormatViews 从网关格式注册表生成 ZCode 可接入性列表。
// 直接读注册表而不是在前端硬编码一份：网关加了新格式这里自动跟上，
// 不会出现"前端能选、后端拒绝"这种不一致。
func zcodeFormatViews(h *Handler) []zcodeFormatView {
	var out []zcodeFormatView
	for _, f := range outputFormatRegistry() {
		id, _ := f["id"].(string)
		name, _ := f["name"].(string)
		regNote, _ := f["note"].(string)
		note, ok := zcodeFormatNote(id)
		if !ok {
			note = regNote + "（" + note + "）"
		}
		out = append(out, zcodeFormatView{ID: id, Name: name, Note: note, OK: ok})
	}
	return out
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

// zcodeFormatView 一个网关输出格式在 ZCode 侧的可接入性，供前端置灰 + 给说明。
type zcodeFormatView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Note string `json:"note"`
	OK   bool   `json:"ok"`
}

// adminZCodeCreate 一键建供应商：写 providerRules，然后按新写的模型补上下文规则。
//
// 分两步而不是合成一个大 diff：建 provider 与同步上下文关心的字段不同
// （前者 api/baseUrl/access，后者 contextWindow），分开算各自的 diff 更清楚，
// 出错时也能定位是哪一步坏的。落盘顺序是先 provider 后上下文——
// 反过来的话，第二步失败会留下一个"有 provider 但上下文全是 200k 兜底"的中间态。
func (h *Handler) adminZCodeCreate(w http.ResponseWriter, r *http.Request) {
	path := ZCodeConfigPath(h.cfg.ZCodeConfigPath)
	var spec ZCodeProviderSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "zcode_create_bad_body", err.Error(),
			"请求体应是 {name, base_url, api_type, api_key, models[]}")
		return
	}
	// base_url 缺省用网关自己的地址，用户想给 ZCode 单独出口就在表单里改。
	if strings.TrimSpace(spec.BaseURL) == "" {
		spec.BaseURL = h.suggestedBaseURL()
	}
	// 模型名必须是网关当前真在发的（防手滑填一个 404 的名字进配置）。
	meta := h.zcodeMetaOf()
	var valid []string
	for _, m := range spec.Models {
		if _, ok := meta[m]; ok {
			valid = append(valid, m)
		}
	}
	if len(valid) == 0 {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "zcode_create_no_model",
			"一个模型都没选，或选的模型都不在网关当前发布清单里",
			"只勾「模型发布清单」里已勾选的模型；也可以先不选模型、只建供应商，之后再在 ZCode 里挑")
		return
	}
	spec.Models = valid

	// changed=false 表示 provider 已是自己要的样子（幂等重放），照样继续走上下文同步。
	_, pid, _, err := UpsertZCodeProvider(path, spec)
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "zcode_create_failed", err.Error(),
			"Base URL 填网关地址（如 127.0.0.1:7864/v1）；providerId 与已有的冲突且指向别处时换个 id")
		return
	}
	// 建完 provider 再算上下文计划（新 provider 的 personalModelIds 已被读进来了）。
	plan, perr := PlanZCode(path, h.suggestedBaseURL(), meta)
	if perr != nil || plan == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"created": true, "provider_id": pid, "context_changed": false,
			"message": "供应商已建；上下文同步这一步没成功，可点「预览改动」重试", "plan": plan,
		})
		return
	}
	ctxApplied := false
	if plan.Changed {
		if aerr := ApplyZCode(plan); aerr == nil {
			ctxApplied = true
		}
	}
	fresh, _ := PlanZCode(path, h.suggestedBaseURL(), meta)
	msg := "供应商已建（" + spec.Name + " · " + pid + "）。"
	if ctxApplied {
		msg += "上下文已一并写入，重启 ZCode 生效。"
	} else if plan.Changed {
		msg += "但上下文同步没成功，点「预览改动」看看。"
	} else {
		msg += "上下文已是最新。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"created": pid != "", "provider_id": pid, "context_changed": ctxApplied,
		"models": valid, "message": msg, "plan": fresh,
	})
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

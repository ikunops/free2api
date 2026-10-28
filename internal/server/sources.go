// sources.go 「获取源」管理端点：把本机各类 AI 工具的登录态一眼看清、一键取进来。
//
// 端点（全部随 config admin.enabled 开关条件注册，未开启时 404）：
//
//	GET  /admin/sources                       → 源清单（含可用性/数量/备注）
//	GET  /admin/sources/{kind}/candidates     → 某源下的候选账号
//	POST /admin/sources/import                → 导入（kind + ids 或 text）
//
// 鉴权策略（与 /status 同源，另加本地例外）：
// 本机（127.0.0.1 / ::1）直连免 key——管理页默认就在本机开，要求先配 key
// 再登录属于自找麻烦；非本机来源（端口被反代/暴露给局域网）仍走 api_key 校验。
// 这是「开箱即用」与「暴露后仍安全」的折中：默认只听本机，就没门槛。
package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"free2api/internal/source"
	"free2api/internal/upstream"
)

// sourceJSONMaxBytes 导入请求体上限（粘贴的账本/快照通常 < 1MB；8MB 足够宽裕）。
const sourceJSONMaxBytes = 8 << 20

// adminSources 列出本机所有可用的源 + 应用内登录态探查结论 + 来源台账概览。
func (h *Handler) adminSources(w http.ResponseWriter, r *http.Request) {
	kinds := source.DetectWithZCodeAppDir(h.cfg.WBLedgerPath, h.cfg.ZCodeDir, h.zcodeAppDir(), h.cfg.AuthDir)
	reg := source.LoadRegistry(h.cfg.AuthDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_dir": h.cfg.AuthDir,
		"kinds":    kinds,
		// probes 是「应用内登录态」这一路的探查明细：本机装了哪些工具、
		// 登录态在哪、什么格式、能不能直接读。全部来自真实 os.Stat/ReadFile。
		"probes": source.ProbeWithZCodeAppDir(h.zcodeAppDir()),
		// 三种来源的展示顺序 + 中文名由后端定死，前端不再自己写一份枚举。
		"method_order":  source.MethodOrder,
		"method_labels": methodLabels(),
		"origin_counts": reg.Counts(),
		"origin_path":   reg.Path(),
	})
}

// methodLabels 三种来源的中文名映射。
func methodLabels() map[string]string {
	out := make(map[string]string, len(source.MethodOrder))
	for _, m := range source.MethodOrder {
		out[m] = source.MethodLabel(m)
	}
	return out
}

// producerTotalsView /status 的「按客户端分组」计数：pool 只给 (producer,total,healthy)，
// 显示名在这里补——前端不各写一份枚举，加客户端时只改 internal/source 一处。
// name 对空 producer 显示「未标注」（历史上没有台账标注的凭证），不是「未知」。
//
// usable = 「健康 且 至少能跑一个模型」的号数，与 healthy 是两回事：zcode 的号状态机
// 全健康，但套餐只给其中一个号发了 glm-5.3-flash，真正能出流量的只有 1 个。判定需要
// 「这个号自己有没有模型」这类 server 侧数据（zcode 权益 / 各家免费目录），所以不在
// pool 里算，放在这里。
func (h *Handler) producerTotalsView() []map[string]any {
	stats := h.cfg.Pool.ProducerTotals()
	usable := h.usableCountsByProducer()
	out := make([]map[string]any, 0, len(stats))
	for _, s := range stats {
		name := source.ProducerLabel(s.Producer)
		if s.Producer == "" {
			name = "未标注"
		}
		u := 0
		if s.Servable {
			u = usable[s.Producer]
		}
		out = append(out, map[string]any{
			"producer": s.Producer,
			"name":     name,
			"total":    s.Total,
			"healthy":  s.Healthy,
			"usable":   u,
			"servable": s.Servable,
		})
	}
	return out
}

// usableCountsByProducer 统计每个来源「健康 且 至少能跑一个模型」的账号数。
//
// 口径按来源分（用户确认）：
//   - workbuddy：目录里有免费模型（-free）→ 健康号即可用（号之间对免费模型可互换）；
//     没有免费模型时才看 credits > 0。
//   - zcode：看该号自己的套餐权益（entitled_models 非空），并且必须有近 24h 的
//     真实 chat 成功证据——套餐权益按账号发，静态反代配额还可能独立失效。
//   - opencode / kilo：匿名免费层，健康即可用。
//   - qoder：登录态不区分套餐权益，能解出 device token 就能出流量，健康即可用。
func (h *Handler) usableCountsByProducer() map[string]int {
	out := map[string]int{}
	if h.cfg.Pool == nil {
		return out
	}
	// 兼容升级前的运行态：历史上真实成功过的 ZCode 号先从号池补种 route 证据。
	// 只补有强证据的项，不把“健康”当成“能出流”。
	h.seedZCodeRoutesFromPool()
	// workbuddy 免费目录：CN / 国际任一份里有 -free 模型，健康号就算可用。
	wbFree := false
	for _, mi := range cachedModelsSnapshot() {
		if mi.Free {
			wbFree = true
			break
		}
	}
	if !wbFree && h.cfg.Upstream != nil {
		for _, mi := range h.cfg.Upstream.GlobalModelInfosSnapshot() {
			if mi.Free {
				wbFree = true
				break
			}
		}
	}
	for _, st := range h.cfg.Pool.List() {
		if st.Disabled || st.ManualDisabled || st.Cooling {
			continue
		}
		switch st.Producer {
		case source.ProducerWorkbuddy:
			if wbFree || st.Credits > 0 {
				out[st.Producer]++
			}
		case source.ProducerZCode:
			if ok, present := zcodeAccountUsable(st.UID); present && ok {
				out[st.Producer]++
			}
		case source.ProducerQoder:
			// Qoder 登录态不区分套餐权益：能解出 device token 就能出流量，
			// 健康即算可用（真失败由 chat 侧的冷却/熔断收敛）。
			out[st.Producer]++
		case source.ProducerOpenCode, source.ProducerKilo:
			out[st.Producer]++
		}
	}
	return out
}

// adminSourceOrigins 返回来源台账（uid → 来源归属），供管理页把号池按来源分组。
func (h *Handler) adminSourceOrigins(w http.ResponseWriter, r *http.Request) {
	reg := source.LoadRegistry(h.cfg.AuthDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"origins":      reg.All(),
		"counts":       reg.Counts(),
		"path":         reg.Path(),
		"method_order": source.MethodOrder,
	})
}

// adminSourceCandidates 列某源下的候选账号。
func (h *Handler) adminSourceCandidates(w http.ResponseWriter, r *http.Request) {
	kind := strings.TrimSpace(r.PathValue("kind"))
	var (
		cands []source.Candidate
		err   error
		note  string
	)
	switch kind {
	case "workbuddy-desktop":
		// WorkBuddy 桌面端登录态（不依赖 wb-switch）：直读客户端自己的 .info 快照，
		// 含 5.6.2 起的字段级解密（见 internal/source/workbuddy_desktop.go）。
		cands, err = source.WbDeskCandidates(h.wbDeskAuthDir(), h.cfg.AuthDir)
		if err == nil && len(cands) == 0 {
			note = "登录态目录里没有可用账号：先在 WorkBuddy 桌面端登录一次"
		}
	case "wb-switch":
		cands, err = source.ListWBSwitchMarked(h.wbLedgerPath(), h.cfg.AuthDir)
	case "auths":
		cands, err = source.ListAuths(h.cfg.AuthDir)
	case "kilo":
		// Kilo Code：免费层无需账号，合成一条「匿名」候选即可。
		cands, err = source.KiloCandidates(h.cfg.AuthDir)
	case "opencode":
		// OpenCode（CLI/TUI）：账号来自本机 ~/.local/share/opencode/auth.json，
		// 只取 OpenCode 自家 provider（opencode=Zen / opencode-go）。第三方 provider
		// 是「agent 上的模型」，不属于账号自带能力（见 internal/source/opencode.go）。
		cands, err = source.OpenCodeCandidates(h.openCodeAuthPath(), h.cfg.AuthDir)
		if err == nil && len(cands) == 0 {
			note = "auth.json 里没有 OpenCode 自家的凭据；第三方 provider（openrouter / deepseek 等）不算账号自带能力，不收"
		}
	case "zcode-app":
		h.zcodeAppCandidatesView(w, r)
		return
	case "qoder-app":
		// Qoder 应用登录态（method=app，最多一个号）。
		cands, err = source.QoderCandidates(source.QoderAppDir(), h.cfg.AuthDir)
		if err == nil && len(cands) == 0 {
			note = "没读到 Qoder 登录态：先在 Qoder 客户端登录"
		}
	case "zcode-switch":
		// zcode 现在是**可取用**的源（上游适配器见 internal/upstream/zcode.go）：
		// 账号自带的智谱 apiKey 抽出来即能反代，与 workbuddy 一样可导入号池。
		// 该分支不走通用 Candidate 形态，而是直接给出带凭据指纹/模型清单/探活/
		// 额度的增强视图（前端据此画 zcode 卡片）。
		h.zcodeCandidatesView(w, r)
		return
	case "paste":
		note = "粘贴导入没有候选列表：请直接把 JSON 贴到导入框"
	default:
		writeOpenAIError(w, http.StatusNotFound, "not_found", "unknown source kind: "+kind)
		return
	}
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "source_error", err.Error(),
			"检查路径是否存在、JSON 是否为数组或对象")
		return
	}
	if cands == nil {
		cands = []source.Candidate{} // 空数组而非 null：前端 .map 不炸
	}
	// 导入 0 条（如账本为空）也要给 note，别让前端显示成"加载失败"。
	if len(cands) == 0 && note == "" {
		note = "该源当前没有可导入的账号"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":       kind,
		"candidates": cands,
		"note":       note,
	})
}

// zcodeCandidatesView zcode 取源清单（增强视图）。
//
// 与其他源的差别：zcode 的「候选」不只是 id/label，还要能看清**这把凭据到底能不能用**
// ——所以这里把凭据指纹、账号自带模型清单、上游 base 一并透出，并在 ?probe=1 时主动
// 探活（?balance=1 时顺带读额度）。默认都不探（保持页面秒开）：探活是每个号一次外网
// 往返，10 个号就是 10 次，不该在打开页面时静默发生。
//
// 安全：凭据明文绝不进响应，只给前 6 后 4 的指纹（api_key_hint）。探活/额度都在服务端
// 做，key 不出进程。
func (h *Handler) zcodeCandidatesView(w http.ResponseWriter, r *http.Request) {
	dir := h.zcodeDir()
	accounts, err := source.ListZCodeAccounts(dir)
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "source_error", err.Error(),
			"检查 config.source.zcode_dir 或本机 ~/.zcode-switch/accounts 是否存在")
		return
	}
	h.writeZCodeCandidates(w, r, "zcode-switch", dir, accounts)
}

// zcodeAppCandidatesView ZCode 应用当前登录态（method=app，最多一个号）。
func (h *Handler) zcodeAppCandidatesView(w http.ResponseWriter, r *http.Request) {
	dir := h.zcodeAppDir()
	acc, err := source.LoadZCodeAppAccount(dir)
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "source_error", err.Error(),
			"先登录 ZCode 应用；或检查 config.source.zcode_app_dir")
		return
	}
	h.writeZCodeCandidates(w, r, "zcode-app", dir, []source.ZCodeAccount{acc})
}

// writeZCodeCandidates zcode 取源清单的共用增强视图。
// ?probe=1 只打 /models（零计费）；?chat=1 才发真实最小 chat（会消耗极少量 token），
// 必须由用户显式点击；?balance=1 只读套餐/余额。
func (h *Handler) writeZCodeCandidates(w http.ResponseWriter, r *http.Request, kind, dir string, accounts []source.ZCodeAccount) {
	doProbe := truthyQuery(r, "probe")
	doChat := truthyQuery(r, "chat")
	doBalance := truthyQuery(r, "balance")
	// 「已在号池」判定：按 uid 比对 auths 目录里的凭证文件（与导入落盘规则同一派生）。
	existing := map[string]bool{}
	if cands, lerr := source.ListAuths(h.cfg.AuthDir); lerr == nil {
		for _, c := range cands {
			existing[c.ID] = true
		}
	}

	out := make([]map[string]any, 0, len(accounts))
	usable := 0
	for _, acc := range accounts {
		uid := source.ZCodeUID(acc.ID)
		importedAs := ""
		if existing[uid] {
			importedAs = filepath.Base(source.AuthFileName(uid))
		}
		row := map[string]any{
			"id":          acc.ID,
			"label":       acc.Name,
			"realm":       "zcode",
			"producer":    source.ProducerZCode,
			"provider":    acc.Provider,
			"encrypted":   acc.Encrypted,
			"usable":      false,
			"has_refresh": false,
			"imported":    existing[uid],
			"imported_as": importedAs,
		}
		models := make([]map[string]any, 0, len(acc.Models))
		ids := make([]string, 0, len(acc.Models))
		for _, m := range acc.Models {
			ids = append(ids, m.ID)
			models = append(models, map[string]any{
				"id": m.ID, "context": m.Context, "output": m.Output,
				"image": m.Image, "reasoning": m.Reasoning,
			})
		}
		row["models"] = models
		row["model_ids"] = ids

		// 读额度凭据（zcodejwttoken + device mid）的就位情况：管理页要能一眼看出
		// 「这个号现在读得到额度吗、读得到的话凭据是哪来的」。
		switch {
		case strings.TrimSpace(acc.JWT) != "":
			row["quota_ready"] = true
			row["quota_cred"] = "登录态"
			if strings.TrimSpace(acc.DeviceMID) != "" {
				row["device_mid"] = acc.DeviceMID
			}
		case acc.JWTEncrypted:
			row["quota_error"] = "zcodejwttoken 是 enc:v1 密文，本机解不开（换机器导出过）"
		default:
			row["quota_error"] = "没有 zcodejwttoken，读不到这个号的套餐到期与余额"
		}

		key, hasKey := acc.UsableKey()
		row["credential_complete"] = hasKey && key.IDSecret
		switch {
		case hasKey:
			detail := "provider=" + acc.Provider + " · key=" + key.Provider
			if key.IDSecret {
				detail += "（id.secret，仅表示凭据形态完整；能否调用还要看真实 chat 探活与静态余额）"
			} else {
				detail += "（裸 32 位，实测已过期/不完整）"
			}
			row["detail"] = detail
			row["upstream"] = key.BaseURL
			row["api_key_hint"] = source.KeyHint(key.APIKey)
		case acc.Encrypted:
			row["detail"] = "上游凭据被 enc:v1 字段级加密，本机解不开（密钥按机器 + 用户名派生，换机器必然解不开）"
		default:
			row["detail"] = "没有指向智谱（open.bigmodel.cn / api.z.ai）的静态 API key；JWT/登录 token 不能当 OpenAI 兼容 key 使用"
		}
		chatModel := firstZCodeModel(acc.Models)
		if hasKey && doProbe {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			pr := upstream.ProbeZCodeKey(ctx, h.cfg.Upstream.HTTP, key.BaseURL, key.APIKey)
			cancel()
			row["probe"] = map[string]any{
				"status": string(pr.Status), "http": pr.HTTP, "msg": pr.Msg, "models": pr.Models,
			}
			if len(pr.Models) > 0 {
				row["live_models"] = pr.Models
			}
		}
		if hasKey && doChat {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			cp := upstream.ProbeZCodeStaticChat(ctx, h.cfg.Upstream.HTTP, key.BaseURL, key.APIKey, chatModel)
			cancel()
			row["static_probe"] = map[string]any{
				"status": string(cp.Status), "http": cp.HTTP, "msg": cp.Msg, "model": chatModel,
			}
			row["static_usable"] = cp.Status == upstream.ZCodeProbeOK
			if cp.Status == upstream.ZCodeProbeOK {
				row["usable"] = true
				usable++
			}
		}
		if doBalance && acc.JWT != "" {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			bal, berr := h.cfg.Upstream.FetchZCodeBalance(ctx, acc.JWT, acc.DeviceMID)
			cancel()
			if berr != nil {
				row["quota_error"] = berr.Error()
			} else {
				row["quota"] = bal
				if ms := bal.EntitledModels(); len(ms) > 0 {
					row["entitled_models"] = ms
				}
				if ps := bal.PlanSummary(); ps != "" {
					row["plan_summary"] = ps
				}
			}
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"kind":        kind,
		"candidates":  out,
		"dir":         dir,
		"total":       len(out),
		"usable":      usable,
		"probed":      doProbe,
		"chat_probed": doChat,
		"balanced":    doBalance,
		"note": "zcode 账号走智谱（BigModel / Z.ai）链路：完整 id.secret 只代表凭据形态可用。" +
			"「探活全部」只打 /models（零计费）；「试静态 chat」才会真实消耗极少量 token；" +
			"返回 429/1113 就是没有静态 API 资源包，需去 BigModel / Z.ai 控制台开通或充值。",
	})
}

// firstZCodeModel 选一个最小静态 chat 探活模型：优先 flash（最便宜），否则第一个。
func firstZCodeModel(models []source.ZCodeModel) string {
	for _, m := range models {
		if strings.Contains(strings.ToLower(m.ID), "flash") {
			return m.ID
		}
	}
	if len(models) > 0 {
		return models[0].ID
	}
	return "glm-5.3-flash"
}

// ctxForProbe 复用请求上下文，但把真实 chat 探活的超时独立出来；/models 已经
// 用掉一部分时间，不能让它把第二次请求挤到没有预算。
func ctxForProbe(r *http.Request) context.Context {
	parent := context.Background()
	if r != nil {
		parent = r.Context()
	}
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	_ = cancel // 探活函数自带 client timeout；这里不提前取消，避免请求刚发出就被切断
	return ctx
}

// truthyQuery 解析 "1"/"true"/"on"/"yes" 为真（管理页勾选框回传的就是 1）。
func truthyQuery(r *http.Request, key string) bool {
	v := strings.ToLower(strings.TrimSpace(r.URL.Query().Get(key)))
	return v == "1" || v == "true" || v == "on" || v == "yes"
}

// adminSourceImport 导入：kind=wb-switch 读账本；kind=import 用 body.text。
// ids 为空 = 全量导入；非空 = 只导命中项。导入后立即热加载账号池。
func (h *Handler) adminSourceImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind string   `json:"kind"`
		IDs  []string `json:"ids"`
		Text string   `json:"text"`
		// Method 账号归属哪种来源（app / switch / file）。留空按 kind 推断：
		// 账本类 → switch，粘贴类 → file。前端「导入时选择来源」传的就是它。
		Method string `json:"method"`
		Detail string `json:"detail"`
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, sourceJSONMaxBytes))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "body must be JSON: "+err.Error())
			return
		}
	}

	opt := source.ImportOptions{
		Method: strings.TrimSpace(body.Method),
		Detail: strings.TrimSpace(body.Detail),
	}
	var res source.ImportResult
	switch strings.TrimSpace(body.Kind) {
	case "", "wb-switch", "switch", "current":
		if opt.Method == "" {
			opt.Method = source.MethodSwitch
		}
		res, err = source.ImportWBSwitchAs(h.wbLedgerPath(), h.cfg.AuthDir, body.IDs, opt)
	case "workbuddy-desktop":
		if opt.Method == "" {
			opt.Method = source.MethodApp
		}
		res, err = source.ImportWorkBuddyDesktopAs(h.wbDeskAuthDir(), h.cfg.AuthDir, body.IDs, opt)
	case "zcode-app":
		if opt.Method == "" {
			opt.Method = source.MethodApp
		}
		res, err = source.ImportZCodeAppAs(h.zcodeAppDir(), h.cfg.AuthDir, body.IDs, opt)
	case "qoder-app":
		if opt.Method == "" {
			opt.Method = source.MethodApp
		}
		res, err = source.ImportQoderAs(source.QoderAppDir(), h.cfg.AuthDir, body.IDs, opt)
	case "zcode-switch", "zcode":
		if opt.Method == "" {
			opt.Method = source.MethodSwitch
		}
		if opt.Producer == "" {
			opt.Producer = source.ProducerZCode
		}
		res, err = source.ImportZCodeAs(h.zcodeDir(), h.cfg.AuthDir, body.IDs, opt)
	case "opencode":
		if opt.Method == "" {
			opt.Method = source.MethodApp
		}
		if opt.Producer == "" {
			opt.Producer = source.ProducerOpenCode
		}
		res, err = source.ImportOpenCodeAs(h.openCodeAuthPath(), h.cfg.AuthDir, body.IDs, opt)
	case "kilo":
		// Kilo 免费层无需账号：导入的就是那条匿名占位号（幂等）。
		if opt.Method == "" {
			opt.Method = source.MethodApp
		}
		if opt.Producer == "" {
			opt.Producer = source.ProducerKilo
		}
		res, err = source.ImportKilo(h.cfg.AuthDir, body.IDs, opt)
	case "import", "paste":
		if strings.TrimSpace(body.Text) == "" {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "text is required for kind=import")
			return
		}
		if opt.Method == "" {
			opt.Method = source.MethodFile
		}
		res, err = source.ImportJSONAs(h.cfg.AuthDir, []byte(body.Text), body.IDs, opt)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "unknown kind: "+body.Kind)
		return
	}
	if err != nil {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "import_failed", err.Error(),
			"检查该来源在本机的路径是否存在、JSON 是否为数组或对象。"+
				"wb-switch 账本默认 ~/.wb-switch/accounts.json；OpenCode 默认 ~/.local/share/opencode/auth.json；"+
				"WorkBuddy 桌面端登录态默认 %LOCALAPPDATA%/CodeBuddyExtension/Data/Public/auth")
		return
	}

	// 导入后立刻对齐账号池：管理页看到"已导入 N 个"时，池子已经是新状态。
	if len(res.Imported) > 0 && h.cfg.Pool != nil {
		h.cfg.Pool.RescanAuthDir(h.cfg.AuthDir)
	}
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	writeJSON(w, http.StatusOK, map[string]any{
		"result":  res,
		"pool":    map[string]int{"total": total, "healthy": healthy},
		"origins": source.LoadRegistry(h.cfg.AuthDir).Counts(),
	})
}

// wbDeskAuthDir WorkBuddy 桌面端登录态目录（配置覆盖 > 本机缺省）。
func (h *Handler) wbDeskAuthDir() string {
	if p := strings.TrimSpace(h.cfg.WbDeskAuthDir); p != "" {
		return p
	}
	return source.WbDeskAuthDir("")
}

// wbLedgerPath wb-switch 账本路径（配置覆盖 > 默认 ~/.wb-switch/accounts.json）。
func (h *Handler) wbLedgerPath() string {
	if strings.TrimSpace(h.cfg.WBLedgerPath) != "" {
		return h.cfg.WBLedgerPath
	}
	return source.DefaultWBSwitchPath()
}

// openCodeAuthPath OpenCode auth.json 路径（config 覆盖 > 本机默认）。
func (h *Handler) openCodeAuthPath() string {
	if p := strings.TrimSpace(h.cfg.OpenCodeAuthPath); p != "" {
		return p
	}
	return source.DefaultOpenCodeAuthPath()
}

// zcodeDir zcode-switch 账号目录（config 指定优先，否则本机默认路径）。
func (h *Handler) zcodeDir() string {
	if strings.TrimSpace(h.cfg.ZCodeDir) != "" {
		return h.cfg.ZCodeDir
	}
	return source.DefaultZCodeSwitchDir()
}

// zcodeAppDir ZCode 应用自己的登录态目录（~/.zcode/v2）。
// 只在「号池文件 + zcode 账本都没有凭据」时被读（见 credits.go 的 resolveZCodeCreds），
// 是读额度凭据的第三层兜底；它只覆盖应用里当前登录的那一个号。
func (h *Handler) zcodeAppDir() string {
	if strings.TrimSpace(h.cfg.ZCodeAppDir) != "" {
		return h.cfg.ZCodeAppDir
	}
	return source.ZCodeAppCredDir()
}

// withLocalOrAuth 本机直连免 key，其余走 withAuth。
// 判定只看 TCP 对端地址（RemoteAddr），不信任 X-Forwarded-For——后者可伪造，
// 信任它等于把管理面白送出去。
func (h *Handler) withLocalOrAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if remoteIsLoopback(r.RemoteAddr) {
			next(w, r)
			return
		}
		h.withAuth(next)(w, r)
	}
}

// remoteIsLoopback 判断 TCP 对端是否回环地址。
func remoteIsLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr // 无端口形态（测试里常见）
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

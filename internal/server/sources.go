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
	kinds := source.Detect(h.cfg.WBLedgerPath, h.cfg.ZCodeDir, h.cfg.AuthDir)
	reg := source.LoadRegistry(h.cfg.AuthDir)
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_dir": h.cfg.AuthDir,
		"kinds":    kinds,
		// probes 是「应用内登录态」这一路的探查明细：本机装了哪些工具、
		// 登录态在哪、什么格式、能不能直接读。全部来自真实 os.Stat/ReadFile。
		"probes": source.Probe(),
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
func (h *Handler) producerTotalsView() []map[string]any {
	stats := h.cfg.Pool.ProducerTotals()
	out := make([]map[string]any, 0, len(stats))
	for _, s := range stats {
		name := source.ProducerLabel(s.Producer)
		if s.Producer == "" {
			name = "未标注"
		}
		out = append(out, map[string]any{
			"producer": s.Producer,
			"name":     name,
			"total":    s.Total,
			"healthy":  s.Healthy,
			"servable": s.Servable,
		})
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
	case "wb-switch":
		cands, err = source.ListWBSwitchMarked(h.wbLedgerPath(), h.cfg.AuthDir)
	case "auths":
		cands, err = source.ListAuths(h.cfg.AuthDir)
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
	doProbe := truthyQuery(r, "probe")
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
			"imported_as": filepath.Base(source.AuthFileName(uid)),
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
			row["quota_cred"] = "zcode 账本"
			if strings.TrimSpace(acc.DeviceMID) != "" {
				row["device_mid"] = acc.DeviceMID
			}
		case acc.JWTEncrypted:
			row["quota_error"] = "账本里的 zcodejwttoken 是 enc:v1 密文，本机解不开（换机器导出过）"
		default:
			row["quota_error"] = "账本里没有 zcodejwttoken，读不到这个号的套餐到期与余额"
		}

		key, hasKey := acc.UsableKey()
		switch {
		case hasKey:
			row["usable"] = true
			usable++
			detail := "provider=" + acc.Provider + " · key=" + key.Provider
			if key.IDSecret {
				detail += "（id.secret）"
			} else {
				detail += "（裸 32 位，实测已过期/不完整）"
			}
			row["detail"] = detail
			row["upstream"] = key.BaseURL
			row["api_key_hint"] = source.KeyHint(key.APIKey)
		case acc.Encrypted:
			row["detail"] = "上游凭据被 enc:v1 字段级加密，本机解不开（密钥按机器 + 用户名派生，换机器必然解不开）；要取这个号的上游 key 得在装 zcode 的那台机器上操作"
		default:
			row["detail"] = "没有指向智谱（open.bigmodel.cn / api.z.ai）的可用凭据：该号只有 z-code 计划 JWT（zcode.z.ai 链路被 captcha 拦）或用户自加的第三方网关 key，不能反代"
		}
		if hasKey && doProbe {
			ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
			pr := upstream.ProbeZCodeKey(ctx, h.cfg.Upstream.HTTP, key.BaseURL, key.APIKey)
			cancel()
			row["probe"] = map[string]any{
				"status": string(pr.Status), "http": pr.HTTP, "msg": pr.Msg, "models": pr.Models,
			}
			// 探活拿到实时清单时以它为准（账号文件里的清单是登录当时的快照）。
			if len(pr.Models) > 0 {
				row["live_models"] = pr.Models
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
				// 这才是「这个账号当前真正能用哪些模型」：由套餐/额度的
				// capabilities 决定，不是 /models 能列出什么（周末活动的号
				// 能列 11 个，但只有 glm-5.3-flash 走额度覆盖）。
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
		"kind":       "zcode-switch",
		"candidates": out,
		"dir":        dir,
		"total":      len(out),
		"usable":     usable,
		"probed":     doProbe,
		"balanced":   doBalance,
		"note": "zcode 账号走智谱（BigModel / Z.ai）链路，凭据可直接反代：抽取账号自带的 " +
			"id.secret 作为 Bearer 打 OpenAI 兼容端点。默认不探活；加 ?probe=1 现场验证凭据、" +
			"?balance=1 顺带读额度。",
	})
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
	case "zcode-switch", "zcode":
		if opt.Method == "" {
			opt.Method = source.MethodSwitch
		}
		if opt.Producer == "" {
			opt.Producer = source.ProducerZCode
		}
		res, err = source.ImportZCodeAs(h.zcodeDir(), h.cfg.AuthDir, body.IDs, opt)
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
			"账本路径/JSON 形状不对；wb-switch 账本默认在 ~/.wb-switch/accounts.json")
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

// wbLedgerPath wb-switch 账本路径（配置覆盖 > 默认 ~/.wb-switch/accounts.json）。
func (h *Handler) wbLedgerPath() string {
	if strings.TrimSpace(h.cfg.WBLedgerPath) != "" {
		return h.cfg.WBLedgerPath
	}
	return source.DefaultWBSwitchPath()
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

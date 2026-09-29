// admin.go 运维管理端点（issue #138 / #118）：账号的临时停用 / 恢复 / 复活。
//
// 设计要点（与维护者在 issue #118 预告的方案一致）：
//   - 手动停用是**独立状态位** manual_disabled，与自动禁用 disabled 并列、互不影响。
//     复用同一字段会让运维意图被签到解冻、refresh 成功等自动复活路径意外解除。
//   - 语义是「对话流量摘除」而非「账号冻结」：不碰冷却/熔断维度，签到与保活照常，
//     凭证和积分都是活的；恢复时拿到的是停用期间真实发生的状态。
//   - 默认关闭（config admin.enabled），开启后与 /status 共用同一把 api_key 鉴权。
//   - 幂等：面板重试不会报错；重复调用只更新原因文案。
package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"free2api/internal/pool"
)

// adminState 管理端点的统一响应体：回显操作后的双位状态，面板据此直接更新 UI，
// 不必再打一次 /status。
type adminState struct {
	UID            string `json:"uid"`
	ManualDisabled bool   `json:"manual_disabled"`
	ManualReason   string `json:"manual_reason,omitempty"`
	// Disabled 保留在响应里让面板能区分「手动摘除」与「系统判定坏了」——
	// 恢复按钮的语义对两者不同（enable 解手动位，revive 解自动位）。
	Disabled bool `json:"disabled"`
	Changed  bool `json:"changed"`
}

// adminUID 提取并校验路径段 uid。返回 false 表示已写出响应（uid 为空 → 400），
// 调用方应直接 return。
// 开关判断不在这里：路由按 cfg.AdminEnabled 条件注册（见 NewHandler），
// 未开启时这些 handler 根本不可达——handler 内再判开关是多余的存在性泄露面。
func adminUID(w http.ResponseWriter, r *http.Request) (string, bool) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return "", false
	}
	return uid, true
}

// adminReasonFromBody 读可选 JSON 体里的 reason 字段。
// 空体/非 JSON/无该字段都返回空串（端点不因体格式拒绝——无体是最常见调用形态）。
func adminReasonFromBody(r *http.Request) string {
	if r.Body == nil {
		return ""
	}
	// 限制读取量：reason 是短文本，避免畸形大请求占用内存。
	raw, err := io.ReadAll(io.LimitReader(r.Body, 4<<10))
	if err != nil || len(raw) == 0 {
		return ""
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Reason)
}

// adminAccountDisable 手动停用：把账号摘出选号池，但保留在池里
// （状态/冷却/成本台账继续归它管，签到与保活照常）。
func (h *Handler) adminAccountDisable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	reason := adminReasonFromBody(r)
	if reason == "" {
		reason = "manual"
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, true, reason)
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, stopReason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: stopReason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountEnable 解除手动停用。若账号仍被系统自动禁用（disabled），它**不会**
// 因此回到选号池——那需要 revive。响应里的 disabled 字段就是给面板看的提示。
func (h *Handler) adminAccountEnable(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	found, changed := h.cfg.Pool.SetManualDisabled(uid, false, "")
	if !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// adminAccountRevive 解除系统自动禁用（清 disabled + reason + 连续 12153 计数）。
// 不碰手动停用位：运维明确摘除的号不应被一次 revive 悄悄放回选号池。
func (h *Handler) adminAccountRevive(w http.ResponseWriter, r *http.Request) {
	uid, ok := adminUID(w, r)
	if !ok {
		return
	}
	if _, _, found := h.cfg.Pool.ManualDisabledState(uid); !found {
		writeOpenAIError(w, http.StatusNotFound, "not_found", "account not found: "+uid)
		return
	}
	changed := h.cfg.Pool.ReviveDisabled(uid)
	stopped, reason, _ := h.cfg.Pool.ManualDisabledState(uid)
	writeJSON(w, http.StatusOK, adminState{
		UID: uid, ManualDisabled: stopped, ManualReason: reason,
		Disabled: h.accountAutoDisabled(uid), Changed: changed,
	})
}

// accountAutoDisabled 读某账号当前的自动禁用位（供响应回显）。
// 复用 Pool.List 的单账号查询：轮询全部账号在小池下开销可忽略，
// 且避免为此在 pool 上再开一个只读访问器（保持接口面最小）。
func (h *Handler) accountAutoDisabled(uid string) bool {
	for _, st := range h.cfg.Pool.List() {
		if st.UID == uid {
			return st.Disabled
		}
	}
	return false
}

// adminAccountsReset 运行态复位（清冷却 + 清失败计数）。作用域三选一：
//
//	{}                       → 全池
//	{"uid":"<uid>"}          → 单个
//	{"uids":["...","..."]}   → 点名一批
//	{"producer":"zcode"}     → 某个 AI 客户端（按来源台账的归属筛）
//
// 为什么要有它：429 / 5xx / WAF 把号打成冷却或熔断后，即使上游已恢复也得等冷却自然
// 到期；排障时「停服 + 删 state.json」既不优雅也会连带丢掉别的状态。这里不掉线复位。
//
// 语义边界（写在这里免得被误用）：
//   - **不解禁**：disabled（系统判坏）与 manual_disabled（运维意图）原样保留，
//     要回池得单独走 revive / enable；响应里的 still_disabled / still_manual_disabled
//     就是给面板提示这件事的。
//   - **不动余额与成本台账**：credits 是上游事实，model_cost 是选号用的历史观测。
//   - 幂等：本来就干净的账号只计入 matched，不计入 reset。
func (h *Handler) adminAccountsReset(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Pool == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable, "no_pool", "本次启动没有挂账号池")
		return
	}
	var body struct {
		UID      string   `json:"uid"`
		UIDs     []string `json:"uids"`
		Producer string   `json:"producer"`
	}
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
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
	}
	var targets []string // nil = 全池
	scope := "all"
	switch {
	case strings.TrimSpace(body.UID) != "":
		targets = []string{strings.TrimSpace(body.UID)}
		scope = "uid"
	case len(body.UIDs) > 0:
		targets = make([]string, 0, len(body.UIDs))
		for _, u := range body.UIDs {
			if u = strings.TrimSpace(u); u != "" {
				targets = append(targets, u)
			}
		}
		scope = "uids"
	case strings.TrimSpace(body.Producer) != "":
		want := strings.TrimSpace(body.Producer)
		// 先置成**非 nil 空切片**：这个客户端一个号都没有时必须「复位 0 个」，
		// 传 nil 会被 ResetRuntime 当成全池（拼错生产者名 → 清掉整池冷却）。
		targets = make([]string, 0)
		for _, st := range h.cfg.Pool.List() {
			if st.Producer == want {
				targets = append(targets, st.UID)
			}
		}
		scope = "producer"
	}
	res := h.cfg.Pool.ResetRuntime(targets)
	out := map[string]any{
		"scope":                 scope,
		"producer":              strings.TrimSpace(body.Producer),
		"matched":               res.Matched,
		"reset":                 res.Reset,
		"still_disabled":        res.StillDisabled,
		"still_manual_disabled": res.StillManualDisable,
		"not_found":             res.NotFound,
		"ts":                    time.Now().Unix(),
	}
	// 一句话结论给面板直接贴：空作用域下 matched=0 通常意味着生产者名写错。
	out["message"] = resetMessage(scope, res)
	writeJSON(w, http.StatusOK, out)
}

// resetMessage 把复位结果压成一句人话（面板 toast 直接显示，不再前端拼一遍）。
func resetMessage(scope string, res pool.ResetResult) string {
	where := map[string]string{"all": "全池", "uid": "指定账号", "uids": "指定账号", "producer": "该客户端"}[scope]
	if where == "" {
		where = "全池"
	}
	msg := fmt.Sprintf("%s：复位 %d / 命中 %d 个账号", where, res.Reset, res.Matched)
	if res.StillDisabled > 0 {
		msg += fmt.Sprintf("；其中 %d 个仍被系统禁用（要单独「复活」）", res.StillDisabled)
	}
	if res.StillManualDisable > 0 {
		msg += fmt.Sprintf("；%d 个仍手动停用（要单独「启用」）", res.StillManualDisable)
	}
	if len(res.NotFound) > 0 {
		msg += fmt.Sprintf("；%d 个 uid 不在池里", len(res.NotFound))
	}
	return msg
}

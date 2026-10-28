package server

// growth_admin.go 成长任务管理面：只读扫描 + 默认 dry-run 的领取。
//
// 不把 scheduler 类型带进 server：gateway 注入两个回调，server 只负责 HTTP/JSON。
// 领取必须显式 dry_run=false 才会写上游，避免页面误点造成写操作。

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func (h *Handler) adminGrowthTasks(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	out, err := h.cfg.GrowthScan(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "growth_scan_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) adminGrowthRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	out, err := h.cfg.GrowthRun(ctx)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "growth_run_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) adminGrowthRunAccount(w http.ResponseWriter, r *http.Request) {
	uid := strings.TrimSpace(r.PathValue("uid"))
	taskCode := strings.TrimSpace(r.URL.Query().Get("task"))
	if uid == "" {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "uid is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()
	out, err := h.cfg.GrowthRunAccount(ctx, uid, taskCode)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		} else if strings.Contains(err.Error(), "不存在") || strings.Contains(err.Error(), "不参与") || strings.Contains(err.Error(), "停用") {
			status = http.StatusBadRequest
		}
		writeOpenAIError(w, status, "growth_run_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *Handler) adminGrowthClaim(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var body struct {
		DryRun *bool `json:"dry_run"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "body must be JSON: "+err.Error())
			return
		}
	}
	dryRun := true
	if body.DryRun != nil {
		dryRun = *body.DryRun
	}
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	out, err := h.cfg.GrowthClaim(ctx, dryRun)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "growth_claim_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

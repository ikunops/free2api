package server

// schedule_admin.go —— 定时任务可见性（GET /admin/schedule）+ 开关（POST /admin/schedule）。
//
// 背景：cmd/server 启动时把 config.json 的 schedule 段喂给 scheduler 起六类定时任务
// （签到 / 猫猫旅行 / 活跃上报 / Token 保活 / 开学季 / 夜猫子）。此前这些只出现在启动日志里，
// 管理页完全看不到「到底开没开、几点跑」。这里把同一份配置读出来给页面展示。
//
// 语义边界：
//   - **只读展示 + 开关写回 config.json**：scheduler 在启动时快照了小时表与开关，
//     进程内没有热重载通道，所以改开关和改端口一样「写入配置、重启生效」——页面会明说，
//     不假装热更新（热更新是骗人的：进程里跑的还是旧排程）。
//   - 小时表（checkin_hours 等）不在页面里改：那是运维级别的排程微调，手改 config.json
//     更可控；页面只负责「看清 + 一键开关」。
//   - 读不到 config.json（嵌入/测试形态）时 readable=false，页面据此提示。

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// scheduleTaskView 单个定时任务在页面上的形态。
type scheduleTaskView struct {
	Key     string `json:"key"`     // checkin / travel / activity / keepalive / school / cat
	Name    string `json:"name"`    // 中文名
	Enabled bool   `json:"enabled"` // 是否启用
	Hours   []int  `json:"hours"`   // 触发整点
	Note    string `json:"note"`    // 一句话说明
	// ReportCount 仅活跃上报用（每号每次上报条数），其余为 0。
	ReportCount int `json:"report_count,omitempty"`
	// Explicit 该开关是否在 config.json 里被显式写过（false = 用内置默认）。
	Explicit bool `json:"explicit"`
}

// defaultScheduleTasks 内置默认（与 internal/config.DefaultSchedule 同口径）。
// 这里复制一份是为了让「config.json 没有 schedule 段」时页面也能显示真实默认值，
// 而不是一片空白。两边漂移的风险由 internal/config/schedule_test.go 的默认值用例兜底。
func defaultScheduleTasks() []scheduleTaskView {
	return []scheduleTaskView{
		{Key: "checkin", Name: "每日签到", Enabled: true, Hours: []int{9, 21}, Note: "签到 + 余额查询解冻"},
		{Key: "travel", Name: "猫猫旅行", Enabled: true, Hours: []int{9, 21}, Note: "独立排程：领养 / 派出 / 领奖"},
		{Key: "activity", Name: "活跃上报", Enabled: true, Hours: []int{10}, Note: "点亮连登 + 补满领猫对话门槛", ReportCount: 5},
		{Key: "keepalive", Name: "Token 保活", Enabled: true, Hours: []int{22}, Note: "刷新 access token，避免掉线"},
		{Key: "school", Name: "开学季任务", Enabled: true, Hours: []int{12}, Note: "school_open_day_2026.py"},
		{Key: "cat", Name: "夜猫子任务", Enabled: true, Hours: []int{1}, Note: "task_runner.py 黑猫"},
	}
}

// scheduleKeys 六个任务的键，与 config.Schedule 的 JSON 字段一一对应。
// hoursKey = "<key>_hours"，enabledKey = "<key>_enabled"。
var scheduleKeys = []string{"checkin", "travel", "activity", "keepalive", "school", "cat"}

// readScheduleConfig 读 config.json 的 schedule 段（原始 JSON），返回解析后的
// map 与「config.json 里有没有 schedule 段」两个信息。读不到时 map 为 nil。
func (h *Handler) readScheduleConfig() (map[string]any, error) {
	if h.cfg.ConfigPath == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil, err
	}
	schedRaw, ok := all["schedule"]
	if !ok || len(schedRaw) == 0 || string(schedRaw) == "null" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(schedRaw, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// scheduleView GET /admin/schedule：六类任务的当前形态 + 配置文件位置。
func (h *Handler) scheduleView() map[string]any {
	tasks := defaultScheduleTasks()
	cfg, err := h.readScheduleConfig()
	readable := true
	var readErr string
	if err != nil {
		readable = false
		readErr = err.Error()
	}
	explicitSection := cfg != nil
	if cfg != nil {
		for i := range tasks {
			t := &tasks[i]
			if v, ok := cfg[t.Key+"_enabled"]; ok {
				if b, ok := v.(bool); ok {
					t.Enabled = b
					t.Explicit = true
				}
			}
			if v, ok := cfg[t.Key+"_hours"]; ok {
				if arr, ok := v.([]any); ok {
					hours := make([]int, 0, len(arr))
					for _, x := range arr {
						if f, ok := x.(float64); ok {
							hours = append(hours, int(f))
						}
					}
					if len(hours) > 0 {
						t.Hours = hours
					}
				}
			}
		}
		if v, ok := cfg["activity_report_count"]; ok {
			if f, ok := v.(float64); ok && f > 0 {
				for i := range tasks {
					if tasks[i].Key == "activity" {
						tasks[i].ReportCount = int(f)
					}
				}
			}
		}
	}
	out := map[string]any{
		"config_path":     h.cfg.ConfigPath,
		"readable":        readable,
		"explicit":        explicitSection,
		"restart_needed":  true,
		"tasks":           tasks,
		"restart_note":    "改开关会写入 config.json，重启网关后生效（定时排程在启动时快照，进程内不热重载）",
		"source_hint":     "小时表（几点跑）请直接改 config.json 的 schedule 段；页面只负责看清与一键开关",
	}
	if readErr != "" {
		out["read_error"] = readErr
	}
	return out
}

// adminScheduleGet GET /admin/schedule。
func (h *Handler) adminScheduleGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, h.scheduleView())
}

// adminSchedulePut POST /admin/schedule：改某个任务的 enabled 开关，写回 config.json。
// body {"key":"checkin","enabled":false}。只动 schedule.<key>_enabled 一个键。
func (h *Handler) adminSchedulePut(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ConfigPath == "" {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "config_readonly", "本次启动未记录 config.json 路径",
			"嵌入/测试形态下改不了定时开关；正常启动的网关可以")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 16<<10))
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	var body struct {
		Key     string `json:"key"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "body must be JSON: "+err.Error())
		return
	}
	key := strings.TrimSpace(body.Key)
	if body.Enabled == nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "enabled 字段必填")
		return
	}
	known := false
	for _, k := range scheduleKeys {
		if k == key {
			known = true
			break
		}
	}
	if !known {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request", "未知任务："+key,
			"合法值："+strings.Join(scheduleKeys, " / "))
		return
	}
	if err := h.writeScheduleEnabled(key, *body.Enabled); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_write_failed", err.Error())
		return
	}
	view := h.scheduleView()
	view["restart_required"] = true
	writeJSON(w, http.StatusOK, view)
}

// writeScheduleEnabled 只替换 config.json 的 schedule.<key>_enabled 键，其余键原样回写。
// 走 map[string]json.RawMessage 保留未知键（与 writeConfigListen 同纪律）。
func (h *Handler) writeScheduleEnabled(key string, enabled bool) error {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return err
	}
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	var sched map[string]json.RawMessage
	if s, ok := all["schedule"]; ok && len(s) > 0 && string(s) != "null" {
		if err := json.Unmarshal(s, &sched); err != nil {
			return err
		}
	}
	if sched == nil {
		sched = map[string]json.RawMessage{}
	}
	enc, err := json.Marshal(enabled)
	if err != nil {
		return err
	}
	sched[key+"_enabled"] = enc
	schedRaw, err := json.Marshal(sched)
	if err != nil {
		return err
	}
	all["schedule"] = schedRaw
	buf, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := h.cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.cfg.ConfigPath)
}

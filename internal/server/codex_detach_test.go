package server

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestPlanCodexDetachRemovesGatewayKeys 还原：顶层 model_provider 必删；顶层 model
// 是网关名（带 realm 冒号）时一并删；用户其它内容与我们的 provider 段原样保留。
func TestPlanCodexDetachRemovesGatewayKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := `model = "global:deepseek-v4.1-flash-free"
model_provider = "free2api"
model_reasoning_effort = "max"

[model_providers.opencode-go]
name = "OpenCode Go"
base_url = "https://opencode.ai/zen/go/v1"

[model_providers.free2api]
name = "Free2API (local)"
base_url = "http://127.0.0.1:7864/v1"
env_key = "WB_API_KEY"
wire_api = "responses"
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanCodexDetach(path)
	if err != nil {
		t.Fatalf("PlanCodexDetach: %v", err)
	}
	if !p.RemovedProvider || !p.RemovedModel {
		t.Fatalf("应删 provider 与 model：%+v", p)
	}
	if regexp.MustCompile(`(?m)^[ \t]*model_provider[ \t]*=`).MatchString(p.After) {
		t.Errorf("After 仍有顶层 model_provider 行:\n%s", p.After)
	}
	if strings.Contains(p.After, "global:deepseek-v4.1-flash-free") {
		t.Errorf("After 仍含网关模型名:\n%s", p.After)
	}
	// 用户内容与我们的段保留
	for _, want := range []string{"model_reasoning_effort", "opencode-go", "[model_providers.free2api]"} {
		if !strings.Contains(p.After, want) {
			t.Errorf("After 丢了 %q:\n%s", want, p.After)
		}
	}
	if !p.HasOurSection {
		t.Errorf("HasOurSection 应为 true")
	}
}

// TestPlanCodexDetachKeepsOfficialModel 顶层 model 是官方名（不含冒号）时必须保留。
func TestPlanCodexDetachKeepsOfficialModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := "model = \"gpt-5.4\"\nmodel_provider = \"free2api\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanCodexDetach(path)
	if err != nil {
		t.Fatalf("PlanCodexDetach: %v", err)
	}
	if p.RemovedModel {
		t.Errorf("官方模型名不该删")
	}
	if p.KeptModel != "gpt-5.4" {
		t.Errorf("KeptModel=%q", p.KeptModel)
	}
	if !strings.Contains(p.After, `model = "gpt-5.4"`) {
		t.Errorf("官方 model 行应保留:\n%s", p.After)
	}
	if regexp.MustCompile(`(?m)^[ \t]*model_provider[ \t]*=`).MatchString(p.After) {
		t.Errorf("model_provider 应删:\n%s", p.After)
	}
}

// TestPlanCodexDetachIdempotent 已经是官方形态时 Changed=false。
func TestPlanCodexDetachIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := "model = \"gpt-5.4\"\n\n[model_providers.free2api]\nname = \"Free2API (local)\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanCodexDetach(path)
	if err != nil {
		t.Fatalf("PlanCodexDetach: %v", err)
	}
	if p.Changed {
		t.Errorf("已是官方形态不该报 Changed:\n%s", p.After)
	}
}

// TestPlanCodexDetachMissingFile 文件不存在：不报错，Changed=false。
func TestPlanCodexDetachMissingFile(t *testing.T) {
	p, err := PlanCodexDetach(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if p.Exists || p.Changed {
		t.Errorf("missing file: %+v", p)
	}
}

// TestApplyCodexDetachBacksUp 落盘：改前先备份，备份内容 = 原文。
func TestApplyCodexDetachBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := "model = \"cn:auto\"\nmodel_provider = \"free2api\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanCodexDetach(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyCodexDetach(p); err != nil {
		t.Fatalf("applyCodexDetach: %v", err)
	}
	bak, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatalf("读备份: %v", err)
	}
	if string(bak) != orig {
		t.Errorf("备份内容应等于原文")
	}
	got, _ := os.ReadFile(path)
	if regexp.MustCompile(`(?m)^[ \t]*model_provider[ \t]*=`).MatchString(string(got)) {
		t.Errorf("落盘后不该含顶层 model_provider:\n%s", string(got))
	}
}

// --- schedule admin ---

// newScheduleTestHandler 造一个带 config.json 的 handler，只挂 schedule 端点。
func newScheduleTestHandler(t *testing.T, cfgJSON string) (*Handler, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(Config{AdminEnabled: true, ConfigPath: path, Listen: "127.0.0.1:7864"})
	return h, path
}

// TestScheduleViewDefaultsNoSection config.json 没有 schedule 段时回显内置默认。
func TestScheduleViewDefaultsNoSection(t *testing.T) {
	h, _ := newScheduleTestHandler(t, `{"listen":"127.0.0.1:7864"}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/schedule", nil))
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var v struct {
		Readable bool `json:"readable"`
		Explicit bool `json:"explicit"`
		Tasks    []struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
			Hours   []int  `json:"hours"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Readable || v.Explicit {
		t.Errorf("readable=%v explicit=%v", v.Readable, v.Explicit)
	}
	if len(v.Tasks) != 6 {
		t.Fatalf("tasks=%d want 6", len(v.Tasks))
	}
	for _, task := range v.Tasks {
		if !task.Enabled {
			t.Errorf("默认应全部启用: %s", task.Key)
		}
	}
}

// TestScheduleViewReadsConfig 有 schedule 段时按配置回显（含显式 false）。
func TestScheduleViewReadsConfig(t *testing.T) {
	h, _ := newScheduleTestHandler(t,
		`{"listen":"x","schedule":{"checkin_enabled":false,"checkin_hours":[3,4],"cat_enabled":false}}`)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/schedule", nil))
	var v struct {
		Explicit bool `json:"explicit"`
		Tasks    []struct {
			Key     string `json:"key"`
			Enabled bool   `json:"enabled"`
			Hours   []int  `json:"hours"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Explicit {
		t.Errorf("explicit 应为 true")
	}
	byKey := map[string]int{}
	for i, task := range v.Tasks {
		byKey[task.Key] = i
	}
	ck := v.Tasks[byKey["checkin"]]
	if ck.Enabled {
		t.Errorf("checkin 显式 false 应回显停用")
	}
	if len(ck.Hours) != 2 || ck.Hours[0] != 3 || ck.Hours[1] != 4 {
		t.Errorf("checkin_hours=%v want [3,4]", ck.Hours)
	}
	cat := v.Tasks[byKey["cat"]]
	if cat.Enabled {
		t.Errorf("cat 显式 false 应回显停用")
	}
}

// TestSchedulePutWritesConfig 改开关：写回 config.json，且不动其它键。
func TestSchedulePutWritesConfig(t *testing.T) {
	h, path := newScheduleTestHandler(t,
		`{"listen":"127.0.0.1:7864","api_key":"secret","schedule":{"checkin_hours":[9,21]}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/schedule", strings.NewReader(`{"key":"checkin","enabled":false}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatalf("回写后不是合法 JSON: %v\n%s", err, raw)
	}
	// 未知/其它键必须保留
	if string(all["api_key"]) != `"secret"` {
		t.Errorf("api_key 被改动: %s", all["api_key"])
	}
	var sched map[string]any
	if err := json.Unmarshal(all["schedule"], &sched); err != nil {
		t.Fatal(err)
	}
	if v, ok := sched["checkin_enabled"].(bool); !ok || v {
		t.Errorf("checkin_enabled 应为 false, got %v", sched["checkin_enabled"])
	}
	if _, ok := sched["checkin_hours"]; !ok {
		t.Errorf("checkin_hours 应保留")
	}
}

// TestSchedulePutRejectsUnknownKey 未知任务名 → 400，且不写文件。
func TestSchedulePutRejectsUnknownKey(t *testing.T) {
	h, path := newScheduleTestHandler(t, `{"listen":"x"}`)
	before, _ := os.ReadFile(path)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/schedule", strings.NewReader(`{"key":"bogus","enabled":true}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("拒绝时不该写文件")
	}
}

// TestSchedulePutMissingEnabled enabled 缺省 → 400。
func TestSchedulePutMissingEnabled(t *testing.T) {
	h, _ := newScheduleTestHandler(t, `{"listen":"x"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/admin/schedule", strings.NewReader(`{"key":"checkin"}`))
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("code=%d want 400", rec.Code)
	}
}

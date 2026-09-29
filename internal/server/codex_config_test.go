package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlanCodexFreshFile 全新文件（无 config.toml）：写入后应含顶层两键 + 我们的段。
func TestPlanCodexFreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	p, err := PlanCodex(path, "http://127.0.0.1:7864/v1", "cn:auto", "")
	if err != nil {
		t.Fatalf("PlanCodex: %v", err)
	}
	if p.Exists {
		t.Errorf("全新文件不应报 Exists")
	}
	for _, want := range []string{
		`model = "cn:auto"`,
		`model_provider = "free2api"`,
		"[model_providers.free2api]",
		`base_url = "http://127.0.0.1:7864/v1"`,
		`env_key = "WB_API_KEY"`,
		`wire_api = "responses"`,
	} {
		if !strings.Contains(p.After, want) {
			t.Errorf("After 缺 %q\n---\n%s", want, p.After)
		}
	}
}

// TestPlanCodexPreservesUserContent 关键回归：只改自己那段，用户的其它 provider、
// 注释、缩进必须原样保留。这正是「不做全量 TOML 解析回写」的理由。
func TestPlanCodexPreservesUserContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := `# 我的 Codex 配置 —— 这行注释必须活下来
model = "gpt-5.6-sol"
model_provider = "opencode-go"
model_reasoning_effort = "high"

[model_providers.opencode-go]
name = "OpenCode Go"
base_url = "https://opencode.ai/zen/go/v1"
env_key = "OPENCODE_GO_API_KEY"
wire_api = "responses"

# 另一个供应商，也不能被碰
[model_providers.mine]
base_url = "http://127.0.0.1:9999/v1"
`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanCodex(path, "http://127.0.0.1:7864/v1", "cn:auto", "")
	if err != nil {
		t.Fatalf("PlanCodex: %v", err)
	}
	after := p.After

	// 用户的注释与其它 provider 必须逐字保留
	for _, keep := range []string{
		"# 我的 Codex 配置 —— 这行注释必须活下来",
		"[model_providers.opencode-go]",
		`name = "OpenCode Go"`,
		`base_url = "https://opencode.ai/zen/go/v1"`,
		"[model_providers.mine]",
		`base_url = "http://127.0.0.1:9999/v1"`,
		`model_reasoning_effort = "high"`,
	} {
		if !strings.Contains(after, keep) {
			t.Errorf("用户内容被破坏，缺 %q\n---\n%s", keep, after)
		}
	}
	// 顶层两键应被替换（不是重复追加）
	if strings.Count(after, `model = "`) != 1 {
		t.Errorf("model 键重复了\n%s", after)
	}
	if !strings.Contains(after, `model = "cn:auto"`) {
		t.Errorf("model 未被替换\n%s", after)
	}
	if strings.Contains(after, `model_provider = "opencode-go"`) {
		t.Errorf("model_provider 未被替换\n%s", after)
	}
	// 我们自己的段应存在
	if !strings.Contains(after, "[model_providers.free2api]") {
		t.Errorf("未追加我们的段\n%s", after)
	}
}

// TestPlanCodexIdempotent 重复写入应幂等：第二次 Changed=false，且段不重复。
func TestPlanCodexIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	p1, err := PlanCodex(path, "http://127.0.0.1:7864/v1", "cn:auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ApplyCodex(p1); err != nil {
		t.Fatalf("ApplyCodex: %v", err)
	}
	p2, err := PlanCodex(path, "http://127.0.0.1:7864/v1", "cn:auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if p2.Changed {
		t.Errorf("重复写入应 Changed=false\n第一次:\n%s\n第二次:\n%s", p1.After, p2.After)
	}
	if n := strings.Count(p2.After, "[model_providers.free2api]"); n != 1 {
		t.Errorf("段重复了 %d 次\n%s", n, p2.After)
	}
}

// TestApplyCodexBacksUp 落盘前必须备份，且备份内容 == 改前原文。
func TestApplyCodexBacksUp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	orig := "model = \"old\"\n"
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	p, err := PlanCodex(path, "http://127.0.0.1:7864/v1", "cn:auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.BackupPath == "" {
		t.Fatal("已存在的文件应给 BackupPath")
	}
	if err := ApplyCodex(p); err != nil {
		t.Fatalf("ApplyCodex: %v", err)
	}
	bak, err := os.ReadFile(p.BackupPath)
	if err != nil {
		t.Fatalf("读备份失败: %v", err)
	}
	if string(bak) != orig {
		t.Errorf("备份内容 != 改前原文\nwant %q\ngot  %q", orig, string(bak))
	}
	now, _ := os.ReadFile(path)
	if string(now) != p.After {
		t.Errorf("落盘内容 != After")
	}
}

// TestPlanCodexValidation base_url / model 空必须报错（不给用户写出半个配置）。
func TestPlanCodexValidation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if _, err := PlanCodex(path, "", "cn:auto", ""); err == nil {
		t.Error("空 base_url 应报错")
	}
	if _, err := PlanCodex(path, "http://127.0.0.1:7864/v1", "", ""); err == nil {
		t.Error("空 model 应报错")
	}
}

// TestCodexConfigPath CODEX_HOME 覆盖优先于默认 ~/.codex。
func TestCodexConfigPath(t *testing.T) {
	if got := CodexConfigPath("D:/custom/config.toml"); got != "D:/custom/config.toml" {
		t.Errorf("显式覆盖未生效: %q", got)
	}
	t.Setenv("CODEX_HOME", filepath.Join("X:", "codexhome"))
	got := CodexConfigPath("")
	if !strings.HasSuffix(got, filepath.Join("codexhome", "config.toml")) {
		t.Errorf("CODEX_HOME 未生效: %q", got)
	}
	t.Setenv("CODEX_HOME", "")
	if got := CodexConfigPath(""); !strings.HasSuffix(got, filepath.Join(".codex", "config.toml")) {
		t.Errorf("默认路径不对: %q", got)
	}
}

// TestReadTopString 顶层键读取不能误读段内同名键。
func TestReadTopString(t *testing.T) {
	s := "model = \"top\"\n\n[model_providers.x]\nmodel = \"inner\"\n"
	if got := readTopString(s, "model"); got != "top" {
		t.Errorf("readTopString=%q want top", got)
	}
	if got := readTopString(s, "nonexistent"); got != "" {
		t.Errorf("不存在的键应回空串，got %q", got)
	}
}

// TestReadProviderBaseURL 读段内 base_url。
func TestReadProviderBaseURL(t *testing.T) {
	s := "[model_providers.free2api]\nbase_url = \"http://127.0.0.1:7864/v1\"\n\n[model_providers.other]\nbase_url = \"http://x/v1\"\n"
	if got := readProviderBaseURL(s, "free2api"); got != "http://127.0.0.1:7864/v1" {
		t.Errorf("got %q", got)
	}
	if got := readProviderBaseURL(s, "missing"); got != "" {
		t.Errorf("不存在的段应回空串，got %q", got)
	}
}
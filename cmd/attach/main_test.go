package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// providerID 必须与控制台（internal/server/codex_config.go 的 codexProviderID）一致。
// 两边不同名会在用户的 config.toml 里留下两段 [model_providers.*]，同一个网关挂两遍。
func TestProviderIDMatchesConsole(t *testing.T) {
	if providerID != "free2api" {
		t.Fatalf("providerID = %q，网页面板写的是 free2api；两边必须同名", providerID)
	}
}

// upsertProvider 必须写出 env_key。Codex 靠它知道去哪读 API key，漏了会直接拒绝该 provider
// （旧实现只写 name/base_url/wire_api，attach 完仍调不通）。
func TestUpsertProviderWritesEnvKey(t *testing.T) {
	var tl toml
	tl.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1")
	out := tl.String()

	for _, want := range []string{
		"[model_providers." + providerID + "]",
		`env_key = "` + envKey + `"`,
		`base_url = "http://127.0.0.1:7864/v1"`,
		`wire_api = "` + wireAPI + `"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("provider 表缺少 %q\n---\n%s", want, out)
		}
	}
}

// 幂等按值判定：值没变时第二次调用不报告改动（桌面端每次重写 config.toml 都会剪掉顶层标量，
// attach 必须能被反复执行而不搅动用户文件）。
func TestUpsertProviderIdempotent(t *testing.T) {
	var tl toml
	if !tl.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1") {
		t.Fatal("首次写入应报告 changed=true")
	}
	if tl.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1") {
		t.Fatal("值未变，第二次应报告 changed=false（幂等）")
	}
}

// 表里已有段但缺 env_key 时（例如旧版 attach 写的、或网页面板早期版本写的），
// 重跑 attach 要把它补上，而不是留个半残的表。
func TestUpsertProviderBackfillsMissingEnvKey(t *testing.T) {
	var tl toml
	tl.lines = strings.Split(strings.TrimRight(`
[model_providers.free2api]
name = "Free2API (local)"
base_url = "http://127.0.0.1:7864/v1"
wire_api = "responses"
`, "\n"), "\n")

	if !tl.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1") {
		t.Fatal("缺 env_key 时应报告 changed=true")
	}
	if !strings.Contains(tl.String(), `env_key = "`+envKey+`"`) {
		t.Fatalf("env_key 没被补上\n---\n%s", tl.String())
	}
}

// 用户其它内容（注释 / 别的 provider 表）必须原样保留。
func TestUpsertProviderPreservesUserContent(t *testing.T) {
	var tl toml
	tl.lines = strings.Split(strings.TrimRight(`
model = "gpt-5.1-codex"

# 用户自己的注释
[features]
js_repl = true

[model_providers.opencode-go]
name = "OpenCode Go"
base_url = "https://opencode.ai/zen/go/v1"
`, "\n"), "\n")

	tl.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1")
	out := tl.String()

	for _, want := range []string{
		"# 用户自己的注释",
		"[features]",
		"[model_providers.opencode-go]",
		`base_url = "https://opencode.ai/zen/go/v1"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("用户内容 %q 被弄丢了\n---\n%s", want, out)
		}
	}
}

// setTopKey 的幂等：值相同不重写（注释差异不算改动）。
func TestSetTopKeyIdempotent(t *testing.T) {
	var tl toml
	tl.lines = []string{`model_provider = "free2api"   # 旧注释`}
	if tl.setTopKey("model_provider", providerID, "新注释") {
		t.Fatal("值相同，应报告 changed=false")
	}
	if !tl.setTopKey("model_provider", "other", "") {
		t.Fatal("值不同，应报告 changed=true")
	}
	if tl.topKey("model_provider") != "other" {
		t.Fatalf("替换后值 = %q，想要 other", tl.topKey("model_provider"))
	}
}

// loadToml 对不存在的文件返回空文档（首次 attach 不报错）。
func TestLoadTomlMissingFile(t *testing.T) {
	dir := t.TempDir()
	tl, err := loadToml(filepath.Join(dir, "nope.toml"))
	if err != nil {
		t.Fatalf("不存在的文件不该报错：%v", err)
	}
	if len(tl.lines) != 0 {
		t.Fatalf("应为空文档，得到 %d 行", len(tl.lines))
	}
	if tl.String() != "" {
		t.Fatalf("空文档 String() 应为空串，得到 %q", tl.String())
	}
}

// 端到端：attach 一个空目录，落盘后能读回目标形态，再 attach 一次不重复写。
func TestDoAttachRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	tl, _ := loadToml(path)
	tl.setTopKey("model_provider", providerID, "test")
	tl.setTopKey("model", "cn:auto", "test")
	tl.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1")
	if err := os.WriteFile(path, []byte(tl.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	back, err := loadToml(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.topKey("model_provider") != providerID {
		t.Errorf("model_provider = %q", back.topKey("model_provider"))
	}
	if back.topKey("model") != "cn:auto" {
		t.Errorf("model = %q", back.topKey("model"))
	}
	if !back.hasProvider(providerID) {
		t.Error("provider 表没落盘")
	}
	// 二次执行不应再有改动
	if back.setTopKey("model_provider", providerID, "test") {
		t.Error("model_provider 重复写")
	}
	if back.setTopKey("model", "cn:auto", "test") {
		t.Error("model 重复写")
	}
	if back.upsertProvider(providerID, providerName, "http://127.0.0.1:7864/v1") {
		t.Error("provider 表重复写")
	}
}
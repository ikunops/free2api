package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAPIKeyGenerateWritesConfig generate:true 应生成一把 sk- 前缀的随机密钥写进
// config.json，并明确回传 restart_required（当前进程用的是启动快照，不热更新）。
func TestAPIKeyGenerateWritesConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"listen":"127.0.0.1:7864","api_key":"","admin":{"enabled":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: serverConfigForTest(p)}
	rec := httptest.NewRecorder()
	h.adminAPIKeyPut(rec, httptest.NewRequest("POST", "/admin/api-key",
		strings.NewReader(`{"generate":true}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		APIKey          string `json:"api_key"`
		Enabled         bool   `json:"enabled"`
		RestartRequired bool   `json:"restart_required"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.APIKey, "sk-") {
		t.Errorf("api_key=%q 应带 sk- 前缀", out.APIKey)
	}
	if len(out.APIKey) < 32 {
		t.Errorf("api_key 长度 %d 太短，不足以当密钥用", len(out.APIKey))
	}
	if !out.Enabled || !out.RestartRequired {
		t.Errorf("enabled=%v restart_required=%v，两者都应为 true", out.Enabled, out.RestartRequired)
	}
	// 落盘校验：且只改 api_key，admin.enabled 等其它键原样保留。
	var disk map[string]any
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &disk); err != nil {
		t.Fatal(err)
	}
	if disk["api_key"] != out.APIKey {
		t.Errorf("config.json api_key=%v，回包=%v", disk["api_key"], out.APIKey)
	}
	if adm, ok := disk["admin"].(map[string]any); !ok || adm["enabled"] != true {
		t.Errorf("admin.enabled 被改坏了：%v", disk["admin"])
	}
}

// TestAPIKeyTwoGeneratesDiffer 两次生成必须不同——crypto/rand 失效时退化成
// 固定串的话这把密钥就等于公开的。
func TestAPIKeyTwoGeneratesDiffer(t *testing.T) {
	a, b := newGatewayAPIKey(), newGatewayAPIKey()
	if a == "" || b == "" {
		t.Fatal("生成失败（不应为空）")
	}
	if a == b {
		t.Error("两次生成得到同一把密钥")
	}
}

// TestAPIKeyClearDisablesAuth 显式传空串 = 关闭鉴权（与"没传该字段"必须区分开，
// 否则手滑少传一个字段就会把用户的密钥悄悄清掉）。
func TestAPIKeyClearDisablesAuth(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"api_key":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: serverConfigForTest(p)}
	rec := httptest.NewRecorder()
	h.adminAPIKeyPut(rec, httptest.NewRequest("POST", "/admin/api-key",
		strings.NewReader(`{"api_key":""}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	raw, _ := os.ReadFile(p)
	var disk map[string]any
	_ = json.Unmarshal(raw, &disk)
	if disk["api_key"] != "" {
		t.Errorf("api_key=%v 应被清空", disk["api_key"])
	}
}

// TestAPIKeyMissingFieldRejected 两个字段都没给必须报错，不能默默当成"清空"。
func TestAPIKeyMissingFieldRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(`{"api_key":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: serverConfigForTest(p)}
	rec := httptest.NewRecorder()
	h.adminAPIKeyPut(rec, httptest.NewRequest("POST", "/admin/api-key", strings.NewReader(`{}`)))
	if rec.Code == http.StatusOK {
		t.Fatalf("缺字段应拒绝，实际 code=%d", rec.Code)
	}
	raw, _ := os.ReadFile(p)
	var disk map[string]any
	_ = json.Unmarshal(raw, &disk)
	if disk["api_key"] != "secret" {
		t.Errorf("被拒的请求不该改文件，api_key=%v", disk["api_key"])
	}
} // serverConfigForTest 只填本组用例需要的字段（Config 是个大结构，填全反而碍事）。
func serverConfigForTest(path string) Config {
	return Config{ConfigPath: path}
}

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminEnabledWithoutKeyFailsFast fail-fast（设计 supplement §4.1）：
// admin.enabled=true 且 api_key 为空 → 启动即报错，而非运行一个
// 未鉴权的 mutation 端点（disable/revive 是可用性操作，风险高于 /status 读泄漏）。
func TestAdminEnabledWithoutKeyFailsFast(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"","admin":{"enabled":true}}`), 0o600)
	_, err := Load(fp)
	if err == nil {
		t.Fatal("admin.enabled=true + 空 api_key 应拒绝启动（fail-fast）")
	}
	if !strings.Contains(err.Error(), "admin") || !strings.Contains(err.Error(), "api_key") {
		t.Errorf("错误文案应同时点到 admin 与 api_key, got %q", err.Error())
	}
}

// TestAdminEnabledWithKeyOK 对照组：admin.enabled=true 且 api_key 非空 → 正常加载。
func TestAdminEnabledWithKeyOK(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":true}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("admin 开 + key 非空应通过: %v", err)
	}
}

// TestAdminDisabledWithoutKeyOK 对照组：admin 未开时空 api_key 仍合法（现状语义不变）。
func TestAdminDisabledWithoutKeyOK(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"","admin":{"enabled":false}}`), 0o600)
	if _, err := Load(fp); err != nil {
		t.Fatalf("admin 关 + 空 key 应通过: %v", err)
	}
}

// TestAdminEnvOverride WB2A_ADMIN_ENABLED env 覆盖（对齐 WB2A_PASSTHROUGH_IP 风格：
// ParseBool，非法值忽略不报错）。
func TestAdminEnvOverride(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":false}}`), 0o600)

	t.Setenv("WB2A_ADMIN_ENABLED", "true")
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.Enabled {
		t.Error("WB2A_ADMIN_ENABLED=true 应开启 admin")
	}

	// env 关闭覆盖 config 的 true
	os.WriteFile(fp, []byte(`{"api_key":"k","admin":{"enabled":true}}`), 0o600)
	t.Setenv("WB2A_ADMIN_ENABLED", "false")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Admin.Enabled {
		t.Error("WB2A_ADMIN_ENABLED=false 应覆盖 config 的 true")
	}

	// 非法值忽略：config 的 true 保持
	t.Setenv("WB2A_ADMIN_ENABLED", "yes-please")
	c, err = Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Admin.Enabled {
		t.Error("非法 env 值应被忽略（保持 config 值）")
	}
}

// TestAdminEnvOverrideFailsFastWhenNoKey env 开启 admin 但无 key 同样触发 fail-fast
// （env 覆盖先于 normalize 校验，两条入口一致拦截）。
func TestAdminEnvOverrideFailsFastWhenNoKey(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"api_key":"","admin":{"enabled":false}}`), 0o600)
	t.Setenv("WB2A_ADMIN_ENABLED", "1")
	if _, err := Load(fp); err == nil {
		t.Fatal("env 开启 admin + 空 api_key 也应 fail-fast")
	}
}

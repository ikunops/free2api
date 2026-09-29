package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestGlobalDefaults 断言 global 段缺省：enabled=true（默认开启）、base 空（回落默认）。
func TestGlobalDefaults(t *testing.T) {
	c := Default()
	if err := c.normalize(); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if !c.Global.Enabled {
		t.Error("global.enabled default should be true")
	}
	if c.Global.ChatBase != "" || c.Global.BillingBase != "" {
		t.Errorf("global bases default should be empty (fallback upstream defaults), got %q/%q",
			c.Global.ChatBase, c.Global.BillingBase)
	}
}

// TestGlobalParsedFromFile 断言 global 段可从 config 文件解析并覆盖。
func TestGlobalParsedFromFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{
		"global":{
			"enabled":true,
			"chat_base":"https://workbuddy-api-qa.workbuddy.ai",
			"billing_base":"https://billing.workbuddy.ai"
		}
	}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Global.Enabled {
		t.Error("global.enabled want true from file")
	}
	if c.Global.ChatBase != "https://workbuddy-api-qa.workbuddy.ai" {
		t.Errorf("chat_base=%q", c.Global.ChatBase)
	}
	if c.Global.BillingBase != "https://billing.workbuddy.ai" {
		t.Errorf("billing_base=%q", c.Global.BillingBase)
	}
}

// TestGlobalEnabledAbsentIsTrue 断言 global 段缺席（旧 config 文件）→ enabled=true（默认开启）。
func TestGlobalEnabledAbsentIsTrue(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"listen":":9999"}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Global.Enabled {
		t.Error("absent global section must default to enabled (true)")
	}
}

// TestGlobalEnabledExplicitOffEscapeHatch 显式 "enabled": false → 关闭（逃生门保留）。
func TestGlobalEnabledExplicitOffEscapeHatch(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "c.json")
	os.WriteFile(fp, []byte(`{"global":{"enabled":false}}`), 0o600)
	c, err := Load(fp)
	if err != nil {
		t.Fatal(err)
	}
	if c.Global.Enabled {
		t.Error("explicit global.enabled=false must disable")
	}
}

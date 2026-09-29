package main

import (
	"testing"

	"free2api/internal/config"
)

// TestNewUpstreamGlobalEnabled registry：cmd/activity 与 cmd/signin、cmd/trial、cmd/credit
// 同风格显式接线 GlobalEnabled=true。activity 虽是 CN 任务中心的上报工具（global 账号已被
// scheduler 的 IsGlobal 门控跳过），但工具自身构造 global 账号请求时不得误打 CN base——
// 开关显式开启把该路径收敛为「不正确但不会跨域误打」（与 signin/trial 同逃生门式接线）。
func TestNewUpstreamGlobalEnabled(t *testing.T) {
	up := newUpstream(&cfgFile{Schedule: config.DefaultSchedule()})
	if !up.GlobalEnabled {
		t.Error("newUpstream should set GlobalEnabled=true (explicit wiring, uniform with signin/trial/credit)")
	}
}

// TestNewUpstreamAppliesTimeout 配置的 upstream.timeout_seconds 仍生效（接线抽出不破坏原行为）。
func TestNewUpstreamAppliesTimeout(t *testing.T) {
	c := &cfgFile{Schedule: config.DefaultSchedule()}
	c.Upstream.TimeoutSeconds = 7
	up := newUpstream(c)
	if up.HTTP.Timeout == 0 {
		t.Fatal("HTTP.Timeout should be set when timeout_seconds > 0")
	}
	if up.HTTP.Timeout.String() != "7s" {
		t.Errorf("HTTP.Timeout=%v want 7s", up.HTTP.Timeout)
	}
}

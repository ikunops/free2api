package server

import "testing"

func TestModelDeadRevive(t *testing.T) {
	m := newModelDeadList(t.TempDir() + "/modeldead.json")
	m.mark("global", "workbuddy", "gpt-6-luna", "entitlement")
	if !m.active("global", "workbuddy", "gpt-6-luna") {
		t.Fatal("mark 后应 active")
	}
	if !m.revive("global", "workbuddy", "gpt-6-luna") {
		t.Fatal("revive 应报告删除成功")
	}
	if m.active("global", "workbuddy", "gpt-6-luna") {
		t.Fatal("revive 后不应 active")
	}
	if m.revive("global", "workbuddy", "gpt-6-luna") {
		t.Fatal("重复 revive 应返回 false")
	}
}

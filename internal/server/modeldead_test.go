package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mark → active 立即可见（producer 空串归一为 workbuddy）；TTL 过期后 active=false
// 且条目被惰性清除。
func TestModelDeadMarkAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "modeldead.json")
	m := newModelDeadList(path)
	if m.active("cn", "", "glm-4.6-x0.23") {
		t.Fatal("未标记的模型不应处于死标记期")
	}
	m.mark("cn", "", "glm-4.6-x0.23", "upstream: no such model")
	if !m.active("cn", "workbuddy", "glm-4.6-x0.23") {
		t.Fatal("mark 后应 active（producer 空串归一为 workbuddy）")
	}

	const k = "cn/workbuddy/glm-4.6-x0.23"
	m.mu.Lock()
	m.entries[k] = modelDeadEntry{Until: time.Now().Add(-time.Minute), Reason: "expired"}
	m.mu.Unlock()
	if m.active("cn", "", "glm-4.6-x0.23") {
		t.Fatal("过期条目不应 active")
	}
	m.mu.Lock()
	_, still := m.entries[k]
	m.mu.Unlock()
	if still {
		t.Fatal("过期条目应被惰性清除")
	}
}

// 落盘回读：mark 后重新加载同一文件，标记仍在（重启不丢）。
func TestModelDeadPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "modeldead.json")
	m := newModelDeadList(path)
	m.mark("global", "workbuddy", "gpt-5.5-x3.31", "upstream: no such model")

	m2 := newModelDeadList(path)
	if !m2.active("global", "", "gpt-5.5-x3.31") {
		t.Fatal("重启（重载文件）后标记应保留")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("落盘文件应存在: %v", err)
	}
	var entries map[string]modelDeadEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("落盘文件应为合法 JSON: %v", err)
	}
	if _, ok := entries["global/workbuddy/gpt-5.5-x3.31"]; !ok {
		t.Fatalf("落盘键不对: %s", raw)
	}
}

// clear 清空全部标记并可见；空表 clear 返回 0。
func TestModelDeadClear(t *testing.T) {
	m := newModelDeadList(filepath.Join(t.TempDir(), "modeldead.json"))
	m.mark("cn", "zcode", "glm-5.3-flash", "test")
	if n := m.clear(); n != 1 {
		t.Fatalf("clear 应清 1 条, got %d", n)
	}
	if m.active("cn", "zcode", "glm-5.3-flash") {
		t.Fatal("clear 后不应 active")
	}
	if n := m.clear(); n != 0 {
		t.Fatalf("再次 clear 应为 0, got %d", n)
	}
}

package pool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"free2api/internal/auth"
)

// writeAuthFile 落一个最小可用凭证文件（字段与 internal/auth 读取格式一致）。
func writeAuthFile(t *testing.T, dir, uid, nick string) string {
	t.Helper()
	doc := map[string]any{
		"account": map[string]any{"uid": uid, "enterpriseId": "", "nickname": nick},
		"auth": map[string]any{
			"accessToken":  "tok-" + uid,
			"refreshToken": "ref-" + uid,
			"expiresAt":    time.Now().Add(24 * time.Hour).Unix(),
			"domain":       "www.workbuddy.ai",
			"realm":        "global",
		},
	}
	raw, _ := json.Marshal(doc)
	p := filepath.Join(dir, "workbuddy-"+uid+".json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestDirFingerprintDetectsChange 指纹对「新增 / 修改 / 删除」三种变化都必须敏感。
func TestDirFingerprintDetectsChange(t *testing.T) {
	dir := t.TempDir()
	base, ok := dirFingerprint(dir)
	if !ok {
		t.Fatal("空目录应可读（ok=true）")
	}
	if base != "" {
		t.Errorf("空目录指纹应为空串，得到 %q", base)
	}

	// 新增
	writeAuthFile(t, dir, "u1", "一号")
	afterAdd, _ := dirFingerprint(dir)
	if afterAdd == base {
		t.Error("新增文件后指纹应变化")
	}

	// 内容修改（同名覆盖，模拟凭证刷新）：文件名不变，仅 mtime/size 变
	time.Sleep(10 * time.Millisecond)
	writeAuthFile(t, dir, "u1", "一号改")
	afterMod, _ := dirFingerprint(dir)
	if afterMod == afterAdd {
		t.Error("同名覆盖后指纹应变化（凭证刷新场景）")
	}

	// 非 json 文件不参与指纹
	if err := os.WriteFile(filepath.Join(dir, "readme.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fp, _ := dirFingerprint(dir); fp != afterMod {
		t.Error("非 .json 文件不应影响指纹")
	}
}

// TestDirFingerprintUnreadable 目录不可读返回空串（调用方据此跳过本轮）。
func TestDirFingerprintUnreadable(t *testing.T) {
	if _, ok := dirFingerprint(filepath.Join(t.TempDir(), "nope")); ok {
		t.Error("不可读目录应返回 ok=false")
	}
}

// TestStartAuthDirWatchNoopOnBadDir 目录不可读时返回可用的 no-op 停止函数，不 panic。
func TestStartAuthDirWatchNoopOnBadDir(t *testing.T) {
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()

	stop := p.StartAuthDirWatch(filepath.Join(t.TempDir(), "missing"))
	stop()
	stop() // 幂等：重复调用不 panic
}

// TestReloadAuthDirAddsAccount 目录新增凭证后热加载进池。
func TestReloadAuthDirAddsAccount(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")

	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)

	if n := len(p.AvailableUIDs()); n != 1 {
		t.Fatalf("初始账号数=%d want 1", n)
	}

	// 新增第二个账号并热加载
	writeAuthFile(t, dir, "u2", "二号")
	p.reloadAuthDir(dir)

	uids := p.AvailableUIDs()
	if len(uids) != 2 {
		t.Fatalf("热加载后账号数=%d want 2 (%v)", len(uids), uids)
	}
	found := false
	for _, u := range uids {
		if u == "u2" {
			found = true
		}
	}
	if !found {
		t.Errorf("新账号 u2 未进池: %v", uids)
	}
}

// TestReloadAuthDirPreservesState 热加载不得重置既有账号的冷却/统计状态。
//
// 这是本特性最关键的契约：upsertLocked 对已存在账号只换凭证。若误写成"重建条目"，
// 用户每次加号都会把全池冷却清空（等于绕过限流惩罚），是严重回归。
func TestReloadAuthDirPreservesState(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)

	// 给 u1 制造状态：软冷却 + 计数
	p.Cooldown("u1", CoolSoft, time.Hour, "test")
	p.NoteError("u1")

	before := statusByUID(t, p, "u1")
	if !before.Cooling {
		t.Fatal("前置：u1 应处于冷却")
	}

	// 刷新 u1 凭证（同名覆盖）+ 新增 u2，然后热加载
	writeAuthFile(t, dir, "u1", "一号新凭证")
	writeAuthFile(t, dir, "u2", "二号")
	p.reloadAuthDir(dir)

	after := statusByUID(t, p, "u1")
	if !after.Cooling {
		t.Error("热加载不得清除既有账号的冷却状态")
	}
	if after.CoolKind != before.CoolKind {
		t.Errorf("cool_kind 被改变: %q → %q", before.CoolKind, after.CoolKind)
	}
	if after.SuccessCount != before.SuccessCount || after.ErrTotal != before.ErrTotal {
		t.Errorf("计数被重置: succ %d→%d, err %d→%d",
			before.SuccessCount, after.SuccessCount, before.ErrTotal, after.ErrTotal)
	}
}

// TestReloadAuthDirRemovesDeleted 文件被删除的账号应从池中剔除。
func TestReloadAuthDirRemovesDeleted(t *testing.T) {
	dir := t.TempDir()
	writeAuthFile(t, dir, "u1", "一号")
	f2 := writeAuthFile(t, dir, "u2", "二号")

	auths, _ := auth.LoadDir(dir)
	p := New(filepath.Join(t.TempDir(), "state.json"))
	defer p.Close()
	p.SyncToDir(auths)
	if n := len(p.AvailableUIDs()); n != 2 {
		t.Fatalf("前置：账号数=%d want 2", n)
	}

	if err := os.Remove(f2); err != nil {
		t.Fatal(err)
	}
	p.reloadAuthDir(dir)

	if n := len(p.AvailableUIDs()); n != 1 {
		t.Errorf("删除文件后账号数=%d want 1", n)
	}
}

// statusByUID 从 List() 取指定账号状态（同包测试辅助）。
func statusByUID(t *testing.T, p *Pool, uid string) Status {
	t.Helper()
	for _, st := range p.List() {
		if st.UID == uid {
			return st
		}
	}
	t.Fatalf("账号 %s 不在池中", uid)
	return Status{}
}

//go:build windows

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// isolateDeployState 把部署发现用到的全局指针/根目录都指到临时位置，
// 避免测试读写真实用户的 LocalAppData 或扫到本机真实部署。
func isolateDeployState(t *testing.T) {
	t.Helper()
	oldPath := deployPointerPathHook
	oldRoots := deploySearchRootsHook
	pointer := filepath.Join(t.TempDir(), "deploy.json")
	deployPointerPathHook = func() string { return pointer }
	deploySearchRootsHook = func() []string { return nil }
	t.Cleanup(func() {
		deployPointerPathHook = oldPath
		deploySearchRootsHook = oldRoots
	})
}

// TestLooksLikeDeployDistinguishesEmptyShell 说明「部署」不能只看目录存在：
// 上次失败启动留下的空 data/ 不算部署，否则新 exe 会接一个空壳。
func TestLooksLikeDeployDistinguishesEmptyShell(t *testing.T) {
	dir := t.TempDir()
	if looksLikeDeploy(dir) {
		t.Fatal("空目录不应被认作部署")
	}
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if looksLikeDeploy(dir) {
		t.Fatal("只有空 data/ 的目录不应被认作部署")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"listen":"127.0.0.1:7864"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !looksLikeDeploy(dir) {
		t.Fatal("含 config.json 的目录应被认作部署")
	}
}

// TestLooksLikeDeployRejectsForeignConfig 覆盖「把别家项目的 config.json
// 当成部署」的事故：用户目录下任意项目都可能有一份同名文件。
func TestLooksLikeDeployRejectsForeignConfig(t *testing.T) {
	dir := t.TempDir()
	foreign := `{"formula_url":"https://example.invalid","free_models_cache":{"ts":1,"models":[]}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(foreign), 0o644); err != nil {
		t.Fatal(err)
	}
	if looksLikeDeploy(dir) {
		t.Fatal("非 free2api 的 config.json 不应被认作部署")
	}
	var out []deployCandidate
	walkDeployCandidates(dir, 4, map[string]bool{}, &out)
	if len(out) != 0 {
		t.Fatalf("扫描把别家项目当成了部署：%+v", out)
	}
}

// TestLooksLikeDeployAcceptsAuthsOnly 覆盖「只有账号、没有配置」的迁移中间态：
// 用户把 auths/ 拷过来但还没写 config.json 时，也不该被当成新部署。
func TestLooksLikeDeployAcceptsAuthsOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	if looksLikeDeploy(dir) {
		t.Fatal("空 auths/ 不应被认作部署")
	}
	if err := os.WriteFile(filepath.Join(dir, "auths", "workbuddy-x.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !looksLikeDeploy(dir) {
		t.Fatal("含账号文件的 auths/ 应被认作部署")
	}
}

// TestDiscoverDeployDirPrefersExeDir 正常覆盖升级：exe 同目录就是部署时，
// 必须原样使用它，不能被别的目录抢走。
func TestDiscoverDeployDirPrefersExeDir(t *testing.T) {
	isolateDeployState(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"listen":"127.0.0.1:7864"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := discoverDeployDir(dir); got != dir {
		t.Fatalf("discoverDeployDir = %q, want %q", got, dir)
	}
}

// TestDiscoverDeployDirFallsBackToExeDir 全新机器：没有任何已有部署时，
// 应退回 exe 同目录，由 ensureConfig 生成默认配置。
func TestDiscoverDeployDirFallsBackToExeDir(t *testing.T) {
	isolateDeployState(t)
	// 用一个不可能存在部署的临时盘符式路径，避免误命中本机真实部署。
	dir := filepath.Join(t.TempDir(), "no-deploy-here")
	if got := discoverDeployDir(dir); got != dir {
		t.Fatalf("discoverDeployDir = %q, want %q", got, dir)
	}
}

// TestFindExistingDeployPicksMostRecent 同根下有多份部署时，按 config.json
// 的修改时间选最近的一份；这是「升级后接手原部署」的核心排序规则。
func TestFindExistingDeployPicksMostRecent(t *testing.T) {
	isolateDeployState(t)
	root := t.TempDir()
	older := filepath.Join(root, "old")
	newer := filepath.Join(root, "new")
	for _, d := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Join(d, "auths"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "config.json"), []byte(`{"listen":"127.0.0.1:7864"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "auths", "a.json"), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(older, "config.json"), past, past); err != nil {
		t.Fatal(err)
	}

	oldRoots := deploySearchRootsHook
	deploySearchRootsHook = func() []string { return []string{root} }
	defer func() { deploySearchRootsHook = oldRoots }()

	got, ok := findExistingDeploy(filepath.Join(root, "somewhere-else"), root)
	if !ok {
		t.Fatal("findExistingDeploy 没找到候选")
	}
	if got.dir != newer {
		t.Fatalf("findExistingDeploy = %q, want %q", got.dir, newer)
	}
}

// TestFindExistingDeployIgnoresNewerForeignConfig 复现 2026-10-19 的事故：
// 用户目录里另一个项目（opencode-widget）的 config.json 更新，且不带 auths。
// 它绝不能被当成 free2api 部署，否则新 exe 会去读一份没有 listen 的配置，
// 端口回落 7863、账号池为空。
func TestFindExistingDeployIgnoresNewerForeignConfig(t *testing.T) {
	isolateDeployState(t)
	root := t.TempDir()
	realDeploy := filepath.Join(root, "work", "dist")
	foreign := filepath.Join(root, "opencode-widget")
	for _, d := range []string{realDeploy, foreign} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(realDeploy, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDeploy, "auths", "a.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDeploy, "config.json"),
		[]byte(`{"listen":"127.0.0.1:7864","auth_dir":"./auths"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// 别家项目：最新修改时间 + 完全不认识的键，且没有 listen/auth_dir。
	if err := os.WriteFile(filepath.Join(foreign, "config.json"),
		[]byte(`{"formula_url":"https://example.invalid","free_models_cache":{"ts":1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(24 * time.Hour)
	if err := os.Chtimes(filepath.Join(foreign, "config.json"), future, future); err != nil {
		t.Fatal(err)
	}

	oldRoots := deploySearchRootsHook
	deploySearchRootsHook = func() []string { return []string{root} }
	defer func() { deploySearchRootsHook = oldRoots }()

	got, ok := findExistingDeploy(filepath.Join(root, "fresh"), root)
	if !ok {
		t.Fatal("findExistingDeploy 没找到真正的部署")
	}
	if got.dir != realDeploy {
		t.Fatalf("findExistingDeploy = %q, want %q（不能接别家项目的 config.json）", got.dir, realDeploy)
	}
}

// TestLooksLikeBackupDir 备份/历史版本目录不能被当成「当前部署」，
// 否则 upgrade 时可能接到一份旧副本，账号看起来又变少了。
func TestLooksLikeBackupDir(t *testing.T) {
	cases := map[string]bool{
		"dist":             false,
		"free2api":         false,
		"dist-bak-20261019": true,
		"backup":           true,
		"old-deploy":       true,
		"dist-v113":        true,
		"v1.1.3":           true,
		"20261019":         true,
		"my-old-stuff":     true,
	}
	for name, want := range cases {
		if got := looksLikeBackupDir(name); got != want {
			t.Errorf("looksLikeBackupDir(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestWalkDeployCandidatesSkipsCaches 扫描时跳过 node_modules 等缓存目录，
// 避免在工程目录里误接一份构建缓存中的旧配置。
func TestWalkDeployCandidatesSkipsCaches(t *testing.T) {
	isolateDeployState(t)
	root := t.TempDir()
	cache := filepath.Join(root, "node_modules", "pkg")
	if err := os.MkdirAll(cache, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache, "config.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out []deployCandidate
	walkDeployCandidates(root, 4, map[string]bool{}, &out)
	if len(out) != 0 {
		t.Fatalf("扫描进了缓存目录：%+v", out)
	}
}

// TestDiscoverDeployDirRepairsUpgradeWithoutLosingAccounts 复现并锁死这次的
// 真实事故：新 exe 解压到空目录后直接双击。discoverDeployDir 必须找到
// 旧部署，且 auth 包能从旧部署里读出账号，而不是按空目录新建一套。
//
// 这是「升级后账号变少」的回归测试 —— 只测目录选择还不够，必须证明
// 选中的目录确实能解析出账号。
func TestDiscoverDeployDirRepairsUpgradeWithoutLosingAccounts(t *testing.T) {
	isolateDeployState(t)
	oldDeploy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(oldDeploy, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDeploy, "config.json"),
		[]byte(`{"listen":"127.0.0.1:7864","auth_dir":"./auths"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	authJSON := `{"account":{"uid":"u-1","nickname":"tester"},"auth":{"accessToken":"tok","refreshToken":"rtok","expiresAt":4102444800,"domain":"www.workbuddy.cn"},"producer":"workbuddy"}`
	if err := os.WriteFile(filepath.Join(oldDeploy, "auths", "workbuddy-u-1.json"), []byte(authJSON), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRoots := deploySearchRootsHook
	deploySearchRootsHook = func() []string { return []string{filepath.Dir(oldDeploy)} }
	defer func() { deploySearchRootsHook = oldRoots }()

	newExeDir := filepath.Join(t.TempDir(), "fresh-unzip")
	if err := os.MkdirAll(newExeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	got := discoverDeployDir(newExeDir)
	if got != oldDeploy {
		t.Fatalf("discoverDeployDir = %q, want %q（应接手旧部署）", got, oldDeploy)
	}
	if !dirHasAuths(got) {
		t.Fatalf("接手目录 %q 里读不到账号", got)
	}
}

// TestDeployPointerBeatsFreshExeDir 复现本次事故的第二条路径：用户把新 exe
// 解压到一个空目录，同目录没有任何部署痕迹。只要之前成功启动过，指针就应把
// 它带回原部署 —— 这是「升级后账号不丢」最可靠的那条路径，不依赖扫描深度。
func TestDeployPointerBeatsFreshExeDir(t *testing.T) {
	isolateDeployState(t)

	oldDeploy := t.TempDir()
	if err := os.MkdirAll(filepath.Join(oldDeploy, "auths"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDeploy, "config.json"), []byte(`{"listen":"127.0.0.1:7864"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDeploy, "auths", "a.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"dir": oldDeploy})
	if err := os.WriteFile(deployPointerPathHook(), payload, 0o644); err != nil {
		t.Fatal(err)
	}

	fresh := filepath.Join(t.TempDir(), "fresh-unzip")
	if err := os.MkdirAll(fresh, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := discoverDeployDir(fresh); got != oldDeploy {
		t.Fatalf("discoverDeployDir = %q, want %q（指针应优先）", got, oldDeploy)
	}
}

// TestDeployPointerIgnoresMissingDir 指针指向的目录被删掉后必须失效，
// 否则程序会一直启动失败、也找不到真正的新部署。
func TestDeployPointerIgnoresMissingDir(t *testing.T) {
	isolateDeployState(t)
	gone := filepath.Join(t.TempDir(), "gone")
	payload, _ := json.Marshal(map[string]string{"dir": gone})
	if err := os.WriteFile(deployPointerPathHook(), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	if got := discoverDeployDir(fresh); got != fresh {
		t.Fatalf("discoverDeployDir = %q, want %q", got, fresh)
	}
}

// TestWriteDeployPointerRecordsDir 成功启动后必须落盘指针，供下一次升级使用。
func TestWriteDeployPointerRecordsDir(t *testing.T) {
	isolateDeployState(t)
	dir := t.TempDir()
	writeDeployPointer(dir)
	got, ok := readDeployPointer()
	if !ok || got != dir {
		t.Fatalf("readDeployPointer = %q,%v, want %q,true", got, ok, dir)
	}
}

package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestRegistryMarkReloadCounts 台账写盘 → 重新加载 → 计数正确。
// 「重新加载」这一步是刻意的：管理页每次请求都重新 LoadRegistry，
// 等价于进程重启后的读路径，必须能读出写进去的东西。
func TestRegistryMarkReloadCounts(t *testing.T) {
	dir := t.TempDir()
	reg := LoadRegistry(dir)
	if err := reg.Mark(map[string]Origin{
		"u1": {Method: MethodSwitch, Producer: ProducerWorkbuddy, Label: "甲"},
		"u2": {Method: MethodApp, Producer: ProducerWorkbuddy, Label: "乙"},
		"u3": {Method: MethodFile, Producer: ProducerWorkbuddy, Label: "丙"},
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	reg2 := LoadRegistry(dir)
	got := reg2.All()
	if len(got) != 3 {
		t.Fatalf("reloaded %d origins, want 3", len(got))
	}
	if o := got["u1"]; o.Method != MethodSwitch || o.Label != "甲" || o.AddedAt == 0 {
		t.Errorf("u1 = %+v, want method=switch label=甲 added_at!=0", o)
	}
	counts := reg2.Counts()
	if counts[MethodSwitch] != 1 || counts[MethodApp] != 1 || counts[MethodFile] != 1 {
		t.Errorf("Counts = %v, want 1/1/1", counts)
	}
}

// TestRegistryPrune 凭证被删后，台账不留孤儿。
func TestRegistryPrune(t *testing.T) {
	dir := t.TempDir()
	reg := LoadRegistry(dir)
	if err := reg.Mark(map[string]Origin{
		"keep": {Method: MethodSwitch},
		"drop": {Method: MethodSwitch},
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if err := reg.Prune([]string{"keep"}); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	got := LoadRegistry(dir).All()
	if _, ok := got["drop"]; ok {
		t.Error("drop 仍在台账里，Prune 没生效")
	}
	if _, ok := got["keep"]; !ok {
		t.Error("keep 被误删")
	}
}

// TestRegistryFileIsNotAnAuthFile 台账文件落在 auths 目录里，但**绝不能被当成凭证**：
// auth.LoadDir 的 glob 是 workbuddy*.json，点号开头的 .origins.json 必须被跳过。
// 这条保证「整个 auths 目录拷到另一台机器」时不会把台账读成账号。
func TestRegistryFileIsNotAnAuthFile(t *testing.T) {
	dir := t.TempDir()
	if err := LoadRegistry(dir).Mark(map[string]Origin{
		"u1": {Method: MethodFile, Label: "甲"},
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}
	if _, err := os.Stat(RegistryPath(dir)); err != nil {
		t.Fatalf("台账文件没落盘: %v", err)
	}
	files, err := auth.LoadAuthFiles(dir)
	if err != nil {
		t.Fatalf("LoadAuthFiles: %v", err)
	}
	for _, f := range files {
		if filepath.Base(f) == ".origins.json" {
			t.Fatalf("台账被当成凭证读进来了: %v", files)
		}
	}
	if len(files) != 0 {
		t.Fatalf("空 auths 目录读出了 %d 个凭证 %v", len(files), files)
	}
}

// TestImportJSONAsRecordsOrigin 导入即记来源：这是「导入时选择账号属于哪个来源」
// 落到底层的那一步，必须真实生效。
func TestImportJSONAsRecordsOrigin(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"accessToken":"tok-1","refreshToken":"ref-1","expiresAt":1795104000,"domain":"www.workbuddy.ai","uid":"uid-1","nickname":"甲"}`)
	res, err := ImportJSONAs(dir, raw, nil, ImportOptions{Method: MethodFile, Detail: "别的机器导出的.json"})
	if err != nil {
		t.Fatalf("ImportJSONAs: %v", err)
	}
	if len(res.Imported) != 1 || res.Imported[0] != "uid-1" {
		t.Fatalf("Imported = %v, want [uid-1]", res.Imported)
	}
	o, ok := LoadRegistry(dir).Get("uid-1")
	if !ok {
		t.Fatal("导入后台账里没有 uid-1")
	}
	if o.Method != MethodFile || o.Producer != ProducerWorkbuddy || o.Label != "甲" || o.Detail != "别的机器导出的.json" {
		t.Errorf("origin = %+v, want method=file producer=workbuddy label=甲 detail=别的机器导出的.json", o)
	}
}

// TestImportDefaultsToFileMethod 不指定来源时按「导入文件」算（贴别机器导出的东西是主场景）。
func TestImportDefaultsToFileMethod(t *testing.T) {
	dir := t.TempDir()
	raw := []byte(`{"accessToken":"tok-2","expiresAt":1795104000,"domain":"www.workbuddy.ai","uid":"uid-2"}`)
	if _, err := ImportJSON(dir, raw, nil); err != nil {
		t.Fatalf("ImportJSON: %v", err)
	}
	o, ok := LoadRegistry(dir).Get("uid-2")
	if !ok || o.Method != MethodFile {
		t.Fatalf("origin = %+v ok=%v, want method=file", o, ok)
	}
}

// TestImportWBSwitchAsSwitchMethod 账本导入默认记「switch」来源。
func TestImportWBSwitchAsSwitchMethod(t *testing.T) {
	dir := t.TempDir()
	ledger := filepath.Join(t.TempDir(), "accounts.json")
	body := `[{"access_token":"tok-3","refresh_token":"ref-3","expiresAt":1795104000888,"domain":"www.codebuddy.cn","uid":"uid-3","nickname":"丙","variant":"cn"}]`
	if err := os.WriteFile(ledger, []byte(body), 0o600); err != nil {
		t.Fatalf("write ledger: %v", err)
	}
	if _, err := ImportWBSwitch(ledger, dir, nil); err != nil {
		t.Fatalf("ImportWBSwitch: %v", err)
	}
	o, ok := LoadRegistry(dir).Get("uid-3")
	if !ok {
		t.Fatal("账本导入后台账里没有 uid-3")
	}
	if o.Method != MethodSwitch {
		t.Errorf("method = %q, want %q", o.Method, MethodSwitch)
	}
	if o.Detail != ledger {
		t.Errorf("detail = %q, want 账本路径 %q", o.Detail, ledger)
	}
}

// TestProbeDoesNotPanic 探查是只读的，任何机器上都不能 panic；
// 不存在的路径必须 exists=false 且给出原因，不能编造账号数。
func TestProbeDoesNotPanic(t *testing.T) {
	targets := Probe()
	if len(targets) == 0 {
		t.Fatal("Probe 返回空列表")
	}
	for _, tg := range targets {
		if tg.ID == "" || tg.Name == "" {
			t.Errorf("探查目标缺 id/name: %+v", tg)
		}
		if !tg.Exists && tg.Accounts != 0 {
			t.Errorf("%s 不存在却报了账号数 %d（不许编造）", tg.ID, tg.Accounts)
		}
		if !tg.Exists && tg.Kind != KindNone {
			t.Errorf("%s 不存在却给了 kind=%q", tg.ID, tg.Kind)
		}
	}
}

// TestClassifyJSON 凭证字段分类：明文 / enc:v1 加密 / 都不是。
func TestClassifyJSON(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		enc   int
		plain int
	}{
		{"plain", `{"accessToken":"abc","refreshToken":"def"}`, 0, 2},
		{"encrypted", `{"oauth:bigmodel:access_token":"enc:v1:aaaa.bbbb.cccc"}`, 1, 0},
		{"mixed", `{"access_token":"enc:v1:x.y.z","refresh_token":"plain"}`, 1, 1},
		{"irrelevant", `{"theme":"dark","window":{"w":100}}`, 0, 0},
		{"nested", `{"zcodejwttoken":"enc:v1:a.b.c","theme":"dark"}`, 1, 0},
	}
	for _, c := range cases {
		var obj map[string]any
		if err := json.Unmarshal([]byte(c.body), &obj); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		enc, plain, _ := classifyJSON(obj)
		if enc != c.enc || plain != c.plain {
			t.Errorf("%s: enc=%d plain=%d, want enc=%d plain=%d", c.name, enc, plain, c.enc, c.plain)
		}
	}
}

// TestRegistryCrossInstanceRefresh 长期持有的实例能看到**别的实例**写盘的变更。
// 网关选号侧只 LoadRegistry 一次并长期持有，而导入路径是另起实例、Mark 后写盘；
// 不重读就会出现「导入成功了，/status 里 producer 还是旧的」。
func TestRegistryCrossInstanceRefresh(t *testing.T) {
	dir := t.TempDir()
	held := LoadRegistry(dir) // 模拟网关长期持有的那一个
	if _, ok := held.Get("u-new"); ok {
		t.Fatal("空台账不该有 u-new")
	}

	// 导入路径：另起一个实例写盘（与 handler 里的用法一致）。
	if err := LoadRegistry(dir).Mark(map[string]Origin{
		"u-new": {Method: MethodFile, Producer: ProducerZCode, Label: "新导入"},
	}); err != nil {
		t.Fatalf("Mark: %v", err)
	}

	// 绕开 1s 节流（生产语义是「最多晚 1 秒可见」，测试不必真等）。
	held.mu.Lock()
	held.checkedAt = time.Time{}
	held.mu.Unlock()

	o, ok := held.Get("u-new")
	if !ok || o.Producer != ProducerZCode || o.Label != "新导入" {
		t.Fatalf("held.Get(u-new) = (%+v, %v)，长期持有的实例没跟上落盘变更", o, ok)
	}
	if c := held.Counts(); c[MethodFile] != 1 {
		t.Errorf("held.Counts() = %v, want file=1", c)
	}
}

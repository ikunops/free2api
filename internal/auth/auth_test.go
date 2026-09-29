package auth

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseNested(t *testing.T) {
	raw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"domain":""},"account":{"uid":"u1","enterpriseId":"e1","nickname":"n1"}}`)
	sa, err := Parse(raw)
	if err != nil {
		t.Fatalf("nested parse err: %v", err)
	}
	if sa.AccessToken != "at" || sa.RefreshToken != "rt" || sa.ExpiresAt != 1753600000 {
		t.Errorf("tokens: %+v", sa)
	}
	if sa.UID != "u1" || sa.EnterpriseID != "e1" || sa.Nickname != "n1" {
		t.Errorf("account: %+v", sa)
	}
}

func TestParseFlat(t *testing.T) {
	raw := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1753600000,"uid":"u2","nickname":"n2"}`)
	sa, err := Parse(raw)
	if err != nil || sa.UID != "u2" || sa.AccessToken != "at" {
		t.Fatalf("flat: %+v %v", sa, err)
	}
}

func TestParseMissingToken(t *testing.T) {
	if _, err := Parse([]byte(`{"uid":"u3"}`)); err == nil {
		t.Fatal("want error for missing accessToken")
	}
}

func TestSaveAtomicRoundtrip(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1753600000,
		UID: "u1", EnterpriseID: "e1", Nickname: "n1", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(fp + ".tmp"); !os.IsNotExist(err) {
		t.Error("tmp file should not remain")
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.AccessToken != "at" || b.UID != "u1" || b.EnterpriseID != "e1" {
		t.Errorf("roundtrip: %+v", b)
	}
}

// TestLoadDirLoadsAllValid 不再按 region 过滤：所有可解析的 auth 文件都被加载，
// 解析失败的文件静默跳过。
func TestLoadDirLoadsAllValid(t *testing.T) {
	dir := t.TempDir()
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"cn1"}}`
	other := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"example.com"},"account":{"uid":"u2"}}`
	bad := `not json`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(cn), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-u2.json"), []byte(other), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy-bad.json"), []byte(bad), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 valid accounts, got %+v", list)
	}
	for _, a := range list {
		if a.FilePath == "" {
			t.Error("FilePath not set")
		}
	}
}

func TestNeedsRefresh(t *testing.T) {
	a := &Auth{ExpiresAt: 0}
	if !a.NeedsRefresh(0) {
		t.Error("zero expiry should need refresh")
	}
	a.ExpiresAt = 9999999999
	if a.NeedsRefresh(0) {
		t.Error("far future should not need refresh")
	}
}

// TestParseDeviceToken 嵌套形与扁平形 auth 文件的顶层 device_token 键均被解析。
func TestParseDeviceToken(t *testing.T) {
	nested := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"u1"},"device_token":"dev-tok-nested"}`)
	sa, err := Parse(nested)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if sa.DeviceToken != "dev-tok-nested" {
		t.Errorf("nested DeviceToken = %q want %q", sa.DeviceToken, "dev-tok-nested")
	}

	flat := []byte(`{"accessToken":"at","refreshToken":"rt","expiresAt":1,"uid":"u2","device_token":"dev-tok-flat"}`)
	fa, err := Parse(flat)
	if err != nil {
		t.Fatalf("flat parse: %v", err)
	}
	if fa.DeviceToken != "dev-tok-flat" {
		t.Errorf("flat DeviceToken = %q want %q", fa.DeviceToken, "dev-tok-flat")
	}
}

// TestSaveAtomicPreservesDeviceToken SaveAtomic 写回后顶层 device_token 被保留并重新解析回来。
func TestSaveAtomicPreservesDeviceToken(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-dt.json")
	a := &Auth{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 1,
		UID: "u1", DeviceToken: "persisted-tok", FilePath: fp}
	if err := a.SaveAtomic(); err != nil {
		t.Fatalf("save: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	b, err := Parse(raw)
	if err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if b.DeviceToken != "persisted-tok" {
		t.Errorf("roundtrip DeviceToken = %q want %q", b.DeviceToken, "persisted-tok")
	}
}

// TestLoadDirBackfillsRealm 存量迁移：LoadDir 加载目录时对空 realm 的 auth 自动
// backfill + SaveAtomic；已有 realm 的保持原值（不被 domain 覆盖）；文件全部带标识。
func TestLoadDirBackfillsRealm(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fixtures := map[string]string{
		"workbuddy-g1.json": `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":"www.workbuddy.ai"},"account":{"uid":"g1"}}`,
		"workbuddy-c1.json": `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"c1"}}`,
		// 已有 realm 的不因 domain 变化被覆盖：global domain + 显式 cn → 保持 cn
		"workbuddy-c2.json": `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":"www.workbuddy.ai","realm":"cn"},"account":{"uid":"c2"}}`,
	}
	for name, body := range fixtures {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("want 3 accounts, got %d", len(list))
	}
	want := map[string]string{"g1": "global", "c1": "cn", "c2": "cn"}
	for _, a := range list {
		// 内存态已补标识
		if got := a.RealmStored(); got != want[a.UID] {
			t.Errorf("uid=%s in-memory realm=%q want %q", a.UID, got, want[a.UID])
		}
		// 落盘文件也带 realm 键
		raw, err := os.ReadFile(a.FilePath)
		if err != nil {
			t.Fatalf("read %s: %v", a.FilePath, err)
		}
		b, err := Parse(raw)
		if err != nil {
			t.Fatalf("reparse %s: %v", a.FilePath, err)
		}
		if got := b.RealmStored(); got != want[a.UID] {
			t.Errorf("uid=%s on-disk realm=%q want %q", a.UID, got, want[a.UID])
		}
	}
}

// TestLoadDirBackfillWriteFailureDoesNotBlock 单个文件 backfill 落盘失败（tmp 预置目录
// 使 WriteFile 失败）不阻断启动：其他文件照常迁移，LoadDir 不向上抛错。
// （历史纯 CN auth 目录一次性迁移时，个别文件不可写不应让整个服务起不来。）
func TestLoadDirBackfillWriteFailureDoesNotBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	good := `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":"www.workbuddy.ai"},"account":{"uid":"g1"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-g1.json"), []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	// 预置同名 .tmp 目录 → SaveAtomic 的 os.WriteFile(".tmp") 报 is a directory。
	if err := os.Mkdir(filepath.Join(dir, "workbuddy-c1.json.tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	bad := `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1},"account":{"uid":"c1"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-c1.json"), []byte(bad), 0o600); err != nil {
		t.Fatal(err)
	}

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load err=%v want nil (write failure must not block startup)", err)
	}
	if len(list) != 2 {
		t.Fatalf("want 2 accounts loaded, got %d", len(list))
	}
	// 好文件迁移成功
	raw, _ := os.ReadFile(filepath.Join(dir, "workbuddy-g1.json"))
	b, _ := Parse(raw)
	if b.RealmStored() != "global" {
		t.Errorf("good file realm=%q want global (migration should succeed)", b.RealmStored())
	}
}

// TestLoadDirDuplicateUIDWarning 同 UID 双 realm auth 文件（概率近零的 EDGE）：LoadDir
// 检测到重复 UID 时打 WARN（含两文件路径），且不改变加载行为——后载入者胜出（返回 1 个、
// 不 panic、realm 为后载入者值）。LoadDir 现在有额外 seenUID 副作用，逐字验证 WARN。
func TestLoadDirDuplicateUIDWarning(t *testing.T) {
	dir := t.TempDir()
	// 同一 UID u9 的两个文件：cn realm 文件按文件名排序在前（workbuddy-a-...），
	// global realm 文件在后 → 后载入者（global）胜出。
	cn := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":"www.codebuddy.cn"},"account":{"uid":"u9"}}`
	gl := `{"auth":{"accessToken":"at2","refreshToken":"r","expiresAt":1,"domain":"www.workbuddy.ai"},"account":{"uid":"u9"}}`
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-a-cn.json"), []byte(cn), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workbuddy-z-global.json"), []byte(gl), 0o600); err != nil {
		t.Fatal(err)
	}

	// 捕获 log 输出（本测试不 t.Parallel：log.SetOutput 是进程级全局，需串行）。
	old := log.Writer()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	log.SetOutput(w)

	list, err := LoadDir(dir)
	_ = w.Close()
	raw, _ := io.ReadAll(r)
	log.SetOutput(old)

	if err != nil {
		t.Fatalf("load err=%v", err)
	}
	// 行为稳定（不改加载结果）：LoadDir 返回全部可解析文件（去重发生在 pool.SyncToDir
	// 的 UID 键 upsert），不 panic。
	if len(list) != 2 {
		t.Fatalf("want 2 accounts loaded (dedup later in pool), got %d", len(list))
	}
	// WARN 已触发且含两文件路径。
	if !strings.Contains(string(raw), "WARN: uid") ||
		!strings.Contains(string(raw), "duplicated") ||
		!strings.Contains(string(raw), "workbuddy-a-cn.json") ||
		!strings.Contains(string(raw), "workbuddy-z-global.json") {
		t.Errorf("expected WARN with both paths, got output: %s", string(raw))
	}
}

// TestAuthFileGlobUnification (P2-10, 发现 10)：网关 LoadDir 与 cmd 运维工具
// （signin/credit/trial）此前用两套 glob——workbuddy*.json vs workbuddy-*.json，
// 不带连字符的文件（如 workbuddy_new.json）被网关加载却被运维工具跳过。
// 统一导出宽侧模式 AuthFileGlob + 文件清单函数 LoadAuthFiles，cmd 工具复用。
func TestAuthFileGlobUnification(t *testing.T) {
	if AuthFileGlob != "workbuddy*.json" {
		t.Errorf("AuthFileGlob=%q want workbuddy*.json（宽侧为准）", AuthFileGlob)
	}
	dir := t.TempDir()
	doc := `{"auth":{"accessToken":"at","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`
	os.WriteFile(filepath.Join(dir, "workbuddy-cn1.json"), []byte(doc), 0o600)
	os.WriteFile(filepath.Join(dir, "workbuddy_new.json"), []byte(doc), 0o600)

	files, err := LoadAuthFiles(dir)
	if err != nil {
		t.Fatalf("LoadAuthFiles: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files=%v want 2（含不带连字符的 workbuddy_new.json）", files)
	}
	// 排序稳定性：与 cmd 工具原 sort.Strings 口径一致。
	if filepath.Base(files[0]) != "workbuddy-cn1.json" || filepath.Base(files[1]) != "workbuddy_new.json" {
		t.Errorf("files not sorted: %v", files)
	}
}

// TestLoadDirLoadsNonHyphenFile 网关侧零回归锚点：不带连字符文件本就
// 被宽侧 glob 加载，此测试锁死该行为不被"统一"改窄。
func TestLoadDirLoadsNonHyphenFile(t *testing.T) {
	dir := t.TempDir()
	doc := `{"auth":{"accessToken":"at1","refreshToken":"r","expiresAt":1,"domain":""},"account":{"uid":"u1"}}`
	os.WriteFile(filepath.Join(dir, "workbuddy_new.json"), []byte(doc), 0o600)

	list, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(list) != 1 || list[0].UID != "u1" {
		t.Fatalf("list=%+v want 1 account (uid=u1)", list)
	}
}

// TestNeedsRefreshStaticKeyProducer 静态密钥型生产者（zcode）的凭证没有过期时间
// （ExpiresAt 恒 0），NeedsRefresh 必须恒 false——否则 chat 每个请求都会走
// 「刷新 → 上游没有刷新端点 → 换号」，把整个号池无谓轮空（实测表现：/status 全绿、
// /v1/models 正常，但 /v1/chat/completions 恒 503 no_healthy_account）。
// workbuddy 的 OAuth 语义（零值 = 需刷新）在同一条测试里对照锚定，防回归。
func TestNeedsRefreshStaticKeyProducer(t *testing.T) {
	t.Parallel()
	zcodeDoc := `{"accessToken":"<32hex>.<16b62>","refreshToken":"","expiresAt":0,
		"domain":"https://open.bigmodel.cn/api/paas/v4","uid":"zcode-1","producer":"zcode",
		"upstream_base":"https://open.bigmodel.cn/api/paas/v4"}`
	a, err := Parse([]byte(zcodeDoc))
	if err != nil {
		t.Fatalf("parse zcode auth: %v", err)
	}
	if a.Producer() != "zcode" {
		t.Fatalf("Producer() = %q want zcode", a.Producer())
	}
	if a.UpstreamBase() != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("UpstreamBase() = %q want 智谱 base（扁平形 upstream_base 未解析）", a.UpstreamBase())
	}
	if a.NeedsRefresh(0) {
		t.Error("zcode NeedsRefresh(0) = true, want false（静态密钥无过期时间，不该刷新）")
	}
	if a.NeedsRefresh(2 * time.Hour) {
		t.Error("zcode NeedsRefresh(2h) = true, want false")
	}

	// 对照：workbuddy 的 OAuth token 零 expiresAt 仍判「需刷新」，零回归。
	wbDoc := `{"accessToken":"at","refreshToken":"rt","expiresAt":0,"domain":"","uid":"wb1"}`
	wb, err := Parse([]byte(wbDoc))
	if err != nil {
		t.Fatalf("parse workbuddy auth: %v", err)
	}
	if !wb.NeedsRefresh(0) {
		t.Error("workbuddy NeedsRefresh(0) = false, want true（零值 = 需刷新，语义不变）")
	}
}

// TestParseProducerBothForms producer / upstream_base 在嵌套形与扁平形两种磁盘
// 形态下都解析，且 SaveAtomic 写回后仍在（单文件整体迁移语义：拷到另一台机器
// 不带来源台账也依然知道自己该打哪套上游）。
func TestParseProducerBothForms(t *testing.T) {
	t.Parallel()
	nestedRaw := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},` +
		`"account":{"uid":"u1"},"producer":"zcode","upstream_base":"https://open.bigmodel.cn/api/paas/v4"}`)
	na, err := Parse(nestedRaw)
	if err != nil {
		t.Fatalf("nested parse: %v", err)
	}
	if na.Producer() != "zcode" || na.UpstreamBase() != "https://open.bigmodel.cn/api/paas/v4" {
		t.Errorf("nested producer=%q upstream_base=%q", na.Producer(), na.UpstreamBase())
	}

	// 未标注 producer 的历史凭证：Producer() 为空串（调用方按 workbuddy 处置），
	// SaveAtomic 也不引入空键（旧文件零改动）。
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-legacy.json")
	legacy := []byte(`{"auth":{"accessToken":"at","refreshToken":"rt","expiresAt":1,"domain":""},"account":{"uid":"legacy"}}`)
	if err := os.WriteFile(fp, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	la, err := Parse(legacy)
	if err != nil {
		t.Fatalf("legacy parse: %v", err)
	}
	la.FilePath = fp
	if la.Producer() != "" {
		t.Errorf("legacy Producer() = %q want 空", la.Producer())
	}
	if err := la.SaveAtomic(); err != nil {
		t.Fatalf("save legacy: %v", err)
	}
	raw, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "producer") || strings.Contains(string(raw), "upstream_base") {
		t.Errorf("空 producer 不该被写回: %s", raw)
	}
}

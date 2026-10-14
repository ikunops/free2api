package source

import (
	"testing"

	"free2api/internal/auth"
)

// TestImportJSONPreservesProducer 粘贴/导入文件通道必须保留条目自带的 producer。
//
// 回归的现场故障：ImportOptions.normalized() 曾把空 Producer 填成 workbuddy，而
// importBlob 又把非空 opt.Producer 当"导入器显式指定"覆盖条目原值，于是「获取源 →
// 导入文件」灌进来的 zcode/opencode/kilo 号全被标成 workbuddy。选号按台账 producer 做
// 生产者过滤（gateway.run.go 的 SetProducerOf 读的就是 .origins.json），错标会把
// workbuddy 模型派给别家号 → 上游 11102 → 封号 6h。
func TestImportJSONPreservesProducer(t *testing.T) {
	dir := t.TempDir()
	blob := []byte(`[
		{"uid":"zcode-aaa","nickname":"Z","producer":"zcode","access_token":"TZ","refresh_token":"RZ","expiresAt":1795104000,"domain":"open.bigmodel.cn","realm":"cn"},
		{"uid":"oc-bbb","nickname":"O","producer":"opencode","access_token":"TO","expiresAt":1795104000,"domain":"opencode.ai","realm":"cn"},
		{"uid":"kilo-ccc","nickname":"K","producer":"kilo","access_token":"TK","expiresAt":1795104000,"domain":"kilo.ai","realm":"cn"},
		{"uid":"plain-ddd","nickname":"P","access_token":"TP","expiresAt":1795104000,"domain":"www.codebuddy.cn","realm":"cn"}
	]`)
	if _, err := ImportJSON(dir, blob, nil); err != nil {
		t.Fatal(err)
	}

	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	byUID := map[string]*auth.Auth{}
	for _, a := range auths {
		byUID[a.UID] = a
	}
	// plain-ddd 没带 producer，才允许落到 workbuddy 兜底。
	want := map[string]string{
		"zcode-aaa": ProducerZCode,
		"oc-bbb":    ProducerOpenCode,
		"kilo-ccc":  ProducerKilo,
		"plain-ddd": ProducerWorkbuddy,
	}
	for uid, w := range want {
		a := byUID[uid]
		if a == nil {
			t.Fatalf("%s 没进池（imported 缺失）", uid)
		}
		if got := a.Producer(); got != w {
			t.Errorf("凭证 %s producer = %q, want %q（被导入器默认值覆盖了？）", uid, got, w)
		}
	}

	// 台账是选号真正读的那一份，必须与凭证一致。
	reg := LoadRegistry(dir)
	for uid, w := range want {
		o, ok := reg.Get(uid)
		if !ok {
			t.Fatalf("台账缺 %s（.origins.json 没记上）", uid)
		}
		if o.Producer != w {
			t.Errorf("台账 %s producer = %q, want %q", uid, o.Producer, w)
		}
		if o.Method != MethodFile {
			t.Errorf("台账 %s method = %q, want %q", uid, o.Method, MethodFile)
		}
	}
}

// TestImportJSONExplicitProducerStillOverrides 显式指定 producer 的通道（各专用导入器、
// 以及带 producer 的 ImportJSONAs 调用方）行为不变：opt.Producer 仍是权威。
func TestImportJSONExplicitProducerStillOverrides(t *testing.T) {
	dir := t.TempDir()
	blob := []byte(`{"uid":"zcode-ddd","nickname":"Z","producer":"zcode","access_token":"TX","expiresAt":1795104000,"domain":"open.bigmodel.cn","realm":"cn"}`)
	opt := ImportOptions{Method: MethodFile, Producer: ProducerQoder}
	if _, err := ImportJSONAs(dir, blob, nil, opt); err != nil {
		t.Fatal(err)
	}
	auths, err := auth.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 || auths[0].Producer() != ProducerQoder {
		t.Fatalf("显式 Producer 未生效: %+v", auths)
	}
	if o, _ := LoadRegistry(dir).Get("zcode-ddd"); o.Producer != ProducerQoder {
		t.Fatalf("台账未记录显式 Producer: %+v", o)
	}
}

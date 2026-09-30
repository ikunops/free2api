// zcode_entitled_stale_test.go 收敛结果「不许中途回退」的回归锁。
//
// 线上症状两条：
//  1. 面板上一秒还写着「ZCode · 可用模型 1 个」，下一秒跳回「11 个」——TTL 到点时
//     旧实现先返回「不知道」，调用方就不收敛了，整家 GLM 目录又列出来；
//  2. 上游一次 401 / 超时把 known 覆盖成 false，同样掀回 11 个。
//
// 收敛结果还落了盘（zcode_entitled.json），跨重启也不能回退。
package server

import (
	"path/filepath"
	"testing"
	"time"

	"free2api/internal/source"
)

// zcodeEntitledState 读包级缓存的快照（测试断言用）。
func zcodeEntitledState() (map[string]bool, bool, time.Time) {
	zcodeEntitledCache.RLock()
	defer zcodeEntitledCache.RUnlock()
	cp := map[string]bool{}
	for k, v := range zcodeEntitledCache.set {
		cp[k] = v
	}
	return cp, zcodeEntitledCache.known, zcodeEntitledCache.fetched
}

// agedZCodeEntitled 把「数据新鲜度」推老（模拟 TTL 到点），但 triedAt 留当下：
// 本测试不触发后台刷新，不许有隐藏的上游 IO。
func agedZCodeEntitled() {
	zcodeEntitledCache.Lock()
	zcodeEntitledCache.fetched = time.Now().Add(-2 * creditsTTL)
	zcodeEntitledCache.triedAt = time.Now()
	zcodeEntitledCache.Unlock()
}

// TestZCodeEntitledStaleStillConverges TTL 到点后仍须用上次问到的集合继续收敛
// （stale-while-revalidate）：宁可按旧结果收，也不能中途退回整家目录。
func TestZCodeEntitledStaleStillConverges(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	agedZCodeEntitled()

	set, known, _ := zcodeEntitledState()
	if _, ok := set["glm-5.3-flash"]; !known || !ok {
		t.Fatalf("过期后仍应拿着旧集合（known=true, glm-5.3-flash），得到 known=%v set=%v", known, set)
	}
	if ids := zcodeIDs(h.modelList()); len(ids) != 1 || ids[0] != "zcode:glm-5.3-flash" {
		t.Fatalf("TTL 到点也不该回退成整家目录，应仍是收敛后的 1 条，得到 %v", ids)
	}
}

// TestZCodeEntitledReadFailureKeepsCache 一行都没从上游读回来（401 / 超时 / 凭据解不开）
// 是「读不到」不是「没有」：不许清缓存，也不许把 fetched 推新（要留着重试）。
func TestZCodeEntitledReadFailureKeepsCache(t *testing.T) {
	newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	before, _, beforeFetched := zcodeEntitledState()

	storeZCodeEntitled([]creditRow{{UID: "zcode-a", Producer: source.ProducerZCode, OK: false}})

	after, known, afterFetched := zcodeEntitledState()
	if !known {
		t.Fatal("读失败不是「没有额度」，known 不该被覆盖成 false（那会把清单掀回 11 个）")
	}
	if len(after) != len(before) || !after["glm-5.3-flash"] {
		t.Fatalf("读失败应原样保留上次问到的集合，before=%v after=%v", before, after)
	}
	if !afterFetched.Equal(beforeFetched) {
		t.Fatalf("读失败不该推新 fetched（否则 TTL 又要等一整轮），before=%v after=%v", beforeFetched, afterFetched)
	}
}

// TestZCodeEntitledTrulyEmptyClears 读成功但套餐确实没有额度（行 OK 而 entitled 为空）：
// 如实写空、known=false，调用方不收敛（不假装能用，也不假装一个模型都没有）。
func TestZCodeEntitledTrulyEmptyClears(t *testing.T) {
	newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))

	storeZCodeEntitled([]creditRow{{UID: "zcode-a", Producer: source.ProducerZCode, OK: true}})

	set, known, _ := zcodeEntitledState()
	if known || len(set) != 0 {
		t.Fatalf("确实没额度要如实清空（known=false, 空集合），得到 known=%v set=%v", known, set)
	}
}

// TestZCodeEntitledPersistsAcrossRestart 收敛结果落盘后跨重启保留：否则重启后的头几秒
// /v1/models 与发布清单会先把整家目录（11 个）列出去再跳回来——用户看到的就是「怎么又
// 变回 11 个了」。
func TestZCodeEntitledPersistsAcrossRestart(t *testing.T) {
	newZCodeCatalogHandler(t)
	path := filepath.Join(t.TempDir(), "zcode_entitled.json")
	SetZCodeEntitledPath(path)
	t.Cleanup(func() { SetZCodeEntitledPath("") })

	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))

	// 模拟进程重启：内存缓存清空，再从盘上读回。
	zcodeEntitledCache.Lock()
	zcodeEntitledCache.set = nil
	zcodeEntitledCache.known = false
	zcodeEntitledCache.fetched = time.Time{}
	zcodeEntitledCache.Unlock()

	LoadZCodeEntitled()

	set, known, _ := zcodeEntitledState()
	if !known || !set["glm-5.3-flash"] || len(set) != 1 {
		t.Fatalf("重启后应从 zcode_entitled.json 读回收敛结果，得到 known=%v set=%v", known, set)
	}
}

// zcode_entitled_test.go zcode 的「上游目录 ≠ 号池能力」契约。
//
// 上游 /models 是**整家厂商的目录**（所有 GLM 型号），而号池里的号套餐常常只覆盖其中
// 一两个。线上实测：周末活动的号 /models 列 11 个、capabilities 只有 glm-5.3-flash。
// 发布清单与对外 /v1/models 都必须按后者收敛，否则客户端会选到一个必然失败的模型。
package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"free2api/internal/source"
	"free2api/internal/upstream"
)

// zcodeVendorCatalog 假上游的 /models 响应：整家厂商目录（11 个 GLM，与线上实测同形）。
const zcodeVendorCatalog = `{"object":"list","data":[` +
	`{"id":"glm-4.5","name":"GLM-4.5"},{"id":"glm-4.5-air","name":"GLM-4.5-Air"},` +
	`{"id":"glm-4.6","name":"GLM-4.6"},{"id":"glm-4.7","name":"GLM-4.7"},` +
	`{"id":"glm-5","name":"GLM-5"},{"id":"glm-5-turbo","name":"GLM-5-Turbo"},` +
	`{"id":"glm-5.1","name":"GLM-5.1"},{"id":"glm-5.2","name":"GLM-5.2"},` +
	`{"id":"glm-5.3","name":"GLM-5.3"},{"id":"glm-5.3-flash","name":"GLM-5.3-Flash"},` +
	`{"id":"glm-5.3-flashx","name":"GLM-5.3-FlashX"}]}`

// zcodeEntitledRow 造一条「这个 zcode 号有额度跑哪些模型」的额度行。
func zcodeEntitledRow(entitled ...string) []creditRow {
	return []creditRow{{UID: "zcode-a", Producer: source.ProducerZCode, OK: true, EntitledModels: entitled}}
}

// stubZCodeEntitled 直接把包级「有额度模型」缓存摆成想要的样子（测试隔离用）。
// triedAt 一并推到当下：冷却窗口内不再触发后台刷新，测试里不许有隐藏的上游 IO。
func stubZCodeEntitled(rows []creditRow) {
	storeZCodeEntitled(rows)
	zcodeEntitledCache.Lock()
	zcodeEntitledCache.triedAt = time.Now()
	zcodeEntitledCache.Unlock()
}

// newZCodeCatalogHandler 一个只有 zcode 号的网关 + 假上游目录。
func newZCodeCatalogHandler(t *testing.T) *Handler {
	t.Helper()
	resetModelsCache()
	stubZCodeEntitled(nil)
	t.Cleanup(func() { resetModelsCache(); stubZCodeEntitled(nil) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/models") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, zcodeVendorCatalog)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not found"}`)
	}))
	t.Cleanup(srv.Close)

	a := testZCodeAuth("zcode-a")
	a.SetZCodeJWT("jwt-a")
	a.SetZCodeDeviceMID("dmid-a")
	return NewHandler(Config{
		Pool: testPoolWith(a),
		Upstream: &upstream.Client{
			HTTP:          &http.Client{},
			ChatBaseZCode: srv.URL + "/api/paas/v4",
		},
	})
}

// zcodeIDs 挑出 /v1/models 结果里的 zcode 段 id。
func zcodeIDs(list []map[string]any) []string {
	out := []string{}
	for _, e := range list {
		if id, _ := e["id"].(string); strings.HasPrefix(id, "zcode:") {
			out = append(out, id)
		}
	}
	return out
}

// TestZCodePublishListMarksUnentitled 发布清单保留整份上游目录（未授权的也要看得见、
// 能勾），但逐条标出号池到底有没有额度——前端据此把没额度的收进折叠区。
func TestZCodePublishListMarksUnentitled(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	got := map[string]bool{}
	for _, e := range h.availableOutputModels() {
		if p, _ := e["producer"].(string); p != upstream.ProducerZCode {
			continue
		}
		id, _ := e["id"].(string)
		ent, _ := e["entitled"].(bool)
		got[id] = ent
	}
	if len(got) != 11 {
		t.Fatalf("zcode 候选应是整份上游目录（11 条，未授权也看得见），得到 %d 条", len(got))
	}
	if !got["glm-5.3-flash"] {
		t.Fatal("glm-5.3-flash 是这个号唯一有额度的模型，应标 entitled=true")
	}
	if got["glm-4.6"] {
		t.Fatal("glm-4.6 号池没额度，应标 entitled=false")
	}
}

// TestModelListHidesUnentitledZCode 对外 /v1/models 按额度收敛：列出去就等于承诺能调。
func TestModelListHidesUnentitledZCode(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	ids := zcodeIDs(h.modelList())
	if len(ids) != 1 || ids[0] != "zcode:glm-5.3-flash" {
		t.Fatalf("/v1/models 的 zcode 段应只剩有额度的那一个，得到 %v", ids)
	}
}

// TestModelListKeepsCatalogWhenEntitlementUnknown 读不到任何套餐额度时**不收敛**：
// 宁可多列，也不能假装号池一个模型都没有（会把能用的模型也一起藏掉）。
func TestModelListKeepsCatalogWhenEntitlementUnknown(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(nil)
	if ids := zcodeIDs(h.modelList()); len(ids) != 11 {
		t.Fatalf("没有任何额度信息时不该收敛，应保留 11 条，得到 %d 条：%v", len(ids), ids)
	}
}

// TestModelListKeepsExplicitlyPublishedUnentitled 用户显式勾进发布清单的照发：
// 白名单就是用户意图，压过这条自动收敛（否则「勾了等于没勾」）。
func TestModelListKeepsExplicitlyPublishedUnentitled(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	st := NewOutputStore("")
	st.cur = OutputConfig{Format: FormatOpenAI, Models: []string{modelKey(upstream.ProducerZCode, "glm-4.6")}}
	h.cfg.Output = st
	ids := zcodeIDs(h.modelList())
	if len(ids) != 1 || ids[0] != "zcode:glm-4.6" {
		t.Fatalf("白名单里的模型要照发，得到 %v", ids)
	}
}

// TestZCodeEntitlementSharedAcrossHandlers 出口是各自独立的 Handler 实例（channels.go
// 从同一份 baseCfg 复制），带自己的 credits 缓存。若把「有额度模型」挂在 handler 上，
// 主口收敛了、zcode 专用口还会列着整家目录——那份数据必须是包级共享的。
func TestZCodeEntitlementSharedAcrossHandlers(t *testing.T) {
	main := newZCodeCatalogHandler(t)
	// 模拟一个出口：同一份 pool / upstream，另起一个 handler（没有自己的额度缓存）。
	exit := NewHandler(Config{Pool: main.cfg.Pool, Upstream: main.cfg.Upstream, ProducerAllow: []string{upstream.ProducerZCode}})
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	for name, h := range map[string]*Handler{"主口": main, "出口": exit} {
		if ids := zcodeIDs(h.modelList()); len(ids) != 1 || ids[0] != "zcode:glm-5.3-flash" {
			t.Fatalf("%s：额度信息该是包级共享的，出口也得收敛成 1 条，得到 %v", name, ids)
		}
	}
}

// TestZCodePublishListUnentitledNotSelectedByDefault 发布清单里的 selected 是「这条现在真的
// 会发出去吗」，不是「白名单允不允许」：全放（空清单）时 zcode 未授权的那些本来就不发
// （见 modelList 同一处收敛），勾选态必须如实为 false。否则面板一打开就显示「未授权也全
// 勾着」，用户随手一保存就把它们写进白名单，反而真的把 11 条全发出去——与「按额度收敛」
// 自相矛盾。
func TestZCodePublishListUnentitledNotSelectedByDefault(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))

	selectedOf := func() map[string]bool {
		out := map[string]bool{}
		for _, e := range h.availableOutputModels() {
			if p, _ := e["producer"].(string); p != upstream.ProducerZCode {
				continue
			}
			id, _ := e["id"].(string)
			s, _ := e["selected"].(bool)
			out[id] = s
		}
		return out
	}

	sel := selectedOf()
	if len(sel) != 11 {
		t.Fatalf("zcode 候选应仍是整份上游目录（11 条），得到 %d", len(sel))
	}
	if !sel["glm-5.3-flash"] {
		t.Error("有额度的 glm-5.3-flash 在全放时该显示成已勾选")
	}
	if sel["glm-4.6"] {
		t.Error("未授权的 glm-4.6 在全放时不该显示成已勾选（它本来就不会发出去）")
	}

	// 用户显式勾上（写进白名单）→ 立刻如实变回已勾选，且对外真的发得出去。
	st := NewOutputStore("")
	if _, err := st.Set(OutputConfig{Format: FormatOpenAI,
		Models: []string{modelKey(upstream.ProducerZCode, "glm-4.6")}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	h.cfg.Output = st
	sel2 := selectedOf()
	if !sel2["glm-4.6"] {
		t.Error("显式勾进白名单的未授权模型，selected 应如实为 true")
	}
	if sel2["glm-5.3-flash"] {
		t.Error("白名单里没有 glm-5.3-flash，它不该显示成已勾选")
	}
	if ids := zcodeIDs(h.modelList()); len(ids) != 1 || ids[0] != "zcode:glm-4.6" {
		t.Errorf("白名单点名了 glm-4.6，对外就该只发这一条，得到 %v", ids)
	}
}

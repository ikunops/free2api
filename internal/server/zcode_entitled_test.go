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

// stubZCodeRoute 把某个 ZCode 号的静态反代证据摆成想要的状态。
// 权益和 route 是两层：只有真实 chat 成功才会写 OK=true。
func stubZCodeRoute(uid string, ok bool, reason string) {
	zcodeEntitledCache.Lock()
	defer zcodeEntitledCache.Unlock()
	if zcodeEntitledCache.routeByUID == nil {
		zcodeEntitledCache.routeByUID = map[string]zcodeRouteState{}
	}
	st := zcodeRouteState{OK: ok, Reason: reason, CheckedAt: time.Now().Unix()}
	if !ok && reason == "" {
		st.Reason = "stub route unavailable"
	}
	zcodeEntitledCache.routeByUID[uid] = st
	saveZCodeEntitledLocked(zcodeEntitledCache.set, zcodeEntitledCache.byUID,
		zcodeEntitledCache.modelsByUID, zcodeEntitledCache.routeByUID)
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

// TestZCodePublishListMarksUnentitled 发布清单保留整份上游目录（未授权/不可路由的也看得见供诊断），
// 但逐条标出号池有没有额度；前端据此禁用不可用行并收进折叠区。
func TestZCodePublishListMarksUnentitled(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	stubZCodeRoute("zcode-a", true, "")
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
	stubZCodeRoute("zcode-a", true, "")
	ids := zcodeIDs(h.modelList())
	if len(ids) != 1 || ids[0] != "zcode:glm-5.3-flash" {
		t.Fatalf("/v1/models 的 zcode 段应只剩有额度的那一个，得到 %v", ids)
	}
}

// TestModelListAllowsFirstUnverifiedRoute 新号有权益但还没有 route 证据时必须允许发布：
// 否则模型永远不出现，客户端没有机会打出第一次成功，route 也就永远无法被证明。
// 但概览「可用」仍保持保守，不能把“未知”算成“可用”。
func TestModelListAllowsFirstUnverifiedRoute(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	if ids := zcodeIDs(h.modelList()); len(ids) != 1 || ids[0] != "zcode:glm-5.3-flash" {
		t.Fatalf("route 未验证时模型应允许首次发布，得到 %v", ids)
	}
	if ok, present := zcodeAccountUsable("zcode-a"); !present || ok {
		t.Fatalf("route 未验证时可用数必须保守为 false（present=%v ok=%v）", present, ok)
	}
}

// TestModelListHidesAllKnownRouteFailures 所有有权益账号都明确失败时，模型必须收敛。
func TestModelListHidesAllKnownRouteFailures(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	stubZCodeRoute("zcode-a", false, "upstream 1113 insufficient balance")
	if ids := zcodeIDs(h.modelList()); len(ids) != 0 {
		t.Fatalf("唯一的权益号已明确失败，模型应隐藏，得到 %v", ids)
	}
}

// TestModelListKeepsRouteWithUnknownAccount 一个号明确失败、另一个号尚未验证时不能
// 把整条模型判死：未知号仍可能成功，必须保留一次验证机会。
func TestModelListKeepsRouteWithUnknownAccount(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled([]creditRow{
		{UID: "zcode-a", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
		{UID: "zcode-b", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
	})
	stubZCodeRoute("zcode-a", false, "upstream 1113 insufficient balance")
	if ids := zcodeIDs(h.modelList()); len(ids) != 1 || ids[0] != "zcode:glm-5.3-flash" {
		t.Fatalf("还有未知权益号时模型应保留，得到 %v", ids)
	}
}

// TestModelListHidesWhenEveryEntitledAccountFailed 多个权益号全部明确失败才隐藏；
// 同时发布清单要透出 route_available=false 供诊断。
func TestModelListHidesWhenEveryEntitledAccountFailed(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled([]creditRow{
		{UID: "zcode-a", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
		{UID: "zcode-b", Producer: source.ProducerZCode, OK: true, EntitledModels: []string{"glm-5.3-flash"}},
	})
	stubZCodeRoute("zcode-a", false, "upstream 1113 insufficient balance")
	stubZCodeRoute("zcode-b", false, "upstream 401/session dead")
	if ids := zcodeIDs(h.modelList()); len(ids) != 0 {
		t.Fatalf("所有权益号都明确失败时模型应隐藏，得到 %v", ids)
	}
	for _, e := range h.availableOutputModels() {
		if p, _ := e["producer"].(string); p != upstream.ProducerZCode {
			continue
		}
		if id, _ := e["id"].(string); id != "glm-5.3-flash" {
			continue
		}
		if route, _ := e["route_available"].(bool); route {
			t.Fatal("所有权益号失败时 route_available 必须为 false")
		}
		return
	}
	t.Fatal("发布清单应保留 glm-5.3-flash 供诊断")
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

// TestModelListKeepsExplicitlyPublishedUnentitled 白名单只能决定“发布哪几条”，
// 不能把没有套餐权益的模型重新放出来。
func TestModelListKeepsExplicitlyPublishedUnentitled(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	stubZCodeRoute("zcode-a", true, "")
	st := NewOutputStore("")
	st.cur = OutputConfig{Format: FormatOpenAI, Models: []string{modelKey(upstream.ProducerZCode, "glm-4.6")}}
	h.cfg.Output = st
	if ids := zcodeIDs(h.modelList()); len(ids) != 0 {
		t.Fatalf("显式白名单不能绕过 entitlement；应无 zcode 模型，得到 %v", ids)
	}
}

// TestModelListExplicitPublishStillNeedsRoute 用户显式勾进白名单也不能绕过 route。
func TestModelListExplicitPublishStillNeedsRoute(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	stubZCodeRoute("zcode-a", false, "upstream 1113 insufficient balance")
	st := NewOutputStore("")
	st.cur = OutputConfig{Format: FormatOpenAI, Models: []string{modelKey(upstream.ProducerZCode, "glm-5.3-flash")}}
	h.cfg.Output = st
	if ids := zcodeIDs(h.modelList()); len(ids) != 0 {
		t.Fatalf("route 不可用时显式白名单也必须被挡住，得到 %v", ids)
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
	stubZCodeRoute("zcode-a", true, "")
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
	stubZCodeRoute("zcode-a", true, "")

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

	// 用户显式勾上未授权模型：草稿里可以显示意图，但它仍然不可路由、不会发布。
	st := NewOutputStore("")
	if _, err := st.Set(OutputConfig{Format: FormatOpenAI,
		Models: []string{modelKey(upstream.ProducerZCode, "glm-4.6")}}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	h.cfg.Output = st
	sel2 := selectedOf()
	if sel2["glm-4.6"] {
		t.Error("未授权模型即使写进白名单，selected 也必须为 false")
	}
	if ids := zcodeIDs(h.modelList()); len(ids) != 0 {
		t.Fatalf("未授权模型不能因为白名单就对外发布，得到 %v", ids)
	}
}

// TestAvailableOutputModelsExposesRouteUnavailable 目录里保留有权益但静态凭据
// 不可路由的行，供诊断；selected=false 且 route_available=false，前端会禁用复选框。
func TestAvailableOutputModelsExposesRouteUnavailable(t *testing.T) {
	h := newZCodeCatalogHandler(t)
	stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
	stubZCodeRoute("zcode-a", false, "upstream 1113 insufficient balance")

	var row map[string]any
	for _, e := range h.availableOutputModels() {
		if p, _ := e["producer"].(string); p != upstream.ProducerZCode {
			continue
		}
		if id, _ := e["id"].(string); id == "glm-5.3-flash" {
			row = e
			break
		}
	}
	if row == nil {
		t.Fatal("发布清单应保留有权益但不可路由的 ZCode 行供诊断")
	}
	if ent, _ := row["entitled"].(bool); !ent {
		t.Fatal("glm-5.3-flash 有权益，entitled 应为 true")
	}
	if route, _ := row["route_available"].(bool); route {
		t.Fatal("静态凭据失败时 route_available 必须为 false")
	}
	if selected, _ := row["selected"].(bool); selected {
		t.Fatal("不可路由的模型不能显示为已选中")
	}
	if reason, _ := row["route_reason"].(string); !strings.Contains(reason, "insufficient balance") {
		t.Fatalf("应透出真实不可路由原因，得到 %q", reason)
	}
}

// TestSeedZCodeRoutesTimestampsPoolOnlyFailure 线上旧快照里存在「明确 1113/401，
// 但没有 checked_at」的负证据。升级后号池仍处于 hard-credit / session-dead 时，
// 补种必须给它一个当前时间，否则发布门控会把它当成“从未验证”，导致
// /admin/output 显示 route_available=true，而 /v1/models 又把模型藏掉。
// 同时该合成时间只能写一次，不能被每次 /status 刷新不断续期。
func TestSeedZCodeRoutesTimestampsPoolOnlyFailure(t *testing.T) {
	cases := []struct {
		name   string
		mark   func(*Handler)
		reason string
	}{
		{
			name:   "hard credit without last error",
			mark:   func(h *Handler) { h.cfg.Pool.CooldownUntilTomorrow4AM("zcode-a", "余额不足") },
			reason: "insufficient balance",
		},
		{
			name:   "session dead without last error",
			mark:   func(h *Handler) { h.cfg.Pool.Disable("zcode-a", "upstream 401/session dead") },
			reason: "session dead",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newZCodeCatalogHandler(t)
			stubZCodeEntitled(zcodeEntitledRow("glm-5.3-flash"))
			tc.mark(h)

			for _, st := range h.cfg.Pool.List() {
				if st.UID == "zcode-a" && !st.LastErrTime.IsZero() {
					t.Fatalf("测试前提失败：号池失败时间应为零，得到 %v", st.LastErrTime)
				}
			}
			h.seedZCodeRoutesFromPool()

			zcodeEntitledCache.RLock()
			route := zcodeEntitledCache.routeByUID["zcode-a"]
			zcodeEntitledCache.RUnlock()
			if route.CheckedAt <= 0 {
				t.Fatal("号池已有明确失败时，补种必须写入 checked_at")
			}
			if !strings.Contains(route.Reason, tc.reason) {
				t.Fatalf("route reason=%q 应包含 %q", route.Reason, tc.reason)
			}
			if ids := zcodeIDs(h.modelList()); len(ids) != 0 {
				t.Fatalf("所有权益号都有明确失败时模型应隐藏，得到 %v", ids)
			}
			blocked, reasons, known := zcodeBlockedModels()
			if !known || !blocked["glm-5.3-flash"] {
				t.Fatalf("明确失败应进入发布门控：known=%v blocked=%v reasons=%v", known, blocked, reasons)
			}

			// 模拟时间已经过去一分钟；再次刷新 /status 不应把它续到现在。
			old := time.Now().Add(-time.Minute).Unix()
			zcodeEntitledCache.Lock()
			route.CheckedAt = old
			zcodeEntitledCache.routeByUID["zcode-a"] = route
			zcodeEntitledCache.Unlock()
			h.seedZCodeRoutesFromPool()
			zcodeEntitledCache.RLock()
			got := zcodeEntitledCache.routeByUID["zcode-a"].CheckedAt
			zcodeEntitledCache.RUnlock()
			if got != old {
				t.Fatalf("同一负证据被刷新续期：checked_at=%d want %d", got, old)
			}
		})
	}
}

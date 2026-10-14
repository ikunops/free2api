package server

import (
	"testing"

	"free2api/internal/auth"
)

// TestRefreshCatalogsRereadsUpstream 周期刷新绕过 1h TTL：上游目录换了一批模型后，
// refreshCatalogs 立刻把新名单写进缓存（自动同步上下架），不必等 TTL 过期。
func TestRefreshCatalogsRereadsUpstream(t *testing.T) {
	body := `{"code":0,"data":{"models":[
		{"id":"m1","maxInputTokens":65536,"maxOutputTokens":8192}
	],"agents":[{"name":"cli","models":["m1"]}]}}`
	up := newFakeUpstream(t, func(authz string) (int, string, bool) {
		return 200, body, false
	})
	resetModelsCache()
	p := testPoolWith(&auth.Auth{UID: "u1", AccessToken: "at1", ExpiresAt: 9999999999})
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: false})

	if got := h.modelList(); len(got) != 1 || got[0]["id"] != "cn:m1" {
		t.Fatalf("initial modelList=%v want [cn:m1]", got)
	}
	// 上游换名单：m1 下架、m2 上架。
	body = `{"code":0,"data":{"models":[
		{"id":"m2","maxInputTokens":65536,"maxOutputTokens":8192}
	],"agents":[{"name":"cli","models":["m2"]}]}}`

	// TTL 内普通读取仍拿旧缓存（1h 未过期）。
	if got := h.modelList(); len(got) != 1 || got[0]["id"] != "cn:m1" {
		t.Fatalf("within TTL modelList=%v want still [cn:m1]", got)
	}
	// 周期刷新：绕过 TTL，立刻反映新名单。
	h.WarmCatalog()
	if got := h.modelList(); len(got) != 1 || got[0]["id"] != "cn:m2" {
		t.Fatalf("after refresh modelList=%v want [cn:m2]", got)
	}
}

// TestRefreshCatalogsNoPoolNoPanic 无号池/无上游时刷新必须安全返回（启动早期或测试形态）。
func TestRefreshCatalogsNoPoolNoPanic(t *testing.T) {
	h := NewHandler(Config{})
	h.WarmCatalog() // 不 panic 即通过
}

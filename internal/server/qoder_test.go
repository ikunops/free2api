package server

import (
	"testing"

	"free2api/internal/upstream"
)

// TestResolveModelRouteQoder 锁定 qoder: 前缀路由：客户端把 /v1/models 的整串
// id 原样回传时必须固定到 qoder 号池，且裸名不含前缀（出站要用裸 key 查目录）。
func TestResolveModelRouteQoder(t *testing.T) {
	cases := []struct{ in, realm, producer, bare string }{
		{"qoder:gfmodel", "cn", "qoder", "gfmodel"},
		{"cn:qoder:gfmodel", "cn", "qoder", "gfmodel"},
		{"qoder:cn:gfmodel", "cn", "qoder", "gfmodel"},
	}
	for _, c := range cases {
		realm, producer, bare := resolveModelRoute(c.in)
		if realm != c.realm || producer != c.producer || bare != c.bare {
			t.Errorf("resolveModelRoute(%q) = (%q,%q,%q)，want (%q,%q,%q)",
				c.in, realm, producer, bare, c.realm, c.producer, c.bare)
		}
	}
	// 前缀必须大小写敏感、且不能被当作裸名的一段。
	if realm, producer, bare := resolveModelRoute("QODER:gfmodel"); producer != "" || bare != "QODER:gfmodel" || realm != "cn" {
		t.Errorf("大写前缀不该被剥离：(%q,%q,%q)", realm, producer, bare)
	}
}

// TestModelIDQoderNoRealm qoder 对外模型名是 producer:bare（无 realm 段），
// 与 /v1/models 的拼装、入站剥除必须对称。
func TestModelIDQoderNoRealm(t *testing.T) {
	h := NewHandler(Config{Output: NewOutputStore("")})
	if got := h.modelIDFor("cn", upstream.ProducerQoder, "gfmodel", "x0.00"); got != "qoder:gfmodel-free" {
		t.Errorf("modelIDFor = %q, want qoder:gfmodel-free", got)
	}
	_, producer, bare := resolveModelPrefixed("qoder:gfmodel-free", "")
	if producer != "qoder" || bare != "gfmodel" {
		t.Errorf("回环解析 = (%q,%q)，want (qoder,gfmodel)", producer, bare)
	}
}

// TestPublishKeyQoder 发布清单键：qoder:bare 独立于同名 CN/其它来源模型，
// 勾了 qoder 的不能顺带放开别家同名模型。
func TestPublishKeyQoder(t *testing.T) {
	if got := publishKey("cn", upstream.ProducerQoder, "gfmodel"); got != "qoder:gfmodel" {
		t.Errorf("publishKey = %q", got)
	}
	if publishKey("cn", upstream.ProducerQoder, "gfmodel") == publishKey("cn", "zcode", "gfmodel") {
		t.Error("不同来源的同名模型不能共用一个发布键")
	}
}

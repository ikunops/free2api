package server

import "testing"

// TestResolveModel 覆盖 PLAN D6 前缀解析协议：
// 取第一个 ":"，前段为 cn/global 才剥离，否则 (cn, 原串)。
func TestResolveModel(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"cn:glm-5.2", "cn", "glm-5.2"},
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"glm-5.2", "cn", "glm-5.2"},
		{"deepseek:v3", "cn", "deepseek:v3"}, // 冒号前段不在枚举内，不剥离
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

// TestResolveModelEdge 边界：空串、仅冒号、空前缀、大小写。
func TestResolveModelEdge(t *testing.T) {
	cases := []struct {
		in        string
		wantRealm string
		wantBare  string
	}{
		{"", "cn", ""},
		{":", "cn", ":"},
		{":model", "cn", ":model"},             // 空前缀不匹配 cn/global，不剥离
		{"GLOBAL:gpt-5", "cn", "GLOBAL:gpt-5"}, // 大小写敏感：不做归一
		{"global:", "global", ""},              // 前缀合法 + 空裸名仍剥离
		{"global:gpt-5.4", "global", "gpt-5.4"},
		{"cn:", "cn", ""},
	}
	for _, c := range cases {
		realm, bare := resolveModel(c.in)
		if realm != c.wantRealm || bare != c.wantBare {
			t.Errorf("resolveModel(%q)=(%q,%q) want (%q,%q)", c.in, realm, bare, c.wantRealm, c.wantBare)
		}
	}
}

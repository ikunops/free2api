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

// TestModelIDNoRealmForNonWorkbuddy 回归：realm 段（cn/global）只属于 WorkBuddy 的
// 「国内版 / 国际版」两种域，其他来源（zcode / opencode / kilo）没有域概念，对外模型名
// 不得带 realm 段。此前 modelIDFor 对所有来源硬拼 realm，opencode 的模型名变成
// "cn:opencode:xxx"，被误读成「这些模型也分国内国际」。
func TestModelIDNoRealmForNonWorkbuddy(t *testing.T) {
	h := NewHandler(Config{Output: NewOutputStore("")})
	cases := []struct{ realm, producer, bare, want string }{
		{"cn", "", "auto", "cn:auto"},                           // 主口裸 workbuddy 名
		{"cn", "workbuddy", "auto", "cn:auto"},                  // 显式 workbuddy 也省 producer 段
		{"global", "", "gpt-5.4", "global:gpt-5.4"},             // 国际版照旧带 realm
		{"cn", "zcode", "glm-4.6", "zcode:glm-4.6"},             // 非 WB：producer 段，无 realm
		{"cn", "opencode", "big-pickle", "opencode:big-pickle"}, // 非 WB：无 realm
		{"cn", "kilo", "kilo-auto/free", "kilo:kilo-auto/free"}, // 非 WB：无 realm
	}
	for _, c := range cases {
		if got := h.modelIDFor(c.realm, c.producer, c.bare, ""); got != c.want {
			t.Errorf("modelIDFor(%q,%q,%q)=%q want %q", c.realm, c.producer, c.bare, got, c.want)
		}
	}
}

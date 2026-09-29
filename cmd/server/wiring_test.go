package main

import (
	"path/filepath"
	"reflect"
	"testing"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/server"
)

// realmPool 构造含 cn/global 账号的池并确保 global 开关开启（缺省）。
func realmPool(t *testing.T) *pool.Pool {
	t.Helper()
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	return p
}

// TestRealmAwareAvailableForModel 断言会话粘性的 realm 感知闭包：
// 带前缀的模型名按 realm 过滤可用账号，裸名走 cn。
func TestRealmAwareAvailableForModel(t *testing.T) {
	p := realmPool(t)
	fn := realmAwareAvailableForModel(p, nil)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1"}},       // 裸名 → cn 集合
		{"cn:glm-5.2", []string{"cn1"}},    // 显式 cn 前缀 → cn 集合
		{"global:gpt-5.4", []string{"g1"}}, // global 前缀 → global 集合
	}
	for _, c := range cases {
		if got := fn(c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

// TestRealmAwareAvailableForModelGlobalDisabled 逃生门：全局开关显式关闭（config
// "enabled": false）时，即便 auth 写了 realm=global 也不路由 global——闭包对 global:
// 前缀返回空集（纯 CN 锁定语义与旧缺省等价）。
func TestRealmAwareAvailableForModelGlobalDisabled(t *testing.T) {
	auth.SetGlobalEnabled(false)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "g1", Domain: "www.workbuddy.ai"})
	fn := realmAwareAvailableForModel(p, nil)

	// 开关关闭 → 该 global 账号 Realm()=="cn"（逃生门），对 global: 前缀不可见。
	if got := fn("global:gpt-5.4"); len(got) != 0 {
		t.Errorf("global disabled: AvailableForModel(global:gpt-5.4)=%v want empty", got)
	}
	// 裸名 → cn：该账号被当作 cn 可见（现状等价，纯 CN 锁定）。
	if got := fn("glm-5.2"); !reflect.DeepEqual(got, []string{"g1"}) {
		t.Errorf("global disabled: AvailableForModel(glm-5.2)=%v want [g1]", got)
	}
}

// TestRealmAwareAvailableForModelDefaultOnCNZeroRegression 开关缺省开启（生产语义）时
// 纯 CN 部署零回归：cn 账号（cn domain / 空 domain）对裸名与 cn: 前缀都可见，
// global: 前缀对纯 CN 池不可见（池里无 global 账号）。
func TestRealmAwareAvailableForModelDefaultOnCNZeroRegression(t *testing.T) {
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	p := pool.New("")
	p.Add(&auth.Auth{UID: "cn1", Domain: "www.codebuddy.cn"})
	p.Add(&auth.Auth{UID: "cn2", Domain: ""}) // 空 domain → cn（老 CN 凭证）
	fn := realmAwareAvailableForModel(p, nil)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1", "cn2"}},    // 裸名 → cn 集合（现状零回归）
		{"cn:glm-5.2", []string{"cn1", "cn2"}}, // cn 前缀 → cn 集合
		{"global:gpt-5.4", nil},                // global 前缀 → 纯 CN 池无可用（不对 spread）
	}
	for _, c := range cases {
		want := c.want
		if want == nil {
			want = []string{}
		}
		if got := fn(c.model); !reflect.DeepEqual(got, want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, want)
		}
	}
}

// TestModelJSONPath model.json 路径推导（context_length 四级查找链第 3 级接线）：
// 与 state.json 同目录同名换缀（Docker ./data volume 持久化）；空 state 路径 →
// 空串（禁用落盘，内存 + 种子仍可用）。
func TestModelJSONPath(t *testing.T) {
	cases := []struct {
		state, want string
	}{
		{"./data/state.json", "data/model.json"},
		{"/app/data/state.json", "/app/data/model.json"},
		{"state.json", "model.json"},
		{"", ""},
	}
	for _, c := range cases {
		// 归一化 got 侧：modelJSONPath 走 filepath.Join，Windows 产出反斜杠；
		// want 本就是正斜杠字面量（跨平台规范形式），对 want 做 ToSlash 是无操作，
		// 反斜杠会原样留在 got 里导致断言在 Windows 必然失败。
		if got := filepath.ToSlash(modelJSONPath(c.state)); got != c.want {
			t.Errorf("modelJSONPath(%q)=%q want %q", c.state, got, c.want)
		}
	}
}

// TestRealmAwareAvailableForModelWithOutputPrefix 输出侧模型前缀（output.model_prefix）
// 不得破坏 realm 路由：客户端拿着 "cn:wb-glm-5.2" 回来时，粘性路由仍须按 cn 域选号。
// 前缀只出现在对外模型名里，入站解析必须先剥前缀再判 realm（与 handler 侧同一函数）。
func TestRealmAwareAvailableForModelWithOutputPrefix(t *testing.T) {
	p := realmPool(t)
	out := server.NewOutputStore("")
	if _, err := out.Set(server.OutputConfig{
		Format: server.FormatOpenAI, ModelPrefix: "wb-", RateHint: server.RateCredit,
	}); err != nil {
		t.Fatalf("set output: %v", err)
	}
	fn := realmAwareAvailableForModel(p, out)

	cases := []struct {
		model string
		want  []string
	}{
		{"glm-5.2", []string{"cn1"}},          // 客户端没带前缀 → 裸名 → cn
		{"cn:wb-glm-5.2", []string{"cn1"}},    // 带前缀的 cn → cn（前缀被剥掉）
		{"global:wb-gpt-5.4", []string{"g1"}}, // 带前缀的 global → global
	}
	for _, c := range cases {
		if got := fn(c.model); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AvailableForModel(%q)=%v want %v", c.model, got, c.want)
		}
	}
}

// TestOutputJSONPath output.json 路径推导：与 state.json 同目录同名换缀；
// 空 state 路径 → 空串（输出配置不落盘，改了只在本次运行内有效）。
func TestOutputJSONPath(t *testing.T) {
	cases := []struct {
		state, want string
	}{
		{"./data/state.json", "data/output.json"},
		{"/app/data/state.json", "/app/data/output.json"},
		{"state.json", "output.json"},
		{"", ""},
	}
	for _, c := range cases {
		if got := filepath.ToSlash(outputJSONPath(c.state)); got != c.want {
			t.Errorf("outputJSONPath(%q)=%q want %q", c.state, got, c.want)
		}
	}
}

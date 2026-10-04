package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 一份形状与 ZCode 实盘完全一致的最小配置（缩进 2 空格、无 BOM、LF）。
const zcodeFixture = `{
  "schemaVersion": 1,
  "config": {
    "providerOrder": [
      "wbhub-agg",
      "ocz"
    ],
    "providerConfigRules": {
      "providerRules": [
        {
          "providerId": "wbhub-agg",
          "providerName": "wbhub 聚合",
          "config": {
            "group": "standard-personal",
            "access": {
              "type": "api-key",
              "apiKey": "123"
            },
            "api": {
              "type": "openai-chat-completions",
              "baseUrl": "http://127.0.0.1:7864/v1"
            },
            "personalModelIds": [
              "cn:space-bunny-x0.03",
              "cn:deepseek-v4.1-flash-x0.11"
            ],
            "modelOrder": [
              "cn:space-bunny-x0.03",
              "cn:deepseek-v4.1-flash-x0.11"
            ]
          }
        },
        {
          "providerId": "ocz",
          "providerName": "ocz",
          "config": {
            "group": "standard-personal",
            "access": {
              "type": "api-key",
              "apiKey": "sk-real"
            },
            "api": {
              "type": "openai-responses",
              "baseUrl": "https://opencode.ai/zen/v1"
            },
            "personalModelIds": [
              "muse-spark-1.3"
            ],
            "modelOrder": [
              "muse-spark-1.3"
            ]
          }
        }
      ]
    },
    "modelConfigRules": {
      "providerModelRules": [
        {
          "modelId": "cn:space-bunny-x0.03",
          "config": {
            "enabled": true
          },
          "providerId": "wbhub-agg"
        },
        {
          "modelId": "cn:deepseek-v4.1-flash-x0.11",
          "config": {
            "enabled": true,
            "properties": {
              "supportsJsonSchemaOutput": true,
              "supportsNativeWebSearch": true,
              "supportsMidConversationSystem": true
            }
          },
          "providerId": "wbhub-agg"
        },
        {
          "modelId": "muse-spark-1.3",
          "config": {
            "enabled": true,
            "properties": {
              "contextWindow": 200000
            }
          },
          "providerId": "ocz"
        }
      ],
      "manualProviderModelRules": []
    }
  }
}
`

func writeZCodeFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "provider_config.json")
	if err := os.WriteFile(p, []byte(zcodeFixture), 0o600); err != nil {
		t.Fatalf("写 fixture 失败：%v", err)
	}
	return p
}

func zcodeTestMeta() map[string]ZCodeModelMeta {
	return map[string]ZCodeModelMeta{
		"cn:space-bunny-x0.03":         {ContextWindow: 1000000, MaxOutputTokens: 128000},
		"cn:deepseek-v4.1-flash-x0.11": {ContextWindow: 1000000, MaxOutputTokens: 128000},
		"opencode:space-bunny-free":    {ContextWindow: 1048576, MaxOutputTokens: 524288},
		"zcode:glm-5.3-flash":          {ContextWindow: 200000},
	}
}

func decodeZCode(t *testing.T, s string) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(s), &root); err != nil {
		t.Fatalf("结果不是合法 JSON：%v", err)
	}
	return root
}

// zcodeRule 按 (providerId, modelId) 取出一条规则。
func zcodeRule(t *testing.T, root map[string]any, pid, mid string) map[string]any {
	t.Helper()
	cfg := root["config"].(map[string]any)
	rules := cfg["modelConfigRules"].(map[string]any)["providerModelRules"].([]any)
	for _, it := range rules {
		r := it.(map[string]any)
		if r["providerId"] == pid && r["modelId"] == mid {
			return r
		}
	}
	return nil
}

// TestZCodePlanWritesContextWindow 核心回归：指向本网关的 provider 缺 contextWindow
// 时要补上，且不碰直连官方上游的 provider。
func TestZCodePlanWritesContextWindow(t *testing.T) {
	p := writeZCodeFixture(t)
	plan, err := PlanZCode(p, "http://127.0.0.1:7864/v1", zcodeTestMeta())
	if err != nil {
		t.Fatalf("PlanZCode 报错：%v", err)
	}
	if !plan.Changed {
		t.Fatal("预期有改动，实际 Changed=false")
	}
	if plan.MatchedProviders != 1 {
		t.Fatalf("MatchedProviders=%d，期望 1（只有 wbhub-agg 指向本网关）", plan.MatchedProviders)
	}
	// context_window 两条（两个模型）+ max_output_tokens 两条 = 4 处改动
	if len(plan.Changes) != 4 {
		t.Fatalf("改动条数=%d，期望 4；实际 %+v", len(plan.Changes), plan.Changes)
	}
	root := decodeZCode(t, plan.After)

	// 缺 contextWindow 的那条要补上，且原有 properties 一个都不能丢
	r := zcodeRule(t, root, "wbhub-agg", "cn:space-bunny-x0.03")
	props := r["config"].(map[string]any)["properties"].(map[string]any)
	if got := numOf(props["contextWindow"]); got != 1000000 {
		t.Fatalf("contextWindow=%d，期望 1000000", got)
	}
	// 已有的其它 properties 必须原样保留（这是泛型 map 而不是强类型 struct 的原因）
	r2 := zcodeRule(t, root, "wbhub-agg", "cn:deepseek-v4.1-flash-x0.11")
	p2 := r2["config"].(map[string]any)["properties"].(map[string]any)
	for _, k := range []string{"supportsJsonSchemaOutput", "supportsNativeWebSearch", "supportsMidConversationSystem"} {
		if _, ok := p2[k]; !ok {
			t.Fatalf("properties.%s 被抹掉了", k)
		}
	}
	// maxOutputTokens 必须落在 optionSpecs.maxOutputTokens.max，**不是** properties
	specs := r2["config"].(map[string]any)["optionSpecs"].(map[string]any)
	if got := numOf(specs["maxOutputTokens"].(map[string]any)["max"]); got != 128000 {
		t.Fatalf("optionSpecs.maxOutputTokens.max=%d，期望 128000", got)
	}
	if _, wrong := p2["maxOutputTokens"]; wrong {
		t.Fatal("maxOutputTokens 被错误写进了 properties —— ZCode 的 pick 白名单不含该键，会被丢掉")
	}
	// 直连官方的 ocz 规则一个字都不能动
	oc := zcodeRule(t, root, "ocz", "muse-spark-1.3")
	ocp := oc["config"].(map[string]any)["properties"].(map[string]any)
	if got := numOf(ocp["contextWindow"]); got != 200000 {
		t.Fatalf("ocz 的 contextWindow 被改了：%d", got)
	}
}

// TestZCodePlanIdempotent 重复点「同步」不应产生改动（前端会据此显示「已是最新」）。
func TestZCodePlanIdempotent(t *testing.T) {
	p := writeZCodeFixture(t)
	meta := zcodeTestMeta()
	first, err := PlanZCode(p, "http://127.0.0.1:7864/v1", meta)
	if err != nil || !first.Changed {
		t.Fatalf("第一次应有改动：err=%v changed=%v", err, first.Changed)
	}
	if err := ApplyZCode(first); err != nil {
		t.Fatalf("ApplyZCode 失败：%v", err)
	}
	second, err := PlanZCode(p, "http://127.0.0.1:7864/v1", meta)
	if err != nil {
		t.Fatalf("第二次 PlanZCode 报错：%v", err)
	}
	if second.Changed || len(second.Changes) != 0 {
		t.Fatalf("第二次仍有改动（不幂等）：changed=%v changes=%d", second.Changed, len(second.Changes))
	}
}

// TestZCodeApplyBackup 落盘必须先备份，且备份内容等于原文件。
func TestZCodeApplyBackup(t *testing.T) {
	p := writeZCodeFixture(t)
	orig, _ := os.ReadFile(p)
	plan, err := PlanZCode(p, "http://127.0.0.1:7864/v1", zcodeTestMeta())
	if err != nil || !plan.Changed {
		t.Fatalf("应有改动：err=%v", err)
	}
	if plan.Backup == "" {
		t.Fatal("BackupPath 为空")
	}
	if err := ApplyZCode(plan); err != nil {
		t.Fatalf("ApplyZCode 失败：%v", err)
	}
	bak, err := os.ReadFile(plan.Backup)
	if err != nil {
		t.Fatalf("读备份失败：%v", err)
	}
	if string(bak) != string(orig) {
		t.Fatal("备份内容与原文件不一致")
	}
	// 落盘后必须是合法 JSON，且临时文件不能留下
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("残留 .tmp 文件")
	}
	decodeZCode(t, string(mustRead(t, p)))
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读 %s 失败：%v", p, err)
	}
	return b
}

// TestSameGateway baseUrl 归一化：/v1 后缀、大小写、0.0.0.0 vs 127.0.0.1 都算同一个网关。
func TestSameGateway(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"http://127.0.0.1:7864/v1", "http://127.0.0.1:7864/v1", true},
		{"http://127.0.0.1:7864/v1", "http://127.0.0.1:7864", true},
		{"http://127.0.0.1:7864/v1/", "127.0.0.1:7864", true},
		{"HTTP://127.0.0.1:7864/V1", "http://127.0.0.1:7864/v1", true},
		{"http://0.0.0.0:7864/v1", "http://127.0.0.1:7864/v1", true},
		{"http://localhost:7864/v1", "http://127.0.0.1:7864/v1", true},
		{"http://127.0.0.1:7863/v1", "http://127.0.0.1:7864/v1", false},
		{"https://opencode.ai/zen/v1", "http://127.0.0.1:7864/v1", false},
		{"http://192.168.1.5:7864/v1", "http://127.0.0.1:7864/v1", false},
		{"", "http://127.0.0.1:7864/v1", false},
	}
	for _, c := range cases {
		if got := sameGateway(c.a, c.b); got != c.want {
			t.Errorf("sameGateway(%q, %q)=%v，期望 %v", c.a, c.b, got, c.want)
		}
	}
}

// TestZCodePlanCreatesMissingRule provider 选了模型但没有对应规则时，要补一条规则。
func TestZCodePlanCreatesMissingRule(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(zcodeFixture), &root); err != nil {
		t.Fatal(err)
	}
	// 把 personalModelIds 加上 opencode:space-bunny-free，但**不加规则**
	cfg := root["config"].(map[string]any)
	prov := cfg["providerConfigRules"].(map[string]any)["providerRules"].([]any)[0].(map[string]any)
	prov["config"].(map[string]any)["personalModelIds"] = []any{
		"cn:space-bunny-x0.03", "opencode:space-bunny-free",
	}
	p := filepath.Join(t.TempDir(), "provider_config.json")
	body, _ := json.MarshalIndent(root, "", "  ")
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanZCode(p, "http://127.0.0.1:7864/v1", zcodeTestMeta())
	if err != nil {
		t.Fatalf("PlanZCode 报错：%v", err)
	}
	r := zcodeRule(t, decodeZCode(t, plan.After), "wbhub-agg", "opencode:space-bunny-free")
	if r == nil {
		t.Fatal("缺规则时没有补出 opencode:space-bunny-free")
	}
	rc := r["config"].(map[string]any)
	if rc["enabled"] != true {
		t.Fatal("补出的规则 enabled 应为 true")
	}
	if got := numOf(rc["properties"].(map[string]any)["contextWindow"]); got != 1048576 {
		t.Fatalf("contextWindow=%d，期望 1048576", got)
	}
	var creates int
	for _, c := range plan.Changes {
		if c.Action == "create" && c.ModelID == "opencode:space-bunny-free" {
			creates++
		}
	}
	if creates == 0 {
		t.Fatal("Changes 里没有标出 create 动作")
	}
}

// TestZCodePlanPreservesUnknownTopLevel 未知的顶层键必须留着（ZCode 每次启动会重构这张表，
// 我们不能顺手抹掉它新加的东西）。
func TestZCodePlanPreservesUnknownTopLevel(t *testing.T) {
	src := zcodeFixture[:len(zcodeFixture)-2] + `,
  "futureField": {
    "keepMe": true
  }
}`
	p := filepath.Join(t.TempDir(), "provider_config.json")
	if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := PlanZCode(p, "http://127.0.0.1:7864/v1", zcodeTestMeta())
	if err != nil || !plan.Changed {
		t.Fatalf("应有改动：err=%v changed=%v", err, plan.Changed)
	}
	root := decodeZCode(t, plan.After)
	ff, ok := root["futureField"].(map[string]any)
	if !ok || ff["keepMe"] != true {
		t.Fatal("未知的顶层字段被抹掉了")
	}
}

// TestZCodeConfigPath 覆盖优先级。
func TestZCodeConfigPath(t *testing.T) {
	if got := ZCodeConfigPath("D:/custom/provider_config.json"); got != "D:/custom/provider_config.json" {
		t.Fatalf("显式覆盖失效：%s", got)
	}
	t.Setenv("ZCODE_HOME", "D:/zchome")
	got := ZCodeConfigPath("")
	if got != filepath.Join("D:/zchome", "v2", "provider_config.json") {
		t.Fatalf("ZCODE_HOME 未生效：%s", got)
	}
}

// TestZCodePlanMissingFile 文件不存在时给出可读原因，而不是 panic。
func TestZCodePlanMissingFile(t *testing.T) {
	plan, err := PlanZCode(filepath.Join(t.TempDir(), "nope.json"), "http://127.0.0.1:7864/v1", zcodeTestMeta())
	if err != nil {
		t.Fatalf("不应报错，应返回带 ReadErr 的计划：%v", err)
	}
	if plan.ReadErr == "" {
		t.Fatal("ReadErr 为空，前端会显示成「一切正常」")
	}
}

// —— 代建供应商 ——

// TestUpsertZCodeProviderCreates 全新 providerId：追加一条，字段与 UI 手建的一致，
// personalModelIds 与 modelOrder 同内容（实盘 ZCode 就是这样）。
func TestUpsertZCodeProviderCreates(t *testing.T) {
	p := writeZCodeFixture(t)
	spec := ZCodeProviderSpec{
		ProviderID: "f2a",
		Name:       "free2api",
		BaseURL:    "http://127.0.0.1:7864/v1",
		APIType:    "openai",
		Models:     []string{"zcode:glm-5.3-flash", "cn:hy3-free"},
	}
	changed, pid, _, err := UpsertZCodeProvider(p, spec)
	if err != nil || !changed || pid != "f2a" {
		t.Fatalf("建供应商失败 changed=%v pid=%s err=%v", changed, pid, err)
	}
	root := decodeZCode(t, string(mustRead(t, p)))
	rules := providerRulesOf(t, root)
	pr := findProviderRule(rules, "f2a")
	if pr == nil {
		t.Fatal("没写进 providerRules")
	}
	if got := strOf(pr["providerName"]); got != "free2api" {
		t.Errorf("providerName = %q", got)
	}
	cfg := mapOf(pr["config"])
	api := mapOf(cfg["api"])
	if got := strOf(api["type"]); got != "openai-chat-completions" {
		t.Errorf("api.type = %q，OpenAI 兼容应映射成 openai-chat-completions", got)
	}
	if got := strOf(api["baseUrl"]); got != "http://127.0.0.1:7864/v1" {
		t.Errorf("baseUrl = %q", got)
	}
	// access.apiKey 必填非空：网关不鉴权也要有占位串
	acc := mapOf(cfg["access"])
	if strOf(acc["apiKey"]) == "" {
		t.Error("apiKey 为空 —— ZCode 侧会校验，必须给占位串")
	}
	ids := strList(cfg["personalModelIds"])
	order := strList(cfg["modelOrder"])
	if len(ids) != 2 || ids[0] != "cn:hy3-free" || ids[1] != "zcode:glm-5.3-flash" {
		t.Errorf("personalModelIds = %v，应排序去重", ids)
	}
	if len(order) != len(ids) {
		t.Errorf("modelOrder 与 personalModelIds 应等长：%d vs %d", len(order), len(ids))
	}
}

// TestUpsertZCodeProviderIdempotent 同参数再跑一次不追加、不改内容。
func TestUpsertZCodeProviderIdempotent(t *testing.T) {
	p := writeZCodeFixture(t)
	spec := ZCodeProviderSpec{ProviderID: "f2a", Name: "free2api",
		BaseURL: "http://127.0.0.1:7864/v1", APIType: "openai",
		Models: []string{"zcode:glm-5.3-flash"}}
	if _, _, _, err := UpsertZCodeProvider(p, spec); err != nil {
		t.Fatal(err)
	}
	after1 := string(mustRead(t, p))
	changed, _, _, err := UpsertZCodeProvider(p, spec)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("第二次应报未改动")
	}
	if after2 := string(mustRead(t, p)); after2 != after1 {
		t.Error("第二次不该改文件")
	}
	// providerId 留空 → 自动生成一个，但不能每次都生成新的（那就变成每次多点一次多一条）
	_, autoPID, _, aerr := UpsertZCodeProvider(p, ZCodeProviderSpec{Name: "x",
		BaseURL: "http://127.0.0.1:7864/v1", APIType: "openai",
		Models: []string{"zcode:glm-5.3-flash"}})
	if aerr != nil {
		t.Fatal(aerr)
	}
	if autoPID == "" || len(autoPID) <= len("free2api-") {
		t.Fatalf("自动生成的 providerId 不合法：%q", autoPID)
	}
}

// TestUpsertZCodeProviderRefusesHijack 同一个 providerId 已存在但指向别的地址时必须拒绝。
// 这是防手滑的关键闸门：悄悄改掉用户正在用的供应商 baseUrl 会把它拉下线。
func TestUpsertZCodeProviderRefusesHijack(t *testing.T) {
	p := writeZCodeFixture(t) // 里面有 providerId=ocz，指向 opencode.ai
	_, _, _, err := UpsertZCodeProvider(p, ZCodeProviderSpec{
		ProviderID: "ocz", Name: "ocz 改名了",
		BaseURL: "http://127.0.0.1:7864/v1", APIType: "openai",
		Models: []string{"zcode:glm-5.3-flash"}})
	if err == nil {
		t.Fatal("指向别处的同名 provider 必须拒绝改写")
	}
	root := decodeZCode(t, string(mustRead(t, p)))
	pr := findProviderRule(providerRulesOf(t, root), "ocz")
	api := mapOf(mapOf(pr["config"])["api"])
	if got := strOf(api["baseUrl"]); got != "https://opencode.ai/zen/v1" {
		t.Errorf("原供应商的 baseUrl 被改了：%q", got)
	}
}

// TestUpsertZCodeProviderUpdatesSameGateway 同 id 但本来指向本网关 → 允许覆盖
// （改显示名 / 换 API 格式 / 换模型列表是我们自己的东西，放行）。
func TestUpsertZCodeProviderUpdatesSameGateway(t *testing.T) {
	p := writeZCodeFixture(t) // wbhub-agg 指向 7864
	changed, _, _, err := UpsertZCodeProvider(p, ZCodeProviderSpec{
		ProviderID: "wbhub-agg", Name: "free2api 聚合",
		BaseURL: "http://127.0.0.1:7864/v1", APIType: "responses",
		Models: []string{"zcode:glm-5.3-flash"}})
	if err != nil || !changed {
		t.Fatalf("同网关应允许覆盖 changed=%v err=%v", changed, err)
	}
	root := decodeZCode(t, string(mustRead(t, p)))
	rules := providerRulesOf(t, root)
	if n := countProvider(rules, "wbhub-agg"); n != 1 {
		t.Fatalf("应就地覆盖不该追加，实际出现 %d 条", n)
	}
	pr := findProviderRule(rules, "wbhub-agg")
	api := mapOf(mapOf(pr["config"])["api"])
	if got := strOf(api["type"]); got != "openai-responses" {
		t.Errorf("api.type = %q，responses 应映射成 openai-responses", got)
	}
	if got := strOf(pr["providerName"]); got != "free2api 聚合" {
		t.Errorf("providerName = %q", got)
	}
}

// TestUpsertZCodeProviderRejectsUnsupportedFormat 网关侧没接的格式要挡掉。
func TestUpsertZCodeProviderRejectsUnsupportedFormat(t *testing.T) {
	p := writeZCodeFixture(t)
	for _, f := range []string{"anthropic", "gemini", ""} {
		if _, _, _, err := UpsertZCodeProvider(p, ZCodeProviderSpec{
			ProviderID: "x", Name: "x", BaseURL: "http://127.0.0.1:7864/v1",
			APIType: f, Models: []string{"zcode:glm-5.3-flash"}}); err == nil {
			t.Errorf("格式 %q 未接入却放行了", f)
		}
	}
}

// TestZCodeFormatNoteOpenAIAndResponses 网关格式 → ZCode api.type 的映射口径。
func TestZCodeFormatNoteOpenAIAndResponses(t *testing.T) {
	if _, ok := zcodeFormatNote("openai"); !ok {
		t.Error("OpenAI 兼容应可用")
	}
	if _, ok := zcodeFormatNote("responses"); !ok {
		t.Error("Responses 应可用")
	}
	if got := zcodeAPITypeOf("responses"); got != "openai-responses" {
		t.Errorf("responses → %q", got)
	}
	if got := zcodeAPITypeOf("openai"); got != "openai-chat-completions" {
		t.Errorf("openai → %q", got)
	}
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}
	return m
}

func strOf(v any) string {
	s, _ := v.(string)
	return s
}

func providerRulesOf(t *testing.T, root map[string]any) []any {
	t.Helper()
	return mapOf(mapOf(root["config"])["providerConfigRules"])["providerRules"].([]any)
}

func findProviderRule(rules []any, pid string) map[string]any {
	for _, it := range rules {
		pr, ok := it.(map[string]any)
		if ok && strOf(pr["providerId"]) == pid {
			return pr
		}
	}
	return nil
}

func countProvider(rules []any, pid string) int {
	n := 0
	for _, it := range rules {
		if pr, ok := it.(map[string]any); ok && strOf(pr["providerId"]) == pid {
			n++
		}
	}
	return n
}

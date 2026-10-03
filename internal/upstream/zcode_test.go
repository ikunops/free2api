package upstream

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// TestZCodeForceStream 锁定 zcode 出站体的**唯一**改写：只动 stream 一个键。
// 网关对所有客户端只有一条上游通道——流式（非流式客户端由 handler 侧 Aggregate
// 聚合 SSE）；客户端写 stream:false 时智谱回非流式 JSON，Aggregate 找不到 data 帧
// 就 502 upstream_parse（本机实测）。其余字段必须逐字保留。
func TestZCodeForceStream(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    map[string]any // 期望的出站字段（子集断言）
		rawSame bool           // true = 必须字节级原样返回
	}{
		{
			name: "stream:false 改 true，其余字段保留",
			in:   `{"model":"glm-4.6","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`,
			want: map[string]any{"stream": true, "model": "glm-4.6", "temperature": 0.7},
		},
		{
			name: "无 stream 字段 → 注入 true",
			in:   `{"model":"glm-5.3","messages":[]}`,
			want: map[string]any{"stream": true, "model": "glm-5.3"},
		},
		{
			name:    "已是流式 → 字节级原样（不做无谓重排）",
			in:      `{"model":"glm-4.6","stream":true,"messages":[]}`,
			rawSame: true,
		},
		{
			name:    "坏 JSON → 字节级原样（不在网关侧二次错误化）",
			in:      `{"model":`,
			rawSame: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := zcodeForceStream([]byte(c.in))
			if c.rawSame {
				if string(out) != c.in {
					t.Fatalf("want 原样 %q, got %q", c.in, string(out))
				}
				return
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("出站体不是 JSON: %v (%s)", err, out)
			}
			for k, v := range c.want {
				if obj[k] != v {
					t.Errorf("%s = %#v, want %#v（出站体=%s）", k, obj[k], v, out)
				}
			}
			// 未被 touched 的字段（自定义扩展）也必须原样在。
			if c.name == "stream:false 改 true，其余字段保留" {
				if _, ok := obj["messages"]; !ok {
					t.Errorf("messages 丢了: %s", out)
				}
			}
		})
	}
}

// TestZCodeNormalizeReasoning 锁定「始终思考」模型的推理档位收敛（智谱 400 code=1210 修复）。
// glm-5.3-flash 只认 low/high/max 或缺省；none/off/minimal/medium 与 thinking.type=disabled
// 必须被收敛到合法档，否则池子换号重试永远是同一个失败（见 demo/data/desktop.log 死循环）。
// 未知模型（glm-4.6 实测宽容）一律字节级原样透传。
func TestZCodeNormalizeReasoning(t *testing.T) {
	c := &Client{}
	cases := []struct {
		name      string
		in        string
		wantField map[string]any // 期望字段（子集）；nil 表示需断言 rawSame
		rawSame   bool
	}{
		{
			name:      "flash + effort=none → 收敛到 low",
			in:        `{"model":"glm-5.3-flash","reasoning_effort":"none","messages":[]}`,
			wantField: map[string]any{"reasoning_effort": "low"},
		},
		{
			name:      "flash + effort=off → low",
			in:        `{"model":"glm-5.3-flash","reasoning_effort":"off"}`,
			wantField: map[string]any{"reasoning_effort": "low"},
		},
		{
			name:      "flash + effort=medium → low（≤请求档的最高支持档）",
			in:        `{"model":"glm-5.3-flash","reasoning_effort":"medium"}`,
			wantField: map[string]any{"reasoning_effort": "low"},
		},
		{
			name:      "flash + effort=minimal → low",
			in:        `{"model":"glm-5.3-flash","reasoning_effort":"minimal"}`,
			wantField: map[string]any{"reasoning_effort": "low"},
		},
		{
			name:      "flash + camel reasoningEffort=none → low",
			in:        `{"model":"glm-5.3-flash","reasoningEffort":"none"}`,
			wantField: map[string]any{"reasoningEffort": "low"},
		},
		{
			name:    "flash + effort=high（已合法）→ 字节级原样",
			in:      `{"model":"glm-5.3-flash","reasoning_effort":"high"}`,
			rawSame: true,
		},
		{
			name:    "flash 无档位字段 → 字节级原样",
			in:      `{"model":"glm-5.3-flash","messages":[]}`,
			rawSame: true,
		},
		{
			name:      "flash + thinking.disabled → 删 thinking 并补最低档",
			in:        `{"model":"glm-5.3-flash","thinking":{"type":"disabled"}}`,
			wantField: map[string]any{"reasoning_effort": "low"},
		},
		{
			name:      "flash + thinking.disabled + effort=high → 删 thinking，档位保持 high",
			in:        `{"model":"glm-5.3-flash","thinking":{"type":"disabled"},"reasoning_effort":"high"}`,
			wantField: map[string]any{"reasoning_effort": "high"},
		},
		{
			name:    "未知模型 glm-4.6 + effort=none → 原样（不擅自改写）",
			in:      `{"model":"glm-4.6","reasoning_effort":"none"}`,
			rawSame: true,
		},
		{
			name:    "坏 JSON → 原样",
			in:      `{"model":`,
			rawSame: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := c.zcodeNormalizeReasoning([]byte(tc.in))
			if tc.rawSame {
				if string(out) != tc.in {
					t.Fatalf("want 原样 %q, got %q", tc.in, string(out))
				}
				return
			}
			var obj map[string]any
			if err := json.Unmarshal(out, &obj); err != nil {
				t.Fatalf("出站体不是 JSON: %v (%s)", err, out)
			}
			for k, v := range tc.wantField {
				if obj[k] != v {
					t.Errorf("%s = %#v, want %#v（出站体=%s）", k, obj[k], v, out)
				}
			}
			// thinking.type=disabled 必须被摘掉，否则仍会被上游 1210 拒。
			if strings.Contains(tc.in, `"type":"disabled"`) {
				if th, ok := obj["thinking"].(map[string]any); ok {
					if typ, _ := th["type"].(string); typ == "disabled" {
						t.Errorf("thinking.type=disabled 未被摘除: %s", out)
					}
				}
			}
		})
	}
}

// TestZCodeChatForcesStreamBearerOnly zcode 出站端到端（httptest）：路径 = base +
// /chat/completions、鉴权只有 Bearer <apiKey>、body 的 stream 被强制 true。
// 同时锚定「不掺 workbuddy 头族」——那套头对智谱是垃圾，注进去反而可能被风控。
func TestZCodeChatForcesStreamBearerOnly(t *testing.T) {
	var gotPath, gotAuth, gotBody string
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotAuth, gotBody = r.URL.Path, r.Header.Get("Authorization"), string(b)
		gotHdr = r.Header.Clone()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{}, ChatBaseZCode: srv.URL + "/api/paas/v4"}
	a := &auth.Auth{UID: "zc1", AccessToken: "key.secret"}
	a.SetProducer(ProducerZCode)

	body := []byte(`{"model":"glm-4.6","stream":false,"temperature":0.7,"messages":[{"role":"user","content":"hi"}]}`)
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	if status != 200 {
		t.Fatalf("status = %d, want 200", status)
	}
	rc.Close()

	if gotPath != "/api/paas/v4/chat/completions" {
		t.Errorf("路径 = %q, want /api/paas/v4/chat/completions", gotPath)
	}
	if gotAuth != "Bearer key.secret" {
		t.Errorf("Authorization = %q, want Bearer key.secret", gotAuth)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(gotBody), &sent); err != nil {
		t.Fatalf("出站 body 不是 JSON: %v", err)
	}
	if sent["stream"] != true {
		t.Errorf("出站 stream = %#v, want true（非流式请求也必须走流式通道）", sent["stream"])
	}
	if sent["model"] != "glm-4.6" || sent["temperature"] != 0.7 {
		t.Errorf("其余字段被改动: %s", gotBody)
	}
	for _, h := range []string{"X-Enterprise-Id", "X-Domain", "X-Device-Token", "X-Conversation-Request-Id"} {
		if v := gotHdr.Get(h); v != "" {
			t.Errorf("workbuddy 专有头 %s = %q 不该出现在智谱请求上", h, v)
		}
	}
}

// TestZCodeClassify 智谱特有的错误分类口径：401/1001/令牌已过期 → 会话失效（连续
// N 次才禁用），余额不足类 → 硬冷却（不会自己恢复）。其余回落通用 Classify。
func TestZCodeClassify(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{401, `{"error":{"message":"令牌已过期"}}`, ErrSessionDead},
		{400, `{"code":1001,"msg":"未收到 Authorization"}}`, ErrSessionDead},
		{429, `{"code":1113,"msg":"余额不足"}`, ErrHardCredit},
		{200, `{}`, ErrNone},
	}
	for _, c := range cases {
		if got := classifyZCode(c.status, c.body); got != c.want {
			t.Errorf("classifyZCode(%d, %s) = %v, want %v", c.status, c.body, got, c.want)
		}
	}
}

// TestZCodeFetchModelsParsesOpenAIShape FetchZCodeModels 解析智谱 /models 的
// {"data":[...]} 形态，context/max_tokens 字段按智谱扩展名取。
func TestZCodeFetchModelsParsesOpenAIShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/paas/v4/models" {
			t.Errorf("路径 = %q, want /api/paas/v4/models", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer key.secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[`+
			`{"id":"glm-4.6","context_window":200000,"max_output_tokens":96000},`+
			`{"id":"glm-5.3"}]}`)
	}))
	defer srv.Close()

	c := &Client{HTTP: &http.Client{}, ChatBaseZCode: srv.URL + "/api/paas/v4"}
	a := &auth.Auth{UID: "zc1", AccessToken: "key.secret"}
	a.SetProducer(ProducerZCode)

	infos, err := c.FetchZCodeModels(context.Background(), a)
	if err != nil {
		t.Fatalf("FetchZCodeModels: %v", err)
	}
	if len(infos) != 2 || infos[0].ID != "glm-4.6" || infos[1].ID != "glm-5.3" {
		t.Fatalf("infos = %+v", infos)
	}
	if infos[0].ContextWindow != 200000 || infos[0].MaxTokens != 96000 {
		t.Errorf("智谱扩展字段未取到: %+v", infos[0])
	}
	if infos[0].Name != "glm-4.6" {
		t.Errorf("name 缺省应回落 id: %q", infos[0].Name)
	}
}

// TestZCodeUpstreamBasePriority 凭据自带 upstream_base 优先于 Client 覆盖（「用户
// 自己选 url」的落点）：单文件迁移时改一个键就能换上游。
func TestZCodeUpstreamBasePriority(t *testing.T) {
	c := &Client{ChatBaseZCode: "https://client-override.example/api/paas/v4"}
	a := &auth.Auth{UID: "zc1", AccessToken: "k"}
	a.SetProducer(ProducerZCode)
	a.SetUpstreamBase("https://from-credential.example/api/paas/v4/")
	if got := c.zcodeBase(a); got != "https://from-credential.example/api/paas/v4" {
		t.Errorf("zcodeBase = %q, want 凭据自带（且剥掉尾斜杠）", got)
	}

	b := &auth.Auth{UID: "zc2", AccessToken: "k"}
	b.SetProducer(ProducerZCode)
	if got := c.zcodeBase(b); got != "https://client-override.example/api/paas/v4" {
		t.Errorf("zcodeBase = %q, want Client 覆盖", got)
	}

	empty := &Client{}
	d := &auth.Auth{UID: "zc3", AccessToken: "k"}
	d.SetProducer(ProducerZCode)
	if got := empty.zcodeBase(d); !strings.Contains(got, "open.bigmodel.cn") {
		t.Errorf("zcodeBase = %q, want 内置缺省智谱 base", got)
	}
}

// TestZCodeBalanceTolerantFields 锁定额度响应的宽容解析。
// 实测：真实号打 billing/balance 时 plans.starts_at 是**数字**（Unix 时间戳），
// 早期按 string 解 → json: cannot unmarshal number ... 整条失败，页面「读失败」。
// grant / expires_at 也出现过字符串形态。这里两种形态都要吃下。
func TestZCodeBalanceTolerantFields(t *testing.T) {
	const raw = `{"code":0,"data":{` +
		`"plans":[{"name":"ZCode Weekend Build","description":"ZCode 周末活动",` +
		`"plan_id":"zcode-v3-start-plan-0924-wk-2","status":"active",` +
		`"starts_at":1750000000,"ends_at":1780000000,` +
		`"entitlements":[{"show_name":"GLM-5.3-Flash","meter":"model_usage",` +
		`"unit_type":"token","capabilities":["model:glm-5.3-flash"],` +
		`"grant_units":"300000000","period":"one_time","priority":110}]}],` +
		`"balances":[{"show_name":"GLM-5.3-Flash","total_units":"500","used_units":12,` +
		`"remaining_units":488,"expires_at":"1780000000",` +
		`"capabilities":["model:glm-5.3-flash"]}]}}`
	bal, err := parseZCodeBalance([]byte(raw))
	if err != nil {
		t.Fatalf("宽容解析失败（数字 starts_at / 字符串 grant_units 都要能吃）: %v", err)
	}
	if len(bal.Plans) != 1 || string(bal.Plans[0].StartsAt) != "1750000000" {
		t.Fatalf("plans.starts_at = %+v, want 数字原样收下", bal.Plans)
	}
	// 真实上游是 entitlements（复数）+ grant_units；写错成 entitlement/grant 时
	// 权益会整段为空 → 页面上「可用模型」就退化成 /models 全量，必须锁死。
	if len(bal.Plans[0].Entitlements) != 1 {
		t.Fatalf("plans.entitlements 长度 = %d, want 1（字段名是复数 entitlements）",
			len(bal.Plans[0].Entitlements))
	}
	if got := bal.Plans[0].Entitlements[0].GrantUnits; int64(got) != 300000000 {
		t.Fatalf("entitlements.grant_units = %v, want 300000000（字符串形态也要解）", got)
	}
	if len(bal.Balances) != 1 {
		t.Fatalf("balances 长度 = %d, want 1", len(bal.Balances))
	}
	b := bal.Balances[0]
	if int64(b.TotalUnits) != 500 || int64(b.UsedUnits) != 12 || int64(b.RemainingUnits) != 488 {
		t.Fatalf("额度明细 = %+v, want 500/12/488", b)
	}
	if int64(b.ExpiresAt) != 1780000000 {
		t.Fatalf("expires_at = %v, want 1780000000", b.ExpiresAt)
	}
}

// TestZCodeBalanceCodeNonZero: 上游用 code!=0 报错时给可读信息，不吞掉。
func TestZCodeBalanceCodeNonZero(t *testing.T) {
	_, err := parseZCodeBalance([]byte(`{"code":41001,"msg":"token expired"}`))
	if err == nil || !strings.Contains(err.Error(), "41001") {
		t.Fatalf("err = %v, want 带 code=41001", err)
	}
}

// TestZCodeEntitledModelsIsTheTruth 锁定「真正可用模型」的判据：
// capabilities 里的 model:<id>，而不是 /models 能列出多少。
// 实测周末活动的号 /models 列 11 个，但 capabilities 只有 glm-5.3-flash。
func TestZCodeEntitledModelsIsTheTruth(t *testing.T) {
	const raw = `{"code":0,"data":{` +
		`"plans":[{"name":"ZCode Weekend Build","description":"ZCode 周末活动",` +
		`"entitlements":[{"show_name":"GLM-5.3-Flash",` +
		`"capabilities":["model:glm-5.3-flash"]}]}],` +
		`"balances":[` +
		`{"show_name":"GLM-5.3-Flash","capabilities":["model:glm-5.3-flash"]},` +
		`{"show_name":"GLM-4.6 赠送","capabilities":["model:glm-4.6","chat"]},` +
		`{"show_name":"重复项","capabilities":["model:glm-4.6"]}]}}`
	bal, err := parseZCodeBalance([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got := bal.EntitledModels()
	want := []string{"glm-4.6", "glm-5.3-flash"}
	if len(got) != len(want) {
		t.Fatalf("EntitledModels = %v, want %v（去重、排序、只收 model: 前缀）", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EntitledModels = %v, want %v", got, want)
		}
	}
	// PlanSummary 优先给人话（description），没有才退回计划名。
	if s := bal.PlanSummary(); s != "ZCode 周末活动" {
		t.Fatalf("PlanSummary = %q, want 优先用 description", s)
	}
	noDesc, err := parseZCodeBalance([]byte(`{"code":0,"data":{"plans":[{"name":"Coding Plan"}]}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if s := noDesc.PlanSummary(); s != "Coding Plan" {
		t.Fatalf("PlanSummary = %q, want 回退到计划名", s)
	}
}

// TestZCodeEntitledModelsEmpty: 没有 capabilities 时返回空（调用方回退到 /models），
// 而不是 panic 或返回空字符串元素。
func TestZCodeEntitledModelsEmpty(t *testing.T) {
	bal, err := parseZCodeBalance([]byte(`{"code":0,"data":{"balances":[{"show_name":"x"}]}}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := bal.EntitledModels(); len(got) != 0 {
		t.Fatalf("EntitledModels = %v, want 空", got)
	}
	var nilBal *ZCodeBalance
	if got := nilBal.EntitledModels(); got != nil {
		t.Fatalf("nil 接收者 EntitledModels = %v, want nil", got)
	}
}

package upstream

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"free2api/internal/auth"
)

// TestQoderBodyCodecRoundTrip 锁定 Qoder 自定义 body 编码：字母表 64 字符、
// `$` 补齐、前后 1/3 交换。任何一处改动都会让上游按「请求体损坏」回 400，
// 且现象只在真实调用时出现，所以必须有往返测试兜底。
func TestQoderBodyCodecRoundTrip(t *testing.T) {
	cases := []string{
		"",
		"a",
		"ab",
		"abc",
		"abcd",
		"{\"model\":\"gfmodel\"}",
		"{\"messages\":[{\"role\":\"user\",\"content\":\"你好，世界\"}]}",
		strings.Repeat("x", 1000),
	}
	for _, in := range cases {
		wire := qoderEncodeBody(in)
		got, err := qoderDecodeBody(wire)
		if err != nil {
			t.Fatalf("decode(%q): %v", in, err)
		}
		if got != in {
			t.Fatalf("往返不一致：in=%q got=%q（wire=%q）", in, got, wire)
		}
	}
}

// TestQoderSwapThirds 边界：长度 0/1/2/3 时 t=len/3 为 0，必须原样返回；
// 长串前 1/3 与后 1/3 严格交换、中段不动。
func TestQoderSwapThirds(t *testing.T) {
	for _, s := range []string{"", "a", "ab"} {
		if got := qoderSwapThirds(s); got != s {
			t.Errorf("swapThirds(%q) = %q, want 原样（len/3==0）", s, got)
		}
	}
	if got := qoderSwapThirds("abc"); got != "cba" {
		t.Errorf("swapThirds(abc) = %q, want cba（1/3 交换）", got)
	}
	if got, want := qoderSwapThirds("abcdefghi"), "ghidefabc"; got != want {
		t.Errorf("swapThirds(abcdefghi) = %q, want %q", got, want)
	}
}

// qoderTestEntry 造一条最小目录条目（带 200K 默认 + 1M 上限 + efforts）。
func qoderTestEntry(t *testing.T) qoderModelEntry {
	t.Helper()
	var e qoderModelEntry
	raw := `{
	  "key": "gfmodel", "display_name": "GLM-5.3-Flash", "source": "zhipu",
	  "enable": true, "is_free": true, "is_vl": true, "is_reasoning": true,
	  "price_factor": 0,
	  "context_config": {"200K":{"token_count":200000,"is_default":true},"1M":{"token_count":1000000}},
	  "thinking_config": {"enabled":{"efforts":{"high":{},"max":{"is_default":true}}}}
	}`
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatalf("构造条目失败：%v", err)
	}
	return e
}

// TestQoderParseCatalog 目录解析：对外展示取最大 1M（真实能力），默认窗口仍取
// is_default 的 200K；免费/多模态/推理/efforts 都要带出来。
func TestQoderParseCatalog(t *testing.T) {
	raw := []byte(`{"app":[` + `{
	  "key": "gfmodel", "display_name": "GLM-5.3-Flash", "source": "zhipu",
	  "enable": true, "is_free": true, "is_vl": true, "is_reasoning": true,
	  "price_factor": 0,
	  "context_config": {"200K":{"token_count":200000,"is_default":true},"1M":{"token_count":1000000}},
	  "thinking_config": {"enabled":{"efforts":{"high":{},"max":{"is_default":true}}}}
	}, {"key":"auto","enable":true},{"key":"off","enable":false}]}`)
	cat, err := qoderParseCatalog(raw)
	if err != nil {
		t.Fatalf("解析目录：%v", err)
	}
	if len(cat.Models) != 1 {
		t.Fatalf("应只保留 1 条（auto/disabled 剔除），得到 %d：%+v", len(cat.Models), cat.Models)
	}
	mi := cat.Models[0]
	if mi.ID != "gfmodel" || mi.Name != "GLM-5.3-Flash" {
		t.Errorf("id/name = %q/%q", mi.ID, mi.Name)
	}
	if mi.ContextWindow != 1000000 {
		t.Errorf("ContextWindow = %d, want 1000000（对外展示真实上限）", mi.ContextWindow)
	}
	if !mi.Free || !mi.SupportsImages || !mi.SupportsReasoning || !mi.SupportsToolCall {
		t.Errorf("能力旗标 = free:%v vl:%v reasoning:%v tools:%v，want 全 true",
			mi.Free, mi.SupportsImages, mi.SupportsReasoning, mi.SupportsToolCall)
	}
	if strings.Join(mi.Efforts, ",") != "high,max" || mi.DefaultEffort != "max" {
		t.Errorf("efforts = %v default=%q，want [high max]/max", mi.Efforts, mi.DefaultEffort)
	}
	if _, ok := cat.Raw["gfmodel"]; !ok {
		t.Error("Raw 里缺 model_config 原对象（请求体必须原样回传）")
	}
}

// TestQoderBuildBodyAlwaysThink 无 thinking_config.disabled 的模型是「始终思考」：
// 不能发 enable_thinking:false（上游会 400）。同时 reasoning_effort 必须收敛到
// 该模型支持的档位（ultra→max）。
func TestQoderBuildBodyAlwaysThink(t *testing.T) {
	e := qoderTestEntry(t)
	cfg := json.RawMessage(`{"key":"gfmodel"}`)
	chat := &qoderChatRequest{
		Model:           "gfmodel",
		Messages:        []qoderChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		ReasoningEffort: "ultra",
	}
	body, err := qoderBuildBody(chat, cfg, e, ChatMeta{ConversationID: "c1"})
	if err != nil {
		t.Fatalf("build body: %v", err)
	}
	var got struct {
		Parameters map[string]any   `json:"parameters"`
		SessionID  string           `json:"session_id"`
		Messages   []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body 不是 JSON: %v", err)
	}
	if v, ok := got.Parameters["enable_thinking"]; !ok || v != true {
		t.Errorf("enable_thinking = %#v, want true（始终思考模型不能关思考）", v)
	}
	if got.Parameters["reasoning_effort"] != "max" {
		t.Errorf("reasoning_effort = %#v, want max（ultra 收敛到受支持档）", got.Parameters["reasoning_effort"])
	}
	if got.Parameters["context_length"] != float64(200000) {
		t.Errorf("context_length = %#v, want 200000（默认窗口装得下就不升档）", got.Parameters["context_length"])
	}
	if len(got.Messages) < 2 || got.Messages[0]["role"] != "system" {
		t.Errorf("首条必须是 system（Qoder agent 协议），得到 %#v", got.Messages)
	}
	if got.SessionID == "" {
		t.Error("session_id 不能为空")
	}
}

// TestQoderHeadersShape COSY 头组形状：Authorization 前缀 Bearer COSY.、
// machine 三件套同值、Cosy-Date 与签名同时生成。
func TestQoderHeadersShape(t *testing.T) {
	a := &auth.Auth{UID: "qoder-u1", AccessToken: "dt", Nickname: "tester"}
	a.SetQoderMachineID("11111111-2222-3333-4444-555555555555")
	c := &Client{}
	req, err := http.NewRequest(http.MethodPost, "https://gateway.qoder.com.cn/algo/api/v2/service/pro/sse/agent_chat_generation", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.qoderHeaders(req, a, []byte("x")); err != nil {
		t.Fatalf("生成头失败：%v", err)
	}
	authz := req.Header.Get("Authorization")
	if !strings.HasPrefix(authz, "Bearer COSY.") || strings.Count(authz, ".") < 2 {
		t.Errorf("Authorization = %q，want Bearer COSY.<payload>.<sig>", authz)
	}
	mid := "11111111-2222-3333-4444-555555555555"
	for _, h := range []string{"Cosy-MachineId", "Cosy-MachineToken", "Cosy-ClientIp"} {
		if req.Header.Get(h) != mid {
			t.Errorf("%s = %q, want %q", h, req.Header.Get(h), mid)
		}
	}
	if req.Header.Get("Cosy-Date") == "" || req.Header.Get("Cosy-Key") == "" {
		t.Error("Cosy-Date / Cosy-Key 不能为空")
	}
	if req.Header.Get("Cosy-User") != "u1" {
		t.Errorf("Cosy-User = %q, want u1（去掉 qoder- 前缀）", req.Header.Get("Cosy-User"))
	}
}

// TestQoderSSEEnvelope 解包 envelope：body 是标准 OpenAI chunk 字符串，
// statusCodeValue 非 200 必须转成 error 帧。空 body 心跳要丢弃。
func TestQoderSSEEnvelope(t *testing.T) {
	in := "data: {\"body\":\"{\\\"choices\\\":[{\\\"delta\\\":{\\\"content\\\":\\\"hi\\\"}}]}\",\"statusCodeValue\":200}\n\n" +
		"data: {\"body\":\"\",\"statusCodeValue\":200}\n\n" +
		"data: [DONE]\n\n"
	s := newQoderSSEStream(io.NopCloser(strings.NewReader(in)), bufio.NewReader(strings.NewReader(in)), nil, nil)
	out, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("读流：%v", err)
	}
	text := string(out)
	if strings.Count(text, "data: ") != 2 {
		t.Fatalf("应输出 chunk + [DONE] 两帧，得到 %q", text)
	}
	if !strings.Contains(text, `"content":"hi"`) || !strings.Contains(text, "data: [DONE]") {
		t.Errorf("解包结果不对：%q", text)
	}
	if strings.Contains(text, "statusCodeValue") {
		t.Errorf("envelope 不该透给客户端：%q", text)
	}

	errIn := "data: {\"body\":\"queue full\",\"statusCodeValue\":429}\n\n"
	s2 := newQoderSSEStream(io.NopCloser(strings.NewReader(errIn)), bufio.NewReader(strings.NewReader(errIn)), nil, nil)
	out2, _ := io.ReadAll(s2)
	if !strings.Contains(string(out2), "qoder_error") || !strings.Contains(string(out2), "429") {
		t.Errorf("非 200 envelope 应转 error 帧，得到 %q", string(out2))
	}
}

// TestQoderFitEffort 档位收敛：受支持的原样返回，未知档收敛到最近一档。
func TestQoderFitEffort(t *testing.T) {
	e := qoderTestEntry(t)
	cases := map[string]string{"high": "high", "max": "max", "ultra": "max", "low": "high", "none": "high"}
	for in, want := range cases {
		if got := qoderFitEffort(in, e); got != want {
			t.Errorf("fitEffort(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ = bytes.MinRead

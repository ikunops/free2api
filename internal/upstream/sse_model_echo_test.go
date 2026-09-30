// sse_model_echo_test.go 锚定出口 model 回填（StreamHintModel）：客户端请求名必须
// 出现在响应每一帧的 model 字段，与上游回程的裸名无关。回归背景：同源多逻辑名
// （cn:x-x0.11 / global:x-free）时裸名回程会被严格校验的 harness 判为模型错配。
package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

const modelEchoSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"bare-upstream\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":2}}\n\n" +
	"data: [DONE]\n\n"

func modelEchoFrames(t *testing.T, model string) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := StreamHintModel(rec, strings.NewReader(modelEchoSSE), nil, model); err != nil {
		t.Fatal(err)
	}
	var frames []map[string]any
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.HasPrefix(ln, "data: [DONE]") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj); err != nil {
			t.Fatalf("bad frame %q: %v", ln, err)
		}
		frames = append(frames, obj)
	}
	return frames
}

// TestStreamHintModelEchoesClientName 每帧 model 都改成客户端请求名。
func TestStreamHintModelEchoesClientName(t *testing.T) {
	frames := modelEchoFrames(t, "cn:deepseek-v4.1-flash-x0.11")
	if len(frames) != 2 {
		t.Fatalf("frames=%d want 2", len(frames))
	}
	for i, fr := range frames {
		if fr["model"] != "cn:deepseek-v4.1-flash-x0.11" {
			t.Errorf("frame %d model=%v want client name", i, fr["model"])
		}
	}
}

// TestStreamHintEmptyModelKeepsUpstream 空串 = 不改写（旧行为，零回归）：
// 上游给了 model 的帧原样保留裸名，没给 model 的帧也不会被补上。
func TestStreamHintEmptyModelKeepsUpstream(t *testing.T) {
	frames := modelEchoFrames(t, "")
	if len(frames) != 2 {
		t.Fatalf("frames=%d want 2", len(frames))
	}
	if frames[0]["model"] != "bare-upstream" {
		t.Errorf("frame 0 model=%v want bare-upstream (no rewrite)", frames[0]["model"])
	}
	if _, ok := frames[1]["model"]; ok {
		t.Errorf("frame without model must stay without model, got %v", frames[1]["model"])
	}
}

// TestStreamHintModelLeavesErrorFrameIntact error 帧原样透传，不注入 model。
func TestStreamHintModelLeavesErrorFrameIntact(t *testing.T) {
	raw := "data: {\"error\":{\"message\":\"rate limited\",\"code\":6004}}\n\ndata: [DONE]\n\n"
	rec := httptest.NewRecorder()
	if err := StreamHintModel(rec, strings.NewReader(raw), nil, "cn:x"); err != nil {
		t.Fatal(err)
	}
	var frames []map[string]any
	for _, ln := range strings.Split(rec.Body.String(), "\n") {
		ln = strings.TrimSpace(ln)
		if !strings.HasPrefix(ln, "data: ") || strings.HasPrefix(ln, "data: [DONE]") {
			continue
		}
		var obj map[string]any
		_ = json.Unmarshal([]byte(strings.TrimPrefix(ln, "data: ")), &obj)
		frames = append(frames, obj)
	}
	if len(frames) != 1 {
		t.Fatalf("frames=%d want 1", len(frames))
	}
	if _, ok := frames[0]["model"]; ok {
		t.Errorf("error frame must not gain a model field: %#v", frames[0])
	}
}

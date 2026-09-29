package session

import (
	"strings"
	"testing"
)

// contentSignature 的 TDD 锚点（任务书 turnkey-signature-impl）：
// 修复 G1——末条/首条 user 消息纯图片（无 text part）时 TurnKey /
// StickyFallbackKey 返回空串 → 聚合头请求级随机碎片化 + 粘性兜底失效。

// imgBody 构造纯图片/图文混合 body（末条 user 在 index=turnIdx 处）。
func imgBody(turnIdx int, text string, urls ...string) []byte {
	return nil
}

// TestTurnKeyImageOnlyStable 锚点1：纯图末条 user 同 body ×2 同键
// （RED：现返回 ""，聚合链退化请求级随机）。
func TestTurnKeyImageOnlyStable(t *testing.T) {
	body := []byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`)
	a := TurnKey(body)
	b := TurnKey(body)
	if a == "" {
		t.Fatal("纯图末条 user 应派生非空轮级键（G1：现为空串碎片化）")
	}
	if a != b {
		t.Fatalf("同 body 纯图应同键: %q vs %q", a, b)
	}
}

// TestTurnKeyImageTextCombined 锚点2：图文混合键 ≠ 纯文本键 ≠ 纯图键。
func TestTurnKeyImageTextCombined(t *testing.T) {
	mixed := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`)
	textOnly := []byte(`{"messages":[{"role":"user","content":"看图"}]}`)
	imageOnly := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`)
	m := TurnKey(mixed)
	tx := TurnKey(textOnly)
	im := TurnKey(imageOnly)
	if m == "" || tx == "" || im == "" {
		t.Fatalf("三形态均应非空: mixed=%q text=%q image=%q", m, tx, im)
	}
	if m == tx {
		t.Errorf("图文混合键应区分于纯文本键: %q", m)
	}
	if m == im {
		t.Errorf("图文混合键应区分于纯图键: %q", m)
	}
	if tx != "u0:看图" {
		t.Errorf("纯文本路径签名应与 contentText 完全一致（向后兼容）: got %q", tx)
	}
}

// TestTurnKeyImageDifferentUrlDifferentKey 锚点3：不同图不同键。
func TestTurnKeyImageDifferentUrlDifferentKey(t *testing.T) {
	a := TurnKey([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`))
	b := TurnKey([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/dog.png"}}]}]}`))
	if a == "" || b == "" {
		t.Fatalf("均应非空: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("不同图应不同键: %q", a)
	}
}

// TestTurnKeyIndexStillSeparatesTurns 锚点4：同图不同序号（跨轮）不同键
// （序号入键防"继续"类跨轮混并的既有设计不得回退）。
func TestTurnKeyIndexStillSeparatesTurns(t *testing.T) {
	a := TurnKey([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`))
	b := TurnKey([]byte(`{"messages":[{"role":"user","content":"第一问"},` +
		`{"role":"assistant","content":"答"},` +
		`{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`))
	if a == "" || b == "" {
		t.Fatalf("均应非空: %q %q", a, b)
	}
	if a == b {
		t.Fatalf("同图不同序号应不同键（跨轮不混并）: %q", a)
	}
}

// TestStickyFallbackKeyFirstImage 锚点5：首条 user 纯图片的会话派生非空
// stickyKey（RED：现返回 ""，粘性盲区）。
func TestStickyFallbackKeyFirstImage(t *testing.T) {
	body := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]}]}`)
	a := StickyFallbackKey(body)
	b := StickyFallbackKey(body)
	if a == "" {
		t.Fatal("首条 user 纯图片应派生非空粘性键（G1 加重形态：现为空串）")
	}
	if a != b {
		t.Fatalf("同 body 应同键: %q vs %q", a, b)
	}
	// 会话推进（历史追加）不换键的 #169 契约对图片形态同样成立。
	longer := []byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"https://img.example/cat.png"}}]},` +
		`{"role":"assistant","content":"答"},{"role":"user","content":"继续"}]}`)
	if c := StickyFallbackKey(longer); c != a {
		t.Fatalf("历史追加不应换键: %q vs %q", c, a)
	}
}

// TestStickyFallbackKeyStableWithSignature 锚点6：签名化后历史追加仍同键
// （回归 #169 契约——纯文本路径键值不得漂移）。
func TestStickyFallbackKeyStableWithSignature(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"开场白"}]}`)
	longer := []byte(`{"messages":[{"role":"user","content":"开场白"},` +
		`{"role":"assistant","content":"好的"},{"role":"user","content":"继续"}]}`)
	if a, b := StickyFallbackKey(first), StickyFallbackKey(longer); a != b || a == "" {
		t.Fatalf("纯文本键回归 #169 契约: first=%q longer=%q", a, b)
	}
}

// TestContentSignatureBounds 签名边界：data: 超长 base64 只入摘要；空/null 不伪造。
func TestContentSignatureBounds(t *testing.T) {
	// data: URL 超长（>1024）只入 sha256 前 8 hex，键长度有界。
	longDataURL := "data:image/png;base64," + strings.Repeat("QUFBQQ", 4096)
	k := TurnKey([]byte(`{"messages":[{"role":"user","content":[` +
		`{"type":"image_url","image_url":{"url":"` + longDataURL + `"}}]}]}`))
	if k == "" {
		t.Fatal("data: 超长图应仍派生键")
	}
	if len(k) > 256 {
		t.Errorf("签名键长度应有界（摘要防超长）: len=%d", len(k))
	}
	// content 为空 / null → ""（不伪造）。
	if got := TurnKey([]byte(`{"messages":[{"role":"user","content":null}]}`)); got != "" {
		t.Errorf("null content 应空串, got %q", got)
	}
	if got := TurnKey([]byte(`{"messages":[{"role":"user","content":[]}]}`)); got != "" {
		t.Errorf("空数组 content 应空串, got %q", got)
	}
}

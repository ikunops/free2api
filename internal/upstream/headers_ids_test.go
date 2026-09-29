package upstream

import (
	"regexp"
	"testing"

	"free2api/internal/auth"
)

// hex32Re / hex16Re 32 位与 16 位 hex 合法格式断言（B3 与消息级 ID 的合规性）。
var hex32Re = regexp.MustCompile(`^[0-9a-f]{32}$`)
var hex16Re = regexp.MustCompile(`^[0-9a-f]{16}$`)

// TestChatHeadersConversationFullMeta 全量 meta 出站头族（issue #35 后台聚合）：
// 聚合主键 X-Conversation-Request-ID 必发且等于传入值，消息级双头同值 32 hex，
// X-Root-Request-ID = conversationRequestID，Trace 透传入站值，B3 链路族格式合法，
// 既有账号头零回归。
func TestChatHeadersConversationFullMeta(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	meta := ChatMeta{
		ConversationID:        "conv-1",
		ConversationRequestID: "0123456789abcdef0123456789abcdef",
		TraceID:               "trace-inbound",
	}
	c.ChatHeaders(req, a, "", meta)

	if got := req.Header.Get("X-Conversation-ID"); got != "conv-1" {
		t.Errorf("X-Conversation-ID = %q want conv-1", got)
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got != meta.ConversationRequestID {
		t.Errorf("X-Conversation-Request-ID = %q want %q", got, meta.ConversationRequestID)
	}
	msgID := req.Header.Get("X-Conversation-Message-ID")
	if msgID != req.Header.Get("X-Request-ID") {
		t.Errorf("X-Conversation-Message-ID(%q) != X-Request-ID(%q)", msgID, req.Header.Get("X-Request-ID"))
	}
	if !hex32Re.MatchString(msgID) {
		t.Errorf("message ID %q not 32 hex", msgID)
	}
	if got := req.Header.Get("X-Root-Request-ID"); got != meta.ConversationRequestID {
		t.Errorf("X-Root-Request-ID = %q want %q", got, meta.ConversationRequestID)
	}
	if got := req.Header.Get("X-Trace-ID"); got != "trace-inbound" {
		t.Errorf("X-Trace-ID = %q want trace-inbound", got)
	}
	if got := req.Header.Get("X-B3-TraceId"); got != meta.ConversationRequestID {
		t.Errorf("X-B3-TraceId = %q want %q (convReqID 32 hex)", got, meta.ConversationRequestID)
	}
	if got := req.Header.Get("X-B3-SpanId"); got != msgID[:16] {
		t.Errorf("X-B3-SpanId = %q want %q (msgID[:16])", got, msgID[:16])
	}
	if got := req.Header.Get("X-B3-Sampled"); got != "1" {
		t.Errorf("X-B3-Sampled = %q want 1", got)
	}
	// 既有头零回归：common + chat 账号头仍旧在。
	if got := req.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization = %q (regression)", got)
	}
}

// TestChatHeadersConversationNoConvID meta.ConversationID 为空 → 不发 X-Conversation-ID
// （透传客户端原值优先，不伪造），其余头族照常。
func TestChatHeadersConversationNoConvID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: "convreq-1"})

	if got := req.Header.Get("X-Conversation-ID"); got != "" {
		t.Errorf("X-Conversation-ID = %q want empty (client didn't send one)", got)
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got != "convreq-1" {
		t.Errorf("X-Conversation-Request-ID = %q want convreq-1", got)
	}
	if got := req.Header.Get("X-Trace-ID"); got != "convreq-1" {
		t.Errorf("X-Trace-ID = %q want convreq-1 (fallback to convReqID)", got)
	}
}

// TestChatHeadersConversationEmptyMeta 零值 meta（如测试/直接调用方）也必发聚合主键：
// conversationRequestID 内部补一个 NewMessageID（32 hex），B3 TraceId 取其值。
func TestChatHeadersConversationEmptyMeta(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	convReqID := req.Header.Get("X-Conversation-Request-ID")
	if !hex32Re.MatchString(convReqID) {
		t.Errorf("X-Conversation-Request-ID = %q want 32 hex (必发)", convReqID)
	}
	if got := req.Header.Get("X-Root-Request-ID"); got != convReqID {
		t.Errorf("X-Root-Request-ID = %q want %q", got, convReqID)
	}
	if got := req.Header.Get("X-B3-TraceId"); got != convReqID {
		t.Errorf("X-B3-TraceId = %q want %q", got, convReqID)
	}
	if got := req.Header.Get("X-Conversation-ID"); got != "" {
		t.Errorf("X-Conversation-ID = %q want empty", got)
	}
}

// TestChatHeadersConversationInvalidTraceConvReqID 入站透传的 conversationRequestID
// 非法 B3 TraceId（含横线/长度非 16/32）→ 仍按聚合主键透传，B3 TraceId 回落消息级
// messageID（32 hex），SpanId 仍为 messageID[:16]。
func TestChatHeadersConversationInvalidTraceConvReqID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	bad := "convreq-abc-123" // 非 hex：长度 13、含横线
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: bad})

	if got := req.Header.Get("X-Conversation-Request-ID"); got != bad {
		t.Errorf("X-Conversation-Request-ID = %q want %q (透传不改)", got, bad)
	}
	if got := req.Header.Get("X-Root-Request-ID"); got != bad {
		t.Errorf("X-Root-Request-ID = %q want %q (Root = conversationRequestID)", got, bad)
	}
	msgID := req.Header.Get("X-Conversation-Message-ID")
	trace := req.Header.Get("X-B3-TraceId")
	if trace == bad {
		t.Errorf("X-B3-TraceId = %q must NOT be invalid trace id", trace)
	}
	if !hex32Re.MatchString(trace) {
		t.Errorf("X-B3-TraceId = %q want 32 hex fallback", trace)
	}
	if got := req.Header.Get("X-B3-SpanId"); got != msgID[:16] {
		t.Errorf("X-B3-SpanId = %q want %q", got, msgID[:16])
	}
	if !hex16Re.MatchString(req.Header.Get("X-B3-SpanId")) {
		t.Errorf("X-B3-SpanId = %q not 16 hex", req.Header.Get("X-B3-SpanId"))
	}
}
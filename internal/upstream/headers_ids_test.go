package upstream

import (
	"regexp"
	"testing"

	"free2api/internal/auth"
)

var hex32Re = regexp.MustCompile(`^[0-9a-f]{32}$`)
var hex16Re = regexp.MustCompile(`^[0-9a-f]{16}$`)
var uuidV7Re = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// TestChatHeadersConversationFullMeta 全量 meta 出站头族（issue #35 后台聚合）：
// 聚合主键 X-Conversation-Request-ID 必发且等于传入值，消息级双头同值 32 hex，
// X-Root-Request-ID = conversationRequestID，B3 链路族格式合法，既有账号头零回归。
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
	// 渠道校验要求整组链路头；非 hex 的入站 trace 不透传，回落消息级 32-hex。
	if got := req.Header.Get("X-Trace-ID"); !hex32Re.MatchString(got) {
		t.Errorf("X-Trace-ID = %q want 32 hex", got)
	}
	if got := req.Header.Get("X-B3-TraceId"); !hex32Re.MatchString(got) {
		t.Errorf("X-B3-TraceId = %q want 32 hex", got)
	}
	if got := req.Header.Get("X-B3-SpanId"); got != msgID[:16] {
		t.Errorf("X-B3-SpanId = %q want %q (msgID[:16])", got, msgID[:16])
	}
	if got := req.Header.Get("X-B3-Sampled"); got != "1" {
		t.Errorf("X-B3-Sampled = %q want 1", got)
	}
	if got := req.Header.Get("traceparent"); !regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-01$`).MatchString(got) {
		t.Errorf("traceparent = %q invalid", got)
	}
	if got := req.Header.Get("b3"); got == "" {
		t.Error("b3 missing")
	}
	if got := req.Header.Get("Authorization"); got != "Bearer at" {
		t.Errorf("Authorization = %q (regression)", got)
	}
}

// TestChatHeadersConversationNoConvID meta.ConversationID 为空 → 必须补 UUIDv7。
func TestChatHeadersConversationNoConvID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: "convreq-1"})

	if got := req.Header.Get("X-Conversation-ID"); !uuidV7Re.MatchString(got) {
		t.Errorf("X-Conversation-ID = %q want UUIDv7", got)
	}
	if got := req.Header.Get("X-Conversation-Request-ID"); got != "convreq-1" {
		t.Errorf("X-Conversation-Request-ID = %q want convreq-1", got)
	}
	if got := req.Header.Get("X-Trace-ID"); !hex32Re.MatchString(got) {
		t.Errorf("X-Trace-ID = %q want 32 hex", got)
	}
}

// TestChatHeadersConversationEmptyMeta 零值 meta 也必发完整官方会话头族。
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
	if got := req.Header.Get("X-B3-TraceId"); !hex32Re.MatchString(got) {
		t.Errorf("X-B3-TraceId = %q want 32 hex", got)
	}
	if got := req.Header.Get("X-Conversation-ID"); !uuidV7Re.MatchString(got) {
		t.Errorf("X-Conversation-ID = %q want UUIDv7", got)
	}
}

// TestChatHeadersConversationInvalidTraceConvReqID 非法 B3 TraceId 回落消息级 32 hex。
func TestChatHeadersConversationInvalidTraceConvReqID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	bad := "convreq-abc-123"
	c.ChatHeaders(req, a, "", ChatMeta{ConversationRequestID: bad})

	if got := req.Header.Get("X-Conversation-Request-ID"); got != bad {
		t.Errorf("X-Conversation-Request-ID = %q want %q (透传不改)", got, bad)
	}
	if got := req.Header.Get("X-Root-Request-ID"); got != bad {
		t.Errorf("X-Root-Request-ID = %q want %q", got, bad)
	}
	msgID := req.Header.Get("X-Conversation-Message-ID")
	if got := req.Header.Get("X-B3-TraceId"); !hex32Re.MatchString(got) {
		t.Errorf("X-B3-TraceId = %q want 32 hex fallback", got)
	}
	if got := req.Header.Get("X-B3-SpanId"); got != msgID[:16] || !hex16Re.MatchString(got) {
		t.Errorf("X-B3-SpanId = %q want %q", got, msgID[:16])
	}
}

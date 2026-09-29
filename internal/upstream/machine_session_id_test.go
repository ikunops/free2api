package upstream

import (
	"regexp"
	"testing"

	"free2api/internal/auth"
)

// hex36Re 36 位 hex 合法格式断言（machineId/sessionId 派生形态）。
var hex36Re = regexp.MustCompile(`^[0-9a-f]{36}$`)

// TestDeriveAccountStableIDStable 同 uid + 同用途两次派生 → 恒同值（跨重启稳定语义：
// 固定盐纯派生，无随机源）。
func TestDeriveAccountStableIDStable(t *testing.T) {
	first := deriveAccountStableID("u1", "machine")
	second := deriveAccountStableID("u1", "machine")
	if first != second {
		t.Errorf("deriveAccountStableID(u1, machine) unstable: %q vs %q", first, second)
	}
}

// TestDeriveAccountStableIDDistinct 不同 uid → 不同值（账号间互异，防多号关联）。
func TestDeriveAccountStableIDDistinct(t *testing.T) {
	if deriveAccountStableID("u1", "machine") == deriveAccountStableID("u2", "machine") {
		t.Errorf("different uids derive same machine ID")
	}
}

// TestDeriveAccountStableIDPurposeIsolation 同 uid + "machine" vs "session" → 不同值
// （用途盐隔离：设备标识与账号固定会话标识不可碰撞）。
func TestDeriveAccountStableIDPurposeIsolation(t *testing.T) {
	if deriveAccountStableID("u1", "machine") == deriveAccountStableID("u1", "session") {
		t.Errorf("machine/session purposes derive same ID")
	}
}

// TestDeriveAccountStableIDFormat 输出恒为 36 hex（对齐 hub md5[:36] 同形态）。
func TestDeriveAccountStableIDFormat(t *testing.T) {
	for _, purpose := range []string{"machine", "session"} {
		got := deriveAccountStableID("uid-abc", purpose)
		if !hex36Re.MatchString(got) {
			t.Errorf("deriveAccountStableID(%q, %q) = %q, want 36 hex", "uid-abc", purpose, got)
		}
	}
}

// TestCommonHeadersInjectsMachineSessionID CommonHeaders 后 req 携带 X-Machine-ID 与
// X-Session-ID，且两头互异、值与直接派生一致（账号级稳定头族全出站路径覆盖）。
func TestCommonHeadersInjectsMachineSessionID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u1"}
	req := mustRequest(t)
	c := &Client{}
	c.CommonHeaders(req, a)

	machine := req.Header.Get("X-Machine-ID")
	session := req.Header.Get("X-Session-ID")
	if !hex36Re.MatchString(machine) {
		t.Errorf("X-Machine-ID = %q, want 36 hex", machine)
	}
	if !hex36Re.MatchString(session) {
		t.Errorf("X-Session-ID = %q, want 36 hex", session)
	}
	if machine != deriveAccountStableID("u1", "machine") {
		t.Errorf("X-Machine-ID = %q, want deriveAccountStableID(u1, machine)", machine)
	}
	if session != deriveAccountStableID("u1", "session") {
		t.Errorf("X-Session-ID = %q, want deriveAccountStableID(u1, session)", session)
	}
	if machine == session {
		t.Errorf("X-Machine-ID == X-Session-ID (must differ by purpose salt)")
	}
}

// TestCommonHeadersEmptyUIDSkipsInjection uid 为空（匿名请求）→ 不注入两头且不 panic。
func TestCommonHeadersEmptyUIDSkipsInjection(t *testing.T) {
	req := mustRequest(t)
	c := &Client{}
	c.CommonHeaders(req, nil)

	if got := req.Header.Get("X-Machine-ID"); got != "" {
		t.Errorf("X-Machine-ID = %q, want empty (no auth)", got)
	}
	if got := req.Header.Get("X-Session-ID"); got != "" {
		t.Errorf("X-Session-ID = %q, want empty (no auth)", got)
	}
	req2 := mustRequest(t)
	c.CommonHeaders(req2, &auth.Auth{AccessToken: "at", UID: ""})
	if got := req2.Header.Get("X-Machine-ID"); got != "" {
		t.Errorf("X-Machine-ID = %q, want empty (empty uid)", got)
	}
	if got := req2.Header.Get("X-Session-ID"); got != "" {
		t.Errorf("X-Session-ID = %q, want empty (empty uid)", got)
	}
}

// TestChatHeadersCarriesMachineSessionID ChatHeaders 走 CommonHeaders 之上叠加 →
// 两头天然继承（chat/billing/refresh 全覆盖路径的回归锚点）。
func TestChatHeadersCarriesMachineSessionID(t *testing.T) {
	a := &auth.Auth{AccessToken: "at", UID: "u9"}
	req := mustRequest(t)
	c := &Client{}
	c.ChatHeaders(req, a, "", ChatMeta{})

	if got := req.Header.Get("X-Machine-ID"); got != deriveAccountStableID("u9", "machine") {
		t.Errorf("X-Machine-ID = %q, want deriveAccountStableID(u9, machine)", got)
	}
	if got := req.Header.Get("X-Session-ID"); got != deriveAccountStableID("u9", "session") {
		t.Errorf("X-Session-ID = %q, want deriveAccountStableID(u9, session)", got)
	}
}

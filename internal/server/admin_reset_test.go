// admin_reset_test.go POST /admin/accounts/reset 的契约测试。
//
// 三个作用域（全池 / 点名 / 按 producer）+ 一条边界：复位**不解禁**。
package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
)

type resetResp struct {
	Scope              string   `json:"scope"`
	Producer           string   `json:"producer"`
	Matched            int      `json:"matched"`
	Reset              int      `json:"reset"`
	StillDisabled      int      `json:"still_disabled"`
	StillManualDisable int      `json:"still_manual_disabled"`
	NotFound           []string `json:"not_found"`
	Message            string   `json:"message"`
}

func doReset(t *testing.T, h *Handler, body string) (*httptest.ResponseRecorder, resetResp) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/accounts/reset", bytes.NewReader([]byte(body))))
	var out resetResp
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode reset response: %v body=%s", err, rec.Body)
		}
	}
	return rec, out
}

// TestAdminAccountsResetAll 空体 = 全池：把冷却清掉，账号立刻回到可用态。
func TestAdminAccountsResetAll(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"}, &auth.Auth{UID: "u2"})
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429 rate limit")
	p.Cooldown("u2", pool.CoolSoft, time.Hour, "429 rate limit")
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	rec, out := doReset(t, h, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if out.Scope != "all" || out.Matched != 2 || out.Reset != 2 {
		t.Fatalf("reset 响应 = %+v", out)
	}
	for _, uid := range []string{"u1", "u2"} {
		if st, _ := p.Status(uid); st.Cooling {
			t.Fatalf("%s 仍在冷却", uid)
		}
	}
	if out.Message == "" {
		t.Error("message 不该为空（面板 toast 直接用）")
	}
}

// TestAdminAccountsResetByProducer 按 AI 客户端复位：只动这个客户端的号。
func TestAdminAccountsResetByProducer(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "z1"}, &auth.Auth{UID: "w1"})
	p.SetProducerOf(func(uid string) string {
		if uid == "z1" {
			return "zcode"
		}
		return "workbuddy"
	})
	p.Cooldown("z1", pool.CoolSoft, time.Hour, "429")
	p.Cooldown("w1", pool.CoolSoft, time.Hour, "429")
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	_, out := doReset(t, h, `{"producer":"zcode"}`)
	if out.Scope != "producer" || out.Matched != 1 || out.Reset != 1 {
		t.Fatalf("reset 响应 = %+v", out)
	}
	if st, _ := p.Status("z1"); st.Cooling {
		t.Fatal("zcode 的号应已复位")
	}
	if st, _ := p.Status("w1"); !st.Cooling {
		t.Fatal("workbuddy 的号不该被动")
	}
	// 生产者名写错时 matched=0，message 仍可用（面板据此提示）
	_, out2 := doReset(t, h, `{"producer":"qoder"}`)
	if out2.Matched != 0 {
		t.Fatalf("未知 producer 应 matched=0，得到 %+v", out2)
	}
}

// TestAdminAccountsResetByUIDList 点名复位 + 未知 uid 回 not_found。
func TestAdminAccountsResetByUIDList(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"}, &auth.Auth{UID: "u2"})
	p.Cooldown("u1", pool.CoolSoft, time.Hour, "429")
	p.Cooldown("u2", pool.CoolSoft, time.Hour, "429")
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	_, out := doReset(t, h, `{"uids":["u1","ghost"]}`)
	if out.Scope != "uids" || out.Matched != 1 || out.Reset != 1 {
		t.Fatalf("reset 响应 = %+v", out)
	}
	if len(out.NotFound) != 1 || out.NotFound[0] != "ghost" {
		t.Fatalf("not_found = %v", out.NotFound)
	}
	if st, _ := p.Status("u2"); !st.Cooling {
		t.Fatal("没点名的 u2 不该被动")
	}
}

// TestAdminAccountsResetKeepsDisabled 复位不是解禁：disabled 保持，并在响应里点明。
func TestAdminAccountsResetKeepsDisabled(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "dead"}, &auth.Auth{UID: "live"})
	p.Disable("dead", "12153 session dead")
	p.Cooldown("live", pool.CoolSoft, time.Hour, "429")
	h := NewHandler(Config{Pool: p, AdminEnabled: true})

	_, out := doReset(t, h, "")
	if out.StillDisabled != 1 {
		t.Fatalf("still_disabled=%d want 1（响应=%+v）", out.StillDisabled, out)
	}
	st, _ := p.Status("dead")
	if !st.Disabled || st.DisabledReason != "12153 session dead" {
		t.Fatalf("disabled 态被复位动过：%+v", st)
	}
}

// TestAdminAccountsResetNeedsAdminFlag 管理面没开时该路径不注册（404，不得 405）。
func TestAdminAccountsResetNeedsAdminFlag(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "u1"})
	h := NewHandler(Config{Pool: p})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/admin/accounts/reset", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d want 404", rec.Code)
	}
}

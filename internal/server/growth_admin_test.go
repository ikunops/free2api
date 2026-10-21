package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGrowthAdminClaimDefaultsDryRun(t *testing.T) {
	var gotDry []bool
	h := NewHandler(Config{
		AdminEnabled: true,
		GrowthScan:   func(context.Context) (any, error) { return map[string]any{"ok": true}, nil },
		GrowthClaim: func(_ context.Context, dry bool) (any, error) {
			gotDry = append(gotDry, dry)
			return map[string]any{"dry_run": dry}, nil
		},
	})
	do := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		var rdr *bytes.Reader
		if body == "" {
			rdr = bytes.NewReader(nil)
		} else {
			rdr = bytes.NewReader([]byte(body))
		}
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/growth/claim", rdr))
		return rec
	}
	if rec := do(""); rec.Code != http.StatusOK {
		t.Fatalf("empty body code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := do(`{}`); rec.Code != http.StatusOK {
		t.Fatalf("{} code=%d body=%s", rec.Code, rec.Body)
	}
	if rec := do(`{"dry_run":false}`); rec.Code != http.StatusOK {
		t.Fatalf("explicit false code=%d body=%s", rec.Code, rec.Body)
	}
	if len(gotDry) != 3 || !gotDry[0] || !gotDry[1] || gotDry[2] {
		t.Fatalf("dry_run calls=%v want [true true false]", gotDry)
	}
}

func TestGrowthAdminRoutesNeedCallbacks(t *testing.T) {
	h := NewHandler(Config{AdminEnabled: true})
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/admin/growth/tasks"},
		{http.MethodPost, "/admin/growth/claim"},
		{http.MethodPost, "/admin/growth/run"},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s code=%d want 404 without callbacks", tc.method, tc.path, rec.Code)
		}
	}

	h = NewHandler(Config{
		AdminEnabled: true,
		GrowthScan:   func(context.Context) (any, error) { return nil, nil },
		GrowthClaim:  func(context.Context, bool) (any, error) { return nil, nil },
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/growth/run", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run without GrowthRun code=%d want 404", rec.Code)
	}
}

func TestGrowthAdminRunInjected(t *testing.T) {
	called := false
	h := NewHandler(Config{
		AdminEnabled: true,
		GrowthScan:   func(context.Context) (any, error) { return nil, nil },
		GrowthClaim:  func(context.Context, bool) (any, error) { return nil, nil },
		GrowthRun: func(context.Context) (any, error) {
			called = true
			return map[string]any{"ok": true}, nil
		},
	})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/admin/growth/run", nil))
	if rec.Code != http.StatusOK || !called {
		t.Fatalf("code=%d called=%v body=%s", rec.Code, called, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["ok"] != true {
		t.Fatalf("body=%v", body)
	}
}

// handler_cost_explore_test.go /status 透出 cost_explore 台账（issue #136 §5）：
// events_total + per_model（realm|model → 最近探索时刻）。
package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"free2api/internal/auth"
)

// TestStatusCostExploreExposed 探索发生后 /status 应透出累计事件数与
// per-model 最近探索时刻（键内 \x1f 输出为 "|"）。
func TestStatusCostExploreExposed(t *testing.T) {
	p := testPoolWith(
		&auth.Auth{UID: "free"},
		&auth.Auth{UID: "unknown"},
	)
	p.SetCostExploreInterval(time.Hour)
	p.NoteModelCost("free", "m", 0, 1000)
	h := NewHandler(Config{Pool: p})

	// 触发一次探索（tier 0 垄断 + tier 1 存在 + 零值 timer）。
	if a := p.PickExcludingForRealm(nil, "m", ""); a == nil || a.UID != "unknown" {
		t.Fatalf("pick=%v want unknown（探索改道）", a)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status code=%d body=%s", rec.Code, rec.Body)
	}
	var body struct {
		CostExplore *struct {
			EventsTotal int64             `json:"events_total"`
			PerModel    map[string]string `json:"per_model"`
		} `json:"cost_explore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("status not json: %v", err)
	}
	if body.CostExplore == nil {
		t.Fatalf("cost_explore 键缺失: %s", rec.Body)
	}
	if body.CostExplore.EventsTotal != 1 {
		t.Errorf("events_total=%d want 1", body.CostExplore.EventsTotal)
	}
	ts, ok := body.CostExplore.PerModel["|m"]
	if !ok {
		t.Fatalf("per_model 缺少键 |m: %v", body.CostExplore.PerModel)
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("per_model[|m]=%q 不是合法时间: %v", ts, err)
	}
}

package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"free2api/internal/auth"
)

func TestListGrowthTasksParsesAndMiniprogramHeader(t *testing.T) {
	var platform, authz atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/activity/growth/tasks" {
			http.Error(w, "not found", 404)
			return
		}
		platform.Store(r.Header.Get("X-Client-Platform"))
		authz.Store(r.Header.Get("Authorization"))
		w.Write([]byte(`{"code":0,"data":{"tasks":[
			{"task_code":"chat_5","task_name":"对话 5 次","accept_status":"accepted","progress":{"current":2,"target":5}},
			{"task_code":"","task_name":"bad"},
			{"task_code":"Sequential_Tasks_1","task_name":"小程序任务","accept_status":"not_accepted","progress":{"current":0,"target":1}}
		]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at"}

	tasks, err := c.ListGrowthTasks(a, GrowthPlatformMiniprogram)
	if err != nil {
		t.Fatalf("ListGrowthTasks: %v", err)
	}
	if len(tasks) != 2 {
		t.Fatalf("tasks=%d want 2（空 task_code 丢弃）", len(tasks))
	}
	if tasks[0].TaskCode != "chat_5" || tasks[0].Progress.Current != 2 || tasks[0].Progress.Target != 5 {
		t.Errorf("tasks[0]=%+v", tasks[0])
	}
	if got := platform.Load(); got != "miniprogram" {
		t.Errorf("X-Client-Platform=%v want miniprogram", got)
	}
	if got := authz.Load(); got != "Bearer at" {
		t.Errorf("Authorization=%v want Bearer at", got)
	}
}

func TestAcceptGrowthTasksBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/activity/growth/tasks/accept" {
			http.Error(w, "not found", 404)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"code":0,"data":{"results":[]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.AcceptGrowthTasks(&auth.Auth{UID: "u1", AccessToken: "at"}, []string{"chat_5"}, GrowthPlatformDefault); err != nil {
		t.Fatalf("AcceptGrowthTasks: %v", err)
	}
	codes, _ := body["task_codes"].([]any)
	if len(codes) != 1 || codes[0] != "chat_5" {
		t.Errorf("body=%v want task_codes=[chat_5]", body)
	}
}

func TestClaimGrowthTaskPathAndOutcome(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(`{"code":0,"data":{"already_claimed":true,"credit":12,"energy":3}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	out, err := c.ClaimGrowthTask(&auth.Auth{UID: "u1", AccessToken: "at"}, "chat_5", GrowthPlatformDefault)
	if err != nil {
		t.Fatalf("ClaimGrowthTask: %v", err)
	}
	if path != "/activity/growth/tasks/chat_5/claim" {
		t.Errorf("path=%q", path)
	}
	if !out.AlreadyClaimed || out.Credit != 12 || out.Energy != 3 {
		t.Errorf("out=%+v", out)
	}
}

func TestIsGrowthAlreadyClaimed(t *testing.T) {
	if !IsGrowthAlreadyClaimed(&Error{Status: 400, Msg: "already_claimed"}) {
		t.Error("already_claimed should be normal")
	}
	if !IsGrowthAlreadyClaimed(&Error{Status: 400, Msg: "任务已领取"}) {
		t.Error("中文已领取 should be normal")
	}
	if IsGrowthAlreadyClaimed(&Error{Status: 500, Msg: "boom"}) {
		t.Error("boom should not be normal")
	}
	if IsGrowthAlreadyClaimed(nil) {
		t.Error("nil should not be already")
	}
}

func TestReportGrowthEventsRawArray(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			http.Error(w, "not found", 404)
			return
		}
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if err := c.ReportGrowthEvents(&auth.Auth{UID: "u1", AccessToken: "at"}, []map[string]any{{"eventCode": "chat_request_send"}}); err != nil {
		t.Fatalf("ReportGrowthEvents: %v", err)
	}
	if !strings.HasPrefix(raw, "[") || !strings.Contains(raw, "chat_request_send") {
		t.Errorf("raw body=%s want bare array", raw)
	}
}

func TestListGrowthMarketExpertsUsesRealIDs(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != growthMarketExpertPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[
			{"expert_id":"ex_1","expert_type":"agent","display_name_zh":"专家一","profession_zh":"职业一"},
			{"source_id":"ex_2","expert_type":"team","profession_zh":"职业二"},
			{"expert_id":"ex_1","display_name_zh":"重复"}
		]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	experts, err := c.ListGrowthMarketExperts(&auth.Auth{UID: "u1", AccessToken: "at"}, "agent")
	if err != nil {
		t.Fatalf("ListGrowthMarketExperts: %v", err)
	}
	if len(experts) != 2 || experts[0].ExpertID != "ex_1" || experts[1].ExpertID != "ex_2" {
		t.Fatalf("experts=%+v", experts)
	}
	if body["expert_type"] != "agent" || body["page_size"] != float64(50) {
		t.Fatalf("body=%v", body)
	}
}

func TestListGrowthMarketExpertsRejectsEmptyMarket(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[]}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	if _, err := c.ListGrowthMarketExperts(&auth.Auth{UID: "u1", AccessToken: "at"}, "agent"); err == nil || !strings.Contains(err.Error(), "no agent experts") {
		t.Fatalf("empty market err=%v", err)
	}
}

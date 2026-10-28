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
			{"task_code":"chat_5","title":"对话 5 次","accept_status":"accepted","progress":{"current":2,"target":5},"reward_credit":100,"reward_energy":5,"has_reward":true},
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
	if tasks[0].TaskName != "对话 5 次" || tasks[0].RewardCredit == nil || *tasks[0].RewardCredit != 100 || tasks[0].RewardEnergy == nil || *tasks[0].RewardEnergy != 5 || !tasks[0].HasReward {
		t.Errorf("reward fields=%+v", tasks[0])
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

func TestNewGrowthMiniChatEventExactShape(t *testing.T) {
	cid := "growth-mp-conv-1"
	ev := NewGrowthMiniChatEvent(cid)
	if ev["eventCode"] != "chat_request_send" || ev["inputLength"] != 14 || ev["maxSteps"] != 500 {
		t.Fatalf("event basics=%v", ev)
	}
	if ev["agentName"] != "mp" || ev["agentType"] != "main" {
		t.Fatalf("agent fields=%v/%v", ev["agentName"], ev["agentType"])
	}
	if ev["conversationId"] != cid || ev["parentConversationId"] != cid || ev["codebuddy.session_id"] != cid {
		t.Fatalf("conversation fields=%v", ev)
	}
	rid, _ := ev["codebuddy.conversation_request_id"].(string)
	if rid == "" || ev["traceId"] != rid || ev["rootRequestId"] != rid {
		t.Fatalf("request fields=%v", ev)
	}
	if msg, _ := ev["messageId"].(string); msg != "msg-"+rid[len(rid)-8:] {
		t.Fatalf("messageId=%v rid=%s", ev["messageId"], rid)
	}
	for _, forbidden := range []string{"mode", "requestModelId", "requestModelName", "requestId", "fileUri", "presentAt", "userId"} {
		if _, ok := ev[forbidden]; ok {
			t.Errorf("mini event must not contain %q: %v", forbidden, ev[forbidden])
		}
	}
}

func TestReportGrowthMPEventsHeadersAndFingerprint(t *testing.T) {
	var got http.Header
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v2/report" {
			http.NotFound(w, r)
			return
		}
		got = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		raw = string(b)
		_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "nick"}
	if err := c.ReportGrowthMPEvents(a, []map[string]any{NewGrowthMiniChatEvent("growth-mp-conv-2")}); err != nil {
		t.Fatalf("ReportGrowthMPEvents: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(raw), "[") {
		t.Fatalf("body must be bare array: %s", raw)
	}
	for _, want := range []string{`"platform":"mini_program"`, `"ideType":"WorkBuddy_MP"`, `"userId":"u1"`, `"eventCode":"chat_request_send"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("body missing %s: %s", want, raw)
		}
	}
	wantHeaders := map[string]string{
		"Authorization":     "Bearer at",
		"X-User-Id":         "u1",
		"X-Client-Product":  "workbuddy-mp",
		"X-Client-Version":  "2.4.0",
		"X-Client-Platform": "mp-weixin",
		"X-Platform":        "wechatmp",
	}
	for k, v := range wantHeaders {
		if got.Get(k) != v {
			t.Errorf("%s=%q want %q", k, got.Get(k), v)
		}
	}
	if ua := got.Get("User-Agent"); !strings.Contains(ua, "MicroMessenger") || !strings.Contains(ua, "miniProgram/") {
		t.Errorf("mini UA=%q", ua)
	}
	for _, h := range []string{"X-CodeBuddy-Request", "X-Domain", "X-Device-Token"} {
		if got.Get(h) != "" {
			t.Errorf("must not send %s=%q", h, got.Get(h))
		}
	}
}

func TestMiniprogramTaskRequestsUseWebViewHeadersAndAcceptPath(t *testing.T) {
	var listHeader, acceptHeader http.Header
	var acceptPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			listHeader = r.Header.Clone()
			_, _ = w.Write([]byte(`{"code":0,"data":{"tasks":[]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/activity/growth/tasks/accept":
			acceptPath = r.URL.Path
			acceptHeader = r.Header.Clone()
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[]}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	a := &auth.Auth{UID: "u1", AccessToken: "at", EnterpriseID: "e1"}
	if _, err := c.ListGrowthTasks(a, GrowthPlatformMiniprogram); err != nil {
		t.Fatalf("ListGrowthTasks: %v", err)
	}
	if _, err := c.AcceptGrowthTasks(a, []string{"Sequential_Tasks_3"}, GrowthPlatformMiniprogram); err != nil {
		t.Fatalf("AcceptGrowthTasks: %v", err)
	}
	if acceptPath != "/activity/growth/tasks/accept" {
		t.Fatalf("accept path=%q", acceptPath)
	}
	for name, h := range map[string]http.Header{"list": listHeader, "accept": acceptHeader} {
		if h.Get("X-Client-Platform") != "miniprogram" {
			t.Errorf("%s X-Client-Platform=%q", name, h.Get("X-Client-Platform"))
		}
		if h.Get("Authorization") != "Bearer at" || h.Get("X-Enterprise-Id") != "e1" {
			t.Errorf("%s identity headers=%v", name, h)
		}
		if ua := h.Get("User-Agent"); !strings.Contains(ua, "MicroMessenger") || !strings.Contains(ua, "miniProgram/") {
			t.Errorf("%s mini UA=%q", name, ua)
		}
		for _, forbidden := range []string{"X-CodeBuddy-Request", "X-Domain", "X-Tenant-Id", "X-Device-Token"} {
			if h.Get(forbidden) != "" {
				t.Errorf("%s must not send %s=%q", name, forbidden, h.Get(forbidden))
			}
		}
	}
	if listHeader.Get("Origin") != "" {
		t.Errorf("GET Origin=%q want empty", listHeader.Get("Origin"))
	}
	if acceptHeader.Get("Origin") != srv.URL || acceptHeader.Get("Referer") != srv.URL+"/profile/growth-center" {
		t.Errorf("POST origin/referer=%q/%q", acceptHeader.Get("Origin"), acceptHeader.Get("Referer"))
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

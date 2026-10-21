package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/upstream"
)

type growthStub struct {
	mu sync.Mutex

	defaultTasks []upstream.GrowthTask
	miniTasks    []upstream.GrowthTask

	accepts   map[string]int
	reports   map[string]int
	claims    map[string]int
	listCalls map[string]int

	onReport func(platform string, reports int)
}

func (g *growthStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	if g.accepts == nil {
		g.accepts = map[string]int{}
	}
	if g.reports == nil {
		g.reports = map[string]int{}
	}
	if g.claims == nil {
		g.claims = map[string]int{}
	}
	if g.listCalls == nil {
		g.listCalls = map[string]int{}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		platform := r.Header.Get("X-Client-Platform")
		if platform == "" {
			platform = "default"
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/activity/growth/tasks":
			g.mu.Lock()
			g.listCalls[platform]++
			tasks := append([]upstream.GrowthTask(nil), g.defaultTasks...)
			if platform == "miniprogram" {
				tasks = append([]upstream.GrowthTask(nil), g.miniTasks...)
			}
			g.mu.Unlock()
			raw, _ := json.Marshal(map[string]any{"code": 0, "data": map[string]any{"tasks": tasks}})
			_, _ = w.Write(raw)
		case r.Method == http.MethodPost && r.URL.Path == "/v2/activity/growth/tasks/accept":
			g.mu.Lock()
			g.accepts[platform]++
			g.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			b, _ := io.ReadAll(r.Body)
			if !strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
				t.Errorf("report body must be bare array: %s", b)
			}
			// /v2/report 不带 X-Client-Platform；小程序事件靠 body.source 区分。
			reportPlatform := platform
			if strings.Contains(string(b), `"source":"mini_program"`) {
				reportPlatform = "miniprogram"
			}
			g.mu.Lock()
			g.reports[reportPlatform]++
			n := g.reports[reportPlatform]
			hook := g.onReport
			if hook != nil {
				hook(reportPlatform, n)
			}
			g.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			code := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/activity/growth/tasks/"), "/claim")
			g.mu.Lock()
			g.claims[platform+":"+code]++
			g.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":10,"energy":2}}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func growthTestScheduler(t *testing.T, stub *growthStub, auths ...*auth.Auth) *Scheduler {
	t.Helper()
	srv := stub.server(t)
	t.Cleanup(srv.Close)
	p := pool.New("")
	for _, a := range auths {
		p.Add(a)
	}
	up := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	return New(Config{Pool: p, Upstream: up})
}

func TestGrowthClaimDryRunOnlyCompletedAndSkipsGlobal(t *testing.T) {
	old := growthClaimDelay
	growthClaimDelay = 0
	t.Cleanup(func() { growthClaimDelay = old })

	stub := &growthStub{
		defaultTasks: []upstream.GrowthTask{
			{TaskCode: "done", TaskName: "已完成", AcceptStatus: "completed", Reward: json.RawMessage(`{"credit":10}`)},
			{TaskCode: "already", TaskName: "已领取", AcceptStatus: "claimed"},
			{TaskCode: "pending", TaskName: "未完成", AcceptStatus: "accepted"},
		},
		miniTasks: []upstream.GrowthTask{
			{TaskCode: "mini_done", TaskName: "小程序已完成", AcceptStatus: "completed", Reward: json.RawMessage(`{"energy":2}`)},
		},
	}
	s := growthTestScheduler(t, stub,
		&auth.Auth{UID: "cn1", AccessToken: "at", Nickname: "CN"},
		&auth.Auth{UID: "g1", AccessToken: "at", Domain: "www.workbuddy.ai", Nickname: "Global"},
	)

	scan := s.ScanGrowthTasks(context.Background())
	if len(scan.Accounts) != 2 {
		t.Fatalf("accounts=%d want cn default + cn mini, global hidden: %+v", len(scan.Accounts), scan.Accounts)
	}
	if stub.listCalls["default"] != 1 || stub.listCalls["miniprogram"] != 1 {
		t.Fatalf("global 不应请求任务列表, calls=%v", stub.listCalls)
	}

	dry := s.ClaimCompletedGrowthTasks(context.Background(), true)
	if !dry.DryRun || len(dry.Results) != 2 {
		t.Fatalf("dry run results=%+v", dry)
	}
	for _, r := range dry.Results {
		if r.Status != "skipped" {
			t.Fatalf("dry result=%+v", r)
		}
	}
	if len(stub.claims) != 0 {
		t.Fatalf("dry run 不应 claim: %v", stub.claims)
	}

	real := s.ClaimCompletedGrowthTasks(context.Background(), false)
	if len(real.Results) != 2 {
		t.Fatalf("real results=%+v", real)
	}
	for _, r := range real.Results {
		if r.Status != "claimed" {
			t.Fatalf("real result=%+v", r)
		}
	}
	if stub.claims["default:done"] != 1 || stub.claims["miniprogram:mini_done"] != 1 {
		t.Fatalf("claims=%v", stub.claims)
	}
	if stub.claims["default:pending"] != 0 || stub.claims["default:already"] != 0 {
		t.Fatalf("只应领取 completed: %v", stub.claims)
	}
}

func TestGrowthRunWhitelistAndReadbackBeforeClaim(t *testing.T) {
	oldClaim, oldAccept, oldGap, oldExec := growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap
	growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap = 0, 0, 0, 0
	t.Cleanup(func() {
		growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap = oldClaim, oldAccept, oldGap, oldExec
	})

	stub := &growthStub{
		defaultTasks: []upstream.GrowthTask{
			{TaskCode: "chat_5", TaskName: "对话十二次", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 12}},
			{TaskCode: "create_canvas", TaskName: "必须真实客户端", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
			{TaskCode: "Sequential_Tasks_1", TaskName: "小程序任务不应走普通口径", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
		},
		miniTasks: []upstream.GrowthTask{
			{TaskCode: "Sequential_Tasks_1", TaskName: "小程序任务", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
		},
	}
	stub.onReport = func(platform string, reports int) {
		// onReport 在 stub.mu 内执行，直接改状态，不能再加锁。
		if platform == "default" && reports >= 12 {
			stub.defaultTasks[0].AcceptStatus = "completed"
			stub.defaultTasks[0].Progress.Current = 12
		}
		if platform == "miniprogram" && reports >= 1 {
			stub.miniTasks[0].AcceptStatus = "completed"
			stub.miniTasks[0].Progress.Current = 1
		}
	}
	s := growthTestScheduler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "CN"})

	rep := s.RunGrowthNowReport(context.Background())
	if stub.accepts["default"] != 1 || stub.accepts["miniprogram"] != 1 {
		t.Fatalf("accept calls=%v", stub.accepts)
	}
	if stub.reports["default"] != 12 || stub.reports["miniprogram"] != 1 {
		t.Fatalf("report calls=%v", stub.reports)
	}
	if stub.claims["default:create_canvas"] != 0 {
		t.Fatalf("非白名单任务不得伪造: %v", stub.claims)
	}
	if stub.claims["default:chat_5"] != 1 || stub.claims["miniprogram:Sequential_Tasks_1"] != 1 {
		t.Fatalf("回读 completed 后才 claim: %v", stub.claims)
	}
	if len(rep.Results) != 2 {
		t.Fatalf("claim report=%+v", rep)
	}
	for _, r := range rep.Results {
		if r.Status != "claimed" {
			t.Fatalf("claim result=%+v", r)
		}
	}
}

func TestGrowthTaskSummaryGroupsAccountsAndHidesGlobal(t *testing.T) {
	res := GrowthScanResult{Accounts: []GrowthTaskStatus{
		{
			UID: "u1", Nickname: "小天才", Realm: "cn", Platform: "default", OK: true,
			Credits: 1460, CreditsExpiring: 260, CreditsUrgent: 60, SuccessCount: 88, LastSuccessTime: time.Date(2026, 10, 21, 22, 28, 0, 0, time.Local),
			Tasks: []upstream.GrowthTask{
				{TaskCode: "done", TaskName: "已完成", AcceptStatus: "completed"},
				{TaskCode: "chat_5", TaskName: "对话 5 次", AcceptStatus: "accepted", Progress: upstream.GrowthProgress{Current: 2, Target: 5}},
				{TaskCode: "canvas", TaskName: "真实客户端", AcceptStatus: "not_accepted"},
				{TaskCode: "claimed", TaskName: "已领取", AcceptStatus: "claimed"},
			},
		},
		{
			UID: "u1", Nickname: "小天才", Realm: "cn", Platform: "miniprogram", OK: true,
			Tasks: []upstream.GrowthTask{
				{TaskCode: "mini", TaskName: "小程序任务", AcceptStatus: "accepted"},
				{TaskCode: "chat_5", TaskName: "重复任务", AcceptStatus: "not_accepted"},
			},
		},
		{
			UID: "g1", Nickname: "Global", Realm: "global", Platform: "default", OK: false, Error: "global",
		},
	}}
	out := GrowthTaskSummary(res)
	accounts, ok := out["accounts"].([]growthSummaryAccount)
	if !ok || len(accounts) != 1 {
		t.Fatalf("accounts=%#v", out["accounts"])
	}
	acc := accounts[0]
	if acc.UID != "u1" || acc.Pending != 3 || acc.CanHelp != 1 || acc.Manual != 2 || acc.Claimable != 1 || acc.Done != 2 {
		t.Fatalf("account=%+v", acc)
	}
	if acc.Credits != 1460 || acc.CreditsExpiring != 260 || acc.CreditsUrgent != 60 || acc.SuccessCount != 88 || acc.LastSuccessTime.IsZero() {
		t.Fatalf("account assets=%+v", acc)
	}
	if out["pending"] != 3 || out["can_help"] != 1 || out["manual"] != 2 || out["claimable"] != 1 || out["done"] != 2 {
		t.Fatalf("summary=%v", out)
	}
	actions := map[string]string{}
	for _, task := range acc.Tasks {
		actions[task.TaskCode] = task.Action
	}
	if actions["done"] != "claim" || actions["chat_5"] != "run" || actions["mini"] != "manual" || actions["canvas"] != "manual" {
		t.Fatalf("actions=%v tasks=%+v", actions, acc.Tasks)
	}
	if _, exists := actions["claimed"]; exists {
		t.Fatalf("claimed task should be hidden: %+v", acc.Tasks)
	}
}

func TestRunGrowthAccountOnlyTarget(t *testing.T) {
	oldClaim, oldAccept, oldGap, oldExec := growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap
	growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap = 0, 0, 0, 0
	t.Cleanup(func() {
		growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap = oldClaim, oldAccept, oldGap, oldExec
	})

	stub := &growthStub{
		defaultTasks: []upstream.GrowthTask{
			{TaskCode: "chat_5", TaskName: "对话 5 次", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 5}},
		},
	}
	stub.onReport = func(platform string, reports int) {
		if platform == "default" && reports >= 5 {
			stub.defaultTasks[0].AcceptStatus = "completed"
			stub.defaultTasks[0].Progress.Current = 5
		}
	}
	s := growthTestScheduler(t, stub,
		&auth.Auth{UID: "u1", AccessToken: "at", Nickname: "目标账号"},
		&auth.Auth{UID: "u2", AccessToken: "at", Nickname: "不动账号"},
	)

	rep, err := s.RunGrowthAccountReport(context.Background(), "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reported != 1 || rep.Claimed != 1 || rep.Failed != 0 {
		t.Fatalf("report=%+v", rep)
	}
	if stub.accepts["default"] != 1 || stub.reports["default"] != 5 || stub.claims["default:chat_5"] != 1 {
		t.Fatalf("target calls accepts=%v reports=%v claims=%v", stub.accepts, stub.reports, stub.claims)
	}
	if _, err := s.RunGrowthAccountReport(context.Background(), "missing", ""); !IsGrowthAccountNotFound(err) {
		t.Fatalf("missing uid err=%v", err)
	}
}

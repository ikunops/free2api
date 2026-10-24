package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
		case r.Method == http.MethodPost && (r.URL.Path == "/v2/activity/growth/tasks/accept" || r.URL.Path == "/activity/growth/tasks/accept"):
			g.mu.Lock()
			g.accepts[platform]++
			g.mu.Unlock()
			_, _ = w.Write([]byte(`{"code":0,"data":{"results":[]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/operation-platform/market/expert/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[{"expert_id":"ex_real_1","expert_type":"agent","display_name_zh":"真实专家","profession_zh":"职业一"}]}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v2/report":
			b, _ := io.ReadAll(r.Body)
			if !strings.HasPrefix(strings.TrimSpace(string(b)), "[") {
				t.Errorf("report body must be bare array: %s", b)
			}
			// /v2/report 不带 X-Client-Platform；小程序事件靠公共指纹识别。
			reportPlatform := platform
			bodyText := string(b)
			if strings.Contains(bodyText, `"source":"mini_program"`) ||
				strings.Contains(bodyText, `"platform":"mini_program"`) ||
				strings.Contains(bodyText, `"ideType":"WorkBuddy_MP"`) {
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
	oldWebChat := growthWebChatFn
	growthWebChatFn = func(context.Context, *upstream.Client, *auth.Auth, string, string, map[string]any) (upstream.GrowthChatReply, error) {
		return upstream.GrowthChatReply{ConversationID: "stub-conv", Text: "OK"}, nil
	}
	t.Cleanup(func() { growthWebChatFn = oldWebChat })
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
	oldClaim, oldAccept, oldGap, oldExec, oldRetry := growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay
	growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = 0, 0, 0, 0, 0
	t.Cleanup(func() {
		growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = oldClaim, oldAccept, oldGap, oldExec, oldRetry
	})

	stub := &growthStub{
		defaultTasks: []upstream.GrowthTask{
			{TaskCode: "chat_5", TaskName: "对话十二次", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 12}},
			{TaskCode: "create_canvas", TaskName: "必须真实客户端", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
			{TaskCode: "Sequential_Tasks_1", TaskName: "小程序任务不应走普通口径", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
		},
		miniTasks: []upstream.GrowthTask{
			{TaskCode: "Sequential_Tasks_1", TaskName: "小程序任务", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
			{TaskCode: "Sequential_Tasks_2", TaskName: "小程序专家任务", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}},
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
			stub.miniTasks[1].AcceptStatus = "completed"
			stub.miniTasks[1].Progress.Current = 1
		}
	}
	s := growthTestScheduler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "CN"})

	rep := s.RunGrowthNowReport(context.Background())
	if stub.accepts["default"] != 2 || stub.accepts["miniprogram"] != 2 { // default: chat_5 + create_canvas; mini: Tasks_1/2
		t.Fatalf("accept calls=%v", stub.accepts)
	}
	if stub.reports["default"] != 13 || stub.reports["miniprogram"] != 2 { // 12 chat + 1 canvas batch; 1 mini chat + 1 mini expert
		t.Fatalf("report calls=%v", stub.reports)
	}
	if stub.claims["default:create_canvas"] != 0 {
		t.Fatalf("未回读 completed 的任务不得 claim: %v", stub.claims)
	}
	if stub.claims["default:chat_5"] != 1 || stub.claims["miniprogram:Sequential_Tasks_1"] != 1 || stub.claims["miniprogram:Sequential_Tasks_2"] != 1 {
		t.Fatalf("回读 completed 后才 claim: %v", stub.claims)
	}
	if len(rep.Results) != 3 {
		t.Fatalf("claim report=%+v", rep)
	}
	for _, r := range rep.Results {
		if r.Status != "claimed" {
			t.Fatalf("claim result=%+v", r)
		}
	}
}

func TestGrowthTaskSummaryGroupsAccountsAndHidesGlobal(t *testing.T) {
	rewardCredit, rewardEnergy := int64(300), int64(5)
	res := GrowthScanResult{Accounts: []GrowthTaskStatus{
		{
			UID: "u1", Nickname: "小天才", Realm: "cn", Platform: "default", OK: true,
			Tasks: []upstream.GrowthTask{
				{TaskCode: "done", TaskName: "已完成", AcceptStatus: "completed", RewardCredit: &rewardCredit, RewardEnergy: &rewardEnergy, HasReward: true},
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
	if acc.UID != "u1" || acc.Done != 2 || acc.Pending != 4 || acc.CanHelp != 1 || acc.Manual != 2 || acc.Claimable != 1 {
		t.Fatalf("account=%+v", acc)
	}
	if out["done"] != 2 || out["pending"] != 4 || out["can_help"] != 1 || out["manual"] != 2 || out["claimable"] != 1 {
		t.Fatalf("summary=%v", out)
	}
	codes, ok := out["executor_codes"].([]string)
	if !ok || !slices.Contains(codes, "chat_5") || !slices.Contains(codes, "expert_use_maidehao") {
		t.Fatalf("executor_codes=%v", out["executor_codes"])
	}
	actions := map[string]string{}
	for _, task := range acc.Tasks {
		actions[task.TaskCode] = task.Action
	}
	if actions["done"] != "claim" || actions["chat_5"] != "run" || actions["mini"] != "manual" || actions["canvas"] != "manual" {
		t.Fatalf("actions=%v tasks=%+v", actions, acc.Tasks)
	}
	var doneTask *growthSummaryTask
	for i := range acc.Tasks {
		if acc.Tasks[i].TaskCode == "done" {
			doneTask = &acc.Tasks[i]
		}
	}
	if doneTask == nil || doneTask.RewardCredit == nil || *doneTask.RewardCredit != 300 || doneTask.RewardEnergy == nil || *doneTask.RewardEnergy != 5 || !doneTask.HasReward {
		t.Fatalf("reward fields not forwarded: %+v", doneTask)
	}
	if _, exists := actions["claimed"]; exists {
		t.Fatalf("claimed task should be hidden: %+v", acc.Tasks)
	}
}

func TestGrowthExpertExecutorRequiresSuccessfulChatBeforeReport(t *testing.T) {
	oldGap := growthEventGap
	growthEventGap = 0
	t.Cleanup(func() { growthEventGap = oldGap })
	oldWebChat := growthWebChatFn
	t.Cleanup(func() { growthWebChatFn = oldWebChat })
	oldLocal := growthLocalExpertFn
	t.Cleanup(func() { growthLocalExpertFn = oldLocal })
	growthLocalExpertFn = func(string) (upstream.GrowthMarketExpert, bool) {
		return upstream.GrowthMarketExpert{ExpertID: "ex_not_used", DisplayNameZH: "本地专家", ExpertType: "agent"}, true
	}

	reportCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/operation-platform/market/expert/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[]}}`))
		case r.URL.Path == "/v2/report":
			reportCalls++
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	growthWebChatFn = func(context.Context, *upstream.Client, *auth.Auth, string, string, map[string]any) (upstream.GrowthChatReply, error) {
		return upstream.GrowthChatReply{}, fmt.Errorf("upstream chat down")
	}
	ex := growthExpertChatExecutor{code: "Expert_lighthouse", target: 1, aliases: []string{"腾讯轻量云专家"}}
	if err := ex.Run(context.Background(), c, &auth.Auth{UID: "u1"}, upstream.GrowthTask{TaskCode: "Expert_lighthouse", TaskName: "腾讯轻量云专家"}); err == nil {
		t.Fatal("chat failure must return error")
	}
	if reportCalls != 0 {
		t.Fatalf("chat failure must not report task events, calls=%d", reportCalls)
	}

	growthWebChatFn = func(context.Context, *upstream.Client, *auth.Auth, string, string, map[string]any) (upstream.GrowthChatReply, error) {
		return upstream.GrowthChatReply{ConversationID: "conv-real", Text: "done"}, nil
	}
	if err := ex.Run(context.Background(), c, &auth.Auth{UID: "u1"}, upstream.GrowthTask{TaskCode: "Expert_lighthouse", TaskName: "腾讯轻量云专家"}); err != nil {
		t.Fatal(err)
	}
	if reportCalls != 1 {
		t.Fatalf("successful chat must report once, calls=%d", reportCalls)
	}
}

func TestGrowthExpertExecutorUsesOfficialWebModelInEvents(t *testing.T) {
	oldGap := growthEventGap
	growthEventGap = 0
	t.Cleanup(func() { growthEventGap = oldGap })
	oldWebChat := growthWebChatFn
	t.Cleanup(func() { growthWebChatFn = oldWebChat })
	oldLocal := growthLocalExpertFn
	t.Cleanup(func() { growthLocalExpertFn = oldLocal })
	growthLocalExpertFn = func(string) (upstream.GrowthMarketExpert, bool) {
		return upstream.GrowthMarketExpert{ExpertID: "ex_real", DisplayNameZH: "真实专家", ExpertType: "agent"}, true
	}

	var chatModel string
	growthWebChatFn = func(_ context.Context, _ *upstream.Client, _ *auth.Auth, _ string, model string, _ map[string]any) (upstream.GrowthChatReply, error) {
		chatModel = model
		return upstream.GrowthChatReply{ConversationID: "conv-real", Text: "done"}, nil
	}
	reported := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/operation-platform/market/expert/list":
			_, _ = w.Write([]byte(`{"code":0,"data":{"experts":[]}}`))
		case "/v2/report":
			raw, _ := io.ReadAll(r.Body)
			reported = string(raw)
			_, _ = w.Write([]byte(`{"code":0,"data":{}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := &upstream.Client{HTTP: srv.Client(), ChatBaseCN: srv.URL, BillingBaseCN: srv.URL}
	ex := growthExpertChatExecutor{code: "expert_use_douyinyin", target: 1, aliases: []string{"抖音策略师"}}
	if err := ex.Run(context.Background(), c, &auth.Auth{UID: "u1"}, upstream.GrowthTask{TaskCode: "expert_use_douyinyin", TaskName: "抖音策略师"}); err != nil {
		t.Fatal(err)
	}
	if chatModel != growthExpertWebModelID {
		t.Fatalf("expert chat model=%q want %q", chatModel, growthExpertWebModelID)
	}
	if !strings.Contains(reported, `"requestModelId":"fast-model"`) || !strings.Contains(reported, `"requestModelName":"fast-model"`) {
		t.Fatalf("expert events must use the real web model, got %s", reported)
	}
}

// TestRunGrowthAccountReportsExecutorFailure 执行器失败时接口必须带回错误明细，
// 不能只写日志再返回 reported=0 让前端显示成功。
func TestRunGrowthAccountReportsExecutorFailure(t *testing.T) {
	oldClaim, oldAccept, oldGap, oldExec, oldRetry := growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay
	growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = 0, 0, 0, 0, 0
	t.Cleanup(func() {
		growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = oldClaim, oldAccept, oldGap, oldExec, oldRetry
	})
	stub := &growthStub{defaultTasks: []upstream.GrowthTask{
		{TaskCode: "chat_5", TaskName: "对话任务", AcceptStatus: "accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 5}},
	}}
	s := growthTestScheduler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "账号"})
	growthWebChatFn = func(context.Context, *upstream.Client, *auth.Auth, string, string, map[string]any) (upstream.GrowthChatReply, error) {
		return upstream.GrowthChatReply{}, fmt.Errorf("upstream chat rejected")
	}
	rep, err := s.RunGrowthAccountReport(context.Background(), "u1", "chat_5")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reported != 0 || len(rep.Errors) == 0 || !strings.Contains(strings.Join(rep.Errors, " "), "upstream chat rejected") {
		t.Fatalf("executor failure must be visible in report: %+v", rep)
	}
}
func TestRunGrowthAccountOnlyTarget(t *testing.T) {
	oldClaim, oldAccept, oldGap, oldExec, oldRetry := growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay
	growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = 0, 0, 0, 0, 0
	t.Cleanup(func() {
		growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = oldClaim, oldAccept, oldGap, oldExec, oldRetry
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
	if rep.Reported != 1 || rep.Claimed != 1 || rep.Failed != 0 || rep.Credit != 10 || rep.Energy != 2 {
		t.Fatalf("report=%+v", rep)
	}
	if stub.accepts["default"] != 1 || stub.reports["default"] != 5 || stub.claims["default:chat_5"] != 1 {
		t.Fatalf("target calls accepts=%v reports=%v claims=%v", stub.accepts, stub.reports, stub.claims)
	}
	if _, err := s.RunGrowthAccountReport(context.Background(), "missing", ""); !IsGrowthAccountNotFound(err) {
		t.Fatalf("missing uid err=%v", err)
	}
}

// TestGrowthSequentialTasks3UsesMiniEventPacing 回归：Sequential_Tasks_3 不能
// 复用桌面对话的 1.05 秒间隔，也不能走真实 Web 对话。它必须按小程序口径补
// 差额事件，并遵守 45 秒级防作弊节奏；测试把间隔和抖动都置 0，只验证事件形状/次数。
func TestGrowthSequentialTasks3UsesMiniEventPacing(t *testing.T) {
	oldGap, oldMiniGap, oldJitter := growthEventGap, growthMiniChatGap, growthMiniChatJitterFn
	growthEventGap, growthMiniChatGap = 0, 0
	growthMiniChatJitterFn = func(*auth.Auth, int) time.Duration { return 0 }
	t.Cleanup(func() { growthEventGap, growthMiniChatGap, growthMiniChatJitterFn = oldGap, oldMiniGap, oldJitter })

	stub := &growthStub{
		miniTasks: []upstream.GrowthTask{
			{TaskCode: "Sequential_Tasks_3", TaskName: "完成 5 次对话", AcceptStatus: "accepted", Progress: upstream.GrowthProgress{Current: 2, Target: 5}},
		},
	}
	stub.onReport = func(platform string, reports int) {
		if platform == "miniprogram" && reports >= 3 {
			stub.miniTasks[0].AcceptStatus = "completed"
			stub.miniTasks[0].Progress.Current = 5
		}
	}
	s := growthTestScheduler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "CN"})

	rep, err := s.RunGrowthAccountReport(context.Background(), "u1", "Sequential_Tasks_3")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reported != 1 || rep.Claimed != 1 || rep.Failed != 0 {
		t.Fatalf("report=%+v", rep)
	}
	if stub.reports["miniprogram"] != 3 || stub.reports["default"] != 0 {
		t.Fatalf("mini reports=%v want 3, default must stay 0", stub.reports)
	}
	if stub.claims["miniprogram:Sequential_Tasks_3"] != 1 {
		t.Fatalf("claim=%v", stub.claims)
	}
}

// TestGrowthTaskSummaryRespectsExecutorPlatform 面板的 action 必须和执行器
// 的平台过滤一致。否则会出现“面板显示可帮做，点击后执行器被跳过”，用户
// 看到按钮却什么也没发生。
// TestGrowthTaskSummaryKeepsScheduledTaskManualUntilRealProof Sequential_Tasks_4
// 只有一组自动化事件并不能让上游落完成状态（实测 reported=1 后仍 not_accepted）。
// 在拿到可复现的真实完成路径前，必须保持 manual，不能给用户假按钮。
func TestGrowthTaskSummaryRunsScheduledTaskAfterRealProof(t *testing.T) {
	res := GrowthScanResult{Accounts: []GrowthTaskStatus{
		{
			UID: "u1", Nickname: "u1", Realm: "cn", Platform: "miniprogram", OK: true,
			Tasks: []upstream.GrowthTask{
				{TaskCode: "Sequential_Tasks_4", TaskName: "创建 1 个定时任务", AcceptStatus: "not_accepted"},
			},
		},
	}}
	out := GrowthTaskSummary(res)
	accounts := out["accounts"].([]growthSummaryAccount)
	if len(accounts) != 1 || len(accounts[0].Tasks) != 1 {
		t.Fatalf("accounts=%+v", accounts)
	}
	if got := accounts[0].Tasks[0].Action; got != "run" {
		t.Fatalf("Sequential_Tasks_4 action=%q want run after real completion proof", got)
	}
}

// TestGrowthSequentialAutomationUsesMPPlatformButDesktopProof 锁定这条任务的
// 混合口径：列表/accept 属于小程序，完成判据却是桌面 automated_task_create_suc。
// 2026-10-25 单账号实测 accepted 0/1 → 上报 → completed 1/1。
func TestGrowthSequentialAutomationUsesMPPlatformButDesktopProof(t *testing.T) {
	old := growthClaimDelay
	growthClaimDelay = 0
	t.Cleanup(func() { growthClaimDelay = old })
	stub := &growthStub{miniTasks: []upstream.GrowthTask{{
		TaskCode: "Sequential_Tasks_4", TaskName: "创建 1 个定时任务",
		AcceptStatus: "not_accepted",
	}}}
	s := growthTestScheduler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "CN"})
	rep, err := s.RunGrowthAccountReport(context.Background(), "u1", "Sequential_Tasks_4")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reported != 1 {
		t.Fatalf("report=%+v", rep)
	}
	if stub.reports["default"] != 1 || stub.reports["miniprogram"] != 0 {
		t.Fatalf("scheduled task report must use desktop/default report path, got %v", stub.reports)
	}
	if stub.accepts["miniprogram"] != 1 {
		t.Fatalf("scheduled task accept must use miniprogram identity, got %v", stub.accepts)
	}
}

// TestGrowthTaskSummaryMatchesExpertByDisplayNameWhenCodeChanges 上游偶尔会改
// task_code，但任务展示名仍保留具体专家名；面板不能因此把真实专家任务降级成
// 手动任务。匹配只认注册的具体别名，且小程序平台仍不能误放行桌面执行器。
func TestGrowthTaskSummaryMatchesExpertByDisplayNameWhenCodeChanges(t *testing.T) {
	res := GrowthScanResult{Accounts: []GrowthTaskStatus{
		{UID: "desktop", Nickname: "desktop", Realm: "cn", Platform: "default", OK: true, Tasks: []upstream.GrowthTask{
			{TaskCode: "some_new_valorant_code", TaskName: "无畏契约拼豆专家", AcceptStatus: "not_accepted"},
			{TaskCode: "some_new_ecommerce_code", TaskName: "卖得好", AcceptStatus: "not_accepted"},
		}},
		{UID: "mini", Nickname: "mini", Realm: "cn", Platform: "miniprogram", OK: true, Tasks: []upstream.GrowthTask{
			{TaskCode: "some_new_valorant_code", TaskName: "无畏契约拼豆专家", AcceptStatus: "not_accepted"},
		}},
	}}
	out := GrowthTaskSummary(res)
	accounts := out["accounts"].([]growthSummaryAccount)
	byUID := map[string]growthSummaryAccount{}
	for _, acc := range accounts {
		byUID[acc.UID] = acc
	}
	action := func(uid, code string) string {
		for _, task := range byUID[uid].Tasks {
			if task.TaskCode == code {
				return task.Action
			}
		}
		return ""
	}
	if got := action("desktop", "some_new_valorant_code"); got != "run" {
		t.Fatalf("valorant alias action=%q want run: %+v", got, byUID["desktop"].Tasks)
	}
	if got := action("desktop", "some_new_ecommerce_code"); got != "run" {
		t.Fatalf("maidehao alias action=%q want run: %+v", got, byUID["desktop"].Tasks)
	}
	if got := action("mini", "some_new_valorant_code"); got != "manual" {
		t.Fatalf("mini expert alias action=%q want manual: %+v", got, byUID["mini"].Tasks)
	}
}

// TestRunGrowthExecutorMatchesExpertByDisplayNameWhenCodeChanges 执行路径也必须
// 用同一套别名匹配，不能只在面板显示“可帮做”、点击后却找不到执行器。
func TestRunGrowthExecutorMatchesExpertByDisplayNameWhenCodeChanges(t *testing.T) {
	oldClaim, oldAccept, oldGap, oldExec, oldRetry := growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay
	growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = 0, 0, 0, 0, 0
	t.Cleanup(func() {
		growthClaimDelay, growthAcceptDelay, growthEventGap, growthExecutorGap, growthClaimRetryDelay = oldClaim, oldAccept, oldGap, oldExec, oldRetry
	})
	stub := &growthStub{defaultTasks: []upstream.GrowthTask{{TaskCode: "renamed_valorant_code", TaskName: "无畏契约拼豆专家", AcceptStatus: "not_accepted", Progress: upstream.GrowthProgress{Current: 0, Target: 1}}}}
	s := growthTestScheduler(t, stub, &auth.Auth{UID: "u1", AccessToken: "at", Nickname: "账号"})
	oldWeb := growthWebChatFn
	oldLocal := growthLocalExpertFn
	t.Cleanup(func() { growthWebChatFn = oldWeb; growthLocalExpertFn = oldLocal })
	growthWebChatFn = func(context.Context, *upstream.Client, *auth.Auth, string, string, map[string]any) (upstream.GrowthChatReply, error) {
		return upstream.GrowthChatReply{ConversationID: "conv-real", Text: "done"}, nil
	}
	growthLocalExpertFn = func(string) (upstream.GrowthMarketExpert, bool) {
		return upstream.GrowthMarketExpert{ExpertID: "ex_real", DisplayNameZH: "无畏契约拼豆专家", ExpertType: "agent"}, true
	}
	rep, err := s.RunGrowthAccountReport(context.Background(), "u1", "renamed_valorant_code")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Reported != 1 {
		t.Fatalf("renamed expert task must still execute by display name: %+v", rep)
	}
}

func TestGrowthTaskSummaryRespectsExecutorPlatform(t *testing.T) {
	res := GrowthScanResult{Accounts: []GrowthTaskStatus{
		{
			UID: "mini", Nickname: "mini", Realm: "cn", Platform: "miniprogram", OK: true,
			Tasks: []upstream.GrowthTask{
				{TaskCode: "Sequential_Tasks_3", TaskName: "小程序对话", AcceptStatus: "accepted"},
				{TaskCode: "Expert_lighthouse", TaskName: "桌面专家", AcceptStatus: "accepted"},
			},
		},
		{
			UID: "desktop", Nickname: "desktop", Realm: "cn", Platform: "default", OK: true,
			Tasks: []upstream.GrowthTask{
				{TaskCode: "Expert_lighthouse", TaskName: "桌面专家", AcceptStatus: "accepted"},
				{TaskCode: "Sequential_Tasks_3", TaskName: "小程序对话", AcceptStatus: "accepted"},
			},
		},
	}}
	out := GrowthTaskSummary(res)
	accounts, ok := out["accounts"].([]growthSummaryAccount)
	if !ok || len(accounts) != 2 {
		t.Fatalf("accounts=%#v", out["accounts"])
	}
	byUID := map[string]growthSummaryAccount{}
	for _, acc := range accounts {
		byUID[acc.UID] = acc
	}
	action := func(uid, code string) string {
		for _, task := range byUID[uid].Tasks {
			if task.TaskCode == code {
				return task.Action
			}
		}
		return ""
	}
	if got := action("mini", "Sequential_Tasks_3"); got != "run" {
		t.Fatalf("mini Sequential_Tasks_3 action=%q want run: %+v", got, byUID["mini"].Tasks)
	}
	if got := action("desktop", "Expert_lighthouse"); got != "run" {
		t.Fatalf("desktop Expert_lighthouse action=%q want run: %+v", got, byUID["desktop"].Tasks)
	}
	if got := action("mini", "Expert_lighthouse"); got != "manual" {
		t.Fatalf("mini Expert_lighthouse action=%q want manual: %+v", got, byUID["mini"].Tasks)
	}
	if got := action("desktop", "Sequential_Tasks_3"); got != "manual" {
		t.Fatalf("desktop Sequential_Tasks_3 action=%q want manual: %+v", got, byUID["desktop"].Tasks)
	}
}

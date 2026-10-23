// growth_tasks.go growth 域通用成长任务：列表 / accept / 事件上报 / claim。
//
// 这里只做协议层，不决定“哪些任务该自动做”。执行策略在 scheduler/growth_executors.go：
// 默认只扫描 + 领取已完成任务；可点亮任务按白名单逐类放行，付费/真人/活动任务不在本层伪造。
package upstream

import (
	"bytes"
	"crypto/md5"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/session"
)

const (
	growthTasksPath       = "/v2/activity/growth/tasks"
	growthAcceptPath      = "/v2/activity/growth/tasks/accept"
	growthTaskClaimFormat = "/activity/growth/tasks/%s/claim"
	growthWebBaseCN       = "https://www.workbuddy.cn"
)

// GrowthTask 是上游成长任务列表里我们关心的字段。未知字段保留在 Raw。
type GrowthTask struct {
	TaskCode     string          `json:"task_code"`
	TaskName     string          `json:"task_name"`
	Title        string          `json:"title,omitempty"`
	TaskDesc     string          `json:"task_desc"`
	AcceptStatus string          `json:"accept_status"`
	Progress     GrowthProgress  `json:"progress"`
	Reward       json.RawMessage `json:"reward"`
	RewardCredit *int64          `json:"reward_credit,omitempty"`
	RewardEnergy *int64          `json:"reward_energy,omitempty"`
	HasReward    bool            `json:"has_reward,omitempty"`
	Raw          json.RawMessage `json:"-"`
}

// GrowthProgress 任务进度。
type GrowthProgress struct {
	Current int `json:"current"`
	Target  int `json:"target"`
}

// Done 报告任务是否已达到领取条件（completed/claimed 或进度达标）。
func (t GrowthTask) Done() bool {
	return t.AcceptStatus == "completed" || t.AcceptStatus == "claimed" ||
		(t.Progress.Target > 0 && t.Progress.Current >= t.Progress.Target)
}

// GrowthTaskPlatform 选择普通成长任务或小程序口径。小程序不是“多一个头”这么简单：
// 列表/accept/claim 三处必须使用同一身份，否则会出现查不到或领不了。
type GrowthTaskPlatform string

const (
	GrowthPlatformDefault     GrowthTaskPlatform = ""
	GrowthPlatformMiniprogram GrowthTaskPlatform = "miniprogram"
)

// ListGrowthTasks 拉取任务列表。平台为空走普通客户端；miniprogram 走小程序口径。
func (c *Client) ListGrowthTasks(a *auth.Auth, platform GrowthTaskPlatform) ([]GrowthTask, error) {
	data, err := c.growthJSONWithPlatform(a, http.MethodGet, growthTasksPath, nil, platform)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]GrowthTask, 0, len(resp.Tasks))
	for _, raw := range resp.Tasks {
		var t GrowthTask
		if err := json.Unmarshal(raw, &t); err != nil {
			continue
		}
		t.Raw = raw
		if t.TaskName == "" {
			t.TaskName = strings.TrimSpace(t.Title)
		}
		if t.TaskCode != "" {
			out = append(out, t)
		}
	}
	return out, nil
}

// AcceptGrowthTasks 批量报名。返回上游 results；调用方必须回读 accept_status，不能只看 HTTP 200。
func (c *Client) AcceptGrowthTasks(a *auth.Auth, codes []string, platform GrowthTaskPlatform) (json.RawMessage, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	body := map[string]any{"task_codes": codes}
	return c.growthJSONWithPlatform(a, http.MethodPost, growthAcceptPath, body, platform)
}

// ClaimGrowthTask 领取单个任务。路径含 task_code、无 body；小程序口径带同一身份头。
// 上游对已领任务可能返回业务错误，调用方用 IsGrowthAlreadyClaimed 识别为正常态。
func (c *Client) ClaimGrowthTask(a *auth.Auth, code string, platform GrowthTaskPlatform) (GrowthClaimOutcome, error) {
	if strings.TrimSpace(code) == "" {
		return GrowthClaimOutcome{}, fmt.Errorf("empty task_code")
	}
	path := fmt.Sprintf(growthTaskClaimFormat, url.PathEscape(code))
	data, err := c.growthJSONWithPlatform(a, http.MethodPost, path, nil, platform)
	if err != nil {
		return GrowthClaimOutcome{}, err
	}
	var out GrowthClaimOutcome
	if len(data) > 0 {
		_ = json.Unmarshal(data, &out)
	}
	return out, nil
}

// GrowthClaimOutcome 是领取接口 data 里对账需要的字段。already_claimed 可能出现在
// data 中（HTTP 200），也可能作为业务错误返回；两种都视为正常幂等态。
type GrowthClaimOutcome struct {
	AlreadyClaimed bool  `json:"already_claimed"`
	Credit         int64 `json:"credit"`
	Energy         int64 `json:"energy"`
}

// IsGrowthAlreadyClaimed 判断领取接口的幂等正常态。不同版本文案可能是
// already_claimed / already claimed / 已领取 / 已领。
func IsGrowthAlreadyClaimed(err error) bool {
	if err == nil {
		return false
	}
	ue, ok := err.(*Error)
	if !ok {
		return false
	}
	msg := strings.ToLower(ue.Msg)
	return strings.Contains(msg, "already_claimed") || strings.Contains(msg, "already claimed") ||
		strings.Contains(msg, "已领取") || strings.Contains(msg, "已领")
}

// growthJSONWithPlatform 在 growthJSON 基础上注入任务平台身份。
func (c *Client) growthJSONWithPlatform(a *auth.Auth, method, path string, body any, platform GrowthTaskPlatform) (json.RawMessage, error) {
	if platform == GrowthPlatformDefault {
		return c.growthJSON(a, method, path, body)
	}
	var rdr *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	var req *http.Request
	var err error
	if rdr == nil {
		req, err = http.NewRequest(method, c.chatBase(a)+path, nil)
	} else {
		req, err = http.NewRequest(method, c.chatBase(a)+path, rdr)
	}
	if err != nil {
		return nil, err
	}
	c.BillingHeaders(req, a)
	req.Header.Set("X-Client-Platform", string(platform))
	return c.doJSON(req)
}

// ReportGrowthEvents 发送裸事件数组。growth 任务服务端只认裸数组，不接受信封。
func (c *Client) ReportGrowthEvents(a *auth.Auth, events []map[string]any) error {
	return c.reportGrowthEvents(a, c.billingBase(a), events)
}

// ReportGrowthDesktopEvents 发送桌面指纹事件。桌面任务（Buddy/RichMeow）使用聊天域
// /v2/report，事件里带稳定 machineId/sessionId；不伪造成本型业务，只补任务判据。
func (c *Client) ReportGrowthDesktopEvents(a *auth.Auth, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	fp := growthDesktopFingerprint(a)
	out := make([]map[string]any, 0, len(events))
	for _, src := range events {
		ev := make(map[string]any, len(src)+len(fp))
		for k, v := range fp {
			ev[k] = v
		}
		for k, v := range src {
			ev[k] = v
		}
		out = append(out, ev)
	}
	return c.reportGrowthEventsWithHeaders(a, c.chatBase(a), out, func(req *http.Request) {
		req.Header.Set("X-Domain", c.chatBase(a))
		req.Header.Set("X-Product", "SaaS")
		req.Header.Set("X-Request-ID", growthStableHex(a, "req")+fmt.Sprintf("%06d", time.Now().UnixNano()%1000000))
	})
}

// ReportGrowthWebEvents 发送 Web 行为事件（Library_read）。浏览器侧上报域是
// www.workbuddy.cn；测试/自定义 base 时沿用 ChatBaseCN，避免硬编码阻断回放。
func (c *Client) ReportGrowthWebEvents(a *auth.Auth, events []map[string]any) error {
	base := growthWebBaseCN
	if c.ChatBaseCN != "" && c.ChatBaseCN != "https://copilot.tencent.com" {
		base = strings.TrimRight(c.ChatBaseCN, "/")
	}
	return c.reportGrowthEventsWithHeaders(a, base, events, func(req *http.Request) {
		req.Header.Set("x-client-platform", "web")
		req.Header.Set("X-Domain", base)
		req.Header.Set("Origin", base)
		req.Header.Set("Referer", base+"/library")
	})
}

func (c *Client) reportGrowthEvents(a *auth.Auth, base string, events []map[string]any) error {
	return c.reportGrowthEventsWithHeaders(a, base, events, nil)
}

func (c *Client) reportGrowthEventsWithHeaders(a *auth.Auth, base string, events []map[string]any, tweak func(*http.Request)) error {
	if len(events) == 0 {
		return nil
	}
	raw, err := json.Marshal(events)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, strings.TrimRight(base, "/")+reportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	c.BillingHeaders(req, a)
	if tweak != nil {
		tweak(req)
	}
	_, err = c.doJSON(req)
	return err
}

func growthDesktopFingerprint(a *auth.Auth) map[string]any {
	now := time.Now().UnixMilli()
	return map[string]any{
		"timezone": "Asia/Shanghai", "reportDelay": 2000,
		"userId": a.UID, "username": a.Nickname, "userNickname": a.Nickname,
		"product": "SaaS", "releaseDate": 1789036585355,
		"commit":  "5f9692923c93033111c51ad7b003eb80204a9b75",
		"ideName": "WorkBuddy", "ideType": "WorkBuddy", "ideVersion": "5.5.6",
		"machineId": growthStableHex(a, "machine"), "sessionId": growthStableHex(a, "session"),
		"extName": "workbuddy-desktop", "extVersion": "5.5.6",
		"os": "win32", "arch": "x64", "osVersion": "10.0.26220",
		"cpuCores": 20, "memorySize": 24, "timestamp": now, "presentAt": now,
	}
}

func growthStableHex(a *auth.Auth, salt string) string {
	sum := md5.Sum([]byte(salt + ":" + a.UID))
	return fmt.Sprintf("%x", sum)[:24]
}

// NewGrowthConversationID 生成任务事件用的会话/请求 id。任务事件不进入用户对话统计，
// 但字段必须像客户端，故使用统一 message id 生成器。
func NewGrowthConversationID(prefix string) string {
	if prefix == "" {
		prefix = "growth"
	}
	return prefix + "-" + session.NewMessageID()
}

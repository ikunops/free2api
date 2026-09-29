// activity 一次性触发器：只跑一次 RunActivityNow（5 连发 + 领猫联动），部署后验证活跃上报闭环用，不常驻。
//
// 用法（部署后手动触发）：
//
//	# 容器内：先 cp 进去再 exec
//	docker cp activity-run free2api:/tmp/activity-run
//	docker exec -w /app free2api /tmp/activity-run
//
//	# 本地：在项目根目录（需 config.json + auths/ + data/）直接 run
//	go run ./cmd/activity
//
//	# 单号模式（实测/消耗类先 1 个号）：uid 前缀过滤，只跑命中账号
//	go run ./cmd/activity 00e26541
//
// 读取工作目录的 config.json（auth_dir / state_file / schedule / upstream.timeout_seconds），
// 加载 auths 后构建 pool + upstream，调用 scheduler.RunActivityNow 立即执行一次。
//
// schedule 段复用 internal/config 的同一份 Schedule 结构 + 默认值（issue #49）：
// 与 cmd/server 共用，缺省 activity_report_count=5 不再各自复制漂移。
package main

import (
	"encoding/json"
	"log"
	"os"
	"strings"
	"time"

	"free2api/internal/auth"
	"free2api/internal/config"
	"free2api/internal/pool"
	"free2api/internal/scheduler"
	"free2api/internal/upstream"
)

// cfgFile 只取本工具需要的字段；Schedule 段直接用 internal/config.Schedule
// （与 cmd/server 同源），其余段保持精简内联。
type cfgFile struct {
	AuthDir   string            `json:"auth_dir"`
	StateFile string            `json:"state_file"`
	Schedule  config.Schedule   `json:"schedule"`
	Upstream  struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	} `json:"upstream"`
}

func main() {
	// 可选位置参数：uid 前缀过滤（如 `./activity-run 00e26541`），只跑单号——
	// 实测/消耗类验证先 1 个号的入口。缺省跑全池（与旧行为一致）。
	var uidPrefix string
	if len(os.Args) > 1 {
		uidPrefix = os.Args[1]
	}
	raw, err := os.ReadFile("config.json")
	if err != nil {
		log.Fatalf("read config: %v", err)
	}
	// 先置默认值再 Unmarshal：键缺席（或为 null）时字段原样保留默认，
	// 与 cmd/server 的 Load 同路——缺省 activity_report_count=5 而非 Go 零值 0。
	c := cfgFile{Schedule: config.DefaultSchedule()}
	if err := json.Unmarshal(raw, &c); err != nil {
		log.Fatalf("parse config: %v", err)
	}
	if err := c.Schedule.Normalize(); err != nil {
		log.Fatalf("normalize schedule: %v", err)
	}
	if c.AuthDir == "" {
		c.AuthDir = "./auths"
	}
	if c.StateFile == "" {
		c.StateFile = "data/state.json"
	}
	log.Printf("activity count=%d", c.Schedule.ActivityReportCount)

	auths, err := auth.LoadDir(c.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	p := pool.New(c.StateFile)
	if uidPrefix != "" {
		// 单号模式：不做 SyncToDir（它会按筛选结果剔除池内其余账号并落盘，
		// 污染共享 state.json）。只 Add 命中账号——池内其余账号不受影响，
		// state.json 也不会被本工具改写。
		var hits int
		for _, a := range auths {
			if strings.HasPrefix(a.UID, uidPrefix) {
				p.Add(a)
				hits++
			}
		}
		if hits == 0 {
			log.Fatalf("no account matches uid prefix %q", uidPrefix)
		}
		log.Printf("single-account mode: %d account(s) match %q", hits, uidPrefix)
	} else {
		log.Printf("loaded %d account(s)", len(auths))
		p.SyncToDir(auths)
	}

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            newUpstream(&c),
		CheckinHours:        c.Schedule.CheckinHours,
		TravelHours:         c.Schedule.TravelHours,
		ActivityHours:       c.Schedule.ActivityHours,
		KeepaliveHours:       c.Schedule.KeepaliveHours,
		ActivityReportCount: c.Schedule.ActivityReportCount,
	})
	sch.RunActivityNow()
	log.Printf("activity run complete")
}

// newUpstream 构造上游客户端并显式接线 global realm 路由。
// activity 是 CN 任务中心的上报工具（global 账号已被 scheduler 的 IsGlobal 门控跳过），
// 但 GlobalEnabled=false 会把工具自造的 global 账号请求路由到 CN base（codebuddy.cn）而必然失败。
// 与 cmd/signin、cmd/credit、cmd/trial 同风格：显式接线，避免 Producer 误读成「未适配」。
func newUpstream(c *cfgFile) *upstream.Client {
	up := upstream.New()
	up.GlobalEnabled = true
	if c.Upstream.TimeoutSeconds > 0 {
		up.HTTP.Timeout = time.Duration(c.Upstream.TimeoutSeconds) * time.Second
	}
	return up
}

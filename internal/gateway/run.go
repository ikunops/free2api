// run.go 装配并运行 free2api 网关（号池 + 调度器 + HTTP 服务）。
//
// 为什么从 cmd/server/main.go 抽出来：桌面程序（cmd/desktop）要在同一台机器上
// 「启动 / 停止」网关。装配逻辑若留在 main() 的一大段里，桌面程序只能另起进程去调
// exe；抽成 Start/Stop 之后两个入口共用同一份装配，行为不会漂移。
package gateway

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/redisstore"
	"free2api/internal/scheduler"
	"free2api/internal/server"
	"free2api/internal/session"
	"free2api/internal/source"
	"free2api/internal/upstream"
)

// Options 启动参数。
type Options struct {
	// ConfigPath config.json 路径；空 = 走内置默认 + 环境变量。
	ConfigPath string
	// Ctx 上级生命周期。为空用 context.Background()；取消即触发优雅停机。
	Ctx context.Context
	// Ready 在开始监听**之前**回调（拿不到"绑定成功"信号，监听失败见 Wait）。
	// 桌面程序用不上（它直接探 /status），留给需要提前记录地址的调用方。
	Ready func(addr string)
}

// cleanupList 收集停机清理动作。Stop 可能来自别的 goroutine，故加锁。
type cleanupList struct {
	mu  sync.Mutex
	fns []func()
}

func (c *cleanupList) add(fn func()) {
	c.mu.Lock()
	c.fns = append(c.fns, fn)
	c.mu.Unlock()
}

// run 逆序执行全部清理动作（与 defer 的语义一致），只执行一次。
func (c *cleanupList) run() {
	c.mu.Lock()
	fns := c.fns
	c.fns = nil
	c.mu.Unlock()
	for i := len(fns) - 1; i >= 0; i-- {
		fns[i]()
	}
}

// Instance 一个已启动的网关实例。Stop 之后不可复用。
type Instance struct {
	// Addr 监听地址（config.json 的 listen）。
	Addr string

	cleanups *cleanupList
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
	err      error
}

// Start 装配并启动网关。立即返回；监听在后台 goroutine 里进行。
// 装配失败时已申请的资源会回滚，不会泄漏。
func Start(opts Options) (*Instance, error) {
	cleanups := &cleanupList{}
	ok := false
	defer func() {
		if !ok {
			cleanups.run()
		}
	}()

	// 严格 Load：配置文件写了但读不动/解析失败/归一化失败一律 fail-fast。
	// 「第一次启动还没有 config.json」由调用方决定怎么办（桌面程序会生成一份最小配置），
	// 网关本身不替用户猜——带着默认端口和空 auths 静默跑起来比报错更难排查。
	cfg, err := Load(opts.ConfigPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		return nil, fmt.Errorf("load auths: %w", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// model.json 本地缓存接线（context_length 四级查找链第 3 级）：数据目录与
	// state.json 同风格（Docker volume 持久化路径 ./data）。首次缺失/损坏自动回落
	// 仓库种子 embed；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(ModelJSONPath(cfg.StateFile))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	cleanups.add(p.Close) // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// 来源台账接线：按 uid 告诉池子「这个号属于哪个 AI 客户端（workbuddy/zcode/…）」。
	// 台账落在 auths/.origins.json（跟目录一起整体迁移），/status 按它给每个账号标
	// producer，选号按它做生产者过滤（见 pool.servableProducer）。
	//
	// 只 LoadRegistry 一次、长期持有：导入路径是另起实例写盘，Registry.Get 内部按
	// 落盘形态变化重读（refreshLocked，1s 节流），所以这里不必在导入后重新注入。
	{
		reg := source.LoadRegistry(cfg.AuthDir)
		p.SetProducerOf(func(uid string) string {
			if o, ok := reg.Get(uid); ok {
				return o.Producer
			}
			return ""
		})
	}

	// auths 目录热加载：新增凭证文件自动进池，免去「加完账号手动重启网关」。
	// 启动时的 SyncToDir 已建立基线，监听只在后续目录内容变化时触发（见 pool/watch.go）。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	cleanups.add(stopWatch)

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 连败降权（issue #114）：ErrClient/传输层连败 N 次临时出池。
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域在途分档（WAF 403 修复 P1-1，默认 2）
	p.SetSoftRateMax(cfg.SoftRateMaxDur)               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）

	// 输出侧配置（模型前缀 / 费率提示）：运行期可改，存 data/output.json。
	// 必须在 session / handler 之前建好——粘性路由的模型名解析要用它的前缀。
	outputStore := server.NewOutputStore(OutputJSONPath(cfg.StateFile))

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: RealmAwareAvailableForModel(p, outputStore),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		cleanups.add(sessRouter.StopGC)
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	sch := scheduler.New(scheduler.Config{
		Pool:                 p,
		Upstream:             up,
		CheckinHours:         cfg.Schedule.CheckinHours,
		TravelHours:          cfg.Schedule.TravelHours,
		ActivityHours:        cfg.Schedule.ActivityHours,
		KeepaliveHours:       cfg.Schedule.KeepaliveHours,
		SchoolHours:          cfg.Schedule.SchoolHours,
		CatHours:             cfg.Schedule.CatHours,
		ActivityReportCount:  cfg.Schedule.ActivityReportCount,
		ExpiringSoonWindow:   cfg.ExpiringSoonDur,   // 临近档（14d）优先消耗（issue:积分过期）
		ExpiringUrgentWindow: cfg.ExpiringUrgentDur, // 紧急档（7d）优先于临近档
		CheckinDisabled:      !cfg.Schedule.CheckinEnabled,
		TravelDisabled:       !cfg.Schedule.TravelEnabled,
		ActivityDisabled:     !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled:    !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:       !cfg.Schedule.SchoolEnabled,
		CatDisabled:          !cfg.Schedule.CatEnabled,
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}

	// opts.Ctx 为空必须回落 Background：WithCancel(nil) 会拿到一个**已取消**的 ctx，
	// 网关起来就立刻触发停机（表现为「监听日志打完就 bye」）。
	parent := opts.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	cleanups.add(cancel)

	// 主处理器 base 配置：多出口的通道处理器由它复制而来，只有「输出参数 + 服务范围」
	// 两项不同（见 internal/server/channels.go）。抽成变量是为了让主口与通道口共用一份，
	// 避免两处各写一遍漏字段。
	baseCfg := server.Config{
		ExpiringSoon:   cfg.ExpiringSoonDur,   // /admin/credits 的临近分桶窗口
		ExpiringUrgent: cfg.ExpiringUrgentDur, // /admin/credits 的紧急分桶窗口
		Pool:           p,
		Upstream:       up,
		APIKey:         cfg.APIKey,
		Session:        sessRouter,
		StickyCount:    sessCount,
		RedisMode:      redisMode,
		SoftCooldown:   cfg.SoftRateDur,
		SlotWait:       cfg.SlotWaitDur, // 在途占满时的排队等待（pool.slot_wait，默认 30s）
		PromptMode:     cfg.Prompt.Mode,
		PromptText:     cfg.PromptText,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// 运维管理端点开关（config admin.enabled，默认 false）。
		AdminEnabled: cfg.Admin.Enabled,
		// 「获取源」：导入目标目录 + 各源路径（账本留空走 ~/.wb-switch 默认）。
		AuthDir:          cfg.AuthDir,
		WBLedgerPath:     cfg.Source.WBLedgerPath,
		ZCodeDir:         cfg.Source.ZCodeDir,
		ZCodeAppDir:      cfg.Source.ZCodeAppDir,
		OpenCodeAuthPath: cfg.Source.OpenCodeAuthPath,
		WbDeskAuthDir:    cfg.Source.WbDeskAuthDir,
		// Codex 一键接入：config.toml 路径覆盖（空 = $CODEX_HOME / ~/.codex）。
		CodexConfigPath: cfg.CodexConfigPath,
		// ZCode 一键接入：provider_config.json 路径覆盖（空 = $ZCODE_HOME / ~/.zcode）。
		ZCodeConfigPath: cfg.ZCodeConfigPath,
		// 输出侧：热可改（模型前缀即时作用于 /v1/models 与入站解析；
		// 费率提示即时作用于模型描述前缀）。
		Output: outputStore,
		// 监听地址回显 + 改端口落点（改完需重启才真正换端口，页面会明说）。
		Listen:     cfg.Listen,
		ConfigPath: opts.ConfigPath,
		// 优雅停机入口（POST /admin/shutdown）：桌面控制台停「别的进程起的」网关时
		// 走这里，落盘/Flush/关监听全走与信号同一条路径。cancel 是幂等的。
		Shutdown: cancel,
	}

	// 多出口：同一进程内按用户配置，给不同来源的账号各开一个端口（也可全放在主口）。
	// 用「同进程多 listener」而不是多进程：号池/粘性/成本/冷却都是共享运行态，
	// 拆进程会让同一个号被两个进程同时选中（上游看到重复并发）。想彻底隔离时
	// 再另起一个进程即可——两条路不冲突。
	runner := server.NewChannelRunner(baseCfg)
	// 主口管理页要能显示各通道是否在听：把 runner 的状态查询挂到主口 handler 上。
	// 通道口自身不挂管理面（channels.go 里 AdminEnabled=false），不需要这个回调。
	baseCfg.ChannelStatus = runner.Status
	h := server.NewHandler(baseCfg)
	// zcode 额度收敛快照：先读回上次的结果（跨重启保留，避免重启后头几秒把整家 GLM
	// 目录当「可用」列出去），再后台热一遍拿新值。见 server.LoadZCodeEntitled / WarmCredits。
	server.SetZCodeEntitledPath(ZCodeEntitledJSONPath(cfg.StateFile))
	server.LoadZCodeEntitled()
	// 请求统计的逐日历史（今天 / 近 7 天 / 近 30 天视图的数据源）：落 state.json 同级的
	// data/stats.json，启动载入、停机强刷。不挂这里的话区间视图会被进程重启清空。
	server.InitMetricsPersist(metricsJSONPath(cfg.StateFile))
	// 逐日统计是后加的特性：启用之前跑过的请求只留在网关流水日志里（stdout.log 及
	// 其轮转备份）。启动时扫一遍把可解析的行补进 stats.json，否则「近 7 天 / 全部」
	// 只剩启用之后的那几天，用户会以为历史丢了。只补还没有的日，幂等。
	go server.BackfillFromLogs(metricsLogDirs(cfg.StateFile)...)
	cleanups.add(server.FlushMetrics)
	// 启动就把额度缓存热一遍（后台，不阻塞起服务）：zcode 的对外模型表按套餐额度收敛，
	// 读的就是这份缓存——见 server.Handler.WarmCredits。
	go h.WarmCredits()
	// 输出配置一变（PUT /admin/output，含 channels 增删改）就调和一次监听器，幂等。
	outputStore.OnChange(runner.Reconcile)
	runner.Reconcile(outputStore.Get())
	cleanups.add(runner.Close)

	go sch.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// max_body_mb 已移除（请求体无上限，交由上游自然响应），超大 body 成为
		// 唯一的自然约束：60s 内传不完会得到连接错误（read timeout）而非 413。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	log.Printf("free2api listening on %s (api_key=%v)", cfg.Listen, cfg.APIKey != "")
	inst := &Instance{Addr: cfg.Listen, cleanups: cleanups, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(inst.done)
		if lErr := srv.ListenAndServe(); lErr != nil && lErr != http.ErrServerClosed {
			inst.err = lErr
		}
	}()
	ok = true
	return inst, nil
}

// metricsLogDirs 网关流水日志可能落在哪几个目录。两种形态都要覆盖：
//   - 数据目录（state.json 同级）：命令行把 stdout 重定向成 ./data/server.out.log，
//     桌面程序写 ./data/desktop.log；
//   - 应用根目录（数据目录的上一级）：宿主控制台的 stdout 落在 ./stdout.log。
//
// 回填只看文件在不在，多给一个目录没有代价；少了就会漏掉历史。
func metricsLogDirs(stateFile string) []string {
	data := "data"
	if stateFile != "" {
		data = filepath.Dir(stateFile)
	}
	dirs := []string{data}
	// 上一级也要扫：默认 stateFile 是 "./data/state.json" 时，filepath.Dir 给出 "."
	// 即应用根目录，而宿主控制台的 stdout.log 正是落在那里。"." 是合法目录，
	// 不能像空串那样跳过；只有「父目录等于自身」（data 本身就是 "."）才不必重复加。
	if parent := filepath.Dir(data); parent != "" && parent != data {
		dirs = append(dirs, parent)
	}
	return dirs
}

// metricsJSONPath 请求统计逐日历史的落盘路径：**与 state.json 同目录**的 stats.json。
// 不再往下面再拼一层 "data"——state_file 缺省就是 ./data/state.json，再拼一层会变成
// data/data/stats.json，载入时静默读不到（踩过）。state_file 为空（嵌入/测试形态）时
// 退到 ./data/stats.json。
func metricsJSONPath(stateFile string) string {
	if stateFile == "" {
		return filepath.Join("data", "stats.json")
	}
	return filepath.Join(filepath.Dir(stateFile), "stats.json")
}

// Done 在监听 goroutine 退出后关闭。桌面程序用它做「启动即失败」的非阻塞探测：
// 端口被占时 Start 仍然会成功返回（绑定在后台 goroutine 里做），只有 Done 关闭 +
// Err 非空才说明这次启动其实没成。
func (i *Instance) Done() <-chan struct{} {
	if i == nil {
		return closedChan
	}
	return i.done
}

// Err 返回监听错误。**只有在 Done 已关闭之后读才有意义**——channel 的关闭提供了
// happens-before，所以这里不需要再加锁（见 run 里 inst.err 的写点）。
func (i *Instance) Err() error {
	if i == nil {
		return nil
	}
	return i.err
}

var closedChan = func() chan struct{} { c := make(chan struct{}); close(c); return c }()

// Wait 阻塞到网关退出，返回监听错误（正常停机返回 nil）。
func (i *Instance) Wait() error {
	if i == nil {
		return nil
	}
	<-i.done
	return i.err
}

// Stop 优雅停机：取消 context 触发 Flush / Shutdown，等监听 goroutine 退出后再
// 释放其余资源。幂等，可安全重复调用。
func (i *Instance) Stop() {
	if i == nil {
		return
	}
	i.stopOnce.Do(func() {
		if i.cancel != nil {
			i.cancel()
		}
		<-i.done
		i.cleanups.run()
	})
}

// Run 装配 + 阻塞运行，直到 ctx 取消或监听失败。cmd/server 用它保持命令行行为。
func Run(ctx context.Context, configPath string) error {
	inst, err := Start(Options{ConfigPath: configPath, Ctx: ctx})
	if err != nil {
		return err
	}
	err = inst.Wait()
	inst.Stop()
	return err
}

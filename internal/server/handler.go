// Package server 暴露 OpenAI 兼容 HTTP 接口，内部驱动 pool 挑号 + upstream 转发。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/pool"
	"free2api/internal/prompt"
	"free2api/internal/responses"
	"free2api/internal/session"
	"free2api/internal/upstream"
	"free2api/internal/webui"
)

// Config handler 依赖。
type Config struct {
	Pool      *pool.Pool
	Upstream  *upstream.Client
	APIKey    string // 空 = 不鉴权
	MaxRotate int    // 单请求最多换号次数，默认 3
	// Session 会话粘性路由器（可选；nil = 关闭粘性，纯 Pick 轮换）。
	Session *session.Router
	// StickyCount 返回当前粘性会话绑定数（供 /status）；nil 时报告 0。
	StickyCount func() int
	// RedisMode 观测字段（"upstash" / "noop"），供 /status 透出。
	RedisMode    string
	SoftCooldown time.Duration // 429/限流文案软冷却基数，默认 600s（连续触发指数退避，封顶 soft_rate_max）
	RefreshSkew  time.Duration // token 提前刷新窗口，默认 10m
	// SlotWait 选号时若「池里有健康号但全被在途上限占满」，最多排队等待多久等名额
	// 释放。0 = 不等待（旧行为零回归），由 config pool.slot_wait 注入（默认 30s）。
	// 单账号池 + 并发突发场景下把「成片 503」变成「排队」，同时对上游维持
	// max_in_flight 的并发压制。
	SlotWait time.Duration

	// PromptMode "passthrough"（默认，透传客户端原始 system）/ "custom"（网关替换）。
	PromptMode string
	// PromptText custom 模式下注入的系统提示词文本（来自 config.PromptText）。
	PromptText string

	// GlobalEnabled global realm 路由开关（config global.enabled，缺省 true）。
	// handler 侧第三道闸（与 main 注入 auth 开关、upstream.GlobalEnabled 呼应）：
	// false（显式逃生门）时即便 auth realm=global 也不提供 global: 模型名
	// （modelList 不列 global 名单）。
	GlobalEnabled bool

	// AdminEnabled 运维管理端点开关（config admin.enabled，默认 false）。
	// 关闭时 /admin/* 一律 404（而非 403——不向外暴露"这里存在管理面"）。
	AdminEnabled bool

	// AuthDir auths 目录：/admin/sources* 导入源时把 auth 文件写到这里。
	AuthDir string
	// WBLedgerPath WorkBuddy Switch 账本路径覆盖；空 = ~/.wb-switch/accounts.json。
	WBLedgerPath string
	// ZCodeDir zcode-switch 账号目录覆盖；空 = ~/.zcode-switch/accounts。
	ZCodeDir string
	// OpenCodeAuthPath OpenCode CLI/TUI auth.json 覆盖；空 = ~/.local/share/opencode/auth.json。
	OpenCodeAuthPath string
	// WbDeskAuthDir WorkBuddy 桌面端登录态目录覆盖；空 = 按平台取缺省
	// （Windows 用 %LOCALAPPDATA%\CodeBuddyExtension\Data\Public\auth）。
	WbDeskAuthDir string
	// ZCodeAppDir ZCode 应用登录态目录覆盖；空 = ~/.zcode/v2。
	// 只在「号池文件 + 账本都没有凭据」时被读（读额度凭据的第三层兜底）。
	ZCodeAppDir string
	// CodexConfigPath Codex config.toml 路径覆盖；空 = $CODEX_HOME/config.toml
	// 或 ~/.codex/config.toml。只被 /admin/codex* 用来「一键写入 Codex 配置」。
	CodexConfigPath string
	// ZCodeConfigPath ZCode provider_config.json 路径覆盖；空 = $ZCODE_HOME/v2/provider_config.json
	// 或 ~/.zcode/v2/provider_config.json。只被 /admin/zcode* 用。
	ZCodeConfigPath string

	// ModelPrefix / RateHint 输出侧默认值：只在没挂 Output 存储时生效
	// （嵌入形态、单元测试）。正常启动走 Output（可热改）。
	ModelPrefix string
	RateHint    string
	// Output 输出配置的运行时容器（/admin/output 读写）。nil = 只读。
	Output *OutputStore
	// Listen 启动时的监听地址（管理页回显用；空 = 不显示）。
	Listen string
	// ConfigPath 本次启动读的 config.json 路径。管理页改端口时改写它的 listen 键
	// （只改这一个键，其余键原样回写）；空 = 不可改端口（嵌入/测试形态）。
	ConfigPath string

	// ProducerAllow 该处理器只服务哪些 AI 客户端（空 = 全部）。多出口用：
	// 每个出口是一份独立 Handler，只有这一项与 Output 不同（见 channels.go）。
	ProducerAllow []string
	// ChannelID / ChannelStatus 只给多出口用：前者标识「我是哪个口」（日志/回显），
	// 后者回读到全部通道的运行状态（主口的管理页据此显示各口是否在听）。
	ChannelID     string
	ChannelStatus func() []ChannelStatus

	// ExpiringSoon 快过期积分窗口（config expiring_soon_days，与调度器签到用同一值）。
	// 仅供 /admin/credits 把余额里「窗口内就要作废」的部分单独标出来；0 = 不分桶。
	ExpiringSoon time.Duration

	// Shutdown 触发进程优雅停机（由 cmd/server 注入 ctx 的 cancel）。
	//
	// 为什么要有这个端点：桌面控制台（cmd/desktop）可能代理到一个**别的进程**起的
	// 网关（计划任务 / 命令行 / 另一个 exe）。用户在那个窗口点「停止网关」的意图是
	// 「把网关停了」，而不是「解除代理」。走 HTTP 让网关自己 cancel 掉生命周期，
	// 比在外部猜 PID、发控制台信号干净得多：落盘、Flush、关监听全走同一条路径。
	// nil 时不注册该路由。
	Shutdown func()
}

// notFoundCooldown 上游 404 的固定短冷却时长。
// 与 SoftCooldown 分流的原因：404 是上游**偶发**路径缺失，不是"本账号在限流"，
// 若共用 soft_rate（600s 起 + 指数升级），一次偶发 404 会把好账号罚 10 分钟并逐次加倍。
// 故固定 60s 防雪崩即可，不随 soft_rate 配置、也不参与软退避指数。
const notFoundCooldown = 60 * time.Second

// wafCooldownBase WAF 403 软冷却基数（任务书 P0-1 建议 60s 起；抖动 ±25% 后落
// [45s,75s]，实际进入 CooldownSoftRate 后再按 softStreak 指数、封顶 soft_rate_max）。
// 与 SoftCooldown 分流的原因：WAF 403 是 IP/指纹维频控（WAF 403 报告 §6），信号比
// 429「账号级限流」轻（账号本身健康、直连 200 实证），但比 404 重（带粘性会连环）；
// 60s 级的快速避让已足够让频控窗口滑过，指数升级由 CooldownSoftRate 既有机制接管。
// 抖动复用 backoff.go jitterDur（单一来源，不重复造轮子）。
const wafCooldownBase = 60 * time.Second

// ServiceName 网关身份标识。经 /healthz 响应体 service 字段与 X-Service 头同时透出：
// 宿主（如 workbuddy-switch 托管网关子进程）探测同端口的旧服务/其他服务时，对方即使
// 返回 2xx 也不带本标识，宿主据此可识别"假成功"。
const ServiceName = "free2api"

// dumpReqMinBytes WB2A_DUMP_REQ 调试落盘的"大请求"固定阈值（4MB）。原判断是
// 「超过 max_body_mb 上限一半」，max_body_mb 移除后改为固定值，语义不变：
// 小探针（{"input":"hi"} 之类）不落盘，避免覆盖真正要看的对话请求。
const dumpReqMinBytes = 4 << 20

// Handler 主路由。
type Handler struct {
	cfg     Config
	mux     *http.ServeMux
	degrade degradeGate
	// credits 余额查询缓存（见 credits.go）：/admin/credits 用，单飞 + 10min TTL。
	credits creditCache
	// wafIP WAF IP 级拦截状态机（fail-fast，wafip.go）：短窗多号 WAF 403 →
	// 激活期轮转遇 WAF 403 直接终止（不放大请求量）。进程内状态、重启清零。
	wafIP wafIPGate
	// modelDead 池级死模型负缓存（modeldead.go）：上游权威答复「无此模型」的
	// 模型从目录与路由剔除，TTL 到期自动复测。落盘持久，重启不丢。
	modelDead *modelDeadList
}

// NewHandler 构建 handler。
func NewHandler(cfg Config) *Handler {
	if cfg.MaxRotate <= 0 {
		cfg.MaxRotate = 3
	}
	if cfg.SoftCooldown <= 0 {
		cfg.SoftCooldown = 600 * time.Second // 软限流基数（连续触发按指数退避放大）
	}
	if cfg.RefreshSkew <= 0 {
		cfg.RefreshSkew = 10 * time.Minute
	}
	if cfg.PromptMode == "" {
		cfg.PromptMode = "passthrough" // 缺省 passthrough：透传客户端原始 system
	}
	h := &Handler{cfg: cfg, mux: http.NewServeMux()}
	// 池级死模型负缓存：落盘路径从 AuthDir 同级 data/ 派生（与 state.json 同目录，
	// 工作目录口径一致——AuthDir 缺省 ./auths → data/modeldead.json）。
	authDir := cfg.AuthDir
	if authDir == "" {
		authDir = "./auths"
	}
	h.modelDead = newModelDeadList(filepath.Join(filepath.Dir(authDir), "data", "modeldead.json"))
	h.mux.HandleFunc("POST /v1/chat/completions", h.withAuth(h.chatCompletions))
	// Responses 出口：Codex 等只认 wire_api="responses" 的客户端直连本进程，
	// 不再需要外挂一个协议翻译进程（见 sink.go / internal/responses）。
	h.mux.HandleFunc("POST /v1/responses", h.withAuth(h.responsesEndpoint))
	h.mux.HandleFunc("GET /v1/models", h.withAuth(h.models))
	h.mux.HandleFunc("GET /status", h.withAuth(h.status))
	h.mux.HandleFunc("GET /v1/stats", h.withAuth(h.stats))
	h.mux.HandleFunc("POST /v1/stats/reset", h.withAuth(h.statsReset))
	// 统计的 SSE 推送：与 /v1/stats 同载荷，只是由服务端主动推。
	// 没有它，前端只能轮询，实时性天然差一个轮询间隔。
	h.mux.HandleFunc("GET /v1/stats/stream", wrapQueryKey(h.withAuth(h.statsStream)))
	// 运维管理端点（默认关闭，config admin.enabled 开启后生效）。
	// 路径用 {uid} 通配而非查询参数：uid 是账号身份，放进路径便于审计与直观。
	// 条件注册而非 handler 内 404（设计 supplement §2.3）：未注册的路由对未鉴权
	// 探测回 mux 默认纯文本 404、对 GET 探测无 405+Allow 头，与真 404 完全不可
	// 区分——路由一旦注册，"带 key 得 401 / GET 得 405 / JSON 信封 404" 三者都会
	// 暴露管理面存在。
	if cfg.AdminEnabled {
		h.mux.HandleFunc("POST /admin/accounts/{uid}/disable", h.withAuth(h.adminAccountDisable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/enable", h.withAuth(h.adminAccountEnable))
		h.mux.HandleFunc("POST /admin/accounts/{uid}/revive", h.withAuth(h.adminAccountRevive))
		// 运行态复位（清冷却 + 清失败计数）：全池 / 点名 / 按生产者。不清 disabled 位。
		h.mux.HandleFunc("POST /admin/accounts/reset", h.withAuth(h.adminAccountsReset))
		// 池级死模型负缓存（modeldead.go）：GET 看标记全量，DELETE 手动清除（误杀兜底）。
		h.mux.HandleFunc("GET /admin/modeldead", h.withAuth(h.adminModelDeadList))
		h.mux.HandleFunc("DELETE /admin/modeldead", h.withAuth(h.adminModelDeadClear))
		// 「获取源」：本机直连免 key（withLocalOrAuth），非本机仍校验 api_key。
		h.mux.HandleFunc("GET /admin/sources", h.withLocalOrAuth(h.adminSources))
		h.mux.HandleFunc("GET /admin/sources/{kind}/candidates", h.withLocalOrAuth(h.adminSourceCandidates))
		h.mux.HandleFunc("POST /admin/sources/import", h.withLocalOrAuth(h.adminSourceImport))
		// 来源台账（账号归属：应用内 / 账本 / 导入文件）与输出侧配置。
		h.mux.HandleFunc("GET /admin/sources/origins", h.withLocalOrAuth(h.adminSourceOrigins))
		h.mux.HandleFunc("GET /admin/output", h.withLocalOrAuth(h.adminOutputGet))
		h.mux.HandleFunc("PUT /admin/output", h.withLocalOrAuth(h.adminOutputPut))
		// /admin/credits 余额只读查询（按需直查上游 billing，补上「刚装好还没到签到点」的空窗）。
		h.mux.HandleFunc("GET /admin/credits", h.withLocalOrAuth(h.adminCredits))
		h.mux.HandleFunc("POST /admin/credits/refresh", h.withLocalOrAuth(h.adminCreditsRefresh))
		// Codex 一键接入：Codex 没有「自定义供应商」入口，只能代它改 config.toml。
		// 读现状 / 预览（不落盘）/ 落盘（先备份）。
		h.mux.HandleFunc("GET /admin/codex", h.withLocalOrAuth(h.adminCodexGet))
		h.mux.HandleFunc("POST /admin/codex/preview", h.withLocalOrAuth(h.adminCodexPreview))
		h.mux.HandleFunc("POST /admin/codex/apply", h.withLocalOrAuth(h.adminCodexApply))
		// Codex 反向操作：还原官方默认（删顶层 model_provider，网关模型名一并删）。
		h.mux.HandleFunc("POST /admin/codex/detach", h.withLocalOrAuth(h.adminCodexDetach))
		// ZCode 一键接入：ZCode 不读 /v1/models 的上下文字段（抓包实证它一次都没请求过），
		// 显示值来自 ~/.zcode/v2/provider_config.json 的 modelConfigRules。
		// 所以只能代它把 contextWindow / maxOutputTokens 写进去。
		h.mux.HandleFunc("GET /admin/zcode", h.withLocalOrAuth(h.adminZCodeGet))
		h.mux.HandleFunc("POST /admin/zcode/preview", h.withLocalOrAuth(h.adminZCodePreview))
		h.mux.HandleFunc("POST /admin/zcode/apply", h.withLocalOrAuth(h.adminZCodeApply))
		h.mux.HandleFunc("POST /admin/zcode/create", h.withLocalOrAuth(h.adminZCodeCreate))
		// 定时任务可见性（读 config.json schedule 段）+ 开关（写回配置，重启生效）。
		h.mux.HandleFunc("GET /admin/schedule", h.withLocalOrAuth(h.adminScheduleGet))
		h.mux.HandleFunc("POST /admin/schedule", h.withLocalOrAuth(h.adminSchedulePut))
		// 改网关密钥（生成/设置/清空）：落盘 config.json，重启生效。
		// 必须 withLocalOrAuth 而不是 withAuth —— 一把还没生效的密钥无法鉴权自己。
		h.mux.HandleFunc("POST /admin/api-key", h.withLocalOrAuth(h.adminAPIKeyPut))
		// 优雅停机（桌面控制台的「停止网关」按钮走这里）。
		// 只在注入了 Shutdown 时才注册：嵌入式用法没生命周期可停，不该暴露这个口子。
		if h.cfg.Shutdown != nil {
			h.mux.HandleFunc("POST /admin/shutdown", h.withLocalOrAuth(h.adminShutdown))
		}
	}
	// 内嵌中文控制台：GET /{$} 是精确根路径（Go 1.22 mux 语法），
	// 不用 "/" 以免变成 catch-all 把 404 语义吃掉。
	// 页面本身不含任何机密（只是前端壳），因此不鉴权；页面里的数据请求
	// 才走 /admin/* 与 /status，那些端点各自有闸。这样即便管理端点没开，
	// 用户也能打开页面看到"为什么不能用"的明确提示，而不是一个 404。
	h.mux.HandleFunc("GET /{$}", h.webuiPage)
	h.mux.HandleFunc("GET /management.html", h.webuiPage)
	h.mux.HandleFunc("GET /healthz", h.healthz)
	return h
}

// webuiPage 输出内嵌控制台页面。
func (h *Handler) webuiPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(webui.Page())
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.APIKey != "" {
			authz := r.Header.Get("Authorization")
			// 常量时间比较（发现 7）：!= 短路时序随前缀长度变化，公网暴露下
			// 理论上可逐字节探测 key 前缀；ConstantTimeCompare 消除该信号。
			provided := strings.TrimPrefix(authz, "Bearer ")
			ok := strings.HasPrefix(authz, "Bearer ") &&
				subtle.ConstantTimeCompare([]byte(provided), []byte(h.cfg.APIKey)) == 1
			if !ok {
				ok = h.queryKeyOK(r)
			}
			if !ok {
				writeOpenAIError(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
				return
			}
		}
		next(w, r)
	}
}

// queryKeyOK 校验 ?api_key=… / ?key=…。只对「浏览器自己发不出自定义头」的
// 请求形态有意义：EventSource（/v1/stats/stream）没法带 Authorization 头，
// WebSocket 同理。这类请求没法自定义 header，只能把 key 放查询串里。
//
// 代价是 key 会进访问日志 / Referer，所以严格限定：仅当该 handler 显式
// 标了 allowQueryKey 时才接受——由 wrapQueryKey 标注，不是全局开口子。
// 常量时间比较，且两个头(名)比完再放行，避免逐字节探测 key 的存在性。
func (h *Handler) queryKeyOK(r *http.Request) bool {
	if h.cfg.APIKey == "" {
		return true
	}
	if !queryKeyAllowed(r) {
		return false
	}
	got := r.URL.Query().Get("api_key")
	if got == "" {
		got = r.URL.Query().Get("key")
	}
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(h.cfg.APIKey)) == 1
}

// qkCtxKey 标记「本条路由允许 api_key 走查询串」。
type qkCtxKey struct{}

// wrapQueryKey 标一条路由为「允许 ?api_key=」，必须与 handler 一起套用。
func wrapQueryKey(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		next(w, r.WithContext(context.WithValue(r.Context(), qkCtxKey{}, true)))
	}
}

func queryKeyAllowed(r *http.Request) bool {
	v, _ := r.Context().Value(qkCtxKey{}).(bool)
	return v
}

func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	total, healthy, _, _, _ := h.cfg.Pool.CountsDetailed()
	// 用 ServableNow 判定：healthy>0 但全占满在途时 chat 会 503，探活必须同口径，
	// 否则负载均衡器会把流量持续打进无法受理的实例。
	status := http.StatusOK
	if !h.cfg.Pool.ServableNow() {
		status = http.StatusServiceUnavailable
	}
	// realm_servable 域可服务维度：不改判活语义（存在性探活保持不变），
	// 只新增 CN/global 各自可达性供双域部署运维观察（任一域不可用单独告警）。
	realmServable := map[string]bool{
		"cn":     h.cfg.Pool.ServableForRealm("cn"),
		"global": h.cfg.Pool.ServableForRealm("global"),
	}
	// 恒无鉴权（负载均衡/编排探活只需 2xx/503 语义），身份靠 service 字段 + X-Service 头双保险。
	w.Header().Set("X-Service", ServiceName)
	writeJSON(w, status, map[string]any{
		"healthy":        healthy,
		"total":          total,
		"service":        ServiceName,
		"realm_servable": realmServable,
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request) {
	total, healthy, cooling, disabled, inFlightFull := h.cfg.Pool.CountsDetailed()
	sticky := 0
	if h.cfg.StickyCount != nil {
		sticky = h.cfg.StickyCount()
	}
	redisMode := h.cfg.RedisMode
	if redisMode == "" {
		redisMode = "noop"
	}
	// cost_explore 探索台账（issue #136 §5 可观测性）：累计探索事件数 + 各
	// (域, 模型) 的最近探索时刻（键 "realm|model"）。与 accounts[].model_costs
	// 行对照即可读出「探索→毕业」全链路（单一事实来源，不做双表示）。零回归只增键。
	exploreEvents, exploreLast := h.cfg.Pool.CostExploreStatus()
	// realm_totals 按域分组的计数汇总（双 realm 并存时运维一眼看到各域可用性）：
	// 只新增字段，既有 total/healthy/cooling/disabled/in_flight_full 汇总键不变（零回归）。
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":        h.cfg.Pool.List(),
		"producer_totals": h.producerTotalsView(),
		"total":           total,
		"healthy":         healthy,
		"cooling":         cooling,
		"disabled":        disabled,
		"in_flight_full":  inFlightFull,
		"realm_totals": map[string]map[string]int{
			"cn":     countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("cn")),
			"global": countsMapFrom(h.cfg.Pool.CountsDetailedForRealm("global")),
		},
		"sticky_sessions": sticky,
		"redis_mode":      redisMode,
		// live_models：此刻正在跑的模型 → 在飞请求数（零值时省略）。
		// 概览「正在发生」区块问的是「现在在跑什么」，而 accounts[].in_flight
		// 只答「哪个号在忙」——两者不是一回事，故单列一个按模型维度的计数。
		"live_models": globalLiveModels.snapshot(),
		// cost_explore 事件与 per-model 时间戳（时间值由 encoding/json 写 RFC3339）。
		"cost_explore": map[string]any{
			"events_total": exploreEvents,
			"per_model":    exploreLast,
		},
	})
}

// countsMapFrom 把 CountsDetailed 五元组编码为 /status realm_totals 的字段对象。
func countsMapFrom(total, healthy, cooling, disabled, inFlightFull int) map[string]int {
	return map[string]int{
		"total":          total,
		"healthy":        healthy,
		"cooling":        cooling,
		"disabled":       disabled,
		"in_flight_full": inFlightFull,
	}
}

// dynamicModelsCache 动态模型缓存。
var dynamicModelsCache struct {
	sync.RWMutex
	ids      []upstream.ModelInfo
	fetched  time.Time // 最近一次成功拉取时间
	lastFail time.Time // 最近一次拉取失败时间（负缓存）
}

const (
	dynamicModelsTTL        = time.Hour
	modelsFetchFailCooldown = 5 * time.Minute
)

// models 返回模型列表：纯动态（缓存 1h），失败/无号返回空列表（无静态兜底）。
//
// 两种对外格式共用同一个路径，按 Codex 的固定查询参数分流（见 codex_catalog.go）：
// 带 ?client_version= 的是 Codex（桌面端/CLI），它只认自己的目录格式；其余客户端
// 拿通用 OpenAI 格式。分流只放在这一处，列表与可调用性的口径仍然只有 modelList() 一份。
func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("client_version") != "" {
		writeJSON(w, http.StatusOK, map[string]any{"models": h.codexModels()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   h.modelList(),
	})
}

// globalModels 国际版（global realm）模型名名单（PLAN §7.2 附录 21 名）——已删。
// 纯动态化后 handler 不再持有任何静态名单：无 global 账号 / 探测失败 → 空列表。

// resolveModel 入站模型名解析：先剥「输出侧模型前缀」，再剥 realm 前缀。
// 前缀是我们自己加给客户端的，请求带回来时必须在解析 realm 之前去掉，
// 否则 "wb-cn:xxx" 里的 "wb-cn" 会被当成裸模型名整串透传给上游。
func (h *Handler) resolveModel(model string) (realm, bare string) {
	r, _, b := resolveModelPrefixed(model, h.outputCfg().ModelPrefix)
	return r, b
}

// resolveRoute 在 resolveModel 之上补 producer 维度：**显式前缀优先**，请求没写
// "zcode:" 时按模型归属推断（见 modelOwner）。推断而非硬默认，是为了让客户端
// 只写裸名 "glm-4.6" 也能自动落到 zcode 号池——写裸名的客户端不知道背后有几家
// 上游，网关有责任替它分对。
//
// 但推断只做「唯一归属」判定：两家目录里都有的模型名一律回落 workbuddy
// （modelOwnerEx 返回 ambiguous），要打 zcode 必须显式写前缀。宁可让用户显式声明，
// 也不做「猜错了把请求发到另一家去」这种静默跨家路由。
//
// 唯一的例外：这个口只服务一家（多出口，ProducerAllow 只有一个来源）时，歧义按这家
// 落地——客户端连的就是「ZCode 专用口」，它写裸名 glm-4.6 不可能是想打 WorkBuddy。
// 那种情况下再回 workbuddy 等于把这个口废掉（请求必被出口范围检查挡下）。
func (h *Handler) resolveRoute(model string) (realm, producer, bare string) {
	// 先剥 realm/producer 前缀，**但先不剥费率后缀**——上游模型名自身就可能以 "-free"
	// 结尾（opencode 免费层：mimo-v2.6-flash-free 等），无条件剥会改成不存在的名字。
	// 后缀剥不剥交给 reconcileRateSuffix 按目录裁决（见下）。
	realm, producer, bare = resolveModelRoute(model)
	if prefix := h.outputCfg().ModelPrefix; prefix != "" {
		bare = strings.TrimPrefix(bare, prefix)
	}
	bare = h.reconcileRateSuffix(bare, realm, producer)
	if producer == "" {
		if owner, ambiguous := h.modelOwnerEx(bare); owner != "" {
			producer = owner
		} else if ambiguous {
			if one, ok := h.singleProducer(); ok {
				producer = one
			}
		}
	}
	return realm, producer, bare
}

// reconcileRateSuffix 决定是否剥掉入站模型名尾部的费率后缀（"-free" / "-x<数字>"）。
// 入参 bare 是**未剥后缀**的裸名（realm/producer 前缀已剥）。
//
// 为什么要按目录裁决：resolveModelPrefixed 为了「客户端拿带后缀的名字回传还能路由」
// 会无条件剥尾巴，但上游模型名自身就可能以 "-free" 结尾（opencode 免费层：
// mimo-v2.6-flash-free 等）。无条件剥会把合法模型名改成不存在的名字
// （mimo-v2.6-flash）→ 上游 400 Model is unavailable，并被误标池级死模型。
//
// 判据：先算出「剥了尾巴」的候选裸名 stripped，再看它是否出现在对应上游目录里
// （workbuddy CN / zcode / opencode / global）。在目录里 → 认它是我们加的费率后缀，剥；
// 不在 → 保留原串（上游模型名自带的尾巴）。查目录只读 1h 缓存，不额外打上游。
func (h *Handler) reconcileRateSuffix(bare, realm, producer string) string {
	stripped := StripRateSuffix(bare)
	if stripped == bare || stripped == "" {
		return bare // 没有可剥的尾巴
	}
	// 原串本身就在目录里 → 它是上游真实模型名（尾巴是自带的，不是我们加的），保留。
	// 这条必须先判：jev-1.13-free 与 jev-1.13 在 zen 目录里都存在，若先看 stripped
	// 会把用户点名要的 -free 版本错路由到另一个模型。
	if h.modelKnown(realm, producer, bare) {
		return bare
	}
	// 原串不在目录、剥完的候选在目录里 → 尾巴是我们加的费率后缀，剥。
	if h.modelKnown(realm, producer, stripped) {
		return stripped
	}
	// 两个都不在目录（目录为空 / 尚未拉取 / 自定义模型）：回落到旧行为「剥」，
	// 保证「客户端拿带费率后缀的名字回传」这一既有契约不因目录缺失而失效。
	return stripped
}

// modelKnown 该裸名是否出现在对应域/来源的上游目录里（含 global）。producer 为空表示
// 未知来源，则四家目录都查一遍。
func (h *Handler) modelKnown(realm, producer, bare string) bool {
	if bare == "" {
		return false
	}
	if realm == "global" {
		return containsModelID(h.cfg.Upstream.GlobalModelInfosSnapshot(), bare)
	}
	if producer == "" || producer == "workbuddy" {
		if containsModelID(h.fetchDynamicModels(), bare) {
			return true
		}
	}
	if producer == "" || producer == upstream.ProducerZCode {
		if containsModelID(h.fetchZCodeCatalog(), bare) {
			return true
		}
	}
	if producer == "" || producer == upstream.ProducerOpenCode {
		if containsModelID(h.fetchOpenCodeCatalog(), bare) {
			return true
		}
	}
	if producer == "" || producer == upstream.ProducerKilo {
		if containsModelID(h.fetchKiloCatalog(), bare) {
			return true
		}
	}
	return false
}

// modelFree 报告某裸名在当前 (realm, producer) 口径下是否为「零积分模型」
// （上游 ModelInfo.Free，口径见 upstream.CreditRateIsZero / isOpenCodeFreeTierModel /
// kilo 全量免费）。选号侧据此决定要不要按余额加权（见 pool.PickExcludingForProducerRealmFree）。
//
// **只读 1h 缓存快照，绝不触发上游探测**——这是选号热路径的一部分，每个请求、每轮
// 轮转都会调用一次，任何一次网络往返都会把对话延迟拖垮（且会在池刚起、缓存冷时给
// 每个请求加一次串行探测）。因此这里刻意不复用 modelKnown/fetchDynamicModels 这些
// 「冷缓存会打上游」的函数，而是直接读各自的 *_Snapshot（口径见 cachedModelsSnapshot
// 等同族函数）。与 cachedModelsSnapshot 的差异只在注释与 Free 字段而非存在性。
//
// 缓存冷 / 模型不在目录 / 自定义模型 → false（**保守按收费处理**，退回既有的余额
// 加权行为）。宁可让一个新模型继续走旧规则（可能把流量堆到余额高的号，但至少会正常
// 扣费），也不能把收费模型误判成免费、让所有请求挤到一个号上把余额烧穿还互相 429。
//
// producer=="" 表示来源未知：四家目录都查，任一家命中且标 Free 即为免费。模型名跨家
// 重名时选号侧仍会靠 producer 谓词落到具体一家，这里只回答「这个名字的免费属性」。
func (h *Handler) modelFree(realm, producer, bare string) bool {
	if bare == "" {
		return false
	}
	if realm == "global" {
		return containsFreeModel(h.cfg.Upstream.GlobalModelInfosSnapshot(), bare)
	}
	if producer == "" || producer == "workbuddy" {
		if containsFreeModel(cachedModelsSnapshot(), bare) {
			return true
		}
	}
	if producer == "" || producer == upstream.ProducerZCode {
		if containsFreeModel(zcodeCatalogSnapshot(), bare) {
			return true
		}
	}
	if producer == "" || producer == upstream.ProducerOpenCode {
		if containsFreeModel(opencodeCatalogSnapshot(), bare) {
			return true
		}
	}
	if producer == "" || producer == upstream.ProducerKilo {
		if containsFreeModel(kiloCatalogSnapshot(), bare) {
			return true
		}
	}
	return false
}

// singleProducer 该口恰好只服务一家时返回它（用于消解裸名的歧义路由，见 resolveRoute）。
func (h *Handler) singleProducer() (string, bool) {
	if len(h.cfg.ProducerAllow) == 1 {
		return h.cfg.ProducerAllow[0], true
	}
	return "", false
}

// producerAllowed 该出口是否服务某个 producer。ProducerAllow 为空 = 全部来源都服务
// （主口与单出口形态）；非空 = 只有列进去的客户端能过（多出口/每客户端一个口）。
// 裸名解析出来的 producer 可能是空串，归一成 workbuddy 再比——空串在网关里的语义
// 就是「CN 默认那家」，与 workbuddy 同义（见 modelOwner/resolveRoute）。
func (h *Handler) producerAllowed(producer string) bool {
	if len(h.cfg.ProducerAllow) == 0 {
		return true
	}
	if producer == "" {
		producer = "workbuddy"
	}
	for _, p := range h.cfg.ProducerAllow {
		if p == producer {
			return true
		}
	}
	return false
}

// modelID 出站模型名拼装：realm + 输出前缀 + 裸名 + 费率后缀（与 resolveModelPrefixed 严格对称）。
// credits 是模型自身携带的倍率原文（上游下发），后缀形状由 output.rate_hint 决定：
// credit 档按自身倍率给（x0.00 → -free，x0.11 → -x0.11，无倍率 → 不加），free 档一律 -free。
func (h *Handler) modelID(realm, bare, credits string) string {
	return h.modelIDFor(realm, "", bare, credits)
}

// modelIDFor 带 producer 段的出站模型名。
//
// 段序：workbuddy 是 realm:（"cn:auto" / "global:gpt-5.4"）；其他来源是
// producer:（"zcode:glm-4.6" / "opencode:big-pickle"）。
//
// realm 段（cn/global）只对 workbuddy 有意义——那是它自己的「国内版 / 国际版 wba」
// 两种域，其他来源（zcode / opencode / kilo…）根本没有域的概念。以前对所有来源都
// 硬拼 realm，于是 opencode / zcode 的模型名变成 "cn:opencode:xxx"，让人误读成
// 「这些模型也分国内国际」。现在非 workbuddy 来源不再带 realm 段。
//
// workbuddy 的 producer 段照旧省略（历史模型名 "cn:auto" 不带 "workbuddy" 段），
// 与 modelKey 把 workbuddy 归一成裸 id 的口径一致。
// 与 resolveModelPrefixed 严格对称：可选 realm + 可选 producer + 前缀 + 裸名 + 后缀。
func (h *Handler) modelIDFor(realm, producer, bare, credits string) string {
	if producer == "workbuddy" {
		producer = ""
	}
	mid := realm + ":"
	if producer != "" {
		mid = producer + ":"
	}
	cfg := h.outputCfg()
	return mid + cfg.ModelPrefix + bare + RateSuffix(cfg.RateHint, credits)
}

// applyModelInfoFields 把上游模型对象全字段（ModelInfo）按「空值省略」写出规则
// 合入 /v1/models 条目：name/description/credits/tags/vendor/能力旗标/
// max_allowed_size/reasoning_effort/reasoning_summary。CN 动态分支与 global
// 探测命中分支共用（两域模型对象同构），保证输出字段集一致。
// 不覆盖 id/object/created/owned_by 及调用方先前写好的基础字段；上游未下发的
// 字段（零值）整体省略——不编造。
func (h *Handler) applyModelInfoFields(entry map[string]any, mi upstream.ModelInfo) map[string]any {
	if mi.Name != "" {
		entry["name"] = mi.Name
	}
	if mi.Description != "" {
		// 描述原文透出：费率不再塞进描述（那会与模型名后缀重复、两处口径看着割裂），
		// 改由 id 后缀（modelIDFor）+ credits 字段承载。
		entry["description"] = mi.Description // descriptionZh 中文描述
	}
	if mi.Credits != "" {
		entry["credits"] = mi.Credits // 积分倍率原文（如 "x0.05"），仅展示
	}
	if len(mi.Tags) > 0 {
		entry["tags"] = mi.Tags
	}
	if mi.Vendor != "" {
		entry["vendor"] = mi.Vendor
	}
	if mi.IsDefault {
		entry["is_default"] = true
	}
	if mi.SupportsImages {
		entry["supports_images"] = true // 多模态能力透出
	}
	if mi.SupportsReasoning {
		entry["supports_reasoning"] = true
	}
	if mi.SupportsToolCall {
		entry["supports_tool_call"] = true
	}
	if mi.OnlyReasoning {
		entry["only_reasoning"] = true
	}
	if mi.MaxAllowedSize > 0 {
		entry["max_allowed_size"] = mi.MaxAllowedSize
	}
	if mi.ReasoningEffort != "" {
		entry["reasoning_effort"] = mi.ReasoningEffort
	}
	if mi.ReasoningSummary != "" {
		entry["reasoning_summary"] = mi.ReasoningSummary
	}
	return entry
}

// modelList 动态获取模型列表并包装成 OpenAI 格式（含 context_length）。
// CN 模型输出统一加 "cn:" 前缀（gateway 路由协议，与 resolveModel 对称）。
// 纯动态：动态拉取失败/无号 → 该域空列表，无静态兜底；
// global.enabled=false（显式逃生门）时只列 CN（global 名单不出现）。
func (h *Handler) modelList() []map[string]any {
	out := make([]map[string]any, 0)
	// published 输出白名单（nil = 全放）。账号能跑什么由上游决定，对外吐什么由用户
	// 在「输出 API」页勾选决定——见 output.go OutputConfig.Models。这里管「列出来」，
	// 「能不能调」由 chatCompletions 用同一份清单判（两侧口径必须一致，否则勾了等于没勾）。
	published := h.outputCfgModelSet()
	// 多出口：这个口不服务 workbuddy 时连目录都不拉（避免给 zcode 专用口白拉 CN 目录）。
	var cnModels []upstream.ModelInfo
	if h.producerAllowed("workbuddy") {
		cnModels = h.fetchDynamicModels()
	}
	for _, mi := range cnModels {
		if !h.publishAllows(published, "cn", "workbuddy", mi.ID) {
			continue
		}
		// 池级可服务性：上游目录里有、但号池实测打不通的（权威「无此模型」已标记）
		// 不对外列出——列出去等于承诺「能调」，客户端选中即必失败。
		if h.modelDead.active("cn", "workbuddy", mi.ID) {
			continue
		}
		entry := map[string]any{
			"id":       h.modelID("cn", mi.ID, mi.Credits),
			"object":   "model",
			"created":  1753600000,
			"owned_by": "workbuddy",
		}
		// context_length / max_output_tokens 四级查找（upstream.context_catalog +
		// model_catalog）：上游动态值（maxInputTokens/maxOutputTokens）权威 → 静态
		// 种子表 → model.json 本地缓存 → models.dev 按需拉取（异步不阻塞本次响应，
		// 拉到后写 model.json 供下次命中）→ 1M 兜底 / max_output_tokens 省略。
		// 上游零值不再透出假 131072（误导 Codex/ZCode 等按 context_length 提前
		// 截断、白白丢上下文）。
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		// 上游模型对象全字段透出（name/描述/标签/倍率/能力旗标等，空值省略）。
		entry = h.applyModelInfoFields(entry, mi)
		// P0：effort 能力透出——远端 supportedEfforts 权威，缺失落到 CN 静态兜底表
		// （issue #84 客户端可发现档位，不再盲传）。无档位→省略字段（非空数组）。
		if efforts, def := upstream.EffortListing("cn", mi.ID, mi.Efforts, mi.DefaultEffort); efforts != nil {
			entry["reasoning_supported_efforts"] = efforts
			if def != "" {
				entry["reasoning_default_effort"] = def
			}
		}
		out = append(out, entry)
	}
	// zcode（智谱）模型名单：从号池里任一健康 zcode 号实时拉上游目录（1h 缓存）。
	// 用 modelIDFor 带上 "zcode" 段，客户端把整串名字回传即路由固定到 zcode 号池
	// （与 resolveModelRoute 对称）；裸名（无前缀）由 modelOwner 按归属自动分流。
	// owned_by 标 zcode 而不是 workbuddy：管理页/客户端据此一眼看出这行是谁家能力。
	var zcodeModels []upstream.ModelInfo
	if h.producerAllowed(upstream.ProducerZCode) {
		zcodeModels = h.fetchZCodeCatalog()
	}
	// 对外只列号池**真的有额度**的 zcode 模型：上游 /models 是整家厂商的目录（所有 GLM
	// 型号），号池里的号套餐常常只覆盖其中一两个——列出去就等于承诺「这个模型能调」，让
	// 客户端选到一个必失败的模型是更差的体验。用户显式勾进发布清单的照旧列出（白名单即
	// 用户意图，覆盖这条收敛）；一条额度信息都还没读到过时也不收敛（宁可多列，不假装没有）。
	zcEnt, zcKnown := h.zcodeEntitledModels()
	for _, mi := range zcodeModels {
		if !h.publishAllows(published, "cn", upstream.ProducerZCode, mi.ID) {
			continue
		}
		if published == nil && zcKnown && !zcEnt[mi.ID] {
			continue
		}
		// 池级可服务性：与 cn 段同一口径（zcode 段的 1211「模型不存在」同样权威）。
		if h.modelDead.active("cn", upstream.ProducerZCode, mi.ID) {
			continue
		}
		entry := map[string]any{
			"id":       h.modelIDFor("cn", upstream.ProducerZCode, mi.ID, mi.Credits),
			"object":   "model",
			"created":  1753600000,
			"owned_by": upstream.ProducerZCode,
		}
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		entry = h.applyModelInfoFields(entry, mi)
		out = append(out, entry)
	}
	// opencode（Zen）模型名单：与 zcode 段同构——从号池里任一健康 opencode 号实时拉
	// 目录（1h 缓存），modelIDFor 带 "opencode" 段，客户端整串回传即路由固定到
	// opencode 号池（与 resolveModelRoute 对称）。免费层模型也在列（2026-09-30 起可直连）
	// 见 upstream/opencode.go 文件头更正。
	var opencodeModels []upstream.ModelInfo
	if h.producerAllowed(upstream.ProducerOpenCode) {
		opencodeModels = h.fetchOpenCodeCatalog()
	}
	for _, mi := range opencodeModels {
		if !h.publishAllows(published, "cn", upstream.ProducerOpenCode, mi.ID) {
			continue
		}
		if h.modelDead.active("cn", upstream.ProducerOpenCode, mi.ID) {
			continue
		}
		entry := map[string]any{
			"id":       h.modelIDFor("cn", upstream.ProducerOpenCode, mi.ID, mi.Credits),
			"object":   "model",
			"created":  1753600000,
			"owned_by": upstream.ProducerOpenCode,
		}
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		entry = h.applyModelInfoFields(entry, mi)
		out = append(out, entry)
	}
	// kilo（Kilo Code 匿名免费层）模型名单：与前面几段同构——目录实时从上游拉
	// （1h 缓存），modelIDFor 带 "kilo" 段，客户端整串回传即路由固定到 kilo 段。
	// Kilo 没有账号：目录拉取用池里那条匿名占位号出去（见 internal/source/kilo.go）。
	var kiloModels []upstream.ModelInfo
	if h.producerAllowed(upstream.ProducerKilo) {
		kiloModels = h.fetchKiloCatalog()
	}
	for _, mi := range kiloModels {
		if !h.publishAllows(published, "cn", upstream.ProducerKilo, mi.ID) {
			continue
		}
		if h.modelDead.active("cn", upstream.ProducerKilo, mi.ID) {
			continue
		}
		entry := map[string]any{
			"id":       h.modelIDFor("cn", upstream.ProducerKilo, mi.ID, mi.Credits),
			"object":   "model",
			"created":  1753600000,
			"owned_by": upstream.ProducerKilo,
		}
		entry["context_length"] = upstream.ContextWindowListingV4(mi.ID, mi.ContextWindow, h.cfg.Upstream.HTTP)
		if mo, ok := upstream.MaxOutputTokensListingV4(mi.ID, mi.MaxTokens, h.cfg.Upstream.HTTP); ok {
			entry["max_output_tokens"] = mo
		}
		entry = h.applyModelInfoFields(entry, mi)
		out = append(out, entry)
	}
	// global 模型名单：仅 GlobalEnabled=true 时列出（逃生门）。
	// 名单 = 探测结果（fetchGlobalModels 纯动态，失败/无号 → 空）；无 global 账号时
	// 空名单且零上游调用。
	//
	// producerAllowed("workbuddy") 这一道不能少：global 域（workbuddy.ai）属于 workbuddy
	// 这家。少了它，zcode 专用口会把整个 global 族列进 /v1/models，而请求侧按来源判定
	// 一律 404（「列表里看得见、一调就 404」）。列表与可调用性必须同口径。
	if h.cfg.GlobalEnabled && h.producerAllowed("workbuddy") {
		// global 域 effort 能力三级查找：探测下发桶（权威）→ 静态兜底表 → 省略。
		// 先 fetchGlobalModels（内部探测并落 effort 桶），再按 id 取快照。
		globalIDs, globalAccount := h.fetchGlobalModels()
		// 探测对象形态的全字段条目（与 fetchGlobalModels 共享同一次探测缓存）：
		// 命中 id 才透出富字段；窄表/失败 → nil，按裸 ID 条目输出（不编造字段）。
		// globalAccount 为 nil（无 global 号）时返回 nil，跳过富字段映射。
		globalInfos := map[string]upstream.ModelInfo{}
		for _, mi := range h.cfg.Upstream.FetchGlobalModelInfos(globalAccount) {
			globalInfos[mi.ID] = mi
		}
		globalEfforts, globalDefaults := h.cfg.Upstream.GlobalEffortSnapshot()
		for _, id := range globalIDs {
			// 白名单口径与 CN 段、与请求侧 404 判定共用 publishAllows 一处。
			if !h.publishAllows(published, "global", "workbuddy", id) {
				continue
			}
			// 池级可服务性：与 CN 段同一口径（global 段的昂贵档被上游拒参数不在此列——
			// 11133 类不自动标记；只有权威「无此模型」才标记，见 modeldead.go）。
			if h.modelDead.active("global", "workbuddy", id) {
				continue
			}
			// 富条目（探测对象形态）能给出 credits → 模型名后缀才能按真实倍率走；
			// 窄表（只有 ID 名单）没有倍率信息，后缀整体省略（不编造）。
			gmi, hasInfo := globalInfos[id]
			gCredits := ""
			if hasInfo {
				gCredits = gmi.Credits
			}
			entry := map[string]any{
				"id":       h.modelID("global", id, gCredits),
				"object":   "model",
				"created":  1753600000,
				"owned_by": "workbuddy",
			}
			// context_length / max_output_tokens 四级查找（upstream.model_catalog，
			// 与 CN 动态分支同口径）：探测富条目真实值权威 → 静态种子表 →
			// model.json 缓存 → models.dev 按需拉取（异步）→ 1M 兜底/省略。
			// 裸 ID 条目（窄表探测）也经种子表补齐，不再裸 131072。
			var remoteCtx, remoteOut int64
			if hasInfo {
				entry = h.applyModelInfoFields(entry, gmi)
				remoteCtx, remoteOut = gmi.ContextWindow, gmi.MaxTokens
			}
			entry["context_length"] = upstream.ContextWindowListingV4(id, remoteCtx, h.cfg.Upstream.HTTP)
			if mo, ok := upstream.MaxOutputTokensListingV4(id, remoteOut, h.cfg.Upstream.HTTP); ok {
				entry["max_output_tokens"] = mo
			}
			if efforts, def := upstream.EffortListing("global", id, globalEfforts[id], globalDefaults[id]); efforts != nil {
				entry["reasoning_supported_efforts"] = efforts
				if def != "" {
					entry["reasoning_default_effort"] = def
				}
			}
			out = append(out, entry)
		}
	}
	return out
}

// modelKey 输出白名单的键。CN（workbuddy）沿用裸 id——与接入本功能之前存的配置
// 保持一致；其余 producer 用 "<producer>:<裸 id>"。
//
// 为什么必须带 producer：白名单是按 id 存的集合，两家的裸 id 会重名
// （CN 有 cn:glm-4.6，zcode 实时目录也有 glm-4.6），共用裸 id 当键会出现
// 「勾了 zcode 的 glm-4.6 把 CN 的也一起放开」这种串台。full_id（zcode:glm-4.6）
// 是给客户端看的，拿它当键又会被用户改的 model_prefix 带偏，所以单独定义一个键。
func modelKey(producer, id string) string {
	if producer == "" || producer == "workbuddy" {
		return id
	}
	return producer + ":" + id
}

// globalModelKey 国际版（wba）模型在发布清单里的键。"global:" 前缀与 CN 的裸 id 分开，
// 两家同名的型号（fast-model / glm-5.3 …）才能各留一把开关。
func globalModelKey(id string) string { return "global:" + id }

// publishAllows 发布清单放行判定：/v1/models 列不列、请求 404 不 404，都必须走这一处。
// 两侧口径分叉过：global 段曾只认裸 id，于是「清单里勾了 global:x」在列表里看得见、
// 一调就被 404 挡回去——列表与可调用性必须是同一口径。
//
// realm=global 用 "global:<裸 id>" 当键；CN 与 zcode 沿用 modelKey。裸 id 回退只在
// ModelsRealmScoped=false 的旧清单上生效（那时国际版跟着 CN 的裸 id 共用一把开关），
// 新清单（面板保存，ModelsRealmScoped=true）两域完全独立，同名型号各留一把开关。
func (h *Handler) publishAllows(published map[string]bool, realm, producer, id string) bool {
	if published == nil {
		return true
	}
	if realm != "global" {
		if published[modelKey(producer, id)] {
			return true
		}
		// 裸 id 回退：老清单（面板早期版本、或脚本批量写入的存档）存的是裸 id 而不是
		// modelKey 的 "zcode:<id>" 形态，于是 publishAllows 一律判否——白名单里明明勾了
		// glm-5.3-flash，/v1/models 却一个 zcode 模型都不吐，ZCode 专用口直接空掉。
		// 实测 7864 主口 42 个模型里 zcode 段为 0、7870 吐 0 个，就是这个键口径分叉。
		//
		// 回退只在「整份清单里一个带 producer 段的键都没有」时才生效（即确属老形态清单）。
		// 否则新形态清单下同名的裸 id 可能是给别的来源勾的，无差别回退会串台。
		// workbuddy 本身就是裸 id，不进回退分支。
		if producer == "" || producer == "workbuddy" {
			return published[id]
		}
		prefix := producer + ":"
		for k := range published {
			if strings.HasPrefix(k, prefix) {
				return false
			}
		}
		return published[id]
	}
	if published[globalModelKey(id)] {
		return true
	}
	return !h.outputCfgRealmScoped() && published[id]
}

// outputCfgRealmScoped 白名单是否按域分开（见 OutputConfig.ModelsRealmScoped）。
func (h *Handler) outputCfgRealmScoped() bool {
	if h.cfg.Output != nil {
		return h.cfg.Output.Get().ModelsRealmScoped
	}
	return false
}

// outputCfgModelSet 当前输出白名单集合（nil = 全放）。没挂 OutputStore 时
// 从 Config 的静态前缀/费率兜底构造一次，与 outputCfg 同口径。
func (h *Handler) outputCfgModelSet() map[string]bool {
	if h.cfg.Output != nil {
		return h.cfg.Output.ModelSet()
	}
	c, err := normalizeOutput(OutputConfig{Format: FormatOpenAI, ModelPrefix: h.cfg.ModelPrefix, RateHint: h.cfg.RateHint})
	if err != nil {
		return nil
	}
	return modelSetOf(c.Models)
}

// availableOutputModels 输出侧可勾选清单：上游当前下发的模型 + 是否已选中。
// 只读 fetchDynamicModels 的 1h 缓存（不额外打上游）；无健康号/拉取失败 → 空数组。
// id 是裸 id（保存时回传这个），full_id 是客户端实际看到的模型名（realm + 输出前缀）。
func (h *Handler) availableOutputModels() []map[string]any {
	sel := h.outputCfgModelSet()
	rateHint := h.outputCfg().RateHint
	out := make([]map[string]any, 0)
	// workbuddy（CN 动态目录）与 zcode（智谱目录）各出一组，带 producer 字段——
	// 「哪一行是谁家能力」在输出侧也必须一眼可见；两家的模型名可能重名，
	// 前端按 producer 分组展示（见 webui/index.html）。
	type group struct {
		producer string
		infos    []upstream.ModelInfo
	}
	// zcode 的 /models 是**整家厂商的目录**（所有 GLM 型号），不等于号池里的号能跑什么
	// （周末活动只送 glm-5.3-flash，目录照样列 11 个）。所以每个条目额外带 entitled 标
	// 记：前端把没额度的收进折叠区并标「未授权」，对外 /v1/models 也据此收敛。workbuddy
	// 的目录本身就是账号口径，恒为 true。
	zcEnt, zcKnown := h.zcodeEntitledModels()
	for _, g := range []group{
		{"workbuddy", h.fetchDynamicModels()},
		{upstream.ProducerZCode, h.fetchZCodeCatalog()},
		{upstream.ProducerOpenCode, h.fetchOpenCodeCatalog()},
		{upstream.ProducerKilo, h.fetchKiloCatalog()},
	} {
		for _, mi := range g.infos {
			if mi.ID == "" {
				continue
			}
			pid := ""
			if g.producer != "workbuddy" {
				pid = g.producer
			}
			entitled := true
			if g.producer == upstream.ProducerZCode && zcKnown {
				entitled = zcEnt[mi.ID]
			}
			key := modelKey(g.producer, mi.ID)
			// selected = 「这条现在真的会发出去吗」，口径必须与 modelList 完全一致：
			// 全放（sel==nil）时 zcode 未授权的那些本来就不发（见 modelList 同一处收敛），
			// 所以勾选态要如实为 false——否则面板一打开就显示「未授权也全勾着」，用户随手
			// 一保存就把它们写进白名单，反而真的把 11 条全发出去。
			selected := h.publishAllows(sel, "cn", g.producer, mi.ID)
			if g.producer == upstream.ProducerZCode && sel == nil && zcKnown && !entitled {
				selected = false
			}
			e := map[string]any{
				"id":       mi.ID,
				"key":      key,
				"full_id":  h.modelIDFor("cn", pid, mi.ID, mi.Credits),
				"producer": g.producer,
				"selected": selected,
				"entitled": entitled,
				// free 该模型在这条通道上是否**不消耗积分**（口径见 upstream.ModelInfo.Free）。
				// 管理页「只看免费」靠它筛行：opencode Zen 的目录是整家厂商的（79 个），
				// 用户实际能用的是免费层那一小撮，不筛就得在 79 行里翻。
				"free": mi.Free,
			}
			if mi.Name != "" {
				e["name"] = mi.Name
			}
			if mi.Credits != "" {
				e["credits"] = mi.Credits
			}
			if suf := RateSuffix(rateHint, mi.Credits); suf != "" {
				e["suffix"] = suf // 客户端实际会看到的模型名后缀
			}
			if mi.IsDefault {
				e["is_default"] = true
			}
			out = append(out, e)
		}
	}
	// global（wba / workbuddy.ai）目录：CN 与 global 的型号**不是同一批**（实测 CN 45
	// 个、global 27 个，16 个只有国际版有：gpt-6-astra / gpt-5.6-* / gemini-3.5-flash /
	// grok-4.7 …）。不列进来，用户在清单里看不见它们，一保存白名单就会把国际版独有的
	// 型号静默砍掉。key 用 "global:<id>"，与 CN 的同名型号（fast-model、glm-5.3 …）
	// 各留一把开关，避免「勾了国内版把国际版一起放开」。
	if h.cfg.GlobalEnabled {
		gids, gacct := h.fetchGlobalModels()
		ginfos := map[string]upstream.ModelInfo{}
		for _, gi := range h.cfg.Upstream.FetchGlobalModelInfos(gacct) {
			ginfos[gi.ID] = gi
		}
		for _, id := range gids {
			if id == "" {
				continue
			}
			gm := ginfos[id]
			key := globalModelKey(id)
			e := map[string]any{
				"id":       id,
				"key":      key,
				"full_id":  h.modelID("global", id, gm.Credits),
				"producer": "workbuddy",
				"realm":    "global",
				"selected": h.publishAllows(sel, "global", "workbuddy", id),
				"entitled": true,
				"free":     gm.Free,
			}
			if gm.Name != "" {
				e["name"] = gm.Name
			}
			if gm.Credits != "" {
				e["credits"] = gm.Credits
			}
			if suf := RateSuffix(rateHint, gm.Credits); suf != "" {
				e["suffix"] = suf
			}
			if gm.IsDefault {
				e["is_default"] = true
			}
			out = append(out, e)
		}
	}
	// 有额度、但上游目录里没列出来的型号（少见）也得让用户勾得到，否则「有额度却发不出去」。
	if zcKnown {
		seen := map[string]bool{}
		for _, e := range out {
			if p, _ := e["producer"].(string); p == upstream.ProducerZCode {
				if id, _ := e["id"].(string); id != "" {
					seen[id] = true
				}
			}
		}
		extra := make([]string, 0, len(zcEnt))
		for id := range zcEnt {
			if !seen[id] {
				extra = append(extra, id)
			}
		}
		sort.Strings(extra)
		for _, id := range extra {
			key := modelKey(upstream.ProducerZCode, id)
			out = append(out, map[string]any{
				"id":       id,
				"key":      key,
				"full_id":  h.modelIDFor("cn", upstream.ProducerZCode, id, ""),
				"producer": upstream.ProducerZCode,
				"selected": h.publishAllows(sel, "cn", upstream.ProducerZCode, id),
				"entitled": true,
			})
		}
	}
	return out
}

// fetchGlobalModels 返回 global 模型名单（纯动态探测结果）及被探测账号。
// 与 fetchDynamicModels（CN 侧）同语义不同归位：缓存/失败回落封在 upstream.FetchGlobalModels
// （内部 1h + 5min 负缓存）。本方法只负责"何时探测"：
//   - 池中无 global 账号 → 空名单 + nil 账号（不发起上游调用）；
//   - 有 global 账号 → 单账号 Pick（global 域谓词），交 upstream 探测。
//
// 返回的 acct 供调用方在同一账号上取富 ModelInfo（FetchGlobalModelInfos 与
// FetchGlobalModels 共享缓存，不会触发第二次上游探测）。
// GlobalEnabled=false 时 modelList 已不进入本分支（逃生门在调用方 gate）。
func (h *Handler) fetchGlobalModels() ([]string, *auth.Auth) {
	// producer 维度必须一起过滤：zcode 号的 realm 也是 cn（智谱国内域），
	// 只按 realm 选会把 zcode 号当成 global/CN 的 workbuddy 号去打 workbuddy 上游。
	acct := h.cfg.Pool.PickExcludingForProducerRealm(nil, "", "workbuddy", "global")
	if acct == nil {
		return nil, nil
	}
	return h.cfg.Upstream.FetchGlobalModels(acct), acct
}

// rewriteModel 把 outbound chat body 的 model 字段替换为 bare（保留其余字段原样）。
// 仅当 bare != 原 model 时由 chatCompletions 调用；body 不可解析时原样返回（不二次错误化）。
func rewriteModel(body []byte, bare string) []byte {
	if len(body) == 0 || bare == "" {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	if cur, ok := obj["model"].(string); !ok || cur == bare {
		return body
	}
	obj["model"] = bare
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// zcodeCatalogCache zcode（智谱）上游模型目录缓存：1h TTL + 5min 负缓存，与
// dynamicModelsCache 同形（纯动态，无静态兜底）。包级共享——测试用 resetModelsCache
// 一并清空（见 handler_test.go）。
var zcodeCatalogCache struct {
	sync.RWMutex
	infos    []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const zcodeCatalogTTL = time.Hour

// zcodeCatalogMaxAttempts 目录拉取最多试几个号。取 8：本机实测同批 zcode 号里既有
// 通（open.bigmodel.cn）也有 TLS 连不上（api.z.ai）与已失效 key（401），单号单试
// 成功率可能低到 3/7；目录是 1h 缓存，多试几个号一次换回「整家上游的模型名单都在」
// 很划算。tried 排除重复，所以这个上限就是「每个 zcode 号最多试一次」。
const zcodeCatalogMaxAttempts = 8

// fetchZCodeCatalog 从池中任一健康 zcode 账号拉智谱上游的**实时**模型清单，缓存 1h。
// 与 fetchDynamicModels 的差异只有两点：选号限定 zcode 生产者、拉取换成
// FetchZCodeModels（智谱 OpenAI 兼容的 /models）。失败同样只进负缓存，不 NoteError
// ——列模型失败 ≠ chat 通道坏了，不该跨界惩罚账号。
func (h *Handler) fetchZCodeCatalog() []upstream.ModelInfo {
	zcodeCatalogCache.RLock()
	if len(zcodeCatalogCache.infos) > 0 && time.Since(zcodeCatalogCache.fetched) < zcodeCatalogTTL {
		out := zcodeCatalogCache.infos
		zcodeCatalogCache.RUnlock()
		return out
	}
	if !zcodeCatalogCache.lastFail.IsZero() && time.Since(zcodeCatalogCache.lastFail) < modelsFetchFailCooldown {
		zcodeCatalogCache.RUnlock()
		return nil
	}
	zcodeCatalogCache.RUnlock()

	// 最多试 3 个号：目录拉取失败最常见的原因是「抽到的号本身打不通」——本机实测
	// 同一批 zcode 号里既有 open.bigmodel.cn（通）也有 api.z.ai（TLS 连不上）、还有
	// 已失效的 key（401）。单号单试的成功率约 3/7，一旦失败就进 5min 负缓存，整家
	// 上游的模型名单对所有客户端都消失（/v1/models 直接少一整段）。
	// 只试 3 个号是收窄的边界：这不是聊天路径，不值得为目录轮转整个池；但也不能
	// 因为一次坏号就假装这家上游不存在。失败原因经 WARN 落日志，不再静默。
	tried := map[string]bool{}
	var lastErr error
	attempts := 0
	for attempts < zcodeCatalogMaxAttempts {
		acct := h.cfg.Pool.PickExcludingForProducerRealm(tried, "", upstream.ProducerZCode, "")
		if acct == nil {
			break
		}
		attempts++
		tried[acct.UID] = true
		// 后台探测用 Background：拉目录没有请求级取消语义，也不该被某个客户端的断连牵连。
		infos, err := h.cfg.Upstream.FetchZCodeModels(context.Background(), acct)
		if err == nil && len(infos) > 0 {
			zcodeCatalogCache.Lock()
			zcodeCatalogCache.infos = infos
			zcodeCatalogCache.fetched = time.Now()
			zcodeCatalogCache.lastFail = time.Time{}
			zcodeCatalogCache.Unlock()
			return infos
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("上游返回空清单")
		}
	}
	if attempts == 0 {
		// 池里一个 zcode 号都没有：这是「这家还没接进来」，不是「这家拉挂了」。
		// 不能写失败缓存——否则用户导入 zcode 号之后的 5 分钟里目录一直是空的
		//（曾实测到：先看空池、再导入、目录要等负缓存过期才出现）。
		return nil
	}
	log.Printf("WARN: [server] zcode 模型目录拉取失败（已试 %d 个号）：%v", attempts, lastErr)
	zcodeCatalogCache.Lock()
	zcodeCatalogCache.lastFail = time.Now()
	zcodeCatalogCache.Unlock()
	return nil
}

// opencodeCatalogCache OpenCode（Zen）上游模型目录缓存：1h TTL + 5min 负缓存，
// 与 zcodeCatalogCache 同形（纯动态，无静态兜底）。缓存的是 Zen 全量清单（含免费层）
// 见 upstream.FetchOpenCodeModels。
var opencodeCatalogCache struct {
	sync.RWMutex
	infos    []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const opencodeCatalogTTL = time.Hour

// opencodeCatalogMaxAttempts 目录拉取最多试几个号（与 zcode 同口径：池里可能有失效
// key，单号单试失败不该让整家目录消失）。
const opencodeCatalogMaxAttempts = 4

// fetchOpenCodeCatalog 从池中任一健康 opencode 账号拉 Zen 上游的实时模型清单，缓存 1h。
// 与 fetchZCodeCatalog 同形：选号限定 opencode 生产者、拉取换成 FetchOpenCodeModels。
// 失败只进负缓存，不 NoteError——列模型失败 ≠ chat 通道坏了。
func (h *Handler) fetchOpenCodeCatalog() []upstream.ModelInfo {
	opencodeCatalogCache.RLock()
	if len(opencodeCatalogCache.infos) > 0 && time.Since(opencodeCatalogCache.fetched) < opencodeCatalogTTL {
		out := opencodeCatalogCache.infos
		opencodeCatalogCache.RUnlock()
		return out
	}
	if !opencodeCatalogCache.lastFail.IsZero() && time.Since(opencodeCatalogCache.lastFail) < modelsFetchFailCooldown {
		opencodeCatalogCache.RUnlock()
		return nil
	}
	opencodeCatalogCache.RUnlock()

	tried := map[string]bool{}
	var lastErr error
	attempts := 0
	for attempts < opencodeCatalogMaxAttempts {
		acct := h.cfg.Pool.PickExcludingForProducerRealm(tried, "", upstream.ProducerOpenCode, "")
		if acct == nil {
			break
		}
		attempts++
		tried[acct.UID] = true
		infos, err := h.cfg.Upstream.FetchOpenCodeModels(context.Background(), acct)
		if err == nil && len(infos) > 0 {
			opencodeCatalogCache.Lock()
			opencodeCatalogCache.infos = infos
			opencodeCatalogCache.fetched = time.Now()
			opencodeCatalogCache.lastFail = time.Time{}
			opencodeCatalogCache.Unlock()
			return infos
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("上游返回空清单")
		}
	}
	if attempts == 0 {
		// 池里一个 opencode 号都没有 = 这家还没接进来，不写失败缓存（与 zcode 同口径）。
		return nil
	}
	log.Printf("WARN: [server] opencode 模型目录拉取失败（已试 %d 个号）：%v", attempts, lastErr)
	opencodeCatalogCache.Lock()
	opencodeCatalogCache.lastFail = time.Now()
	opencodeCatalogCache.Unlock()
	return nil
}

// kiloCatalogCache Kilo 上游免费模型目录缓存：1h TTL + 5min 负缓存，与
// opencodeCatalogCache 同形（纯动态，无静态兜底）。缓存的是 /gateway/models 过滤
// 出 pricing 双零之后的清单（见 upstream.FetchKiloModels）。
var kiloCatalogCache struct {
	sync.RWMutex
	infos    []upstream.ModelInfo
	fetched  time.Time
	lastFail time.Time
}

const kiloCatalogTTL = time.Hour

// kiloCatalogMaxAttempts 目录拉取最多试几个号（与 zcode / opencode 同口径）。
const kiloCatalogMaxAttempts = 2

// fetchKiloCatalog 从池中那条 kilo 匿名号拉上游目录，缓存 1h。
// 失败只进负缓存，不 NoteError——列模型失败 ≠ chat 通道坏了（与 zcode/opencode 同）。
func (h *Handler) fetchKiloCatalog() []upstream.ModelInfo {
	kiloCatalogCache.RLock()
	if len(kiloCatalogCache.infos) > 0 && time.Since(kiloCatalogCache.fetched) < kiloCatalogTTL {
		out := kiloCatalogCache.infos
		kiloCatalogCache.RUnlock()
		return out
	}
	if !kiloCatalogCache.lastFail.IsZero() && time.Since(kiloCatalogCache.lastFail) < modelsFetchFailCooldown {
		kiloCatalogCache.RUnlock()
		return nil
	}
	kiloCatalogCache.RUnlock()

	tried := map[string]bool{}
	var lastErr error
	attempts := 0
	for attempts < kiloCatalogMaxAttempts {
		acct := h.cfg.Pool.PickExcludingForProducerRealm(tried, "", upstream.ProducerKilo, "")
		if acct == nil {
			break
		}
		attempts++
		tried[acct.UID] = true
		infos, err := h.cfg.Upstream.FetchKiloModels(context.Background(), acct)
		if err == nil && len(infos) > 0 {
			kiloCatalogCache.Lock()
			kiloCatalogCache.infos = infos
			kiloCatalogCache.fetched = time.Now()
			kiloCatalogCache.lastFail = time.Time{}
			kiloCatalogCache.Unlock()
			return infos
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("上游返回空清单")
		}
	}
	if attempts == 0 {
		// 池里没有 kilo 号 = 这家还没接进来，不写失败缓存（与 zcode/opencode 同口径）。
		return nil
	}
	log.Printf("WARN: [server] kilo 模型目录拉取失败（已试 %d 个号）：%v", attempts, lastErr)
	kiloCatalogCache.Lock()
	kiloCatalogCache.lastFail = time.Now()
	kiloCatalogCache.Unlock()
	return nil
}

// modelOwner 推断某裸模型名该走哪家上游。返回 "" = 不唯一/不确定，按 workbuddy。
//
// 判据只有一条：**zcode 目录里有、workbuddy 目录里没有** → zcode。两家都有 →
// ""（归 workbuddy，保持现状语义；要打 zcode 就显式写 "zcode:" 前缀）。
// 不做模糊匹配、不做前缀猜测——路由错家的代价（拿别家凭据打别家上游）远高于
// 让用户多打几个字符。
func (h *Handler) modelOwner(bare string) string {
	p, _ := h.modelOwnerEx(bare)
	return p
}

// modelOwnerEx 与 modelOwner 同判据，额外把「两家目录里都有」标成 ambiguous：
// 返回 ("", true) 表示真歧义（不是「确定归 workbuddy」）。两者在裸名路由里默认同结果
// （都按 workbuddy），区别只在出口只服务一家时——那时歧义按该家落地。
func (h *Handler) modelOwnerEx(bare string) (producer string, ambiguous bool) {
	if bare == "" {
		return "", false
	}
	zc := h.fetchZCodeCatalog()
	oc := h.fetchOpenCodeCatalog()
	// 两家静态目录都没接进来时直接归 workbuddy，**不拉 CN 目录**——与接入这两家之前的行为一致（那时 fetchZCodeCatalog 为空即早返回）。
	// fetchDynamicModels 会打上游探测，在「只有 workbuddy 一家」的常见形态下没必要为每次裸名解析付这笔探测。
	if len(zc) == 0 && len(oc) == 0 {
		return "", false
	}
	inWB := containsModelID(h.fetchDynamicModels(), bare)
	inZC := len(zc) > 0 && containsModelID(zc, bare)
	inOC := len(oc) > 0 && containsModelID(oc, bare)
	kl := h.fetchKiloCatalog()
	inKL := len(kl) > 0 && containsModelID(kl, bare)
	// 数「目录里有这个裸名的家数」：workbuddy 也是一家（与接入 opencode 之前的口径
	// 一致——那时 inWB&&inZC 就判歧义）。0 家 → 按 workbuddy（默认，不算歧义）；
	// 1 家且是 workbuddy → 同样按 workbuddy；1 家是别家 → 确定归它；≥2 家 → 真歧义。
	n := 0
	owner := ""
	if inWB {
		n++
	}
	if inZC {
		n++
		owner = upstream.ProducerZCode
	}
	if inOC {
		n++
		owner = upstream.ProducerOpenCode
	}
	if inKL {
		n++
		owner = upstream.ProducerKilo
	}
	switch {
	case n == 0:
		return "", false
	case n >= 2:
		return "", true
	case inWB:
		return "", false
	default:
		return owner, false
	}
}

// containsModelID 模型 id 线性查找（目录规模是几十条，不值得为它建索引；
// 且两个调用点都在 1h 缓存之上，不是热路径）。
func containsModelID(infos []upstream.ModelInfo, id string) bool {
	for _, mi := range infos {
		if mi.ID == id {
			return true
		}
	}
	return false
}

// containsFreeModel 同 containsModelID 的线性查找口径，额外要求该条目标了 Free。
// 与 containsModelID 并列而不是合成一个「返回 *ModelInfo」的查找：两处调用点都不需要
// 其余字段，返回指针会把「目录条目是不是拷贝」这种实现细节泄漏给调用方。
func containsFreeModel(infos []upstream.ModelInfo, id string) bool {
	for _, mi := range infos {
		if mi.ID == id {
			return mi.Free
		}
	}
	return false
}

// fetchDynamicModels 从池中任一健康 CN 账号拉模型列表（含 contextWindow/maxTokens），缓存 1h。
// 拉取失败记录时间戳进入 5min 负缓存，冷却期内直接返回 nil（纯动态，无静态表兜底），
// 避免反复打上游。
// 只从 (producer=workbuddy, realm=CN) 的账号拉取：全局账号的模型列表未必与 CN 一致，
// 动态模型表只服务 CN 前缀（global 走独立探测）；zcode 号另有独立目录（fetchZCodeCatalog）。
func (h *Handler) fetchDynamicModels() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	if len(dynamicModelsCache.ids) > 0 && time.Since(dynamicModelsCache.fetched) < dynamicModelsTTL {
		out := dynamicModelsCache.ids
		dynamicModelsCache.RUnlock()
		return out
	}
	// 失败负缓存：冷却期内不再请求上游。
	if !dynamicModelsCache.lastFail.IsZero() && time.Since(dynamicModelsCache.lastFail) < modelsFetchFailCooldown {
		dynamicModelsCache.RUnlock()
		return nil
	}
	dynamicModelsCache.RUnlock()

	// producer 维度必须一起过滤（见 fetchGlobalModels 同名注释）：zcode 号的
	// realm 也是 cn，只按 realm 选会把「智谱凭据」拿去打 workbuddy 的 /v3/config。
	acct := h.cfg.Pool.PickExcludingForProducerRealm(nil, "", "workbuddy", "cn")
	if acct == nil {
		// 没有 CN 号 = 这家还没接进来，不写失败缓存（与 fetchZCodeCatalog 同口径）。
		return nil
	}
	infos, err := h.cfg.Upstream.FetchModels(acct)
	if err != nil || len(infos) == 0 {
		// 拉取失败只进负缓存（5min lastFail），不 NoteError（P1-6/发现 6）：
		// NoteError 喂的是 chat 熔断器，models 端点偶发 5xx 跨界惩罚 chat 通道
		// 健康的账号；models 拉取失败 ≠ 账号 chat 不可用。
		dynamicModelsCache.Lock()
		dynamicModelsCache.lastFail = time.Now()
		dynamicModelsCache.Unlock()
		return nil
	}
	dynamicModelsCache.Lock()
	dynamicModelsCache.ids = infos
	dynamicModelsCache.fetched = time.Now()
	dynamicModelsCache.lastFail = time.Time{} // 成功则清空负缓存
	dynamicModelsCache.Unlock()
	return infos
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	h.chatEndpoint(w, r, protocolChat)
}

// responsesEndpoint POST /v1/responses（OpenAI Responses 出口）。
//
// 与 chatCompletions 是**同一条主干**的两个入口：请求体先翻成 Chat 形态，
// 出口再按 Responses 事件序列编码。号池/轮转/重试/粘性/统计全部共用，不另起一套。
func (h *Handler) responsesEndpoint(w http.ResponseWriter, r *http.Request) {
	h.chatEndpoint(w, r, protocolResponses)
}

// chatProtocol 本次请求的对外协议（决定入口翻译与出口编码）。
type chatProtocol int

const (
	protocolChat      chatProtocol = iota // /v1/chat/completions（OpenAI 兼容）
	protocolResponses                     // /v1/responses（OpenAI Responses）
)

// chatEndpoint chat 协议主干：读体 →（按协议翻译）→ realm/producer 路由 → 会话粘性
// → 提示词改写 → 轮转选号 → 上游调用 → 错误分类/冷却 → 出口编码。
// 协议差异只有两处：入口的 responses.RequestToChat、出口的 chatSink。
func (h *Handler) chatEndpoint(w http.ResponseWriter, r *http.Request, proto chatProtocol) {
	// 请求体无大小上限（max_body_mb 已移除）：完整读入，超限类问题交由上游自然返回
	// 错误（其响应经既有错误分类链路透出，信息量更大）。#41 的截断防御语义保留在
	// 读错误路径——移除预拦截后，截断只可能来自客户端自己断流，读 body 出错就地 400，
	// 不把半截 JSON 喂上游 unmarshal 报 unexpected EOF 冤枉罚号。
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	// 调试开关：设置 WB2A_DUMP_REQ 即把上游侧收到的原始请求体落盘，供离线二分定位指纹命中行。
	// 仅在排查上游指纹拦截时开启；不设置时零开销、不落盘。
	// 只落"大请求"（≥4MB 固定阈值，原 max_body_mb/2 语义的接替）：小探针
	// （{"input":"hi"} 之类）会覆盖掉真正要看的对话请求。
	if os.Getenv("WB2A_DUMP_REQ") != "" && len(raw) >= dumpReqMinBytes {
		if err := os.WriteFile("/app/data/last_request.json", raw, 0o600); err != nil {
			log.Printf("ERR: [server] dump req: %v", err)
		}
	}
	// Responses 入口：先把请求体翻成 Chat 形态，后续路由/粘性/统计/轮转全部基于
	// 翻译后的 Chat body（与 /v1/chat/completions 走完全同一条路径）。
	// WB2A_DUMP_REQ 落盘的是**客户端原始**请求体（raw），排障时看到的就是客户端发的字节。
	body := raw
	if proto == protocolResponses {
		conv, cerr := responses.RequestToChat(raw)
		if cerr != nil {
			writeOpenAIErrorHint(w, http.StatusBadRequest, "invalid_request",
				"无法把 Responses 请求翻译成 Chat 请求："+cerr.Error(),
				"请求体需符合 OpenAI Responses 规范（至少 model + input）；也可改用 /v1/chat/completions")
			return
		}
		body = conv
	}

	var peek struct {
		Stream bool   `json:"stream"`
		Model  string `json:"model"`
	}
	_ = json.Unmarshal(body, &peek)

	// 出口编码器：chat 原样透传；responses 翻译成 Responses 对象/事件流。
	// model 传 peek.Model（客户端原始请求里的名字，含前缀）——Responses 对象里的
	// model 字段要原样回显客户端自己写的名字。
	var sink chatSink = chatSinkOpenAI{model: peek.Model}
	if proto == protocolResponses {
		sink = chatSinkResponses{model: peek.Model}
	}

	// realm 前缀解析（D6）：model 名可能带 "[realm:]" 前缀。剥出 realm + bareModel，
	// bareModel 用于选号/粘性/账本/出站 body 重写（前缀是网关侧路由协议，上游只认裸名）。
	// 裸名 → ("cn", 原串)，CN 现状零回归。
	realm, producer, bareModel := h.resolveRoute(peek.Model)

	// 多出口：出口只服务自己范围内的来源。跨口请求一律 404——否则「zcode 专用口」
	// 还能被裸名 cn:auto 打通，modelList 的过滤就形同虚设。错误信息直接告诉用户
	// 该去哪儿调，而不是一个干巴巴的 404。
	if !h.producerAllowed(producer) {
		allowed := strings.Join(h.cfg.ProducerAllow, ", ")
		if allowed == "" {
			allowed = "全部来源"
		}
		writeOpenAIErrorHint(w, http.StatusNotFound, "model_not_found",
			"模型 "+peek.Model+" 不在本出口的服务范围（本出口只服务："+allowed+"）",
			"换用本出口发布的模型名，或改用主口/对应来源的出口")
		return
	}

	// 发布清单：只在「输出 API」页勾上的模型才对外可用（空清单 = 全放，默认零回归）。
	// 放在来源判定之后，错误信息才不至于驴唇不对马嘴——先告诉用户「这个口不服务这一家」，
	// 再说「这一家也没勾这个模型」。少了这一道，勾选就只是「列表过滤器」：客户端拿着
	// 清单外的旧名字照样能穿透，「发布清单」名不副实。
	if published := h.outputCfgModelSet(); !h.publishAllows(published, realm, producer, bareModel) {
		writeOpenAIErrorHint(w, http.StatusNotFound, "model_not_found",
			"模型 "+peek.Model+" 不在本网关的发布清单里",
			"在「输出 API」页 →「模型发布清单」里把它勾上，或改用清单里已有的模型名")
		return
	}

	// 池级死模型快速失败：上游权威答复过「所有账号均无此模型」的，不再烧轮转
	// （换号注定同样失败）。放在发布清单判定之后——错误语义逐层收窄：先范围、
	// 再清单、再可服务性。TTL 到期标记自动失效，请求会重新进轮转实测。
	if h.modelDead.active(realm, producer, bareModel) {
		writeOpenAIErrorHint(w, http.StatusServiceUnavailable, "model_unavailable",
			"模型 "+peek.Model+" 已被标记为池级不可用（上游所有账号均无此模型），标记将于 24h 后自动过期复测",
			"从 /v1/models 选一个在列模型；若认为标记有误，DELETE /admin/modeldead 清除后重试")
		return
	}

	// 请求级统计：出口即打一行表格日志（任何路径都会走到）。
	st := newChatStat(time.Now(), body, peek.Stream)
	// 按来源统计：producer 在这里已经解析出来（含「裸名推断」与「单来源口兜底」），
	// 直接记进统计对象——再晚就只剩模型名，来源维度就丢了。
	st.producer = producer
	defer st.done()

	tried := map[string]bool{}
	var lastErr error

	// 会话粘性：从请求体提取会话键并解析绑定号（找不到/无效则 stickyUID 为空，走普通轮换）。
	// 按模型解析：同一个会话可能换模型，绑定号若在当前模型上被 6004 限额（对其他模型
	// 仍可用），必须重分配——否则会被钉在这个号上反复失败。
	// 提取与下方会话头族的聚合键共用同一结果，故**不受粘性开关影响**：粘性未启用
	// （Session==nil）时聚合键仍应是会话级，而不是退化成轮级。
	sessKey := session.ExtractKey(body)
	// stickyKey 是**粘性专用**键，与 sessKey（会话头族聚合用）分开：
	// sessKey 为空时（OpenAI 兼容客户端——dsh / Codex 等既无 conversationId 也无
	// metadata）用首条 user 消息派生会话级 fallback 键，使粘性仍能生效。
	// 不能直接改 sessKey：那会连带改变上游头族轮级复合键（sessKey 入键）的聚合
	// 语义，属于另一条链路的契约。
	stickyKey := sessKey
	if stickyKey == "" {
		stickyKey = session.StickyFallbackKey(body)
	}
	stickyUID := ""
	if h.cfg.Session != nil && stickyKey != "" {
		// 传给 ResolveForModel 的是**完整**模型名（peek.Model，含 realm 前缀）。
		// 粘性命中校验走 injected AvailableForModel 闭包 → 闭包内部 resolveModel 剥前缀
		// 得 realm+bare，再按 realm 过滤可用集合。若传已剥前缀的 bareModel，闭包对裸名
		// 恒剥出 realm=cn，跨 realm 粘性会话会被错误钉回 CN 集合；完整前缀才能让
		// 闭包正确过滤到 global 集合（见 cmd/server/wiring.go realmAwareAvailableForModel）。
		// 模型名也参与成本账本与选号过滤，不能用 "-" 占位污染模型键。
		// Lookup（而非 ResolveForModel）：只复用既有绑定，绝不在本处自行分配账号。
		// 新建会话时 stickyUID 为空 → 下方选中 acct 后由流末统一 Bind（见"粘性跟随
		// 最终成功号"），于是**选号策略只有 pool.pick 一条**：成本分层、快过期积分
		// 硬分层、免费模型不按余额加权全都生效。此前用 ResolveForModel 由 session
		// 包自行按"空闲号哈希"分配，绕过了全部选号策略（2026-09-30 修复）。
		if uid, ok := h.cfg.Session.Lookup(stickyKey, peek.Model); ok {
			stickyUID = uid
		}
	}

	// 轮级聚合键：按 body 里最后一条 user 消息派生（同轮内所有上游调用同键，
	// 换 user 消息换键）。#170 起带会话键的客户端也统一走轮级（与官方桌面 CLI 的
	// X-Conversation-Request-ID 轮级语义对齐），故不再限 sessKey=="" 才计算；
	// sessKey 由调用侧以复合键方式入键（防不同会话同轮文本互撞）。
	// 必须在下方 prompt.Rewrite / rewriteModel 之前取——改写会动 messages 内容。
	turnKey := session.TurnKey(body)

	// gateway_hint 判定所需的请求形态（image_url part）：在改写前取（与 turnKey
	// 同理）。11133「模型不支持图片」指向的前提。
	reqHasImage := hasImagePart(body)

	// 在途租约：成功选中即占名额；函数出口（含成功 return 与 panic）统一释放。
	var heldUID string
	defer func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
		}
	}()
	releaseHeld := func() {
		if heldUID != "" {
			h.cfg.Pool.Release(heldUID)
			heldUID = ""
		}
	}
	// unbindSticky 解绑当前会话粘性号（stickyUID 非空时）。供「粘性号不可用/被抢」与 fail 共用。
	// 幂等：stickyUID 已空则空操作；不会误解绑其他轮的绑定。仅当 Session != nil 时 stickyUID 才会非空。
	unbindSticky := func() {
		if stickyUID != "" {
			h.cfg.Session.Unbind(stickyKey)
			stickyUID = ""
		}
	}
	// fail 在轮转失败分支统一：释放租约 + 若失败号正是粘性号则解绑（下次请求重新分配）。
	fail := func(uid string) {
		releaseHeld()
		if stickyUID != "" && uid == stickyUID {
			unbindSticky()
		}
	}

	// 系统提示词改写（出站前、轮转前；每个请求一次）。
	//   - custom：用自有提示词替换客户端 system/developer（从源头消灭 system 指纹误报）。
	//   - append：开头连续 system/developer 块后插自有提示词，既有消息逐字不动
	//     （客户端项目规范/工具约定与网关提示词并用，issue #129）。
	//   - passthrough + 降级期：换 Degraded 中性提示词直达，不再先撞 400。
	//   - passthrough / append 非降级期：透传客户端原始 system（append 则再插一条网关 system）。
	// 降级裁决：append 在降级期退化为 replace（Rewrite(Degraded)）——append 带
	// 指纹原文重试是确定性再撞墙，replace 是一次性最小抢救（issue #129 设计 §4）。
	degradedApplied := false
	if h.cfg.PromptMode == "custom" && h.cfg.PromptText != "" {
		body = prompt.Rewrite(body, h.cfg.PromptText)
	} else if h.cfg.PromptMode == "append" && h.cfg.PromptText != "" && !h.degrade.Active() {
		body = prompt.Append(body, h.cfg.PromptText)
	} else if (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && h.degrade.Active() {
		body = prompt.Rewrite(body, prompt.Degraded)
		degradedApplied = true
	}

	// outbound model 名重写为 bareModel（D6）：realm 前缀是网关侧路由协议，
	// 上游不认前缀（global 账号也请求裸模型名）。裸名时 bareModel==peek.Model 恒等。
	if bareModel != peek.Model {
		body = rewriteModel(body, bareModel)
	}

	// 会话头族（issue #35 / #170）：后台按 X-Conversation-Request-ID 聚合请求，官方
	// 客户端一次 user send 内所有 tool call/重试/换号复用同一个 ID。**统一轮级**
	// （对齐官方桌面 CLI：TraceStartHook 每次 USER_PROMPT_SUBMIT 清空重生成
	// conversationRequestId，同轮内复用、跨轮必换；官方云链路 lfConvReqId 的会话级
	// 是服务端指令，走本网关的客户端不属于该形态）。此处**轮转循环外**生成一次，
	// 循环内每次出站原样复用 → 换号/重试/降级全部同 ID，后台不再碎片化（此前网关
	// 一个都不发，上游按 HTTP 请求逐条记账，同一对话几十上百个 RequestID）。
	//   - conversationID：body 提取（透传客户端原值，缺省空串——不伪造，见
	//     ResolveConversationID；官方后台不校验一致，空会话则不建立聚合键）；
	//   - conversationRequestID：入站 X-Conversation-Request-ID 透传优先（客户端已
	//     有自己的对话轮 ID 则以客户端为准）。派生分两态：
	//     * turnKey 非空（有末条 user 消息）→ 带会话键客户端走 TurnRequestID(
	//       sessKey+":"+turnKey) 复合键（会话段入键保证不同会话同轮文本不互撞，
	//       轮级粒度对齐官方 CLI）；无会话键客户端走既有 TurnRequestID(turnKey)
	//       纯轮级键（存量会话键值零漂移）。
	//     * turnKey 为空（残留空态：无 user 消息/无可签名内容）→ sessKey 非空时
	//       回落 RequestIDForKey(sessKey)（会话级兜底，好于请求级随机）；sessKey
	//       也空走 NewMessageID 请求级（TurnRequestID 空键行为）——轮转内捕获
	//       一次即共享。
	//   - messageID 在 ChatHeaders 内每条消息生成（消息级独立，无需外部可见）。
	chatMeta := upstream.ChatMeta{ConversationID: session.ResolveConversationID(body)}
	if v := r.Header.Get("X-Conversation-Request-ID"); v != "" {
		chatMeta.ConversationRequestID = v
	} else if turnKey != "" && sessKey != "" {
		// 轮级复合键：sessKey 入键防跨会话同轮文本互撞。
		chatMeta.ConversationRequestID = session.TurnRequestID(sessKey + ":" + turnKey)
	} else if turnKey != "" {
		// 无会话键客户端：纯轮级键（既有兜底语义不变，存量会话键值零漂移）。
		chatMeta.ConversationRequestID = session.TurnRequestID(turnKey)
	} else if sessKey != "" {
		// 残留空态兜底：无轮可聚合时维持会话级聚合（同会话恒同值）。
		chatMeta.ConversationRequestID = session.RequestIDForKey(sessKey)
	} else {
		// 无会话键也无轮级键：请求级随机（轮转内捕获一次即共享）。
		chatMeta.ConversationRequestID = session.TurnRequestID("")
	}
	chatMeta.TraceID = r.Header.Get("X-Trace-ID")

	for i := 0; i < h.cfg.MaxRotate; i++ {
		// 选号：粘性号优先（PickByUIDForModel 已校验该模型可用性 + 在途未满），否则普通轮换。
		var acct *auth.Auth
		if stickyUID != "" {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
			// producer 谓词与 realm 谓词同层：粘性绑定号若属于另一家上游（会话跨客户端
			// 换了模型名，或裸名归属变了），必须解绑重分配——把 zcode 的号钉在
			// workbuddy 模型上会拿智谱凭据打 codebuddy 上游，属静默跨家错路由。
			if acct == nil || (realm != "" && acct.Realm() != realm) ||
				(producer != "" && acct.Producer() != producer) {
				// 粘性号在当前模型不可用（冷却/占满/该模型被 6004 限额）或 realm 不符 → 解绑。
				unbindSticky()
			}
		}
		if acct == nil {
			// 模型感知 + realm 感知选号：模型非空时启用 6004 模型级冷却豁免
			// （healthyForModel），realm 谓词过滤跨域账号。
			// freeModel：零积分模型跳过成本分层并按「不读余额」的权重选号（见
			// pool.weightOfFree）——免费调用不扣积分，按余额加权只会把流量堆到
			// 余额高的号上白耗它的上游限额。判定只读目录缓存，每轮多一次线性查找。
			acct = h.cfg.Pool.PickExcludingForProducerRealmFree(
				tried, bareModel, producer, realm, h.modelFree(realm, producer, bareModel))
		}
		// 选号失败分两种：池里真没号（冷却/禁用/来源不符）→ 立即 503；有健康号只是被并发
		// 占满在途名额 → 短暂排队等名额释放（单账号池突发并发不再成片 503）。
		if acct == nil {
			acct = h.waitForSlot(r.Context(), tried, bareModel, producer, realm)
		}
		if acct == nil {
			st.status = http.StatusServiceUnavailable
			break
		}
		// 占用在途名额：Pick 已跳过满额账号，此处 CAS 兜底并发抢名额的竞态。
		if !h.cfg.Pool.Acquire(acct.UID) {
			// 名额被并发抢走：本次**没有**真正打上游，不能算「这个号试过」——撤销
			// tried 标记。否则单账号池会把自己唯一可用的号排除在候选外，后续选号
			// 恒为 nil，突发并发成片 503（本轮实测复现）。
			delete(tried, acct.UID)
			// 若被抢的正是粘性号，立即解绑并回落普通轮换，避免下一轮仍撞同一个
			// 满载粘性号再浪费一次粘性命中往返（语义与 fail()/粘性命中-nil 的解绑一致）。
			if stickyUID != "" && acct.UID == stickyUID {
				unbindSticky()
			}
			// 有健康号只是名额占满 → 有界等待名额释放（单账号池突发并发的背压）；
			// 等不到（池里真没可等号/超时/断连）再走轮转退避换号。
			if w := h.waitForSlot(r.Context(), tried, bareModel, producer, realm); w != nil {
				acct = w
				if !h.cfg.Pool.Acquire(acct.UID) {
					// 等到的名额又被并发抢走：撤销标记并回退轮转。
					delete(tried, acct.UID)
					if !rotateBackoff(i, r.Context()) {
						break
					}
					continue
				}
			} else {
				if !rotateBackoff(i, r.Context()) {
					// 客户端已断连：换号重试无意义，终止轮转走末端错误透传。
					break
				}
				continue // 最后一个名额被并发抢走且无可等号 → 换号
			}
		}
		st.uid = acct.UID
		// 同步昵称：请求流水行只写 uid8 时无法直观看是哪一号，昵称随本次选号带入日志行。
		st.nick = acct.Nickname
		// tried 只在**成功占到在途名额**后标记：它表达「这个号本轮已真实使用」
		// （失败换号时要跳过），不是「pick 曾提名过」。
		tried[acct.UID] = true
		heldUID = acct.UID

		// token 临近过期 → 先 refresh（失败冷却换号）
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if err := h.cfg.Upstream.RefreshToken(acct); err != nil {
				lastErr = err
				var ue *upstream.Error
				if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
					// 12153 一次失败不杀号（临时触发会误杀）：与 scheduler keepalive/checkin
					// 同口径走连续计数，达到 sessionDeadThreshold 才禁用。
					h.cfg.Pool.NoteSessionDead(acct.UID)
				} else {
					h.cfg.Pool.NoteError(acct.UID)
				}
				fail(acct.UID)
				if !rotateBackoff(i, r.Context()) {
					break // ctx 取消：终止轮转（refresh 失败换号退避，WAF P0-2）
				}
				continue
			}
			acct.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := acct.SaveAtomic(); err != nil {
				// 刷新成功但落盘失败：下次启动会用旧 token，必须暴露
				log.Printf("ERR: [server] chat refresh acct=%s: save auth failed: %v", logfmt.Label(acct.UID, acct.Nickname), err)
			}
		}

		// 客户端 IP 透传（仅 PassthroughIP 开启）：按请求取首段作为参数传入 ChatStream，
		// 不再读写共享字段——并发请求各自携带独立 IP，互不串扰（issue：ClientIP 竞态）。
		var clientIP string
		if h.cfg.Upstream.PassthroughIP {
			clientIP = upstream.ExtractClientIP(r)
		}
		// 传 r.Context()：客户端断连/请求取消立即中断在途上游调用并释放租约，
		// 不再让"幽灵请求"占满账号在途名额直到 IdleTimeout。
		rc, status, respBody, terr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, body, clientIP, chatMeta)
		// 分类信封一次成型：upstream 已在错误路径返回 *upstream.Error（Kind +
		// Retry-After 头解析，见 ChatStreamContext 注释）。传输层错误（非 *Error）走
		// 抖动换号分支；防御分支（terr 为 nil 但 status>=400，如 ErrNone 兜底）回落
		// 本地 Classify，双保险不改变语义。
		var uerr *upstream.Error
		if errors.As(terr, &uerr) {
			status = uerr.Status
		}
		if uerr == nil && terr != nil {
			// 网络层抖动：只换号，不喂熔断计数（传输层错误对连续失败连坐熔断过于严苛）。
			// 连败兜底（issue #114）：喂连败计数——连不上上游是「不知道原因的失败」，
			// 连败 N 次临时出池，单次/偶发不罚（NoteFailures 内部达阈才动作）。
			// 上游 client 已打 transport error 日志。
			st.status = http.StatusServiceUnavailable
			lastErr = terr
			h.cfg.Pool.NoteFailures(acct.UID)
			fail(acct.UID)
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（传输层错误换号退避，WAF P0-2）
			}
			continue
		}
		if status >= 400 {
			st.status = status
			var kind upstream.ErrKind
			if uerr != nil {
				kind = uerr.Kind
			} else {
				kind = upstream.Classify(status, string(respBody))
				uerr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody)}
			}
			// 内容拦截误报（passthrough/append 模式首遇）：判定为 system 指纹误报，
			// 触发降级到次日 00:00 CST，换 Degraded 中性提示词同请求内重试（append
			// 降级重试同样退化为 replace——原文在场只会确定性再撞 400）。
			// 第二次仍被拦（用户内容本身触发审核）→ 回内容防火墙错误（见下分支）。
			// 内容问题非账号问题：applyErrorPolicy 不罚账号（见 ErrContentBlocked 分支）。
			if kind == upstream.ErrContentBlocked && (h.cfg.PromptMode == "passthrough" || h.cfg.PromptMode == "append") && !degradedApplied {
				h.degrade.Trigger()
				body = prompt.Rewrite(body, prompt.Degraded)
				degradedApplied = true
				delete(tried, acct.UID) // 单账号池也能拿到重试机会（降级重试占一次名额）
				releaseHeld()
				log.Printf("WARN: [server] content-blocked (likely fingerprint false positive) -> degraded prompt retry")
				continue
			}
			if kind == upstream.ErrContentBlocked {
				// 内容命中网关内容防火墙：立即回客户端，**不轮转**——换任何账号都会撞同一
				// 审核，轮转纯属浪费时间。不罚账号（ErrContentBlocked 分支无冷却/熔断/NoteError）。
				// error-passthrough：message 装上游 body 原文（code/msg/requestId 原样，
				// 任务书授权上游错误码/账号语义对客户端可见），不再改写成网关固定文案。
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					// 空 body 兜底：无上游原文可透传，保留可读分类文案（不编造原文）。
					msg = "content blocked by upstream content firewall"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "content_blocked", msg,
					h.hintOf(upstream.ErrContentBlocked, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11115「prompt is too long」：立即透传上游原文回客户端，**不罚号不轮转**
			// ——上下文超限是请求的问题（同一 body 换任何号都超限，白扔健康号配额；
			// 与 WAF IP fail-fast 同哲学：确定与账号无关的错误直接终止轮转）。
			// applyErrorPolicy ErrPromptTooLong 分支零动作（不冷却/不熔断/不 NoteError，
			// 不喂连败），fail 只释放租约。error-passthrough：message 装上游 body 原文
			// （code/msg/requestId 原样，含真实 token 数与上限值——上游原文是最有价值
			// 的错误信息，客户端必须看到，禁止固定词覆盖）。
			if kind == upstream.ErrPromptTooLong {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				writeOpenAIErrorHint(w, http.StatusBadRequest, "prompt_too_long", promptTooLongMessage(string(respBody)),
					h.hintOf(upstream.ErrPromptTooLong, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 图片格式/数据无效：立即透传上游原文回客户端，不罚号不轮转。
			// 同一 body 换账号仍是同样的解析结果，轮转只会放大无效请求。
			if kind == upstream.ErrImageInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "image request was rejected by upstream"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "image_invalid", msg,
					h.hintOf(upstream.ErrImageInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// 11133 model_param_invalid：立即透传上游原文回客户端，不罚号不轮转。
			// 同一份 body 换任何账号都会被同样拒绝（模型侧能力边界），轮转只会把
			// 一次请求放大成 MaxRotate 次无效上游调用，最后还兜底成误导性的 503。
			if kind == upstream.ErrModelParamInvalid {
				h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
				fail(acct.UID)
				msg := string(respBody)
				if strings.TrimSpace(msg) == "" {
					msg = "request parameters were rejected by the model provider"
				}
				writeOpenAIErrorHint(w, http.StatusBadRequest, "model_param_invalid", msg,
					h.hintOf(upstream.ErrModelParamInvalid, string(respBody), bareModel, reqHasImage, uerr))
				st.status = http.StatusBadRequest
				return
			}
			// lastErr 携带完整 body（uerr.Msg 在 upstream 侧截断 200 字符，透传语义
			// 5755fe3 要求原文全量）+ Kind/RetryAfter（末端映射与冷却时长共用）。
			lastErr = &upstream.Error{Kind: kind, Status: status, Msg: string(respBody), RetryAfter: uerr.RetryAfter}
			h.applyErrorPolicy(acct.UID, kind, string(respBody), bareModel, uerr)
			fail(acct.UID)
			// WAF IP 级 fail-fast（优先于 rotateBackoff 退避——IP 级拦截时退避无意义，
			// 任务书设计纪律）：该次 WAF 403 喂入 IP 级状态机，若激活（短窗多号命中，
			// IP 被拦而非账号）则立即终止轮转——继续换号只会把请求放大 MaxRotate 倍
			// 打同一出口 IP，加重风控。账号级软冷却已在上方 applyErrorPolicy 照常记账
			// （单号偶发 403 仍冷却），IP 级状态只改变「是否继续轮转」——协同不叠加。
			if kind == upstream.ErrWafBlock && h.wafIP.noteWaf(acct.UID) {
				break
			}
			if !rotateBackoff(i, r.Context()) {
				break // ctx 取消：终止轮转（分类错误换号退避，WAF P0-2）
			}
			continue
		}
		h.cfg.Pool.NoteSuccess(acct.UID)
		// 11102 负缓存清命：该账号该模型实测成功，立即解除避让（不必等 TTL 到期）。
		// BlockModelClear 按 "11102" reason 前缀识别，只清 11102 条目、不碰 6004 独立冷却。
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		// 粘性跟随最终成功号：本轮成功的账号成为该会话的粘性绑定（覆盖旧绑定）。
		// 若 sticky 号失败、轮换到别的号成功，这里把会话重绑到新号，多轮对话下一跳不再随机抽。
		if stickyKey != "" && h.cfg.Session != nil {
			h.cfg.Session.Bind(stickyKey, acct.UID)
		}
		if peek.Stream {
			// 流式：透传结束后立即关闭上游 body，避免 defer 在轮转场景下堆积 fd。
			st.status = http.StatusOK
			stats := newChatStatsReaderSince(rc, st.start)
			// gateway_hint（SSE）：成功状态 200 已开流，中途 error 帧透传时附加
			// hint 字段（hintFn 惰性求值——正常流零开销，只有真撞到 error 帧才
			// 组装请求上下文做判定）。
			sErr := sink.Stream(w, stats, upstream.FrameHintFunc(func() upstream.HintContext {
				return h.hintContext(bareModel, reqHasImage)
			}))
			if upstream.IsEmptyStreamError(sErr) {
				// 上游 200 但空流（0 有效帧）：StreamHint 已写 error 帧 + [DONE]
				// 兜底（HTTP 头已发出只能 200），但这是上游缺陷不是成功——日志/
				// 状态收敛到 502 观测，与非流式 Aggregate 空流→502 upstream_parse
				// 同语义（此前 `_ =` 吞错把失败流记成 200，运维看到假成功）。
				// 只认 IsEmptyStreamError：客户端断连的写失败不误标（人已走，
				// 502 观测没有意义）。
				st.status = http.StatusBadGateway
				log.Printf("WARN: [server] stream acct=%s model=%s: empty upstream stream (200+0 frames)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			}
			st.ttfb = stats.TTFB()
			// usage 缺失时保留 chatStat.toks 的 -1 哨兵（观测缺失 → 显示 "-"），
			// 不写入零值——否则「没观测到 usage」被伪造成「测得 0 token」，
			// 与非流式走 completionTokens 返回 -1 的口径不一致。
			toks, hasUsage := stats.Tokens()
			if hasUsage {
				st.toks = toks
			}
			// metrics 采集：token 三段 + 缓存三段 + 真实扣费（供 /v1/stats）。
			// 与成本账本同源同口径（都读末帧 usage），故此处一并带出，避免二次解析。
			st.hasUsage = hasUsage
			st.prompt = stats.PromptTokens()
			st.cacheHit, st.cacheMiss, st.cacheWr = stats.CacheTokens()
			if credit, ok := stats.Credit(); ok {
				st.credit = credit
				st.hasCredit = true
			}
			// 成本账本：末帧 usage 带 credit 与 token 总数时记录实测单价，
			// 供下次选号把免费/便宜的号排在前面。
			if credit, ok := stats.Credit(); ok {
				h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, stats.TotalTokens())
			} else if hasUsage {
				// R9(c) 防护观测：usage 存在但 credit 缺失（如 global SSE 末帧未带 credit）。
				// 不算合法成本观测（缺失≠0），仅记一条 WARN 协助排障，绝不写入账本。
				log.Printf("WARN: [server] stream usage without credit acct=%s model=%s (no cost observation)", logfmt.Label(acct.UID, acct.Nickname), bareModel)
			}
			rc.Close()
			return
		}
		resp, err := upstream.Aggregate(rc)
		rc.Close()
		if err != nil {
			// 上游流解析失败：客户端还没看到任何输出，回 502 并告知原因。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", err.Error())
			st.status = http.StatusBadGateway
			return
		}
		out, oerr := sink.Complete(resp)
		if oerr != nil {
			// 出口编码失败（如 Responses 翻译撞上畸形上游结果）：客户端还没拿到任何内容，
			// 回 502 并告知原因，观测口径与非流式上游解析失败一致。
			writeOpenAIError(w, http.StatusBadGateway, "upstream_parse", oerr.Error())
			st.status = http.StatusBadGateway
			return
		}
		writeJSON(w, http.StatusOK, out)
		st.status = http.StatusOK
		st.toks = completionTokens(resp)
		// 成本账本（非流式）：从聚合响应的 usage 取 credit 与 token 总数。
		if credit, total, ok := usageCreditTotal(resp); ok {
			h.cfg.Pool.NoteModelCost(acct.UID, bareModel, credit, total)
		}
		// metrics 采集（非流式）：与流式同口径，从同一份 usage 带出。
		fillStatFromUsage(st, resp)
		return
	}
	// 末端错误透传（error-passthrough）：上游返回的错误原样透传，不再规范化成固定文案。
	//
	// 背景：此前把上游原始错误文本统一改写，防账号 UID / 上游内部错误码（11128 / 12153 /
	// 6004 后台措辞）泄露——但副作用是客户端看不到真实错误，根本没法排查上游问题。
	// 任务书规定上游错误码/账号语义**允许**泄露给客户端（有意为之），排查必须看到原文。
	//
	//   - 上游返回（*upstream.Error）→ error.message 装**上游 body 原文**（code/msg/
	//     requestId 原样保留，如 {"code":6004,"msg":"…","requestId":"…"}）。HTTP 状态码
	//     按 OpenAI 兼容口径映射类别：ErrSoftRate → 429（限流语义、客户端应等待重试），
	//     其余保持 503（网关侧无健康账号可用）。业务 code 取本地分类可读名
	//     （rate_limit_exceeded / no_healthy_account）。
	//   - 本地调度类错误（无可用账号 acct==nil、传输层抖动、非上游返回的 lastErr）
	//     → 保留自有文案 no_healthy_account（本地错误没有上游原文可透传，不编造）。
	status := http.StatusServiceUnavailable
	code := "no_healthy_account"
	msg := "all accounts are temporarily unavailable, please retry later"
	// gateway_hint（末端透传）：上游错误按 Kind + 原文 + 请求形态判定（11133/11135
	// 在 hint 层自带形态判定，ErrClient 家族也能带上 hint）；本地调度类错误
	// （无上游原文）固定 no_healthy_account hint。
	hint := upstream.NoHealthyAccountHint()
	var ue *upstream.Error
	if errors.As(lastErr, &ue) {
		hint = h.hintOf(ue.Kind, ue.Msg, bareModel, reqHasImage, ue)
		// 池级死模型标记：末态上游错误是「权威性无此模型」（11102/1211 类，
		// ErrModelBlocked）→ 该模型在当前号池打不通，标记后从目录与路由剔除。
		// 11133 参数类不在此列（可能是请求侧问题，见 modeldead.go 头注）。
		if ue.Kind == upstream.ErrModelBlocked {
			h.modelDead.mark(realm, producer, bareModel, "upstream: no such model on backend")
		}
		switch ue.Kind {
		case upstream.ErrSoftRate:
			status = http.StatusTooManyRequests
			code = "rate_limit_exceeded"
			msg = "rate limited: all accounts are cooling down, please wait a moment and try again"
		case upstream.ErrWafBlock:
			if h.wafIP.active() {
				// IP 级拦截措辞（fail-fast 终止路径）：空 body 时给出明确可读文案——
				// 网关出口 IP 被 WAF 拦截、轮转已止损、窗口 X 秒后自动解除。客户端
				// 提前重试无意义（换号不换 IP）；有上游原文时原文优先（下方统一）。
				code = "waf_ip_blocked"
				msg = "waf ip-level block: upstream firewall is blocking the gateway IP, rotation stopped; retry after the block window expires"
			}
		}
		if s := strings.TrimSpace(ue.Msg); s != "" {
			// 上游原文优先：透传 code/msg/requestId，不拼接本地前缀。
			msg = s
		}
	}
	writeOpenAIErrorHint(w, status, code, msg, hint)
	st.status = status
}

// promptTooLongMessage 11115 透传 message：上游 body 原文（含真实 token 数/
// 上限值/requestId，客户端自行排查）；空 body 兜底为可读分类短文案（不编造原文）。
func promptTooLongMessage(body string) string {
	if strings.TrimSpace(body) == "" {
		return "prompt is too long"
	}
	return body
}

// slotWaitStep 在途名额等待的轮询步长：每步检查一次是否有名额释放 + 重试选号。
// 50ms 在「及时性」与「不空转 CPU」之间取平衡（池内选号是内存遍历，单次成本极低）。
const slotWaitStep = 50 * time.Millisecond

// waitForSlot 在「池里有对该模型健康的号、但全部被在途上限挡住」时，最多等待
// h.cfg.SlotWait 那么久，等一个名额释放后返回可用的候选号；池里确实没有可等号
// （冷却/禁用/来源不符/模型级冷却）或等待超时/客户端断连则返回 nil，由调用方走 503。
//
// 为什么需要它：单账号池（如只有 1 个 opencode/kilo 号）配 max_in_flight=3 时，
// 第 4 个并发请求此前会立刻 503，客户端看到的是「瞬间打满」。上游单号并发本就要
// 压住（WAF），正确的背压形态是「排队等前面请求腾名额」，而不是把突发全判失败。
// SlotWait<=0 时完全不等待（旧行为，零回归）。
func (h *Handler) waitForSlot(ctx context.Context, tried map[string]bool, bareModel, producer, realm string) *auth.Auth {
	if h.cfg.SlotWait <= 0 {
		return nil
	}
	deadline := time.Now().Add(h.cfg.SlotWait)
	for {
		if acct := h.cfg.Pool.PickExcludingForProducerRealmFree(
			tried, bareModel, producer, realm, h.modelFree(realm, producer, bareModel)); acct != nil {
			return acct
		}
		// 没有「只差一个在途名额」的号：等待不会让选号成功，立即放弃。
		if !h.cfg.Pool.HasHealthyInFlightFull(tried, bareModel, realm, producer) {
			return nil
		}
		if !time.Now().Before(deadline) {
			return nil
		}
		if !sleepCtx(ctx, slotWaitStep) {
			return nil // 客户端断连/优雅停机：排队中的请求就地放弃，不占名额
		}
	}
}

// rotateBackoff 轮转间指数退避 + 抖动（WAF 403 修复 P0-2，报告 §6）：
// 第 i 次轮转失败（continue 换号前）等待 backoffAfter(i)（500ms·2^i 封顶 8s，
// ±25% 抖动），ctx 取消（客户端断连/优雅停机）返回 false——调用方立即终止轮转
// （客户端已走，换号重试无意义）。退避是「换号前歇一下」让上游频控窗口滑过；
// 正常单号请求（首次成功）不经过本函数，零开销。
func rotateBackoff(i int, ctx context.Context) bool {
	d := backoffAfter(i)
	if d <= 0 {
		return ctx.Err() == nil
	}
	if !sleepCtx(ctx, d) {
		log.Printf("WARN: [server] rotate backoff aborted: ctx cancelled")
		return false
	}
	return true
}

// applyErrorPolicy 按错误分类对账号施加冷却/禁用/熔断策略（最终版状态机）。
// kind 是唯一权威分类（来自 upstream.Classify / ChatStreamContext 的 *Error 信封），
// 此处不再按原始 status 二次判断。仅在 chatCompletions 轮转循环内调用：内容拦截
// 会立即 400 返回，其余种类 continue 换号（continue 前由 rotateBackoff 退避）。
//
// 十条路径，各司其职：
//   - ErrHardCredit → CooldownUntilTomorrow4AM：即时硬冷却到次日 04:00（等签到恢复）。
//   - ErrSoftRate → 优先对齐上游重置墙钟（带「将在 … 重置」时 6004 走模型级豁免、
//     非 6004 走账号级，均不指数堆加）；无重置时间才走有界退避（soft_rate 基数起、
//     softStreak 翻倍、封顶 soft_rate_max，冷却中兜底探测不翻倍）。P1-2 后冷却时长
//     优先采信 Retry-After 头（uerr.RetryAfter，body 文案墙钟之外的头形态来源）。
//   - ErrWafBlock → 账号级软冷却（WAF 403 修复 P0-1）：**不 Disable**——WAF 403 是
//     IP/指纹维频控信号（报告 §6：双账号 403 后账号本身健康），罚过即走、到期自愈。
//     时长优先 Retry-After 头（P1-2）；缺失按 wafCooldownBase(60s) 起 · 2^softStreak
//     封顶 soft_rate_max 的既有 CooldownSoftRate 有界退避（比 429 的 soft_rate 严：
//     基数小但响应快；WAF 信号带 IP 级粘性故指数升级保底存在）。基数经 jitterDur
//     抖动（复用 backoff.go 单一抖动来源，防多账号同相位冷却到期再聚团）。
//   - ErrNotFound → Cooldown(CoolSoft, notFoundCooldown 固定 60s)：短冷却防雪崩，不随 soft_rate 退避。
//   - ErrSessionDead → Disable：session 死亡，永久禁用（需人工重登）。
//   - ErrContentBlocked → 不罚账号（无冷却/熔断/NoteError）；passthrough 首遇触发
//     降级重试，最终仍拦则回 400 content_blocked（防火墙文案，不含账号/错误码）。
//   - ErrBadParams → 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇），但仍轮转。
//   - ErrPromptTooLong → 11115「prompt is too long」：请求的问题不是账号的问题
//     （同一 body 换任何号都超限）。零动作（不冷却/不熔断/不 NoteError、不喂连败，
//     同 ErrContentBlocked 待遇），chatCompletions 已直接透传原文返回不轮转——
//     该分支只为文档完备，不指望走到换号路径。
//   - ErrImageInvalid → 图片格式/数据无效：请求的问题不是账号的问题（同一 body
//     换任何号都会得到相同的解析错误）。零动作（不冷却/不熔断/不 NoteError、
//     不喂连败），chatCompletions 已直接透传原文返回不轮转。
//   - ErrServer → NoteError：喂单一连续失败计数器 fails + 累计错误 errTotal，
//     达到 breakerThreshold 触发熔断（指数退避）。
//   - ErrModelBlocked → BlockModelBackoff：(账号, 模型) 11102 负缓存避让（复用 modelCooldowns
//     机制，Until=指数退避 TTL，选号侧 healthyForModel 避开，切模型即可用）。
//   - 其他（default：ErrClient/ErrNone）→ 只换号不罚（防雪崩），不喂熔断；ErrClient
//     额外喂连败计数（NoteFailures，issue #114）：未知 4xx 连败 N 次临时出池——
//     「不知道原因的兜底」，与冷却「知道原因的惩罚」并存取更长者不叠加（health
//     或门；带权威分类的错误不喂连败，防重复计罚）。ErrNone 零防御路径不喂。
//
// body 仅在 ErrSoftRate 分支用于识别上游 6004 模型级限流并解析重置时间；model 为请求
// 携带的模型名（触发 6004 时记录以便后续切模型豁免）。uerr 是 ChatStreamContext 返回的
// 分类信封（可携带 RetryAfter，P1-2）；零值/防御路径下为 nil，冷却时长回落既有计算。
//
// 恢复出口：CoolSoft/CoolHard 各自到期自动恢复；熔断按其指数退避截止到期；
// 成功（NoteSuccess）清 fails/熔断；签到解冻（ReenableIfCredits→reviveCoolingLocked）只清冷却，不动熔断。
func (h *Handler) applyErrorPolicy(uid string, kind upstream.ErrKind, body, model string, uerr *upstream.Error) {
	switch kind {
	case upstream.ErrHardCredit:
		// 402 + 余额关键词即积分耗尽：同步冷却到次日 04:00（签到任务 09/21 点恢复），
		// 不需要异步核查（冗余）。立即换号。
		h.cfg.Pool.CooldownUntilTomorrow4AM(uid, "余额不足")
	case upstream.ErrSoftRate:
		// 统一对齐上游重置时间（重构核心）：只要 body 带「将在 … 重置」，无论业务
		// code 是 6004 还是 11140 rate-limiting 等形态，都精确冷却到该墙钟、绝不
		// softStreak 指数堆加。
		//   - 模型级（6004）→ CooldownSoftForModel：写 modelCooldowns[model]，切模型
		//     豁免（既有 issue #31 语义）。
		//   - 账号级（非 6004）→ CooldownSoftRate：写账号级 until，不产生模型豁免
		//     （普通账号级限流不该因切模型绕过）。
		if resetAt, ok := upstream.ParseRateReset(body); ok {
			if upstream.IsModelRateLimit(body) {
				h.cfg.Pool.CooldownSoftForModel(uid, h.cfg.SoftCooldown, resetAt, model, "6004 model rate limit")
				return
			}
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, resetAt, "429 rate limit")
			return
		}
		// P1-2：body 无重置文案但带 Retry-After 头 → 冷却到该时刻（不做指数堆加，
		// 与重置墙钟同一对齐语义）。头优先于「有界退避」，但**低于** body 重置文案
		// （上方已 return）——文案是上游更权威的口径（WAF 403 报告 §2.1：intl CLI
		// 同序，Retry-After 也只在无重置文案时兜底）。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Now().Add(uerr.RetryAfter), "429 rate limit (retry-after)")
			return
		}
		// 无重置时间 → 账号级有界退避（soft_rate 基数起、softStreak 翻倍、封顶
		// soft_rate_max）；已在冷却中的兜底探测不翻倍（见 CooldownSoftRate）。
		h.cfg.Pool.CooldownSoftRate(uid, h.cfg.SoftCooldown, time.Time{}, "429 rate limit")
	case upstream.ErrWafBlock:
		// P0-1：WAF 403（无业务信封拦截形态）。软冷却复用 CooldownSoftRate 家族
		// （不新建平行冷却系统）：基数 wafCooldownBase（60s，抖动后落 [45s,75s]）、
		// softStreak 指数升级、封顶 soft_rate_max、冷却中兜底探测不翻倍——全部继承
		// 既有语义。Retry-After 头优先（P1-2，WAF 拦截页可能带该头）。不 Disable。
		if uerr != nil && uerr.RetryAfter > 0 {
			h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Now().Add(uerr.RetryAfter), "waf 403 block (retry-after)")
			return
		}
		h.cfg.Pool.CooldownSoftRate(uid, jitterDur(wafCooldownBase), time.Time{}, "waf 403 block")
	case upstream.ErrSessionDead:
		h.cfg.Pool.Disable(uid, "12153 session dead")
	case upstream.ErrNotFound:
		// 404 短冷却（软冷却），防雪崩。固定 notFoundCooldown，不随 soft_rate 退避：
		// 偶发路径缺失不是限流信号，不该按限流惩罚升级。
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, notFoundCooldown, "upstream 404")
	case upstream.ErrAccountFault:
		// 账号级授权/配额故障按 msg 分野（口径与 Classify 的 accountFaultMarkers 一致）：
		//   - "request illegal"（code 11140）→ 账号级**授权封禁**：软冷却到期也不会自动
		//     恢复（需重新 OAuth 登录），到期后重新选号只会再撞 403 浪费一次轮换——
		//     硬禁用（Disable），不再参与选号。/status 以 disabled + disabled_reason 呈现。
		//   - 14017（trial not activated）→ register 未完成，补完 register 后可能自愈，
		//     **保持软冷却**（禁用会让用户补完 register 后仍无法用）。
		// 两条路径对坏号都立刻换号（同一请求轮转出池），只是后续可恢复性不同。
		// 大小写不敏感（与 Classify 的 marker 匹配同口径）。
		if strings.Contains(strings.ToLower(body), "request illegal") {
			h.cfg.Pool.Disable(uid, "account banned by upstream (11140 request illegal), re-login required")
			return
		}
		h.cfg.Pool.Cooldown(uid, pool.CoolSoft, h.cfg.SoftCooldown, "account fault (14017)")
	case upstream.ErrServer:
		// 5xx 上游故障：Classify 已把 ≥500 判为 ErrServer，在此喂熔断计数（不再手写 status>=500）。
		h.cfg.Pool.NoteError(uid)
	case upstream.ErrContentBlocked:
		// 内容策略拦截：内容问题非账号问题，不罚账号（无冷却/熔断/NoteError）。
		// passthrough 首遇由 chatCompletions 内降级重试处理；最终仍拦则回 400
		// content_blocked（防火墙文案），不再轮转、不暴露账号/冷却/错误码。
	case upstream.ErrPromptTooLong:
		// 11115「prompt is too long」：请求的问题不是账号的问题（同一 body 换任何
		// 号都超限）。零动作（不冷却/不熔断/不 NoteError，同 ErrContentBlocked
		// 待遇），chatCompletions 已直接透传原文返回不轮转——该分支只为文档完备，
		// 不指望走到换号路径。
	case upstream.ErrImageInvalid:
		// 图片格式/数据无效：请求的问题不是账号的问题（同一 body 换任何号都会
		// 得到相同解析错误）。零动作，chatCompletions 已 fail-fast 透传。
	case upstream.ErrModelParamInvalid:
		// 11133 model_param_invalid：请求参数被模型供应商拒绝，同 ErrImageInvalid
		// 性质（请求的问题不是账号的问题）。零动作，chatCompletions 已 fail-fast
		// 透传 400——不落到轮转路径，免得一个确定性错误把 MaxRotate 个号全试一遍
		// 最后对外报成 503 no_healthy_account。
	case upstream.ErrBadParams:
		// 请求体解析失败（400 + Unmarshal chat params failed / 11101）：发给上游的 body
		// 有问题（网关截断已由 413 消灭，剩余为客户端畸形 JSON）。换了账号照样 400，
		// 不罚账号（无冷却/熔断/NoteError，同 ErrContentBlocked 待遇）；但**仍然轮转**
		// ——不同账号可能有不同的模型权限，值得换号再试一次。
	case upstream.ErrModelBlocked:
		// 11102「该后端无此模型」：(账号, 模型) 负缓存避让。复用 modelCooldowns 机制
		// （与 6004 同域），写 modelCooldowns[model]，Until 为指数退避 TTL（6h 起、封顶
		// 24h）。选号侧 healthyForModel 对该账号自动避开该模型；切模型/切账号即可用。
		// 立即换号（本轮 continue），该账号该模型冷却，下次选号避开。
		h.cfg.Pool.BlockModelBackoff(uid, model, upstream.ModelBlockReason)
	default:
		// 其余（ErrClient/ErrNone）：只换号不罚（防雪崩），不喂熔断。
		// ErrClient（未知 4xx）喂连败计数（issue #114）：连续 N 次该形态失败 →
		// 账号临时出池（NoteFailures 达阈降权），单次/偶发不罚（不误伤）。ErrNone
		// 到这里属防御路径（status>=400 但分类成功），语义不明不喂。
		if kind == upstream.ErrClient {
			h.cfg.Pool.NoteFailures(uid)
		}
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func writeOpenAIError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "api_error",
			"code":    code,
		},
	})
}

// writeOpenAIErrorHint 同 writeOpenAIError，另在 error 对象上附加
// error.gateway_hint（hint 为空串时不带字段——未覆盖形态不编造）。
// message 仍是上游原文透传（hint 只做并列补充，绝不替换/包装 message）。
func writeOpenAIErrorHint(w http.ResponseWriter, status int, code, msg, hint string) {
	if hint == "" {
		writeOpenAIError(w, status, code, msg)
		return
	}
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message":      msg,
			"type":         "api_error",
			"code":         code,
			"gateway_hint": hint,
		},
	})
}

// hasImagePart 报告聊天请求体是否携带多模态 image_url part（OpenAI 兼容形态
// messages[].content[] {type:"image_url"}）。畸形/其他形态一律 false（hint 侧
// 宁缺勿滥：判不出带图就不给「模型不支持图片」指向）。
func hasImagePart(body []byte) bool {
	var peek struct {
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &peek) != nil {
		return false
	}
	for _, m := range peek.Messages {
		for _, p := range m.Content {
			if p.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// hintContext 组装 chatCompletions 的 gateway_hint 判定上下文：请求裸模型名 +
// 是否带图 + 模型目录 supports_images 声明（目录未收录 → ModelInCatalog=false，
// 不做「不支持」判定，防查不到误判）。仅错误路径调用（成功请求零开销）。
//
// 目录查询只读既有缓存快照（cachedModelsSnapshot），**不触发上游拉取**：错误路径
// 加一次 FetchModels 网络调用既拖慢错误响应、又污染上游调用语义（错误风暴时放大
// 请求量——与 WAF IP fail-fast 的「不放大请求量」哲学相悖）。缓存冷（最近 1h 未
// 拉过）→ ModelInCatalog=false，11133 退中性 hint（宁缺勿滥，不编造能力事实）。
func (h *Handler) hintContext(bareModel string, hasImage bool) upstream.HintContext {
	ctx := upstream.HintContext{Model: bareModel, HasImage: hasImage}
	if bareModel == "" {
		return ctx
	}
	for _, mi := range cachedModelsSnapshot() {
		if mi.ID == bareModel {
			ctx.ModelInCatalog = true
			ctx.ModelSupportsImages = mi.SupportsImages
			return ctx
		}
	}
	return ctx
}

// zcodeCatalogSnapshot / opencodeCatalogSnapshot / kiloCatalogSnapshot 只读 1h 缓存
// 快照（冷/过期 → nil），供选号热路径查 Free 字段用——绝不触发上游探测。
// 与 cachedModelsSnapshot（CN workbuddy 目录）同形同口径，命名对齐便于对照。
func zcodeCatalogSnapshot() []upstream.ModelInfo {
	zcodeCatalogCache.RLock()
	defer zcodeCatalogCache.RUnlock()
	if len(zcodeCatalogCache.infos) == 0 || time.Since(zcodeCatalogCache.fetched) >= zcodeCatalogTTL {
		return nil
	}
	return zcodeCatalogCache.infos
}

func opencodeCatalogSnapshot() []upstream.ModelInfo {
	opencodeCatalogCache.RLock()
	defer opencodeCatalogCache.RUnlock()
	if len(opencodeCatalogCache.infos) == 0 || time.Since(opencodeCatalogCache.fetched) >= opencodeCatalogTTL {
		return nil
	}
	return opencodeCatalogCache.infos
}

func kiloCatalogSnapshot() []upstream.ModelInfo {
	kiloCatalogCache.RLock()
	defer kiloCatalogCache.RUnlock()
	if len(kiloCatalogCache.infos) == 0 || time.Since(kiloCatalogCache.fetched) >= kiloCatalogTTL {
		return nil
	}
	return kiloCatalogCache.infos
}

// cachedModelsSnapshot 只读模型目录缓存（TTL 内快照）；缓存冷/空 → nil。
// 不发起任何上游调用（与 fetchDynamicModels 的差异点，见 hintContext 注释）。
func cachedModelsSnapshot() []upstream.ModelInfo {
	dynamicModelsCache.RLock()
	defer dynamicModelsCache.RUnlock()
	if len(dynamicModelsCache.ids) == 0 || time.Since(dynamicModelsCache.fetched) >= dynamicModelsTTL {
		return nil
	}
	return dynamicModelsCache.ids
}

// hintOf 末端错误透传的统一 hint 入口：kind + 上游原文 + 请求上下文 →
// gateway_hint 文案（upstream.GatewayHint 单一事实来源）。uerr 为 nil 时回落
// body 原文判定（防御路径）。transport 层错误（lastErr 非 *upstream.Error 且
// 上游没回 body）→ 无 hint（不编造）。
func (h *Handler) hintOf(kind upstream.ErrKind, body, bareModel string, hasImage bool, uerr *upstream.Error) string {
	msg := body
	if uerr != nil && uerr.Msg != "" {
		msg = uerr.Msg
	}
	return upstream.GatewayHint(kind, msg, h.hintContext(bareModel, hasImage))
}

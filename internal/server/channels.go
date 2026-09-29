// channels.go 多出口（输出通道）：一个进程里给不同来源的账号各开一个端口。
//
// 为什么是「同进程多 listener」而不是「每个客户端起一个进程」：
//   - 号池、粘性会话、成本台账、冷却状态都是**共享的运行态**，拆进程等于把这些
//     状态复制若干份，同一个账号会被两个进程同时选中（上游会看到重复并发）。
//   - 同进程多 listener 只是多一张网卡上的 socket，账号选择、路由、监控都是同一份。
//   - 想彻底隔离（比如给不同的人不同权限）时再开第二个进程即可——两条路不冲突，
//     配置里每个通道一个端口，等于「选择权交给用户」。
//
// 每个通道 = 一份**受限的 Handler**：只服务指定 producer 的模型、只发布该通道的
// 白名单、带自己的模型前缀与费率提示。base 配置（号池/上游/密钥/会话）全部共享。
package server

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ChannelStatus 一个通道的运行时状态（/admin/output 回显给面板）。
type ChannelStatus struct {
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
	Listen      string   `json:"listen"`
	BaseURL     string   `json:"base_url,omitempty"`
	Format      string   `json:"format,omitempty"`
	RateHint    string   `json:"rate_hint,omitempty"`
	ModelPrefix string   `json:"model_prefix,omitempty"`
	Producers   []string `json:"producers,omitempty"`
	// Running=false 时 Error 一定有值：绑定失败（端口被占等）要说清楚，
	// 不能只显示一个「未运行」让人猜。
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
}

// channelInst 一个已启动的通道。
type channelInst struct {
	cfg OutputChannel
	srv *http.Server
	ln  net.Listener
	// eff 这个通道真正生效的输出参数（自身字段盖过主口之后的结果）。
	// 主口改了格式/前缀/费率时，靠它判断那些「继承主口」的通道要不要重建。
	eff OutputConfig
	// stopped 由 stop() 置位。Serve 因为我们主动关 listener 而返回的 net.ErrClosed
	// 不是故障，不能写回面板冒充「服务退出」（之前就是这个竞态让删掉/重建的出口
	// 一直显示「未运行：use of closed network connection」）。
	stopped atomic.Bool
}

// ChannelRunner 通道监听器的生命周期管理：按配置 diff 出「要起哪些 / 要停哪些」。
// 配置一变就调和一次（Reconcile 幂等）。
type ChannelRunner struct {
	mu    sync.Mutex
	base  Config
	insts map[string]*channelInst
	stat  map[string]ChannelStatus
}

// NewChannelRunner 建管理器。base 是主处理器用的同一份 Config：通道处理器由它复制而来，
// 只有输出参数与 producer 范围不同。
func NewChannelRunner(base Config) *ChannelRunner {
	return &ChannelRunner{base: base, insts: map[string]*channelInst{}, stat: map[string]ChannelStatus{}}
}

// Reconcile 把当前运行的通道对齐到 cfg.Channels：新增/改动的重建，删掉/改名的停掉。
// 单个通道起不来只影响它自己（status 里带 Error），不影响主监听与其他通道。
func (r *ChannelRunner) Reconcile(cfg OutputConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := map[string]OutputChannel{}
	for _, ch := range cfg.Channels {
		want[ch.ID] = ch
	}
	// 停：不需要了，或配置变了（端口/参数任一不同 → 重建，避免半新半旧）
	for id, inst := range r.insts {
		next, ok := want[id]
		// 除了通道自身字段，还要比「生效输出」：格式/前缀/费率留空 = 继承主口，
		// 所以主口改了这一份，继承它的通道也得跟着重建，否则出口还跑着旧格式。
		if !ok || !sameChannel(inst.cfg, next) || !sameOutput(inst.eff, effectiveOutput(r.base.Output, next)) {
			inst.stop()
			delete(r.insts, id)
			delete(r.stat, id)
		}
	}
	// 起：新出现的（起不来的不记进 insts：下次调和会再试一次，状态里保留失败原因）
	for id, ch := range want {
		if _, ok := r.insts[id]; ok {
			continue
		}
		if inst := r.start(ch); inst != nil {
			r.insts[id] = inst
		}
	}
}

// sameChannel 判断两个通道配置是否等价（含默认值的展开），决定要不要重建监听器。
func sameChannel(a, b OutputChannel) bool {
	return a.Listen == b.Listen &&
		strings.Join(a.Producers, ",") == strings.Join(b.Producers, ",") &&
		a.ModelPrefix == b.ModelPrefix && a.RateHint == b.RateHint &&
		a.Format == b.Format && a.Name == b.Name
}

// start 启动一个通道；绑定失败不 panic，把原因写进 status（面板能看到为什么没起来）。
func (r *ChannelRunner) start(ch OutputChannel) *channelInst {
	// 生效输出 = 通道自身字段盖过主口之后的结果（留空即继承主口）。
	scoped := scopedOutput(r.base.Output, ch)
	eff := scoped.Get()
	st := ChannelStatus{
		ID: ch.ID, Name: ch.Name, Listen: ch.Listen, BaseURL: baseURLOf(ch.Listen),
		Format: eff.Format, RateHint: eff.RateHint,
		ModelPrefix: eff.ModelPrefix, Producers: ch.Producers,
	}
	ln, err := net.Listen("tcp", ch.Listen)
	if err != nil {
		st.Error = "监听 " + ch.Listen + " 失败：" + err.Error()
		r.stat[ch.ID] = st
		return nil
	}
	cfg := r.base
	cfg.Output = scoped
	cfg.ProducerAllow = ch.Producers
	// 通道口不挂管理面：它对外可用性更高（用户可能绑 0.0.0.0），管理端点留在主口。
	cfg.AdminEnabled = false
	cfg.ChannelID = ch.ID
	cfg.Listen = ch.Listen // /admin/output 回显 base URL 时用
	child := NewHandler(cfg)
	srv := &http.Server{Handler: child, ReadHeaderTimeout: 30 * time.Second}
	inst := &channelInst{cfg: ch, srv: srv, ln: ln, eff: eff}
	go func() {
		serr := srv.Serve(ln)
		if !shouldReportServeExit(serr, inst.stopped.Load()) {
			return
		}
		r.mu.Lock()
		st.Error = "服务退出：" + serr.Error()
		st.Running = false
		r.stat[ch.ID] = st
		r.mu.Unlock()
	}()
	st.Running = true
	r.stat[ch.ID] = st
	return inst
}

// shouldReportServeExit 判断 Serve 的返回要不要报成「服务退出」。
//
// nil 与 http.ErrServerClosed 是正常收工；我们主动停的通道（stopped=true）以及 listener
// 被关掉产生的 net.ErrClosed 也不是故障。这两种以前会被当成异常写回状态，于是删掉或
// 重建过的出口在面板上永远挂着「未运行 + 服务退出：use of closed network connection」——
// 看着像出口起不来，其实只是收过一次工。只有真正的意外退出才值得占一行红字。
//
// 抽成纯函数是为了能稳定地测：这个竞态在多核机器上才容易命中（listener 先关、Server.Close
// 还没置 inShutdown，Serve 就会返回 net.ErrClosed 而不是 ErrServerClosed），靠集成测试碰运气
// 抓不住。
func shouldReportServeExit(serr error, stopped bool) bool {
	if serr == nil || errors.Is(serr, http.ErrServerClosed) {
		return false
	}
	if stopped || errors.Is(serr, net.ErrClosed) {
		return false
	}
	return true
}

// Status 当前通道状态（按 id 排序，输出稳定）。
func (r *ChannelRunner) Status() []ChannelStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ChannelStatus, 0, len(r.stat))
	for _, st := range r.stat {
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Close 停掉所有通道（进程退出时调用）。
func (r *ChannelRunner) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, inst := range r.insts {
		inst.stop()
		delete(r.insts, id)
		delete(r.stat, id)
	}
}

// stop 关掉一个通道。listener 必须显式关：http.Server.Close 只关它已经 track 上的
// listener，Serve 还没来得及登记就被 Close 的竞态下它会漏掉，端口就悬在那儿了。
// 两处 Close 都是幂等的，重复调只返回错误，忽略即可。
func (i *channelInst) stop() {
	if i == nil {
		return
	}
	i.stopped.Store(true)
	_ = i.ln.Close()
	_ = i.srv.Close()
}

// effectiveOutput 把「通道自身的输出设置」盖在「主口设置」之上，得到真正生效的一份。
// 通道之间本来只在「服务哪些来源」上分叉：格式/模型前缀/费率后缀/发布清单留空即继承主口，
// 用户在「输出 API」页改一次，所有继承的出口跟着变，不必每个出口重复填一遍。
func effectiveOutput(base *OutputStore, ch OutputChannel) OutputConfig {
	parent := DefaultOutputConfig()
	if base != nil {
		parent = base.Get()
	}
	eff := OutputConfig{Format: ch.Format, ModelPrefix: ch.ModelPrefix, RateHint: ch.RateHint, Models: ch.Models}
	if strings.TrimSpace(eff.Format) == "" {
		eff.Format = parent.Format
	}
	if strings.TrimSpace(eff.ModelPrefix) == "" {
		eff.ModelPrefix = parent.ModelPrefix
	}
	if strings.TrimSpace(eff.RateHint) == "" {
		eff.RateHint = parent.RateHint
	}
	if len(eff.Models) == 0 {
		eff.Models = parent.Models
	}
	// 域作用域标记不属于「出口之间的分叉」：CN 与国际版是否分开控制是全网关一次的选择，
	// 通道不单独存一份，跟主口走（通道自己的清单为空时连清单一起继承）。
	eff.ModelsRealmScoped = parent.ModelsRealmScoped
	out, err := normalizeOutput(eff)
	if err != nil {
		return parent
	}
	out.Channels = nil
	return out
}

// sameOutput 比较两份输出参数是否等价（Models 是切片，用分隔符拼串比，避免写反射）。
func sameOutput(a, b OutputConfig) bool {
	return a.Format == b.Format && a.ModelPrefix == b.ModelPrefix &&
		a.RateHint == b.RateHint && strings.Join(a.Models, "\x00") == strings.Join(b.Models, "\x00")
}

// scopedOutput 把一个通道生效的输出参数包成独立 store（内存态，落盘统一走 output.json）。
func scopedOutput(base *OutputStore, ch OutputChannel) *OutputStore {
	s := NewOutputStore("")
	if _, err := s.Set(effectiveOutput(base, ch)); err != nil {
		// 归一化失败不该发生（PUT 已校验过）；真发生就退回默认，通道仍可用。
		return NewOutputStore("")
	}
	return s
}

// baseURLOf 把监听地址转成客户端能直接填的 Base URL。
// ":7870" 这种只写端口的形态补 127.0.0.1（面板上给出的是一个能直接用的串）。
func baseURLOf(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/v1"
	}
	if host == "" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("http://%s:%s/v1", host, port)
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

//go:build windows

// main_windows.go free2api 桌面控制台（WebView2）。
//
// 为什么是「同进程 + 反向代理」而不是「另起一个 free2api.exe 子进程」：
//  1. 启停开关就在这个窗口里，同进程调 gateway.Start/Stop 最直接：不用猜子进程
//     PID，也不会碰上「PID 文件是陈旧的」这类问题。
//  2. 管理页由桌面程序自己内嵌提供（internal/webui），所以**网关停着的时候窗口照样
//     能开**——用户才能在那个页面上把网关再打开。页面里的相对请求全部反向代理到
//     网关端口；网关没跑就回 503，页面显示「已停止」。
//  3. 端口上已经有 free2api 在跑时（例如网关作为计划任务常驻），探测到就直接代理
//     过去，窗口退化成纯控制台，而不是报错白屏。
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	webview "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"

	"free2api/internal/gateway"
	"free2api/internal/webui"
)

// iconResourceID cmd/desktop/rsrc_windows_*.syso 里「图标组」（RT_GROUP_ICON）的资源号。
//
// 为什么不是 1：同一个 .syso 里还嵌了应用清单（RT_MANIFEST，ID 固定为 1），rsrc 给
// 图标组分配的是下一个可用号，实测是 2。写死成 1 的话 LoadImageW 返回 0，窗口拿到
// HICON=NULL，任务栏与标题栏就没有图标（已踩过）。desktop_test.go 的
// TestIconResourceMatchesConstant 会枚举真实资源号盯着这个常量：哪天 rsrc 换了分配
// 策略，测试先炸，而不是悄悄退化成没图标。
const iconResourceID = 2

// desktop 桌面控制台的全部可变状态。
//
// 锁只保护字段读写；Start/Stop 这类慢动作都在锁外做（见 startGateway / stopGateway），
// 否则一次停机就能把 /__desktop/state 这个高频轮询端点卡住。
type desktop struct {
	mu      sync.Mutex
	cfgPath string
	exeDir  string

	inst     *gateway.Instance
	starting bool
	lastErr  string

	gwAddr string // config.json 的 listen，例如 127.0.0.1:7864
	proxy  *httputil.ReverseProxy

	probeAt time.Time // 上次探测「外部是否已有 free2api」的时间
	probeOK bool

	// floatWin 桌面悬浮窗。指针常驻：它的生命周期独立于主控制台窗口
	//（关掉控制台不关浮窗，只随进程退出），所以必须在 desktop 上有个稳定地址。
	floatWin *floatWin
}

func main() {
	cfgFlag := flag.String("config", "", "config.json 路径（默认取 exe 同目录）")
	headless := flag.Bool("headless", false, "只起控制服务、不开窗口（自检/无人值守用）")
	ctrlPort := flag.Int("ctrl", 0, "控制服务端口（0 = 随机空闲端口）")
	flag.Parse()

	exe, err := os.Executable()
	if err != nil {
		fatal("找不到自身路径: " + err.Error())
	}
	exeDir := filepath.Dir(exe)

	cfgPath := *cfgFlag
	if cfgPath == "" {
		cfgPath = filepath.Join(exeDir, "config.json")
	}
	if abs, aerr := filepath.Abs(cfgPath); aerr == nil {
		cfgPath = abs
	}
	// 工作目录跟着 config.json 走：config 里的 auth_dir / state_file 都是相对路径，
	// 「exe + config.json + auths/ + data/ 放一个文件夹」整体迁移才成立。
	if werr := os.Chdir(filepath.Dir(cfgPath)); werr != nil {
		log.Printf("WARN: chdir %s: %v", filepath.Dir(cfgPath), werr)
	}

	setupLogging(exeDir)

	// 第一次运行还没有 config.json：生成一份最小可用的，而不是让窗口里只显示一条
	// 「load config: ... no such file」。生成的内容全部可以在「输出 API」页里改。
	if err := ensureConfig(cfgPath); err != nil {
		log.Printf("WARN: 生成默认配置失败: %v", err)
	}

	d := &desktop{cfgPath: cfgPath, exeDir: exeDir}
	d.gwAddr = resolveListen(cfgPath)
	d.proxy = newProxy(d.gwAddr)

	// 端口上已经有 free2api（计划任务 / 命令行 / 另一个 exe）时不要抢：抢不到只会
	// 在页面上留下一条「bind: Only one usage」的噪音错误，用户还以为是自己搞坏了。
	if d.externalAlive(d.gwAddr) {
		log.Printf("端口 %s 上已有网关在跑，本窗口只当控制台", d.gwAddr)
	} else if err := d.startGateway(); err != nil {
		log.Printf("首次启动网关失败（窗口照常打开，可在概览页重试）: %v", err)
	}

	d.floatWin = newFloatWin(filepath.Join(exeDir, "data", "floatwin.json"), d.gwAddr, "")
	floatOwner = d.floatWin
	go d.syncFloatGateway()

	// 之前开着悬浮窗就把它恢复回来。缺这一步的表现是：设置页开关显示「已开启」，
	// 但重启桌面程序后小窗不见了 —— 因为 enabled 只在用户点开关（floatApply）时
	// 被读，从来没在启动路径上被读。配置里写着开，程序却当它关着。
	if d.floatWin.wantsWindow() {
		// 等网关真正能应答再做首连，然后挂一个自愈循环。
		//
		// 为什么不能只 sleep 1200ms 拉倒：踩过两回 ——
		//  1. 另一实例正在退、SSE 端点还没起来，首连没拿到数据；
		//  2. 建窗线程偶发 5s 超时（认领 UI 消息队列的那一下），f.hwnd 已置位、
		//     窗口其实在跑，但调用方已经放弃，之后没人再管它，一直显示「网关未连接」。
		// 现在的语义：start 失败就重试（幂等，hwnd!=0 早退），连上且收到数据后
		// 每 5s 复查一次 /healthz；断供超过 20s（网关重启/地址变了）就重连，
		// 用 d.gwAddr 现读，不捕旧地址。
		go ensureFloatLoop(d)
	}

	ctrlAddr, err := serve(d, *ctrlPort)
	if err != nil {
		fatal("控制服务起不来: " + err.Error())
	}
	log.Printf("桌面控制台控制服务: http://%s/  （网关 %s）", ctrlAddr, d.gwAddr)

	uiURL := "http://" + ctrlAddr + "/management.html"

	if *headless {
		log.Printf("headless 模式：浏览器打开 %s 即可操作；Ctrl+C 退出", uiURL)
		waitSignal()
		d.stopFloat()
		_ = d.stopGateway()
		return
	}

	winW, winH := defaultWindowSize()
	w := webview.NewWithOptions(webview.WebViewOptions{
		AutoFocus: true,
		DataPath:  filepath.Join(exeDir, "data", "webview2"),
		WindowOptions: webview.WindowOptions{
			Title: "Free2API · 网关控制台",
			// 窗口与任务栏用嵌入的图标组；不传的话是系统默认图标。
			IconId: iconResourceID,
			// 默认尺寸按主屏工作区算（defaultWindowSize），不再写死 1280x860：
			// 那在 2K 屏上只占一半宽，首屏 KPI 还会掉到 3 列。
			Width:  uint(winW),
			Height: uint(winH),
			Center: true,
		},
	})
	if w == nil {
		fatal("WebView2 初始化失败。请确认已安装 Microsoft Edge WebView2 Runtime。\n" +
			"临时替代：用 -headless 参数启动，然后浏览器打开控制台地址。")
	}
	defer w.Destroy()
	w.SetSize(960, 640, webview.HintMin) // 最小尺寸：再小布局就散了
	// 库自带的 Center 按整个屏幕居中，会压到任务栏；这里按工作区再摆一次。
	centerWindowInWorkArea(uintptr(w.Window()), winW, winH)
	w.Navigate(uiURL)
	w.Run() // 阻塞到窗口关闭

	d.stopFloat()
	// 关窗口只停「本窗口托管」的网关。端口上如果是别的进程（计划任务常驻），
	// 用户关掉控制台不该顺手把服务停了——那种「关窗口杀服务 → 看门狗一分钟
	// 后又拉起来」的拉锯，正是端口生命周期看起来没闭环的来源。真停外部进程
	// 请点页面里的「停止网关」。
	done := make(chan struct{})
	go func() {
		d.stopHostedGateway()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		// 兜底：停机路径被某个后台任务卡住时，窗口已经关了、进程却还占着端口，
		// 下一次启动就会 bind 失败。这里直接结束进程，由系统回收全部 socket。
		log.Printf("退出清理超过 8s 未完成，强制结束进程（端口由系统回收）")
		os.Exit(0)
	}
	log.Printf("窗口已关闭，退出")
}

// ensureFloatLoop 悬浮窗启动 + 自愈：首连失败就重试，数据断供就重连，
// 网关重启换了地址也跟得上（每次都现读 d.gwAddr）。进程退出由 stopChan 收尾。
func ensureFloatLoop(d *desktop) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	lastFail := ""
	for {
		f := d.floatWin
		if f == nil || !f.wantsWindow() {
			return // 用户把它关了；再开走 applyFloat，不用这个循环重新拉起
		}
		if !f.running() {
			if err := f.start(); err != nil {
				if msg := err.Error(); msg != lastFail {
					lastFail = msg
					log.Printf("按上次的设置恢复悬浮窗失败（重试中）: %v", err)
				}
			} else {
				lastFail = ""
			}
		}
		// 地址/密钥每条循环都同步一次；setGateway 内部只在真的变了时才断流。
		d.syncFloatGateway()
		if _, readAt, ok := f.snapState(); ok {
			// 连接悬死判定：SSE 每 20s 至少一个心跳字节，40s 没读到任何字节
			// 说明这条连接已经没人喂了（网关重启/网络切换），主动断开重连。
			if !readAt.IsZero() && time.Since(readAt) > 40*time.Second {
				log.Printf("悬浮窗数据源 40s 无字节，主动重连")
				f.dropGateway()
			}
		}
		// 网关停了就断开，别把「旧地址的残连」当还连着；网关回来后
		// streamLoop 的重连会立即接上。
		if !d.externalAlive(d.gwAddr) {
			f.dropGateway()
		}
		select {
		case <-f.stopChan():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- 网关启停

func (d *desktop) startGateway() error {
	d.mu.Lock()
	if d.inst != nil {
		d.mu.Unlock()
		return nil
	}
	if d.starting {
		d.mu.Unlock()
		return errors.New("网关正在启动中，请稍候")
	}
	d.starting = true
	cfgPath := d.cfgPath
	d.mu.Unlock()

	defer func() {
		d.mu.Lock()
		d.starting = false
		d.mu.Unlock()
	}()

	// 端口号自己读一遍（宽松解析），真正的配置校验交给 gateway.Start：
	// 这样「配置写坏了」报的是网关原话，桌面程序不重复实现一套校验规则。
	listen := resolveListen(cfgPath)

	inst, err := gateway.Start(gateway.Options{ConfigPath: cfgPath})
	if err != nil {
		d.setErr(err)
		return err
	}

	// 绑定 socket 在 Start 的后台 goroutine 里做，所以「端口被占」不会让 Start 返回错误。
	// 等一拍再确认：真失败了就当成启动失败报出去，而不是留个假运行态。
	select {
	case <-inst.Done():
		if lerr := inst.Err(); lerr != nil {
			inst.Stop()
			d.setErr(lerr)
			return lerr
		}
	case <-time.After(700 * time.Millisecond):
	}

	d.mu.Lock()
	d.inst = inst
	d.gwAddr = listen
	d.proxy = newProxy(listen)
	d.lastErr = ""
	d.probeOK = false
	d.probeAt = time.Time{}
	d.mu.Unlock()
	log.Printf("网关已启动，监听 %s", listen)
	d.syncFloatGateway()
	return nil
}

// stopHostedGateway 只停本窗口托管的实例，绝不去动端口上别人的进程。
// 窗口关闭路径用它；页面里的「停止网关」按钮走 stopGateway（含外部进程）。
func (d *desktop) stopHostedGateway() {
	d.mu.Lock()
	inst := d.inst
	d.inst = nil
	d.mu.Unlock()
	if inst != nil {
		inst.Stop()
		log.Printf("网关已停止（本窗口托管）")
	}
}

func (d *desktop) stopGateway() error {
	d.mu.Lock()
	inst := d.inst
	addr := d.gwAddr
	d.inst = nil
	d.mu.Unlock()

	if inst != nil {
		inst.Stop()
		log.Printf("网关已停止（本窗口托管）")
		return nil
	}
	// 没有托管实例：端口上那个网关是别的进程拉起来的。用户点的还是「停止网关」，
	// 所以去把它停掉，而不是无声无息地什么都不做。
	if !probeFree2API(addr) {
		log.Printf("网关本来就没在跑")
		return nil
	}
	if err := stopExternalGateway(addr); err != nil {
		d.setErr(err)
		log.Printf("停止外部网关失败: %v", err)
		return err
	}
	d.mu.Lock()
	d.probeAt = time.Time{}
	d.probeOK = false
	d.lastErr = ""
	d.mu.Unlock()
	log.Printf("网关已停止（外部进程）")
	return nil
}

func (d *desktop) setErr(err error) {
	d.mu.Lock()
	if err != nil {
		d.lastErr = err.Error()
	} else {
		d.lastErr = ""
	}
	d.mu.Unlock()
}

// ---------------------------------------------------------------- 状态

type stateView struct {
	Desktop    bool   `json:"desktop"`
	Running    bool   `json:"running"`
	External   bool   `json:"external"`
	Starting   bool   `json:"starting"`
	Listen     string `json:"listen"`
	Base       string `json:"base"`
	ConfigPath string `json:"config_path"`
	Error      string `json:"error,omitempty"`
}

func (d *desktop) state() stateView {
	d.mu.Lock()
	inst := d.inst
	starting := d.starting
	lastErr := d.lastErr
	gwAddr := d.gwAddr
	cfgPath := d.cfgPath
	d.mu.Unlock()

	running := false
	if inst != nil {
		select {
		case <-inst.Done():
			if e := inst.Err(); e != nil && lastErr == "" {
				lastErr = e.Error()
			}
		default:
			running = true
		}
	}

	external := false
	if !running && !starting {
		external = d.externalAlive(gwAddr)
	}
	if external && lastErr != "" {
		// 端口上有个能应答的 free2api：之前那条 bind 失败（或别的启动错误）已经不代表现状，
		// 挂在页面上只会误导。真正需要看的错误会在下次启动失败时重新写进来。
		lastErr = ""
	}

	return stateView{
		Desktop:    true,
		Running:    running,
		External:   external,
		Starting:   starting,
		Listen:     gwAddr,
		Base:       baseURL(gwAddr),
		ConfigPath: cfgPath,
		Error:      lastErr,
	}
}

// -------------------------------------------------- 悬浮窗
//
// 浮窗的三个端点与生命周期都挂在控制服务上，不经过网关：这样「网关停着的时候
// 也能把浮窗关掉」——否则一个指向死端口的浮窗用户就只能杀进程。

func (d *desktop) floatView() map[string]any {
	d.mu.Lock()
	f := d.floatWin
	d.mu.Unlock()
	if f == nil {
		return map[string]any{"available": false, "enabled": false, "running": false}
	}
	return f.view()
}

func (d *desktop) applyFloat(req floatApply) error {
	d.mu.Lock()
	f := d.floatWin
	d.mu.Unlock()
	if f == nil {
		return errors.New("当前构建未带悬浮窗（仅 Windows 桌面程序支持）")
	}
	return f.apply(req)
}

func (d *desktop) stopFloat() {
	d.mu.Lock()
	f := d.floatWin
	d.mu.Unlock()
	if f != nil {
		f.closeWin()
	}
}

// syncFloatGateway 把网关地址/密钥同步给浮窗的数据源。
// 启停网关后 listen 可能变（配置里改过端口），不跟着走的话浮窗会一直连旧地址。
func (d *desktop) syncFloatGateway() {
	d.mu.Lock()
	f := d.floatWin
	addr := d.gwAddr
	inst := d.inst
	d.mu.Unlock()
	if f == nil {
		return
	}
	_ = inst
	// api_key 直接从 config.json 读（宽松解析）：Instance 不暴露配置，
	// 而浮窗只需要这一串用来拼 SSE 的 ?api_key=。
	f.setGateway(addr, resolveAPIKey(d.cfgPath))
}

// externalAlive 端口上是否已经有一个 free2api 在应答。3 秒缓存：状态端点是页面
// 轮询的高频入口，不能每次请求都往外打一次 TCP。
func (d *desktop) externalAlive(addr string) bool {
	d.mu.Lock()
	if !d.probeAt.IsZero() && time.Since(d.probeAt) < 3*time.Second {
		ok := d.probeOK
		d.mu.Unlock()
		return ok
	}
	d.mu.Unlock()

	ok := probeFree2API(addr)

	d.mu.Lock()
	d.probeAt = time.Now()
	d.probeOK = ok
	d.mu.Unlock()
	return ok
}

func probeFree2API(addr string) bool {
	client := &http.Client{Timeout: 900 * time.Millisecond}
	resp, err := client.Get(baseURL(addr) + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusServiceUnavailable {
		return false
	}
	// 认服务名：端口上可能是别的程序，不能因为「有人应答」就当成网关。
	var body struct {
		Service string `json:"service"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if json.Unmarshal(raw, &body) != nil {
		return false
	}
	return body.Service == "free2api"
}

// ---------------------------------------------------------------- HTTP 控制面

func serve(d *desktop, port int) (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /__desktop/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.state())
	})
	mux.HandleFunc("POST /__desktop/gateway/start", func(w http.ResponseWriter, r *http.Request) {
		if err := d.startGateway(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": map[string]string{"message": err.Error()},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": d.state()})
	})
	mux.HandleFunc("POST /__desktop/gateway/stop", func(w http.ResponseWriter, r *http.Request) {
		if err := d.stopGateway(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": map[string]string{"message": err.Error()},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": d.state()})
	})
	// 悬浮窗：走控制服务而不是网关，所以网关停着也能开关/改配置。
	mux.HandleFunc("GET /__desktop/floatwin", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.floatView())
	})
	mux.HandleFunc("POST /__desktop/floatwin", func(w http.ResponseWriter, r *http.Request) {
		var req floatApply
		if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"ok": false, "error": map[string]string{"message": "请求体解析失败: " + err.Error()},
			})
			return
		}
		if err := d.applyFloat(req); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"ok": false, "error": map[string]string{"message": err.Error()},
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": d.floatView()})
	})
	// 管理页由桌面程序自己发：网关停着时这个页面也必须能打开。
	mux.HandleFunc("GET /{$}", d.page)
	mux.HandleFunc("GET /management.html", d.page)
	// 其余一律转给网关。
	mux.HandleFunc("/", d.serveProxy)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return "", err
	}
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 20 * time.Second}
	go func() {
		if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
			log.Printf("控制服务退出: %v", serr)
		}
	}()
	return ln.Addr().String(), nil
}

// page 发内嵌的管理页，并在 <head> 里塞一个「真实网关地址」。
// 页面的 Base URL 显示必须指向网关端口，而不是这个随机的控制端口。
func (d *desktop) page(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	base := baseURL(d.gwAddr)
	d.mu.Unlock()

	html := string(webui.Page())
	inject := "<script>window.__F2A_BASE__=" + jsString(base) + ";window.__F2A_DESKTOP__=true;</script>"
	if strings.Contains(html, "<head>") {
		html = strings.Replace(html, "<head>", "<head>\n"+inject, 1)
	} else {
		html = inject + html
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, html)
}

func (d *desktop) serveProxy(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	inst := d.inst
	proxy := d.proxy
	gwAddr := d.gwAddr
	d.mu.Unlock()

	running := false
	if inst != nil {
		select {
		case <-inst.Done():
		default:
			running = true
		}
	}
	if !running {
		running = d.externalAlive(gwAddr)
	}
	if !running || proxy == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": map[string]string{"message": "网关未启动：在「概览」页点「启动网关」。"},
		})
		return
	}
	proxy.ServeHTTP(w, r)
}

func newProxy(addr string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: hostPort(addr)}
	p := httputil.NewSingleHostReverseProxy(target)
	// FlushInterval=-1：每个写入立即冲刷，否则 SSE 会被 bufio 攒住，流式输出看起来卡住。
	p.FlushInterval = -1
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		msg, _ := json.Marshal("连不上网关：" + err.Error())
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = fmt.Fprintf(w, `{"error":{"message":%s}}`, msg)
	}
	return p
}

// ---------------------------------------------------------------- 小工具

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// baseURL 把 listen 形态（":7863" / "127.0.0.1:7863"）翻成可点的 URL。
func baseURL(addr string) string { return "http://" + hostPort(addr) }

func hostPort(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return defaultListen
	}
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		if strings.Trim(strings.TrimSpace(addr[:i]), "[]") == "" {
			return "127.0.0.1" + addr[i:]
		}
		return addr
	}
	// 只写了端口（"7863"）：补回环主机。
	return "127.0.0.1:" + addr
}

// resolveListen 只为「还没启动网关」时也能显示地址：配置读不动就回落默认端口。
// 这里刻意不用 gateway.Load：它要连带做归一化校验，而此刻我们只是想知道端口号，
// 配置哪怕有一处非法也不该让窗口连地址都显示不出来（真正的报错留给 Start）。
// resolveAPIKey 宽松读 config.json 的 api_key，读不到就当没配。
func resolveAPIKey(cfgPath string) string {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return ""
	}
	var probe struct {
		APIKey string `json:"api_key"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return ""
	}
	return strings.TrimSpace(probe.APIKey)
}

func resolveListen(cfgPath string) string {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return defaultListen
	}
	var probe struct {
		Listen string `json:"listen"`
	}
	if json.Unmarshal(raw, &probe) != nil || strings.TrimSpace(probe.Listen) == "" {
		return defaultListen
	}
	return probe.Listen
}

const defaultListen = "127.0.0.1:7863"

// defaultConfig 首次运行生成的最小配置。
//
// listen 固定回环：config 里 admin.enabled=true 且 api_key 为空时，网关只允许
// 监听纯回环地址（见 gateway.listenIsLoopbackOnly）。写 "127.0.0.1:7863" 而不是
// ":7863"，用户第一次双击才不会撞上那条 fail-fast。
const defaultConfig = `{
  "listen": "127.0.0.1:7863",
  "api_key": "",
  "auth_dir": "./auths",
  "state_file": "./data/state.json",
  "admin": { "enabled": true },
  "global": { "enabled": true }
}
`

// ensureConfig 配置文件不存在时写一份最小配置（已存在则原样不动）。
func ensureConfig(cfgPath string) error {
	if _, err := os.Stat(cfgPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(cfgPath, []byte(defaultConfig), 0o644); err != nil {
		return err
	}
	log.Printf("已生成默认配置 %s（端口 %s，可在「输出 API」页修改）", cfgPath, defaultListen)
	return nil
}

// ---------------------------------------------------------------- 窗口几何
//
// 这两个系统调用 x/sys 没有封装，按仓库里 MessageBoxW 的先例裸调 user32。
// 全部是只读查询 + 一次摆位，不碰窗口样式，所以不影响 WebView2 的宿主行为。

// rECT 与 Win32 RECT 内存布局一致（四个 int32）。
type rECT struct{ Left, Top, Right, Bottom int32 }

// spiGetWorkArea 是 SystemParametersInfoW 的 SPI_GETWORKAREA。
const spiGetWorkArea = 0x0030

var (
	user32ge             = windows.NewLazySystemDLL("user32.dll")
	procGetSystemMetrics = user32ge.NewProc("GetSystemMetrics")
	procSystemParameters = user32ge.NewProc("SystemParametersInfoW")
	procGetWindowRect    = user32ge.NewProc("GetWindowRect")
	procSetWindowPos     = user32ge.NewProc("SetWindowPos")
)

// primaryWorkArea 返回主屏工作区（已扣掉任务栏）的像素矩形。
// ok=false 表示两种算法都拿不到，调用方自己退回默认值。
func primaryWorkArea() (rECT, bool) {
	var r rECT
	ret, _, _ := procSystemParameters.Call(spiGetWorkArea, 0, uintptr(unsafe.Pointer(&r)), 0)
	if ret != 0 && r.Right > r.Left && r.Bottom > r.Top {
		return r, true
	}
	// 退路：整屏尺寸（含任务栏），至少保证窗口落在屏幕里。
	sw, _, _ := procGetSystemMetrics.Call(0) // SM_CXSCREEN
	sh, _, _ := procGetSystemMetrics.Call(1) // SM_CYSCREEN
	if int32(sw) <= 0 || int32(sh) <= 0 {
		return r, false
	}
	return rECT{Right: int32(sw), Bottom: int32(sh)}, true
}

// defaultWindowSize 按主屏工作区算启动尺寸：
// 宽取工作区的 3/4、高取 6/7，再夹在 [1280x860, 1760x1120] 内，
// 最后不允许超过工作区本身。
//
// 为什么不是写死：以前固定 1280x860，在 2560x1080 的主屏上只占一半宽，
// 首屏 5 个 KPI 还会因为 1400px 断点掉到 3 列，白白浪费右半边屏幕。
func defaultWindowSize() (int, int) {
	const (
		minW, minH = 1280, 860
		maxW, maxH = 1760, 1120
	)
	wa, ok := primaryWorkArea()
	if !ok {
		return minW, minH
	}
	workW := int(wa.Right - wa.Left)
	workH := int(wa.Bottom - wa.Top)
	w := workW * 3 / 4
	h := workH * 6 / 7
	if w < minW {
		w = minW
	}
	if h < minH {
		h = minH
	}
	if w > maxW {
		w = maxW
	}
	if h > maxH {
		h = maxH
	}
	// 极窄屏：宁可小于 minW 也别超出去，否则标题栏会被推到屏幕外。
	if w > workW {
		w = workW
	}
	if h > workH {
		h = workH
	}
	return w, h
}

// centerWindowInWorkArea 把窗口摆到主屏工作区正中。
//
// 库自带的 Center 用 GetSystemMetrics(SM_CXSCREEN/CYSCREEN) 按整屏算位置，
// 任务栏在底部时窗口整体偏下、底部会被压住；这里按工作区重算一次。
func centerWindowInWorkArea(hwnd uintptr, w, h int) {
	if hwnd == 0 {
		return
	}
	wa, ok := primaryWorkArea()
	if !ok {
		return
	}
	// 优先用 GetWindowRect 的实测外框，避开 DPI 换算和标题栏宽度的猜测。
	ow, oh := w, h
	var r rECT
	if ret, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret != 0 {
		if cw, ch := int(r.Right-r.Left), int(r.Bottom-r.Top); cw > 0 && ch > 0 {
			ow, oh = cw, ch
		}
	}
	workW := int(wa.Right - wa.Left)
	workH := int(wa.Bottom - wa.Top)
	x := int(wa.Left) + (workW-ow)/2
	y := int(wa.Top) + (workH-oh)/2
	if x < int(wa.Left) {
		x = int(wa.Left)
	}
	if y < int(wa.Top) {
		y = int(wa.Top)
	}
	const (
		swpNoZOrder   = 0x0004
		swpNoActivate = 0x0010
	)
	_, _, _ = procSetWindowPos.Call(hwnd, 0, uintptr(x), uintptr(y),
		uintptr(ow), uintptr(oh), swpNoZOrder|swpNoActivate)
}

func setupLogging(exeDir string) {
	log.SetFlags(log.LstdFlags)
	dir := filepath.Join(exeDir, "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.SetOutput(io.Discard)
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "desktop.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.SetOutput(io.Discard)
		return
	}
	log.SetOutput(io.MultiWriter(f))
}

func waitSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
}

func fatal(msg string) {
	log.Printf("FATAL: %s", msg)
	messageBox("Free2API 桌面控制台", msg)
	os.Exit(1)
}

// messageBox 在 GUI 子系统下没有控制台可用，出错只能靠弹窗让人看见。
func messageBox(title, text string) {
	user32 := windows.NewLazySystemDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	const mbIconError = 0x00000010
	_, _, _ = proc.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), mbIconError)
}

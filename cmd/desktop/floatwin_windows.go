//go:build windows

// floatwin_windows.go 桌面悬浮窗：常驻小窗，实时显示网关的活跃度。
//
// 为什么是原生 Win32 + GDI，而不是再开一个 WebView2：
//   - 第二个 WebView2 会再拉起一整套 Chromium 子进程（80~120MB 常驻），为一块
//     260x100 的数字窗不值；原生方案的常驻开销就是一个窗口句柄 + 一块小 DIB。
//   - WebView2 在「无边框 + 逐像素透明 + 圆角 + 不抢焦点」这四件事上都很别扭，
//     而这四条恰好是悬浮窗的硬需求。
//
// 透明实现走 WS_EX_LAYERED + UpdateLayeredWindow（AC_SRC_ALPHA）：内容是自绘的
// 32bpp 预乘 DIB，圆角、描边、半透明底都是算出来的，不依赖系统主题。文本用 GDI
// 画（CreateFontW + DrawTextW，灰度抗锯齿），画完再把 alpha 字节补齐——GDI 只改
// RGB 不动 alpha，不补整块就会透明。
//
// 数据源复用网关已有的 SSE：GET /v1/stats/stream（载荷与 /v1/stats 同源，另加
// live_models）。只连一条长连接就够，不额外轮询 /status。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ---------------------------------------------------------------- 配置项

// 设置页里可逐项开关的显示内容。
const (
	floatItemInFlight = "inflight" // 在飞请求数 + 当前模型
	floatItemRate     = "rate"     // 实时 token 速度
	floatItemRequests = "requests" // 区间请求数
	floatItemTokens   = "tokens"   // 区间 token 总量
	floatItemSource   = "source"   // 区间内主要来源
	floatItemClick    = "passthru" // 鼠标穿透
	floatItemTopmost  = "topmost"  // 置顶
)

// floatItemDef 供前端渲染开关列表（后端定死枚举，避免前端硬编码另一份）。
func floatItemDef() []map[string]string {
	return []map[string]string{
		{"ID": floatItemInFlight, "Name": "在飞请求", "Hint": "正在跑的请求数与此刻跑的模型"},
		{"ID": floatItemRate, "Name": "Token 速度", "Hint": "短窗口滚动速率，这一行是真实时值"},
		{"ID": floatItemRequests, "Name": "区间请求数", "Hint": "跟随这里选的区间（今天 / 近 7 天 / 近 30 天 / 全部）"},
		{"ID": floatItemTokens, "Name": "区间 Token", "Hint": "同上区间的 token 总量"},
		{"ID": floatItemSource, "Name": "主要来源", "Hint": "区间内请求数最多的 producer"},
		{"ID": floatItemTopmost, "Name": "窗口置顶", "Hint": "关掉后会浮到别的窗口下面"},
		{"ID": floatItemClick, "Name": "鼠标穿透", "Hint": "开启后点击穿透，不挡下面的窗口"},
	}
}

func floatItemKnown(id string) bool {
	for _, it := range floatItemDef() {
		if it["ID"] == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 落盘配置

type floatConfig struct {
	Enabled bool     `json:"enabled"`
	Range   string   `json:"range"`
	Theme   string   `json:"theme"`
	Items   []string `json:"items"`
	X       int      `json:"x"`
	Y       int      `json:"y"`
	HavePos bool     `json:"have_pos"`
}

func defaultFloatConfig() floatConfig {
	return floatConfig{
		// 默认不开：常驻小窗会干扰正常干活（之前看门狗一直闪就是前车之鉴）。
		Enabled: false,
		Range:   "today",
		Theme:   "dark",
		Items: []string{
			floatItemInFlight, floatItemRate, floatItemRequests,
			floatItemTokens, floatItemSource, floatItemTopmost,
		},
	}
}

func (c *floatConfig) sanitize() {
	switch c.Range {
	case "today", "7d", "30d", "all":
	default:
		c.Range = "today"
	}
	if c.Theme != "dark" && c.Theme != "light" {
		c.Theme = "dark"
	}
	seen := map[string]bool{}
	keep := make([]string, 0, len(c.Items))
	for _, id := range c.Items {
		if floatItemKnown(id) && !seen[id] {
			seen[id] = true
			keep = append(keep, id)
		}
	}
	if len(keep) == 0 {
		keep = []string{floatItemInFlight, floatItemRate, floatItemRequests}
	}
	c.Items = keep
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- 数据

// floatSnapshot 浮窗关心的那一小块统计，其余字段直接丢弃。
type floatSnapshot struct {
	Range string `json:"range"`
	Total struct {
		Requests    int64 `json:"requests"`
		TotalTokens int64 `json:"total_tokens"`
	} `json:"total"`
	Live struct {
		WindowSec    int     `json:"window_sec"`
		Requests     int64   `json:"requests"`
		CompTok      int64   `json:"completion_tokens"`
		TokensPerSec float64 `json:"tokens_per_sec"`
	} `json:"live"`
	LiveModels map[string]int `json:"live_models"`
	Producers  []struct {
		Producer string `json:"producer"`
		Requests int64  `json:"requests"`
	} `json:"producers"`
}

// floatWin 悬浮窗的全部运行态。
type floatWin struct {
	mu      sync.Mutex
	cfgPath string
	cfg     floatConfig

	hwnd  uintptr
	ready chan error // 建窗结果（成功为 nil）

	stop      chan struct{}
	className string
	// pending 建窗线程已派出、还没报告结果。重试期间用它挡住第二次 spawn，
	// 否则超时后的重试会再建一个窗口线程（两个消息队列、两个 HWND）。
	pending bool
	// readyDone 建窗线程结果的广播；winErr 是对应错误。start 的调用方超时
	// 撤回后，仍有一个常驻接收者把结果落进这两个字段，供下一轮重试判断。
	readyDone chan struct{}
	winErr    error
	// streamOn 保证全进程只有一条 SSE 数据源。窗口关了再开不会叠加第二条流。
	streamOn bool

	snap     floatSnapshot
	snapOK   bool
	lastData time.Time
	// liveReadAt 最后一次从 SSE 连接读到**任何字节**（含 20s 一次的心跳帧）。
	// 为什么单独立一个：/v1/stats/stream 只在统计变化时推数据帧，网关空闲时
	// lastData 可以几分钟不动 —— 那不代表连接死了。心跳才是连接活着的证据。
	liveReadAt time.Time
	// streamBody 当前在读的 SSE 响应体。watchdog 判定网关停了/连接悬死时
	// 主动 Close 它，让阻塞中的 ReadString 立刻报错退出、进入重连——
	// 否则要等下一个心跳帧（最长 20s）才会发现自己已经没在数据源上了。
	streamBody io.Closer
	// gen 网关地址或密钥每变化一次 +1。SSE 循环带着建连时的 gen，
	// 读到不一致就退出重连，保证切换地址后旧连接不会继续喂旧网关的数据。
	gen    uint64
	gwAddr string
	apiKey string

	phase float64 // 呼吸点相位，按帧推进

	blitLogMu     sync.Mutex // 贴图失败日志去重（见 logBlitErr）
	lastBlitErr   string
	lastBlitErrAt time.Time
}

// floatClassName 窗口类名。进程内唯一，重复注册会拿到 ERROR_CLASS_ALREADY_EXISTS。
const floatClassName = "Free2APIFloatWindow"

// floatOwner 全进程唯一：主窗关掉时它还活着。
var floatOwner *floatWin

func newFloatWin(cfgPath, gwAddr, apiKey string) *floatWin {
	f := &floatWin{cfgPath: cfgPath, gwAddr: gwAddr, apiKey: apiKey, className: floatClassName}
	f.cfg = defaultFloatConfig()
	if raw, err := os.ReadFile(cfgPath); err == nil {
		var c floatConfig
		if json.Unmarshal(raw, &c) == nil {
			f.cfg = c
		}
	}
	f.cfg.sanitize()
	return f
}

func (f *floatWin) save() {
	f.mu.Lock()
	c := f.cfg
	f.mu.Unlock()
	c.sanitize()
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(f.cfgPath), 0o755); err != nil {
		return
	}
	tmp := f.cfgPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, f.cfgPath)
}

// view 给设置页的快照。
func (f *floatWin) view() map[string]any {
	f.mu.Lock()
	items := append([]string(nil), f.cfg.Items...)
	v := map[string]any{
		"enabled":   f.cfg.Enabled,
		"running":   f.hwnd != 0,
		"range":     f.cfg.Range,
		"theme":     f.cfg.Theme,
		"items":     items,
		"available": floatItemDef(),
		"position":  map[string]any{"x": f.cfg.X, "y": f.cfg.Y, "saved": f.cfg.HavePos},
		"connected": f.snapOK,
		"last_data": f.lastData,
	}
	f.mu.Unlock()
	return v
}

// applyRequest 前端 POST 过来的设置。
type floatApply struct {
	Enabled *bool    `json:"enabled"`
	Range   string   `json:"range"`
	Theme   string   `json:"theme"`
	Items   []string `json:"items"`
}

func (f *floatWin) apply(req floatApply) error {
	f.mu.Lock()
	if req.Enabled != nil {
		f.cfg.Enabled = *req.Enabled
	}
	if req.Range != "" {
		f.cfg.Range = req.Range
	}
	if req.Theme != "" {
		f.cfg.Theme = req.Theme
	}
	if req.Items != nil {
		f.cfg.Items = req.Items
	}
	want := f.cfg.Enabled
	f.mu.Unlock()
	f.cfg.sanitize()
	f.save()

	// 行为类开关（置顶 / 穿透）改了要立刻生效，不必重开窗。
	f.applyClickThrough()
	f.applyTopmost()
	if want {
		if err := f.start(); err != nil {
			return err
		}
		f.retick()
		return nil
	}
	f.closeWin()
	return nil
}

// setGateway 网关地址/密钥变化时更新数据源（启停后 listen 可能变）。
// 地址或密钥真的变了才断开现有流：不变时这条函数每 5s 被 watchdog 调一次，
// 不能跟着掐连接，否则浮窗永远在重连。
func (f *floatWin) setGateway(addr, apiKey string) {
	f.mu.Lock()
	changed := f.gwAddr != addr || f.apiKey != apiKey
	f.gwAddr, f.apiKey = addr, apiKey
	f.mu.Unlock()
	if changed {
		f.dropGateway()
	}
	f.retick()
}

// dropGateway 主动断开当前 SSE 连接并置「未连接」。
//
// 两条调用路径：setGateway 发现地址变了；watchdog 发现网关已停 / 连接悬死。
// 断开是必须的——只把 snapOK 置 false 的话，阻塞在 ReadString 上的旧流
// 会继续活着，等网关回来时「重连」的其实是那条旧连接，新地址永远连不上。
func (f *floatWin) dropGateway() {
	f.mu.Lock()
	body := f.streamBody
	if !f.snapOK && body == nil {
		f.mu.Unlock()
		return // 已经处于断开状态，别空转
	}
	f.gen++
	f.snapOK = false
	f.streamBody = nil
	f.mu.Unlock()
	if body != nil {
		_ = body.Close()
	}
	f.retick()
}

// snapState 数据源健康快照：最后一次数据帧时间、最后一次读到字节的时间
// （含心跳）、当前是否连上。watchdog 用它区分「网关空闲」与「连接已死」。
func (f *floatWin) snapState() (dataAt, readAt time.Time, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastData, f.liveReadAt, f.snapOK
}

func (f *floatWin) retick() {
	f.mu.Lock()
	h := f.hwnd
	f.mu.Unlock()
	if h != 0 {
		postFloatMessage(h, wmFloatData, 0, 0)
	}
}

// wantsWindow 上次退出时用户是不是开着悬浮窗。启动路径据此自动恢复，
// 不然「配置里 enabled=true 但重启后小窗不见了」。
func (f *floatWin) wantsWindow() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg.Enabled
}

func (f *floatWin) running() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hwnd != 0
}

// start 在专用线程上建窗口。每线程一套消息队列，不能塞进 webview 的主循环。
//
// 建窗与消息循环分两步汇报：建窗一完成就发 ready（调用方要马上知道成功没有，
// 否则开关点了没反应），消息循环则一直在跑直到 closeWin 发 WM_CLOSE。
func (f *floatWin) start() error {
	f.mu.Lock()
	if f.hwnd != 0 {
		f.mu.Unlock()
		return nil
	}
	if f.pending {
		f.mu.Unlock()
		return fmt.Errorf("悬浮窗窗口线程正在启动")
	}
	f.pending = true
	readyDone := make(chan struct{})
	f.readyDone = readyDone
	f.mu.Unlock()

	ready := make(chan error, 1)
	go func() {
		// 锁死 OS 线程：CreateWindowExW 的 HWND 与创建它的线程绑定，
		// 而 GetMessage 循环必须在同一线程上。不锁的话 Go 可能把后续代码
		// 调度到别的线程，循环永远收不到消息（窗口存在但一片空白）。
		runtime.LockOSThread()
		f.runWindow(ready)
	}()
	// 常驻接收者：即使 start 的调用方超时撤了，结果也必须被收下——
	// 超时曾把「窗口其实建出来了」当成失败，之后再没人管它，浮窗就
	// 一直挂在「网关未连接」。这里把结果落盘（winErr）并广播 readyDone。
	go func() {
		err := <-ready
		f.mu.Lock()
		f.pending = false
		f.winErr = err
		f.mu.Unlock()
		if err == nil {
			f.ensureStream()
		}
		close(readyDone)
	}()

	// 8s：建窗本身是毫秒级，慢只可能慢在 OS 消息线程的调度上。等够长的
	// 同时保留 pending——超时只是「这一轮没等到」，不是「放弃建窗」。
	select {
	case <-readyDone:
		f.mu.Lock()
		err, h := f.winErr, f.hwnd
		f.mu.Unlock()
		if err != nil {
			return err
		}
		if h == 0 {
			return fmt.Errorf("悬浮窗窗口线程报告成功但窗口不存在")
		}
		return nil
	case <-time.After(8 * time.Second):
		return fmt.Errorf("悬浮窗窗口线程启动超时（仍在后台等待）")
	}
}

// ensureStream 启动全进程唯一的数据源循环。幂等。
func (f *floatWin) ensureStream() {
	f.mu.Lock()
	if f.streamOn {
		f.mu.Unlock()
		return
	}
	f.streamOn = true
	f.mu.Unlock()
	go f.streamLoop()
}

func (f *floatWin) closeWin() {
	f.mu.Lock()
	h := f.hwnd
	f.hwnd = 0
	f.mu.Unlock()
	if h != 0 {
		postFloatMessage(h, wmClose, 0, 0)
	}
}

// runWindow 建窗 + 消息循环。建窗结果（成功为 nil）通过 ready 汇报一次；
// 之后的循环一直阻塞到 WM_QUIT。
func (f *floatWin) runWindow(ready chan<- error) {
	className, err := windows.UTF16PtrFromString(floatClassName)
	if err != nil {
		ready <- err
		return
	}
	// hInstance 取本模块句柄：用 0 注册的类在某些环境下 CreateWindowExW 会找不到类。
	hmod, _, _ := pGetModuleHandleW.Call(0)
	inst := windows.Handle(hmod)
	wc := wndClassExW{
		Size:      uint32(unsafe.Sizeof(wndClassExW{})), // cbSize 必填：留 0 注册必失败
		Style:     0x0008 | 0x0002,                      // CS_DBLCLKS | CS_HREDRAW
		WndProc:   floatWindowProcAddr(),
		Instance:  inst,
		ClassName: className,
	}
	// 类只注册一次：重复注册会失败，且第二次的 WndProc 不会被采纳。
	if ret, _, rerr := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); ret == 0 {
		// 1410 = ERROR_CLASS_ALREADY_EXISTS：别的线程/上一次已注册过同名类，正常。
		if errno, ok := rerr.(syscall.Errno); !ok || uint32(errno) != 1410 {
			err := fmt.Errorf("RegisterClassExW: %v (size=%d style=%d hmod=%d)",
				callErr(rerr), wc.Size, wc.Style, hmod)
			ready <- err
			return
		}
	}

	title, _ := windows.UTF16PtrFromString("Free2API")
	style, exStyle := floatWindowStyles()
	hwnd, _, cerr := pCreateWindowExW.Call(
		uintptr(exStyle),
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		uintptr(style),
		0, 0, 10, 10,
		0, 0, uintptr(hmod), 0)
	if hwnd == 0 {
		ready <- fmt.Errorf("CreateWindowExW: %v (ex=0x%x style=0x%x hmod=%d class=%s)",
			callErr(cerr), exStyle, style, hmod, f.className)
		return
	}

	f.mu.Lock()
	f.hwnd = hwnd
	f.mu.Unlock()
	f.applyClickThrough()
	f.applyTopmost()

	// 摆位：记住上次的位置；没有或已移出屏幕（换显示器、分辨率变了）就回落到右下角。
	// 一次取齐：layout() 内部自己会取 f.mu，这外面**不能**再持着（会自死锁，
	// 表现是「点开浮窗 5 秒后报启动超时」——自死锁的现场只有一把锁，很难一眼看出）。
	f.mu.Lock()
	theme, snapOK := f.cfg.Theme, f.snapOK
	savedX, savedY, havePos := f.cfg.X, f.cfg.Y, f.cfg.HavePos
	f.mu.Unlock()
	w, h := sc96(floatBaseWidth), floatHeightFor(f.layout(paletteFor(theme), floatSnapshot{}, snapOK, 0.55, 0))
	wa, ok := primaryWorkArea()
	x, y := int(wa.Right)-w-24, int(wa.Bottom)-h-24
	if !ok {
		x, y = 80, 80
	}
	if havePos && onScreen(savedX, savedY, w, h, wa, ok) {
		x, y = savedX, savedY
	}

	// 摆到最终位置（此时窗口仍不可见，用户看不到任何中间态）。
	const swpNoActivate = 0x0010
	_, _, _ = pSetWindowPos.Call(hwnd, hwndTopmost,
		uintptr(int32(x)), uintptr(int32(y)), uintptr(int32(w)), uintptr(int32(h)),
		swpNoActivate)

	// 先画第一帧再显示：UpdateLayeredWindow 之后内容就交出去了，如果先 ShowWindow
	// 再 paint，中间会有一帧是上一次遗留的位图（重开开关时就是旧数据，甚至是空的）。
	f.paint(hwnd)
	_, _, _ = pShowWindow.Call(hwnd, swShowNoActivate)

	// 40ms 一帧 ≈ 25fps：呼吸点够顺滑，且几乎不耗电。
	_, _, _ = pSetTimer.Call(hwnd, 1, 40, 0)
	ready <- nil

	var m msgT
	for {
		r, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		_, _, _ = pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}

	_, _, _ = pKillTimer.Call(hwnd, 1, 0)
	f.savePosition(hwnd)

	f.mu.Lock()
	if f.hwnd == hwnd {
		f.hwnd = 0
	}
	f.mu.Unlock()
}

// floatWindowStyles 悬浮窗的窗口样式 / 扩展样式。
//
// 抽成函数是为了让「绝不能带 WS_VISIBLE」这条变成可断言的契约（见
// TestFloatWindowStylesNotVisibleAtCreation）。
//
// WS_POPUP：无标题栏无边框。WS_EX_LAYERED：逐像素透明。
// WS_EX_NOACTIVATE：绝不抢焦点（否则会在你打字时把输入焦点抢走）。
//
// **刻意不带 WS_VISIBLE**：带了的话 CreateWindowExW 一返回窗口就已可见，而此刻它
// 还停在调用里给的占位矩形（0,0,10,10）上、也还没画过任何内容——屏幕上会「啪」地
// 闪出一个左上角的小方块，再被后面的 SetWindowPos 挪走。真实截图里就是这个方块。
// 顺序必须是：隐藏建窗 → 摆位 → 画好第一帧 → ShowWindow。
func floatWindowStyles() (style, exStyle uint32) {
	return uint32(wsPopup), uint32(wsExLayered | wsExToolWindow | wsExNoActivate | wsExTopmost)
}

// onScreen 判断记住的位置是否还落在（主屏）工作区里。
func onScreen(x, y, w, h int, wa rECT, ok bool) bool {
	if !ok {
		return false
	}
	return x >= int(wa.Left)-40 && y >= int(wa.Top)-40 &&
		x < int(wa.Right)-40 && y < int(wa.Bottom)-40
}

func (f *floatWin) savePosition(hwnd uintptr) {
	var r rECT
	if ret, _, _ := pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r))); ret == 0 {
		return
	}
	f.mu.Lock()
	f.cfg.X, f.cfg.Y, f.cfg.HavePos = int(r.Left), int(r.Top), true
	f.mu.Unlock()
	f.save()
}

// applyClickThrough 按 floatItemClick 重设鼠标穿透位。
func (f *floatWin) applyClickThrough() {
	f.mu.Lock()
	h := f.hwnd
	pass := containsStr(f.cfg.Items, floatItemClick)
	f.mu.Unlock()
	if h == 0 {
		return
	}
	cur, _, _ := pGetWindowLongPtrW.Call(h, uintptr(gwlpExStyle))
	next := cur
	if pass {
		next |= wsExTransparent
	} else {
		next &^= wsExTransparent
	}
	if next != cur {
		_, _, _ = pSetWindowLongPtrW.Call(h, uintptr(gwlpExStyle), next)
	}
}

func (f *floatWin) applyTopmost() {
	f.mu.Lock()
	h := f.hwnd
	top := containsStr(f.cfg.Items, floatItemTopmost)
	f.mu.Unlock()
	if h == 0 {
		return
	}
	const (
		swpNoMove   = 0x0002
		swpNoSize   = 0x0001
		swpNoActi   = 0x0010
		swpNoOwnerZ = 0x0200
		flags       = swpNoMove | swpNoSize | swpNoActi | swpNoOwnerZ
	)
	after := uintptr(0xFFFFFFFF) // HWND_TOPMOST
	if !top {
		after = uintptr(0xFFFFFFFE) // HWND_NOTOPMOST
	}
	_, _, _ = pSetWindowPos.Call(h, after, 0, 0, 0, 0, flags)
}

// ---------------------------------------------------------------- SSE 数据源

// streamLoop 订阅网关的 /v1/stats/stream，按设置里的区间。
//
// 为什么要自己解 SSE：gateway 的 key 只对这条路由放行 ?api_key=，而 http.Client
// 带不了自定义 UA 之外的问题——SSE 只是文本帧，直接 bufio 扫 "data: " 前缀即可，
// 不引入任何依赖（EventSource 是浏览器 API）。
func (f *floatWin) streamLoop() {
	backoff := time.Second
	for {
		select {
		case <-f.stopChan():
			return
		default:
		}
		// 窗口线程报错（比如上次超时后实际建窗失败）：别在这里无声重试，
		// 交回 ensureLoop / 设置页；但窗口不在了要清掉连接状态。
		f.mu.Lock()
		winErr := f.winErr
		f.mu.Unlock()
		if winErr != nil {
			f.mu.Lock()
			f.snapOK = false
			f.mu.Unlock()
			select {
			case <-f.stopChan():
				return
			case <-time.After(3 * time.Second):
			}
			continue
		}

		started := time.Now()
		err := f.streamOnce()
		if err != nil {
			// 这次流「活过多久」决定退避怎么走：一上来就失败（端点没起来/地址不
			// 对）要退避重试；连上并跑了一阵才断（网关重启、网络抖动）则立即
			// 重连，否则每次重启都要白白等 2s/4s/8s，浮窗会挂着「网关未连接」。
			f.mu.Lock()
			if time.Since(started) > 10*time.Second {
				backoff = time.Second
			}
			f.snapOK = false
			f.mu.Unlock()
			log.Printf("悬浮窗数据源断开: %v（%s 后重连）", err, backoff)
			f.retick()
		}
		select {
		case <-f.stopChan():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// stopChan 数据源退出信号。窗口关闭不结束它（下次开关窗直接复用），
// 只有进程退出才停。
func (f *floatWin) stopChan() <-chan struct{} {
	f.mu.Lock()
	if f.stop == nil {
		f.stop = make(chan struct{})
	}
	ch := f.stop
	f.mu.Unlock()
	return ch
}

func (f *floatWin) streamOnce() error {
	f.mu.Lock()
	base, key, rng, gen := f.gwAddr, f.apiKey, f.cfg.Range, f.gen
	started := time.Now()
	f.mu.Unlock()
	if base == "" {
		return fmt.Errorf("网关地址未就绪")
	}
	url := "http://" + base + "/v1/stats/stream?range=" + rng
	if key != "" {
		url += "&api_key=" + urlQueryEscape(key)
	}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f.mu.Lock()
	// 连上后又换了地址（setGateway）：这条流已经过期，立刻收掉重开，
	// 否则会一直读着旧网关的数据，而新地址再也不会有连接。
	stale := f.gen != gen
	f.snapOK = !stale
	if !stale {
		f.streamBody = resp.Body
		f.liveReadAt = started
	}
	f.mu.Unlock()
	f.retick()
	if stale {
		return fmt.Errorf("网关地址已变化，重连")
	}
	defer func() {
		f.mu.Lock()
		if f.streamBody == resp.Body {
			f.streamBody = nil
		}
		f.mu.Unlock()
	}()

	rd := bufio.NewReaderSize(resp.Body, 64*1024)
	for {
		line, rerr := rd.ReadString('\n')
		if rerr != nil {
			f.mu.Lock()
			f.liveReadAt = time.Now()
			f.mu.Unlock()
			return rerr
		}
		f.mu.Lock()
		f.liveReadAt = time.Now()
		f.mu.Unlock()
		line = strings.TrimRight(line, "\r\n")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var s floatSnapshot
		if json.Unmarshal([]byte(payload), &s) != nil {
			continue
		}
		f.mu.Lock()
		f.snap, f.snapOK, f.lastData = s, true, time.Now()
		f.mu.Unlock()
		f.retick()
	}
}

func urlQueryEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	return b.String()
}

// ---------------------------------------------------------------- Win32 常量

const (
	wsPopup         = 0x80000000
	wsVisible       = 0x10000000
	wsExTopmost     = 0x00000008
	wsExToolWindow  = 0x00000080
	wsExLayered     = 0x00080000
	wsExTransparent = 0x00000020
	wsExNoActivate  = 0x08000000

	swShowNoActivate = 4

	wmDestroy    = 0x0002
	wmClose      = 0x0010
	wmEraseBkgnd = 0x0014
	wmNCHitTest  = 0x0084
	wmTimer      = 0x0113
	wmPaint      = 0x000F
	wmApp        = 0x8000
	wmFloatData  = wmApp + 1

	htCaption = 2

	gwlpExStyle = ^uintptr(19) // -20 = GWL_EXSTYLE

	ulwAlpha     = 0x00000002
	acSrcAlpha   = 0x01
	biRGB        = 0
	dibRGBColors = 0

	taTop  = 0x00000000
	taLeft = 0x00000000
	wmTA   = taTop | taLeft
	// antialiased = 4 而不是 cleartype = 5：ClearType 是给不透明背景设计的，
	// 在 32bpp 带 alpha 的 DIB 上它会把文字按子像素混色，深色底上实测发闷
	// （前景 232 落到屏上只剩 ~140）。灰度抗锯齿在这里观感好得多。
	antialiased   = 4
	transparentBk = 1
	psSolid       = 0
	psEndRound    = 2

	// DPI 基准：设计与缩放都在 96 DPI 上写死，按实际 DPI 整体放大。
	baseDPI = 96.0
)

var (
	fuser32   = windows.NewLazySystemDLL("user32.dll")
	fkernel32 = windows.NewLazySystemDLL("kernel32.dll")
	fgdi32    = windows.NewLazySystemDLL("gdi32.dll")

	pRegisterClassExW    = fuser32.NewProc("RegisterClassExW")
	pCreateWindowExW     = fuser32.NewProc("CreateWindowExW")
	pDefWindowProcW      = fuser32.NewProc("DefWindowProcW")
	pDestroyWindow       = fuser32.NewProc("DestroyWindow")
	pShowWindow          = fuser32.NewProc("ShowWindow")
	pUpdateWindow        = fuser32.NewProc("UpdateWindow")
	pGetMessageW         = fuser32.NewProc("GetMessageW")
	pTranslateMessage    = fuser32.NewProc("TranslateMessage")
	pDispatchMessageW    = fuser32.NewProc("DispatchMessageW")
	pPostMessageW        = fuser32.NewProc("PostMessageW")
	pPostQuitMessage     = fuser32.NewProc("PostQuitMessage")
	pGetDpiForWindow     = fuser32.NewProc("GetDpiForWindow")
	pSetWindowPos        = fuser32.NewProc("SetWindowPos")
	pGetWindowRect       = fuser32.NewProc("GetWindowRect")
	pGetDC               = fuser32.NewProc("GetDC")
	pReleaseDC           = fuser32.NewProc("ReleaseDC")
	pSetTimer            = fuser32.NewProc("SetTimer")
	pKillTimer           = fuser32.NewProc("KillTimer")
	pSetForegroundWindow = fuser32.NewProc("SetForegroundWindow")
	pUpdateLayeredWindow = fuser32.NewProc("UpdateLayeredWindow")
	pGetWindowLongPtrW   = fuser32.NewProc("GetWindowLongPtrW")
	pSetWindowLongPtrW   = fuser32.NewProc("SetWindowLongPtrW")
	pLoadImageW          = fuser32.NewProc("LoadImageW")

	pCreateCompatibleDC = fgdi32.NewProc("CreateCompatibleDC")
	pDeleteDC           = fgdi32.NewProc("DeleteDC")
	pCreateDIBSection   = fgdi32.NewProc("CreateDIBSection")
	pSelectObject       = fgdi32.NewProc("SelectObject")
	pDeleteObject       = fgdi32.NewProc("DeleteObject")
	pCreateFontW        = fgdi32.NewProc("CreateFontW")
	pCreateSolidBrush   = fgdi32.NewProc("CreateSolidBrush")
	pCreatePen          = fgdi32.NewProc("CreatePen")
	pSetBkMode          = fgdi32.NewProc("SetBkMode")
	pSetTextColor       = fgdi32.NewProc("SetTextColor")
	pSelectClipRgn      = fgdi32.NewProc("SelectClipRgn")
	pCreateRoundRectRgn = fgdi32.NewProc("CreateRoundRectRgn")
	pCreateRectRgn      = fgdi32.NewProc("CreateRectRgn")
	pCreateEllipticRgn  = fgdi32.NewProc("CreateEllipticRgn")
	pOffsetRgn          = fgdi32.NewProc("OffsetRgn")
	pFillRgn            = fgdi32.NewProc("FillRgn")
	pSetTextAlign       = fgdi32.NewProc("SetTextAlign")
	pGetModuleHandleW   = fkernel32.NewProc("GetModuleHandleW")
	pFillRect           = fuser32.NewProc("FillRect")
	pDrawTextW          = fuser32.NewProc("DrawTextW")
	pGdiFlush           = fgdi32.NewProc("GdiFlush")
)

type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

type point struct{ X, Y int32 }
type msgT struct {
	HWnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type blendFunction struct {
	BlendOp             byte
	BlendFlags          byte
	SourceConstantAlpha byte
	AlphaFormat         byte
}

const hwndTopmost = ^uintptr(0)   // HWND_TOPMOST = -1
const hwndNotopmost = ^uintptr(1) // HWND_NOTOPMOST = -2
const swpNoSize = 0x0001
const swpNoMove = 0x0002
const swpNoActivate = 0x0010
const swpNoOwnerZ = 0x0200

// lastErr 取 Win32 last-error。仅用于失败分支的日志文案，走 GetLastError 裸调
// （x/sys/windows 没有把它导出成函数，只有 Errno 常量）。
// callErr 把 syscall.LazyProc.Call 的第三个返回值（errno）转成可读文本。
//
// 为什么不用 GetLastError()：它读的是「当前」last-error，而前一次成功的
// user32/gdi32 调用已经把它清掉了。syscall 在 Call 内部已经抓下本次调用的
// last-error 作为 errno 返回，直接用它才有诊断价值（实测 CreateWindowExW 失败时
// windows.GetLastError() 永远给 0，errno 才有真正的 1410/1400 之类）。
// logBlitErr 悬浮窗贴图失败的去重日志。贴图一秒数帧，真出问题时逐帧打日志会瞬间
// 吃掉几百 MB 日志（且日志本身抢磁盘、把浮窗拖得更卡）。同一个错误只打第一遍，
// 之后只在「间隔超过 30s」时再提醒一次——浮窗在跑就说明还在尝试贴，故障持续可见。
func (f *floatWin) logBlitErr(msg string) {
	f.blitLogMu.Lock()
	defer f.blitLogMu.Unlock()
	now := time.Now()
	if f.lastBlitErr == msg && now.Sub(f.lastBlitErrAt) < 30*time.Second {
		return
	}
	f.lastBlitErr, f.lastBlitErrAt = msg, now
	log.Printf("UpdateLayeredWindow: %s", msg)
}
func callErr(err error) string {
	if err == nil {
		return "0(nil)"
	}
	if errno, ok := err.(syscall.Errno); ok {
		return fmt.Sprintf("%d(%s)", uint32(errno), errno.Error())
	}
	return err.Error()
}

// floateWindowProc 返回窗口过程地址。Go 函数不能直接当 C 回调用，必须给一个
// 稳定的汇编桩；这里走 windows 的 SyscallN 之外最省事的办法：用
// x/sys 提供的 NewCallback 语义不成立时的兜底——直接取函数指针并配合
// 下面的回调桥。
var floatWndProc uintptr

// floatWindowProcAddr 把 Go 函数变成 C 可调用的回调地址。
// windows.NewCallback 会把闭包/GC 管理的函数体固定在一个永久地址上，
// 句柄缓存起来避免每次建窗都新建一份（每份都要在 syscall 里永久登记）。
func floatWindowProcAddr() uintptr {
	if floatWndProc == 0 {
		floatWndProc = windows.NewCallback(floatWindowProc)
	}
	return floatWndProc
}

// floatWindowProc 窗口过程。
func floatWindowProc(hwnd uintptr, msg uint32, wparam, lparam uintptr) uintptr {
	switch msg {
	case wmNCHitTest:
		// 整窗当标题栏：按住任意位置即可拖动。小窗没有标题栏，
		// 不这样用户根本挪不动它。
		return htCaption
	case wmDestroy:
		_, _, _ = pPostQuitMessage.Call(0)
		return 0
	case wmEraseBkgnd:
		// 全窗口自绘（UpdateLayeredWindow 的 alpha 已含圆角外的 0），
		// 返回 1 阻止系统用类背景刷擦一遍（那会糊掉圆角）。
		return 1
	case wmTimer:
		if f := floatFromHWND(hwnd); f != nil {
			f.mu.Lock()
			f.phase += 0.055
			f.mu.Unlock()
			f.paint(hwnd)
		}
		return 0
	case wmFloatData:
		if f := floatFromHWND(hwnd); f != nil {
			f.paint(hwnd)
		}
		return 0
	case wmClose:
		_, _, _ = pDestroyWindow.Call(hwnd)
		return 0
	case 0x0202: // WM_LBUTTONUP
		if f := floatFromHWND(hwnd); f != nil {
			// 鼠标穿透时事件到不了这里；能收到就是可点的：把控制台叫到前面。
			_, _, _ = pSetForegroundWindow.Call(hwnd)
		}
		return 0
	}
	ret, _, _ := pDefWindowProcW.Call(hwnd, uintptr(msg), wparam, lparam)
	return ret
}

func floatFromHWND(hwnd uintptr) *floatWin {
	// 窗口创建时 hInstance=0，没有可用的 GWLP_USERDATA 载体；
	// 这里用全局 floatOwner（进程唯一浮窗）直接拿状态，避免一堆 SetWindowLongPtr
	// 的跨线程指针游戏。
	return floatOwner
}

func postFloatMessage(hwnd uintptr, msg uint32, wp, lp uintptr) {
	_, _, _ = pPostMessageW.Call(hwnd, uintptr(msg), wp, lp)
}

// ---------------------------------------------------------------- 尺寸 / DPI

// floatRow 一行待绘制的内容。**先把行列出来再决定窗口多高**，是这个布局能不出错的
// 唯一办法：早先的写法是「先按猜的行数定高，再按开关逐行往上画」，结果用户多开两
// 个开关，内容就画到窗口外面去了（屏幕上表现为「只有第一行，后面的看不见」）。
type floatRow struct {
	text     string
	fontSize int32
	weight   int32
	color    [3]uint8
	// spaceBefore 这一行上方额外留白（行距）。0 = 默认紧贴上一行。
	spaceBefore int32
	// bullet 这行前面画一个呼吸点（首行标题用）。
	bullet bool
}

// layout 把当前配置 + 数据摊成若干行。
func (f *floatWin) layout(pal floatPalette, snap floatSnapshot, snapOK bool, pulse float64, phase float64) []floatRow {
	f.mu.Lock()
	cfg := f.cfg
	f.mu.Unlock()

	rows := []floatRow{{
		text: f.headlineText(snap, snapOK), fontSize: 19, weight: fwBold,
		color: [3]uint8{pal.fgR, pal.fgG, pal.fgB}, spaceBefore: 0, bullet: true,
	}}
	dotCol := f.dotColor(pal, snap, pulse, phase)
	rows[0].color = dotCol // bullet 自己画颜色，标题文字仍用 fg
	rows[0].text = f.headlineText(snap, snapOK)

	first := true
	for _, item := range cfg.Items {
		var r floatRow
		switch item {
		case floatItemInFlight:
			r = floatRow{text: f.inflightLine(snap), fontSize: 13, color: [3]uint8{pal.dimR, pal.dimG, pal.dimB}}
		case floatItemRate:
			r = floatRow{text: f.rateLine(snap), fontSize: 16, weight: fwBold, color: [3]uint8{pal.acR, pal.acG, pal.acB}}
		case floatItemRequests:
			r = floatRow{text: f.rangeLabel(cfg.Range) + " " + fmtInt64(snap.Total.Requests) + " 次",
				fontSize: 13, color: [3]uint8{pal.dimR, pal.dimG, pal.dimB}}
		case floatItemTokens:
			r = floatRow{text: "总用量 " + fmtInt64(snap.Total.TotalTokens),
				fontSize: 13, color: [3]uint8{pal.dimR, pal.dimG, pal.dimB}}
		case floatItemSource:
			r = floatRow{text: f.sourceLine(snap), fontSize: 13, color: [3]uint8{pal.dimR, pal.dimG, pal.dimB}}
		default:
			continue // topmost / passthru 是行为开关，不占行
		}
		if first {
			r.spaceBefore = 8 // 标题与正文之间留一道缝
			first = false
		} else {
			r.spaceBefore = 4 // 行与行之间的小行距
		}
		rows = append(rows, r)
	}
	return rows
}

// rowHeight 一行占用的高度：文字高度 + 上方留白。留一点下沿余量给抗锯齿。
func rowHeight(r floatRow) int32 { return r.spaceBefore + r.fontSize + 5 }

// floatBaseWidth 基准宽度（96 DPI 下）。
const floatBaseWidth = 300

// sc96 96 DPI 上的裸像素值（窗口摆位等不需要 DPI 缩放的地方用）。
func sc96(v int) int { return v }

// floatHeightFor 按行高累加出窗口高度。**唯一的高度来源**——窗口与内容都由它算，
// 不会再出现「内容画到窗口外面」。
func floatHeightFor(rows []floatRow) int {
	h := 0
	for _, r := range rows {
		h += int(rowHeight(r))
	}
	return h + 16 // 上下各 8px 内边距
}

func (f *floatWin) windowDPI(hwnd uintptr) float64 {
	if ret, _, _ := pGetDpiForWindow.Call(hwnd); ret != 0 {
		return float64(ret)
	}
	return baseDPI
}

// ---------------------------------------------------------------- 绘制

type floatPaint struct {
	bg     [4]uint8 // R,G,B,A（预乘 alpha 用）
	fg     [3]uint8
	fgDim  [3]uint8
	accent [3]uint8
	scale  float64
	radius int
}

type floatPalette struct {
	bgR, bgG, bgB    uint8
	fgR, fgG, fgB    uint8
	dimR, dimG, dimB uint8
	acR, acG, acB    uint8
}

func paletteFor(theme string) floatPalette {
	if theme == "light" {
		return floatPalette{250, 250, 250, 24, 24, 27, 113, 113, 122, 9, 102, 234}
	}
	return floatPalette{20, 21, 26, 232, 234, 240, 148, 152, 168, 96, 165, 250}
}

// paint 一帧自绘：建 32bpp DIB → 画内容 → 补 alpha → UpdateLayeredWindow。
func (f *floatWin) paint(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	dpi := f.windowDPI(hwnd)
	s := dpi / baseDPI
	w, h := sc96(floatBaseWidth), floatHeightFor(f.probeRows())

	pixels := f.render(s)
	if pixels == nil {
		return
	}
	f.blit(hwnd, s, w, h, pixels)
}

// render 把一帧画进一块新建的 32bpp 预乘 DIB，返回 BGRA 像素。
//
// 为什么单独拆出来：painting 是「能不能看见」的唯一关口，而 UpdateLayeredWindow
// 一旦把内容交上去就再也读不回来——出了 bug 只能靠屏幕截图，单元测试完全够不着。
// 拆开之后单测直接对 render 的返回值数像素（见 floatwin_paint_test.go）。
func (f *floatWin) render(s float64) []byte {
	f.mu.Lock()
	cfg := f.cfg
	snap := f.snap
	snapOK := f.snapOK
	phase := f.phase
	f.mu.Unlock()

	pal := paletteFor(cfg.Theme)
	sc := func(v float64) int32 { return int32(v*s + 0.5) }
	// 呼吸点：只有真有请求在飞时才呼吸。空闲时钉在暗档不闪——否则会出现
	//「明明没在跑、亮点却一直闪」，那正是用户被看门狗闪到过的原因。
	pulse := 0.55
	if len(snap.LiveModels) > 0 {
		pulse = 0.55 + 0.45*(0.5+0.5*math.Sin(phase))
	}

	rows := f.layout(pal, snap, snapOK, pulse, phase)
	w := int32(sc96(floatBaseWidth))
	h := int32(sc96(floatHeightFor(rows)))

	screenDC, _, _ := pGetDC.Call(0)
	if screenDC == 0 {
		return nil
	}
	memDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		pReleaseDC.Call(0, screenDC)
		return nil
	}
	bih := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(w),
		Height:      -int32(h), // 负数 = 自上而下
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	var bits unsafe.Pointer
	dib, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bih)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 {
		pDeleteDC.Call(memDC)
		pReleaseDC.Call(0, screenDC)
		return nil
	}
	oldBmp, _, _ := pSelectObject.Call(memDC, dib)

	// 清成全透明：UpdateLayeredWindow 按 alpha 合成，残留字节会显出脏边。
	pixels := unsafe.Slice((*byte)(bits), w*h*4)
	for i := range pixels {
		pixels[i] = 0
	}
	// RGB 填底色，后面的 GDI 笔画与文本直接画在这一层上。
	for i := 0; i < len(pixels); i += 4 {
		pixels[i], pixels[i+1], pixels[i+2] = pal.bgR, pal.bgG, pal.bgB
	}

	// 圆角裁剪：GDI 画的东西自动被裁圆。
	radius := radiusFor(s)
	clip, _, _ := pCreateRoundRectRgn.Call(0, 0, uintptr(w), uintptr(h),
		uintptr(2*radius), uintptr(2*radius))
	oldClip, clipSel := uintptr(0), false
	if clip != 0 {
		oldClip, _, _ = pSelectClipRgn.Call(memDC, clip)
		clipSel = true
	}

	// 底色
	const fillAlpha = uint8(238)
	br, _, _ := pCreateSolidBrush.Call(uintptr(colorRef(pal.bgR, pal.bgG, pal.bgB)))
	if br != 0 {
		var rect rECT
		rect.Right, rect.Bottom = int32(w), int32(h)
		_, _, _ = pFillRect.Call(memDC, uintptr(unsafe.Pointer(&rect)), br)
		pDeleteObject.Call(br)
	}

	padX := sc(14)
	padTop := sc(8)
	textX := padX
	// 有呼吸点时标题往右让一个点的位置。
	if len(rows) > 0 && rows[0].bullet {
		dotR := sc(4)
		dotY := padTop + sc(6) + dotR
		dotCol := f.dotColor(pal, snap, pulse, phase)
		dbr, _, _ := pCreateSolidBrush.Call(uintptr(colorRef(dotCol[0], dotCol[1], dotCol[2])))
		if dbr != 0 {
			rgn, _, _ := pCreateEllipticRgn.Call(0, 0, uintptr(2*dotR), uintptr(2*dotR))
			_, _, _ = pOffsetRgn.Call(rgn, uintptr(padX-dotR), uintptr(dotY-dotR))
			_, _, _ = pFillRgn.Call(memDC, rgn, dbr)
			pDeleteObject.Call(rgn)
			pDeleteObject.Call(dbr)
		}
		textX = padX + sc(13)
	}
	innerW := int32(w) - textX - padX

	y := padTop
	for i, r := range rows {
		y += sc(float64(r.spaceBefore))
		fontSize := sc(float64(r.fontSize))
		lineH := fontSize + sc(5)
		weight := int32(0)
		if r.weight != 0 {
			weight = r.weight
		}
		col := r.color
		if i == 0 {
			col = [3]uint8{pal.fgR, pal.fgG, pal.fgB} // 标题用前景色，点自己画
		}
		drawFloatText(memDC, r.text, textX, y, innerW, lineH, fontSize, weight,
			col[0], col[1], col[2], antialiased)
		y += lineH
	}

	// GDI 只改 RGB 不动 alpha，这里统一补：圆角内部 alpha=fillAlpha，圆角外=0，
	// 同时按 alpha 预乘（UpdateLayeredWindow 的 AC_SRC_ALPHA 要求预乘）。
	applyRoundedAlpha(pixels, int(w), int(h), radius, fillAlpha)

	out := make([]byte, len(pixels))
	copy(out, pixels)

	if clipSel {
		pSelectClipRgn.Call(memDC, oldClip)
		pDeleteObject.Call(clip)
	}
	pSelectObject.Call(memDC, oldBmp)
	pDeleteObject.Call(dib)
	pDeleteDC.Call(memDC)
	pReleaseDC.Call(0, screenDC)
	return out
}

// probeRows 用「空数据」摊一次行——只为了在开窗时先知道窗口该多大。
func (f *floatWin) probeRows() []floatRow {
	f.mu.Lock()
	cfg := f.cfg
	snapOK := f.snapOK
	f.mu.Unlock()
	return f.layout(paletteFor(cfg.Theme), floatSnapshot{}, snapOK, 0.55, 0)
}

// dotColor 呼吸点的颜色：空闲 = 暗档固定；在飞 = 主色按 phase 明暗呼吸。
func (f *floatWin) dotColor(pal floatPalette, snap floatSnapshot, pulse float64, phase float64) [3]uint8 {
	_ = phase
	if len(snap.LiveModels) == 0 {
		d := 0.55
		return [3]uint8{uint8(float64(pal.dimR) * d), uint8(float64(pal.dimG) * d), uint8(float64(pal.dimB) * d)}
	}
	return [3]uint8{
		uint8(float64(pal.acR)*pulse + float64(pal.dimR)*(1-pulse)),
		uint8(float64(pal.acG)*pulse + float64(pal.dimG)*(1-pulse)),
		uint8(float64(pal.acB)*pulse + float64(pal.dimB)*(1-pulse)),
	}
}

// blit 把 render 的结果贴到窗口上（UpdateLayeredWindow，源 DC 来自一块一次性位图）。
func (f *floatWin) blit(hwnd uintptr, s float64, w, h int, pixels []byte) {
	_ = s
	screenDC, _, _ := pGetDC.Call(0)
	if screenDC == 0 {
		return
	}
	defer pReleaseDC.Call(0, screenDC)
	memDC, _, _ := pCreateCompatibleDC.Call(screenDC)
	if memDC == 0 {
		return
	}
	defer pDeleteDC.Call(memDC)

	src := copyPixelsToDIB(memDC, pixels, w, h)
	if src == 0 {
		return
	}
	defer pDeleteObject.Call(src)
	old, _, _ := pSelectObject.Call(memDC, src)
	defer pSelectObject.Call(memDC, old)

	sz := point{X: int32(w), Y: int32(h)}
	ptSrc := point{X: 0, Y: 0}
	ptDst := point{}
	var wr rECT
	if ret, _, _ := pGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&wr))); ret != 0 {
		ptDst.X, ptDst.Y = wr.Left, wr.Top
	}
	blend := blendFunction{BlendOp: acSrcAlpha, BlendFlags: 0, SourceConstantAlpha: 255, AlphaFormat: acSrcAlpha}
	_, _, ferr := pUpdateLayeredWindow.Call(hwnd, screenDC,
		uintptr(unsafe.Pointer(&ptDst)), uintptr(unsafe.Pointer(&sz)),
		memDC, uintptr(unsafe.Pointer(&ptSrc)),
		0, uintptr(unsafe.Pointer(&blend)), ulwAlpha)
	// 判错必须比 errno 的零值，不能比 nil。syscall.Proc.Call 第三个返回值是具体的
	// Errno 类型（不是 error 接口）：Errno(0) 作为 interface 与 nil 比较恒为 true，
	// 于是这一行在**每一帧成功时**都会执行。悬浮窗一秒数帧，desktop.log 就是这样
	// 被刷成 25 MB / 天的。之前那句 `ferr != nil` 等于「只要在画就报错」。
	if errno, ok := ferr.(syscall.Errno); !ok || errno != 0 {
		f.logBlitErr(callErr(ferr))
	}
}

// copyPixelsToDIB 把一段 BGRA 像素塞进一块新建的 DIB 并返回它的句柄（已选入 DC）。
func copyPixelsToDIB(memDC uintptr, pixels []byte, w, h int) uintptr {
	bih := bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       int32(w),
		Height:      -int32(h),
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}
	var bits unsafe.Pointer
	dib, _, _ := pCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bih)), dibRGBColors,
		uintptr(unsafe.Pointer(&bits)), 0, 0)
	if dib == 0 {
		return 0
	}
	copy(unsafe.Slice((*byte)(bits), len(pixels)), pixels)
	return dib
}

func radiusFor(s float64) int { return int(float64(16) * s) }

func colorRef(r, g, b uint8) uint32 { return uint32(r) | uint32(g)<<8 | uint32(b)<<16 }

// roundedRectSDF 圆角矩形的有符号距离场，内部为负、外部为正。
//
// 标准形式（iq）：q = |p - 中心| - (半尺寸 - r)；d = length(max(q,0))
// + min(max(qx,qy), 0) - r。
//
// 这里踩过一个坑值得记下来：漏掉末尾那个 `- r`，函数在形状**内部**会返回
// 正数（外面反而更负），于是一层本该半透明的窗会整块被判成「全透明」，
// 而内部区又恒等于 cov>1 被夹到 1 —— 最终现象是「窗子只剩下圆角一小圈」。
// 顶层有 TestRoundedRectSDF 直接钉住这几个符号，回归时先炸这里。
func roundedRectSDF(fx, fy, w, h, r float64) float64 {
	qx := math.Abs(fx-w/2) - (w / 2) + r
	qy := math.Abs(fy-h/2) - (h / 2) + r
	ox, oy := math.Max(qx, 0), math.Max(qy, 0)
	return math.Hypot(ox, oy) + math.Min(math.Max(qx, qy), 0) - r
}

// alphaSamples 每像素的采样数（4x4 = 16 个子采样点）。
//
// 为什么必须超采样：alpha 覆盖率如果只用「像素中心算一次」，那圆角这种斜边
// 在每个像素上都会**整颗**落到内或外——0 或 238，中间值一个都没有，视觉上就是
// 硬锯齿。16 个子采样点按「多少比例落在形状内」来定 alpha，斜边才有真正的
// 半透明过渡。16 是够用的最小值（3x3 在浅色背景上还能看出台阶）。
const (
	alphaSubSamples = 4
	alphaSubTotal   = alphaSubSamples * alphaSubSamples
)

// applyRoundedAlpha 给 DIB 补 alpha 并按 alpha 预乘：圆角矩形内部 = alpha、
// 外部 = 0，圆角弧上按覆盖率做羽化。
//
// 为什么不能「圆内就 alpha、圆外就 0」：那样边界是 0→238 的硬跳变，屏幕上
// 浮窗四周有一圈肉眼可见的锯齿和一圈发暗的黑边（真实截图里确实是这样）。
//
// 预乘是必须的：UpdateLayeredWindow 的 AC_SRC_ALPHA 要求 RGB 已经乘过 alpha，
// 不乘的话半透明区域整体偏亮（分层窗口最经典的「发灰/发亮」故障）。
func applyRoundedAlpha(pixels []byte, w, h, radius int, alpha uint8) {
	if radius < 0 {
		radius = 0
	}
	// 半径不能超过边长一半，否则角上的圆与对边相交，形状会退化。
	if radius > w/2 {
		radius = w / 2
	}
	if radius > h/2 {
		radius = h / 2
	}
	// 内缩 feather 像素，让**整圈**边界（不只四角）都有 alpha 过渡。
	//
	// 为什么必须这么做：fillAlpha 是 238 而不是 255。若形状边界与窗口矩形
	// 重合，最外一列/一行就是满的 238 —— 17/255（≈6.7%）的背景从直边整条
	// 渗出来，而这一跳是 0→238 的**硬切**，屏幕上就是一圈与圆角风格完全不搭的
	// 直角残边（实测 dump：直边 x=0..5 全是 238，圆角处却 5 像素就从 0 爬满）。
	//
	// feather 必须 < 1，且要**四边对称内缩**：像素列 0 覆盖 [0,1]，内缩到 1.5
	// 会让它整个落在形状内（覆盖 100%，仍是满 alpha，等于没改）。0.5 时边界落在
	// 这一列正中，覆盖率 50%，alpha ≈ 119 —— 硬切被就地化成一次正常羽化。
	//
	// 注意 roundedRectSDF 把形状锚在原点（中心在 fw/2），只把 fw 改小的话
	// 右边内缩了、左边还贴在 0 上，直边依旧硬。所以采样点也要同步平移，
	// 让形状真正落在 [feather, w-feather] 这个居中的框里。
	//
	// 半径必须**同步加上 feather**，否则圆角会整圈变小、盖不住原来的角，
	// 缺口就以直角残边的形式露出来。实测（不补半径）：弧线各行起点比理想圆
	// 整体偏外 0.35~1.76px（均值 0.96px）—— 用户看到的正是这块。补上之后
	// 外轮廓大小不变，只是边界从「贴边硬切」变成「边缘 1px 羽化」。
	const feather = 0.5
	r := float64(radius)
	fw, fh := float64(w)-2*feather, float64(h)-2*feather
	step := 1.0 / float64(alphaSubSamples)
	// 子采样点在像素内的偏移（取格子中心，避免共边）。
	var offs [alphaSubSamples]float64
	for i := range offs {
		offs[i] = (float64(i) + 0.5) * step
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			inside := 0
			for _, oy := range offs {
				fy := float64(y) + oy - feather
				for _, ox := range offs {
					if roundedRectSDF(float64(x)+ox-feather, fy, fw, fh, r) <= 0 {
						inside++
					}
				}
			}
			off := (y*w + x) * 4
			if inside == 0 {
				pixels[off+3] = 0
				continue
			}
			if inside == alphaSubTotal {
				finalA := int(alpha)
				pixels[off] = uint8(int(pixels[off]) * finalA / 255)
				pixels[off+1] = uint8(int(pixels[off+1]) * finalA / 255)
				pixels[off+2] = uint8(int(pixels[off+2]) * finalA / 255)
				pixels[off+3] = uint8(finalA)
				continue
			}
			// 部分覆盖：alpha 按覆盖比例缩放，RGB 按最终 alpha 预乘一次。
			finalA := int(math.Round(float64(alpha) * float64(inside) / float64(alphaSubTotal)))
			pixels[off] = uint8(int(pixels[off]) * finalA / 255)
			pixels[off+1] = uint8(int(pixels[off+1]) * finalA / 255)
			pixels[off+2] = uint8(int(pixels[off+2]) * finalA / 255)
			pixels[off+3] = uint8(finalA)
		}
	}
}

const (
	fwNormal = 400
	fwBold   = 700
)

func drawFloatText(memDC uintptr, text string, x, y, w, h int32, fontSize int32, weight int32,
	cr, cg, cb uint8, quality uint32) {
	if text == "" || memDC == 0 {
		return
	}
	// LOGFONTW 里 FaceName 是内嵌 WCHAR[32]，不是指针。这样整块结构体是纯值，
	// 可以安全交给 syscall（往 Win32 里传「含 Go 指针的结构体」会被 GC 拒绝，
	// 传含野指针的又直接 0xc0000005 崩）。
	lf := logFontW{
		Height:      -fontSize, // 负数 = 字符高度
		Weight:      weight,
		CharSet:     1, // DEFAULT_CHARSET
		OutPrecis:   0, // OUT_DEFAULT_PRECIS
		ClipPrecis:  0, // CLIP_DEFAULT_PRECIS
		Quality:     byte(quality),
		PitchFamily: 0, // DEFAULT_PITCH | FF_SWISS
	}
	if !lf.setFace(floatFontFace) {
		return
	}
	font, _, _ := pCreateFontW.Call(
		uintptr(lf.Height), 0, 0, 0, // height, width, escapement, orientation
		uintptr(lf.Weight), 0, 0, 0, // weight, italic, underline, strikeout
		uintptr(lf.CharSet), uintptr(lf.OutPrecis), uintptr(lf.ClipPrecis),
		uintptr(lf.Quality), uintptr(lf.PitchFamily),
		uintptr(unsafe.Pointer(&lf)))
	if font == 0 {
		return
	}
	defer pDeleteObject.Call(font)
	oldFont, _, _ := pSelectObject.Call(memDC, font)
	defer pSelectObject.Call(memDC, oldFont)
	_, _, _ = pSetBkMode.Call(memDC, transparentBk)
	_, _, _ = pSetTextColor.Call(memDC, uintptr(colorRef(cr, cg, cb)))
	_, _, _ = pSetTextAlign.Call(memDC, wmTA)

	buf, err := windows.UTF16FromString(text)
	if err != nil || len(buf) == 0 {
		return
	}
	var rect rECT
	rect.Left, rect.Top, rect.Right, rect.Bottom = x, y, x+w, y+h
	// DT_SINGLELINE|DT_VCENTER|DT_END_ELLIPSIS|DT_NOPREFIX：超长模型名省略而不是换行，
	// 小窗上换行会把布局撑破。
	const dtSingleLine = 0x00000020
	const dtVCenter = 0x00000004
	const dtEndEllipsis = 0x00008000
	const dtNoPrefix = 0x00000800
	_, _, _ = pDrawTextW.Call(memDC, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)),
		uintptr(unsafe.Pointer(&rect)), uintptr(dtSingleLine|dtVCenter|dtEndEllipsis|dtNoPrefix))
}

// logFontW 与 Win32 LOGFONTW 内存布局一致（FaceName 是尾部内嵌的 WCHAR[32]）。
type logFontW struct {
	Height      int32
	Width       int32
	Escapement  int32
	Orientation int32
	Weight      int32
	Italic      byte
	Underline   byte
	StrikeOut   byte
	CharSet     byte
	OutPrecis   byte
	ClipPrecis  byte
	Quality     byte
	PitchFamily byte
	FaceName    [32]uint16
}

// setFace 写入字体名（UTF-16，超长截断）。返回 false 表示字体名太长放不下。
func (lf *logFontW) setFace(name string) bool {
	u, err := windows.UTF16FromString(name)
	if err != nil {
		return false
	}
	if len(u) > len(lf.FaceName) {
		return false
	}
	copy(lf.FaceName[:], u)
	return true
}

// floatFontFace 字体名。Segoe UI 是 Windows 自带 UI 字体，数字宽度稳定，
// 深浅色主题下都有对应的抗锯齿版本。
const floatFontFace = "Segoe UI"

// ---------------------------------------------------------------- 文案

func (f *floatWin) headlineText(snap floatSnapshot, ok bool) string {
	if !ok {
		return "网关未连接"
	}
	return "Free2API"
}

func (f *floatWin) inflightLine(snap floatSnapshot) string {
	models := snap.LiveModels
	if len(models) == 0 {
		return "空闲 · 0 在飞"
	}
	keys := make([]string, 0, len(models))
	for k := range models {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if models[keys[i]] != models[keys[j]] {
			return models[keys[i]] > models[keys[j]]
		}
		return keys[i] < keys[j]
	})
	total := 0
	for _, v := range models {
		total += v
	}
	head := fmtInt64(int64(total)) + " 在飞 · " + shortModel(keys[0])
	if len(keys) > 1 {
		head += " +" + strconv.Itoa(len(keys)-1)
	}
	return head
}

func (f *floatWin) rateLine(snap floatSnapshot) string {
	win := snap.Live.WindowSec
	if win <= 0 {
		win = 60
	}
	if snap.Live.TokensPerSec <= 0.01 && snap.Live.Requests == 0 {
		return fmt.Sprintf("%.1f tok/s · %ds 无请求", 0.0, win)
	}
	return fmt.Sprintf("%.1f tok/s · %ds 窗口 · %d 次", snap.Live.TokensPerSec, win, snap.Live.Requests)
}

func (f *floatWin) rangeLabel(r string) string {
	switch r {
	case "today":
		return "今天"
	case "7d":
		return "近 7 天"
	case "30d":
		return "近 30 天"
	default:
		return "全部"
	}
}

func (f *floatWin) sourceLine(snap floatSnapshot) string {
	if len(snap.Producers) == 0 {
		return "来源 —"
	}
	best := snap.Producers[0]
	for _, p := range snap.Producers {
		if p.Requests > best.Requests {
			best = p
		}
	}
	if best.Requests <= 0 {
		return "来源 —"
	}
	return "来源 " + producerDisplay(best.Producer)
}

func producerDisplay(p string) string {
	switch p {
	case "workbuddy":
		return "WorkBuddy"
	case "zcode":
		return "ZCode"
	case "opencode":
		return "OpenCode"
	case "kilo":
		return "Kilo Code"
	case "qoder":
		return "Qoder"
	}
	if p == "" {
		return "未标注"
	}
	return p
}

// shortModel 模型名太长时截断（保留前 24 字符）。
func shortModel(m string) string {
	if len(m) <= 24 {
		return m
	}
	return m[:23] + "…"
}

// fmtInt64 千分位整数。
func fmtInt64(n int64) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

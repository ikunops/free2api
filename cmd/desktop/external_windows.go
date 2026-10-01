//go:build windows

// external.go 停掉「不是本窗口拉起来的」那个网关进程。
//
// 为什么需要它：网关很可能是计划任务 / 命令行 / 另一个 exe 起的，这个桌面窗口只是
// 代理到那个端口。用户在概览页点「停止网关」的意图是「把网关停了」，不是
// 「把本窗口的托管关系解除」。所以先找到占用该端口的进程，确认它确实是 free2api
// （靠 /healthz 的 service 字段 + 映像名双重确认），再让它优雅退出。
//
// 优先发 WM_CLOSE 而不是 TerminateProcess：free2api.exe 是控制台程序，收到
// CTRL_CLOSE_EVENT 会走正常退出路径（落盘 state.json / Flush / 关监听）。
// 只有它不理会时才强杀——毕竟用户按的是「停止」，不是「拔电源」。
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	iphlpapi                  = windows.NewLazySystemDLL("iphlpapi.dll")
	procGetExtendedTcpTable   = iphlpapi.NewProc("GetExtendedTcpTable")
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	procQueryFullProcessImage = kernel32.NewProc("QueryFullProcessImageNameW")
)

const (
	afInet                 = 2
	tcpTableOwnerPidAll    = 5
	mibTCPStateListen      = 2
	processQueryLimited    = 0x1000
	processTerminate       = 0x0001
	processSetInformation  = 0x0200
	stillActive            = 259
)

// pidListeningOn 找占用 addr 端口、且处于 LISTEN 的进程 PID。找不到返回 0。
func pidListeningOn(addr string) (uint32, error) {
	_, portStr, err := splitHostPort(addr)
	if err != nil {
		return 0, err
	}
	var port int
	if _, err := fmt.Sscanf(portStr, "%d", &port); err != nil || port <= 0 {
		return 0, fmt.Errorf("无法解析端口 %q", addr)
	}

	var size uint32
	// 第一次调用只为拿所需缓冲区大小（必然返回 ERROR_INSUFFICIENT_BUFFER）。
	_, _, _ = procGetExtendedTcpTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPidAll, 0)
	if size == 0 {
		return 0, errors.New("GetExtendedTcpTable 没给出缓冲区大小")
	}
	buf := make([]byte, size)
	ret, _, _ := procGetExtendedTcpTable.Call(uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)), 0, afInet, tcpTableOwnerPidAll, 0)
	if ret != 0 {
		return 0, fmt.Errorf("GetExtendedTcpTable 失败（code %d）", ret)
	}

	// MIB_TCPTABLE_OWNER_PID = { DWORD dwNumEntries; MIB_TCPROW_OWNER_PID table[] }，
	// 每行 6 个 DWORD（24 字节）。这里按字节解析而不是映射成 Go struct：
	// unsafe.Pointer 转数组指针会被 go vet 判为 possible misuse，而且 struct 布局
	// 依赖对齐假设，字节解析更直白。
	const (
		headerSize = 4
		rowSize    = 24
		offState   = 0
		offLPort   = 8
		offPID     = 20
	)
	n := int(binary.LittleEndian.Uint32(buf[0:4]))
	for i := 0; i < n; i++ {
		off := headerSize + i*rowSize
		if off+rowSize > len(buf) {
			break
		}
		if binary.LittleEndian.Uint32(buf[off+offState:]) != mibTCPStateListen {
			continue
		}
		// dwLocalPort 是网络字节序（大端）的 16 位端口号，存在 DWORD 的低 16 位里。
		p := int(binary.BigEndian.Uint16(buf[off+offLPort:]))
		if p == port {
			return binary.LittleEndian.Uint32(buf[off+offPID:]), nil
		}
	}
	return 0, nil
}

// stopExternalGateway 停掉占用 addr 的那个 free2api 进程。
//
// 两条路，优先第一条：
//  1. POST /admin/shutdown —— 网关自己 cancel 生命周期，落盘 / Flush / 关监听
//     全走与 Ctrl+C 完全相同的那条路。这是「干净」的定义。
//  2. 找不到端点（老版本网关 / admin 关着 / api_key 拦了）时退回进程级：
//     找到监听端口的 PID，确认映像名里有 free2api，再 CTRL_BREAK / TerminateProcess。
func stopExternalGateway(addr string) error {
	if err := shutdownViaHTTP(addr); err == nil {
		return nil
	} else {
		lastHTTPErr = err
	}

	pid, err := pidListeningOn(addr)
	if err != nil {
		return err
	}
	if pid == 0 {
		return errors.New("端口上没有找到监听进程（可能刚好已经退了）")
	}
	h, err := windows.OpenProcess(processQueryLimited|processTerminate|processSetInformation, false, pid)
	if err != nil {
		return fmt.Errorf("打开进程 %d 失败（权限不足？）：%w", pid, err)
	}
	defer windows.CloseHandle(h)

	name, err := processImageName(h)
	if err != nil {
		return err
	}
	// 只肯停「名字里有 free2api」的进程：端口复用/配置改错时，别把别人的服务干掉。
	if !strings.Contains(strings.ToLower(name), "free2api") {
		return fmt.Errorf("端口 %s 上跑的是 %s，不像 free2api，已拒绝停止", addr, name)
	}

	// 控制台程序收 CTRL_BREAK 会走正常退出（main 里的 signal.NotifyContext 接得住）。
	// 先附到它的控制台再发；附不上（没有控制台/不是本会话）就退回强杀。
	freeConsole()
	attached := attachConsole(pid)
	if attached {
		_ = generateConsoleCtrlEvent(1 /* CTRL_BREAK */, pid)
	}
	if waitExit(pid, 2500*time.Millisecond) {
		return nil
	}
	if attached {
		freeConsole()
	}
	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("停止进程 %d 失败（HTTP 停机也失败了：%v）：%w", pid, lastHTTPErr, err)
	}
	if !waitExit(pid, 3*time.Second) {
		return fmt.Errorf("进程 %d 已发终止信号但没退出", pid)
	}
	return nil
}

// lastHTTPErr 记下 HTTP 停机为什么没成，进程级兜底失败时一起报出来，省得用户猜。
var lastHTTPErr error

// shutdownViaHTTP 让网关自己停。带上本机免鉴权的便利：admin 端点对回环地址放行，
// 但网关若配了 api_key 且监听在非回环地址，这一步会 401 —— 那就走进程级兜底。
func shutdownViaHTTP(addr string) error {
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodPost, baseURL(addr)+"/admin/shutdown", bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/admin/shutdown 返回 HTTP %d", resp.StatusCode)
	}
	// 端点是「先回 200 再 cancel」，所以拿到 200 只代表停机已开始。等端口真的没人听。
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !probeFree2API(addr) {
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return errors.New("已请求 /admin/shutdown，但网关仍在应答")
}

func processImageName(h windows.Handle) (string, error) {
	buf := make([]uint16, windows.MAX_PATH)
	size := uint32(len(buf))
	ret, _, err := procQueryFullProcessImage.Call(uintptr(h),
		0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)))
	if ret == 0 {
		return "", fmt.Errorf("QueryFullProcessImageName 失败：%w", err)
	}
	return windows.UTF16ToString(buf[:size]), nil
}

// waitExit 轮询到进程退出（或超时）。退出码 259 = STILL_ACTIVE。
func waitExit(pid uint32, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !processAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(120 * time.Millisecond)
	}
}

func processAlive(pid uint32) bool {
	h, err := windows.OpenProcess(processQueryLimited, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}

func attachConsole(pid uint32) bool {
	proc := kernel32.NewProc("AttachConsole")
	ret, _, _ := proc.Call(uintptr(pid))
	return ret != 0
}

func freeConsole() {
	proc := kernel32.NewProc("FreeConsole")
	_, _, _ = proc.Call()
}

func generateConsoleCtrlEvent(event, group uint32) error {
	proc := kernel32.NewProc("GenerateConsoleCtrlEvent")
	ret, _, err := proc.Call(uintptr(event), uintptr(group))
	if ret == 0 {
		return err
	}
	return nil
}

// splitHostPort 容忍 ":7863" / "127.0.0.1:7863" / "localhost:7863" 三种形态。
func splitHostPort(addr string) (string, string, error) {
	addr = strings.TrimSpace(addr)
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return "", "", fmt.Errorf("地址 %q 里没有端口", addr)
	}
	host := strings.Trim(strings.TrimSpace(addr[:i]), "[]")
	port := strings.TrimSpace(addr[i+1:])
	if port == "" {
		return "", "", fmt.Errorf("地址 %q 里没有端口", addr)
	}
	return host, port, nil
}

//go:build windows

// singleton_windows.go 「一个网关」语义：接管、全局关闭、看门狗暂停。
//
// 为什么需要这一层：
// 以前端口对不对得上全靠「谁先绑上」——计划任务一个 exe、双击又开一个窗口、
// 旧版没退干净还剩一个。结果是多个 free2api 进程同时活着：一个占着 7864，
// 另一个只当控制台；谁被停掉了也不清场，页面上的状态点、账号数、报错各说各的。
// 用户看到的就是「开关不生效」「关了又自己起来」。
//
// 现在的约定很简单：
//  1. 一个端口只能有一个「控制者」。控制权是命名互斥体（Local\free2api-gw-<port>），
//     不是「谁先绑上谁算」——同时双击两次也不会出现两个控制台各说各话。
//  2. 桌面程序就是网关的唯一宿主。启动时先抢控制权、清掉别的 free2api 进程，
//     再在自己进程里起来。
//  3. 「停止网关」是全局关闭：托管实例、端口上的外部进程、以及任何残留的
//     free2api 进程一起收掉，并写下 keepalive.pause —— 看门狗看到就收手。
//  4. 再点「启动网关」（或重新打开 exe）时先撤掉 pause，再按 2 接管。
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	kernel32dll                  = windows.NewLazySystemDLL("kernel32.dll")
	procCreateToolhelp32Snapshot = kernel32dll.NewProc("CreateToolhelp32Snapshot")
	procProcess32FirstW          = kernel32dll.NewProc("Process32FirstW")
	procProcess32NextW           = kernel32dll.NewProc("Process32NextW")
	procTerminateProcess         = kernel32dll.NewProc("TerminateProcess")
	procCreateMutexW             = kernel32dll.NewProc("CreateMutexW")
	procReleaseMutex             = kernel32dll.NewProc("ReleaseMutex")
)

const (
	th32csSnapProcess  = 0x00000002
	errorAlreadyExists = 183
)

// processEntry32W 与 Win32 的 PROCESSENTRY32W 对齐（x64 下 568 字节）。
// 字段顺序照抄 SDK，Go 的对齐规则正好落在同一组偏移上（uintptr 占 8 字节）。
type processEntry32W struct {
	Size            uint32
	CntUsage        uint32
	ProcessID       uint32
	DefaultHeapID   uintptr
	ModuleID        uint32
	CntThreads      uint32
	ParentProcessID uint32
	PriClassBase    int32
	Flags           uint32
	ExeFile         [windows.MAX_PATH]uint16
}

// ---------------------------------------------------------------- 看门狗暂停

// keepalivePausePath 与 scripts/keepalive.vbs 读取的是同一个文件：
// 脚本把 exe 所在目录当根，pause 落在 <root>/data/keepalive.pause。
func keepalivePausePath(exeDir string) string {
	return filepath.Join(exeDir, "data", "keepalive.pause")
}

// setKeepalivePaused 开/关看门狗。paused=true 时写文件，false 时删掉（不存在不算错）。
//
// 注意这是**全局**信号：文件在，看门狗这一轮就只打一行 PAUSED 然后退出，
// 不会替任何人把网关拉起来。桌面程序自己启动时会先把它删掉，所以「重新打开 exe」
// 仍然等于「自动开启网关」。
func setKeepalivePaused(exeDir string, paused bool) error {
	p := keepalivePausePath(exeDir)
	if !paused {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("console paused at "+time.Now().Format(time.RFC3339)+"\r\n"), 0o644)
}

// ---------------------------------------------------------------- 进程清场

// gatewayPIDs 列出「本次安装」的网关进程 ID，排除 self。
//
// 两个条件同时成立才算：映像名里带 free2api，且映像所在目录就是本次 exe 所在目录。
// 只看名字太宽——机器上可能还有另一份部署（另一个目录、另一个端口），那是别人的
// 网关，不该被这边一按停止就顺手杀掉；而「同一目录下的多个 free2api 进程」正好
// 就是新老版本纠缠、旧件没退干净的那种局面，必须清。
func gatewayPIDs(self int, exeDir string) []uint32 {
	snap, _, _ := procCreateToolhelp32Snapshot.Call(th32csSnapProcess, 0)
	if snap == 0 || windows.Handle(snap) == windows.InvalidHandle {
		return nil
	}
	defer windows.CloseHandle(windows.Handle(snap))

	wantDir := filepath.Clean(exeDir)
	var out []uint32
	var pe processEntry32W
	pe.Size = uint32(unsafe.Sizeof(pe))
	ret, _, _ := procProcess32FirstW.Call(snap, uintptr(unsafe.Pointer(&pe)))
	for ret != 0 {
		pid := int(pe.ProcessID)
		name := strings.ToLower(windows.UTF16ToString(pe.ExeFile[:]))
		pe.Size = uint32(unsafe.Sizeof(pe))
		ret, _, _ = procProcess32NextW.Call(snap, uintptr(unsafe.Pointer(&pe)))
		if pid == 0 || pid == self || !strings.Contains(name, "free2api") {
			continue
		}
		h, err := windows.OpenProcess(processQueryLimited, false, uint32(pid))
		if err != nil {
			continue // 别的会话/权限不足：不是我这套安装，放手
		}
		img, ierr := processImageName(h)
		windows.CloseHandle(h)
		if ierr != nil || img == "" {
			continue
		}
		if strings.EqualFold(filepath.Clean(filepath.Dir(img)), wantDir) {
			out = append(out, uint32(pid))
		}
	}
	return out
}

// killGatewayProcesses 强杀本次安装里其它网关进程，等它们真的退出。
//
// 为什么直接 TerminateProcess 而不是先礼后兵：走到这里说明用户在要求「一个都不许留」，
// 而这些进程正是互相打架的来源。占端口的那个在 reclaimOn 里已经先走过优雅停机
// （/admin/shutdown，能落盘 state.json），剩下的都是没绑端口的残壳，强杀没有副作用。
func killGatewayProcesses(self int, exeDir string) (killed []uint32, failed []string) {
	for _, pid := range gatewayPIDs(self, exeDir) {
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, pid)
		if err != nil {
			failed = append(failed, fmt.Sprintf("%d(打不开:%v)", pid, err))
			continue
		}
		_, _, _ = procTerminateProcess.Call(uintptr(h), 1)
		windows.CloseHandle(h)
		if waitExit(pid, 3*time.Second) {
			killed = append(killed, pid)
		} else {
			failed = append(failed, fmt.Sprintf("%d(未退出)", pid))
		}
	}
	return killed, failed
}

// reclaimOn 接管 addr 上的网关：先优雅停机，再把残留进程清干净。
// 启动和停止都会走它，保证「这一刻只有一个 free2api」。
func (d *desktop) reclaimOn(addr string) {
	if probeFree2API(addr) {
		if err := stopExternalGateway(addr); err != nil {
			log.Printf("接管时停机失败（继续按进程清理）：%v", err)
		}
	}
	killed, failed := killGatewayProcesses(os.Getpid(), d.exeDir)
	if len(killed) > 0 {
		log.Printf("已清掉其它网关进程：%v", killed)
	}
	if len(failed) > 0 {
		log.Printf("WARN: 仍有网关进程没退出：%v", failed)
	}
}

// ---------------------------------------------------------------- 端口租约

// leaseName 端口控制权的互斥体名。按端口分：同机不同端口各管各的，
// 同一个端口只能有一个持有者。
func leaseName(addr string) string {
	_, port, err := splitHostPort(addr)
	if err != nil || port == "" {
		port = "unknown"
	}
	return `Local\free2api-gw-` + port
}

// acquirePortLease 抢端口控制权。
//
// 这是「7864 只能由一个程序控制」的落点：不靠谁先 bind 赢，而是先占一个命名互斥体。
// held=true 不是错误——说明别的进程还握着，调用方负责清场后重试。
// Windows 在进程退出（含被强杀）时自动回收互斥体句柄，不会留下死锁。
func acquirePortLease(addr string) (windows.Handle, bool, error) {
	name, _ := windows.UTF16PtrFromString(leaseName(addr))
	r, _, e := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	if r == 0 {
		return 0, false, fmt.Errorf("CreateMutex 失败：%v", e)
	}
	if e == windows.Errno(errorAlreadyExists) {
		// 必须马上关掉：留着这个句柄会让互斥体对象在持有者退出后依然存在，
		// 下一次启动就会误以为还有人握着控制权。
		windows.CloseHandle(windows.Handle(r))
		return 0, true, nil
	}
	return windows.Handle(r), false, nil
}

// holdPortLease 拿住租约（已持有则只做一次 Wait，让「拥有者」语义成立）。
func holdPortLease(h windows.Handle) {
	if h == 0 {
		return
	}
	_, _, _ = procReleaseMutex.Call(uintptr(h)) // 先释放一次可能残留的递归计数
	_, _ = windows.WaitForSingleObject(h, 0)
}

func releasePortLease(h windows.Handle) {
	if h == 0 {
		return
	}
	_, _, _ = procReleaseMutex.Call(uintptr(h))
	windows.CloseHandle(h)
}

// portOwnerHint 端口被谁占着（报错用）。读不到就只说 PID。
func portOwnerHint(addr string) string {
	pid, err := pidListeningOn(addr)
	if err != nil || pid == 0 {
		return ""
	}
	h, oerr := windows.OpenProcess(processQueryLimited, false, pid)
	if oerr != nil {
		return fmt.Sprintf("PID %d", pid)
	}
	defer windows.CloseHandle(h)
	name, nerr := processImageName(h)
	if nerr != nil || name == "" {
		return fmt.Sprintf("PID %d", pid)
	}
	return fmt.Sprintf("%s（PID %d）", filepath.Base(name), pid)
}

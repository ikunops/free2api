//go:build windows

// deploydir_windows.go 解决「升级后账号变少」的根因：程序只认 exe 同目录，
// 新 exe 被放到另一个文件夹时，就在空目录里新建一份配置，再按 ./auths
// 读空目录 —— 账号、状态、端口看起来全丢了（其实都在原部署里）。
//
// 这里给 desktop 一个「运行态归属目录」的发现顺序：
//  1. 显式 -config：完全以用户指定为准，不做任何猜测；
//  2. 部署指针（%LOCALAPPDATA%\free2api\deploy.json）指向的目录仍然有效：
//     这是最可靠的「上次用的是哪份部署」；新 exe 从任意目录启动都回到这里；
//  3. exe 同目录已有 config.json 或 auths/：正常启动/覆盖升级；
//  4. 本机已有部署：取 config.json 最近被修改的那一份（同分时看日志活跃度），
//     让新 exe 直接接手原账号、状态与端口；
//  4. 什么都没有：退回 exe 同目录，按首次运行生成默认配置。
//
// 为什么按 config.json 的 mtime 而不是扫描全盘找「账号最多」的目录：
//  - mtime 是用户可见的「最近一次使用」信号，不需要维护额外的注册表/指针，
//    也不会因为旧备份目录里账号更多就误接到一份陈旧部署；
//  - 只扫固定几个根目录、限定深度，避免在别人的机器上全盘遍历。
package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// deploySearchRoots 发现已有部署时扫描的根目录。
//
// 只放「程序通常被放在哪」的位置：用户目录下的桌面/下载/文档，以及各盘
// 根下一层。不递归整块磁盘：既是性能考虑，也避免误接管别人的部署。
var deploySearchRootsHook = deploySearchRoots

func deploySearchRoots() []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, "Desktop"),
			filepath.Join(home, "Downloads"),
			filepath.Join(home, "Documents"),
		)
	}
	for _, drive := range "CDEFGH" {
		roots = append(roots, string(drive)+":\\")
	}
	return roots
}

// deployCandidate 扫描到的一份已有部署。
type deployCandidate struct {
	dir     string
	cfgMod  time.Time
	logMod  time.Time
	hasCfg  bool
	hasAuth bool
}

// discoverDeployDir 决定本次启动使用哪个目录存放 config.json / auths / data。
//
// exeDir 是 exe 实际所在目录；返回的目录可能不同（新 exe 接手旧部署）。
func discoverDeployDir(exeDir string) string {
	exeDir = filepath.Clean(exeDir)

	// 部署指针优先：上次成功启动的目录就是用户真正在用的那一份。
	// 放在固定位置（LocalAppData）而不是某个部署目录里，这样新 exe 被解压到
	// 任意位置都能找到它；指针失效（目录被删/移走）时自动忽略。
	if dir, ok := readDeployPointer(); ok && looksLikeDeploy(dir) {
		log.Printf("按部署指针接手 %s（上次使用的部署）", dir)
		return dir
	}

	// exe 所在目录本身就是一份部署：正常升级（覆盖 exe）走这里最稳。
	if looksLikeDeploy(exeDir) {
		writeDeployPointer(exeDir)
		return exeDir
	}

	// 只在本程序所在的盘、以及用户目录下找：新 exe 被解压到桌面/下载时
	// 能接回原部署；放到别的盘则按首次运行处理，避免误接别人的环境。
	best, ok := findExistingDeploy(exeDir, exeDir)
	if !ok {
		return exeDir
	}
	log.Printf("检测到已有部署 %s（config.json %s），本次直接接手其账号与状态",
		best.dir, best.cfgMod.Format("2006-01-02 15:04:05"))
	writeDeployPointer(best.dir)
	return best.dir
}

// deployPointerPath 部署指针文件位置。
//
// 用 LocalAppData 而不是 exe 同目录：指针的意义正是「exe 换了位置也能找到
// 原来的部署」，放在部署目录里就会跟着一起搬走，失去作用。
var deployPointerPathHook = deployPointerPath

func deployPointerPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "free2api", "deploy.json")
}

// readDeployPointer 读取上次成功启动的部署目录。
func readDeployPointer() (string, bool) {
	raw, err := os.ReadFile(deployPointerPathHook())
	if err != nil {
		return "", false
	}
	var p struct {
		Dir string `json:"dir"`
	}
	if json.Unmarshal(raw, &p) != nil || strings.TrimSpace(p.Dir) == "" {
		return "", false
	}
	dir := filepath.Clean(p.Dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return "", false
	}
	return dir, true
}

// writeDeployPointer 记录当前部署目录。失败只记日志，不影响启动。
func writeDeployPointer(dir string) {
	path := deployPointerPathHook()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		log.Printf("WARN: 记录部署指针失败: %v", err)
		return
	}
	payload, err := json.MarshalIndent(map[string]string{
		"dir":   filepath.Clean(dir),
		"since": time.Now().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		log.Printf("WARN: 写部署指针失败: %v", err)
	}
}

// looksLikeDeploy 目录里是否已有部署痕迹。只认「像 free2api 的 config.json」
// 或带账号的 auths/，不接受只有空的 data/ —— 那可能只是上次失败启动留下的壳。
//
// 为什么不能只看 config.json 是否存在：用户目录下大量项目都带自己的
// config.json（编辑器、脚本、别家网关）。只按「有配置文件 + 修改时间」排序，
// 会把那些目录误认成 free2api 部署，于是新 exe 跑去读一份没有 listen /
// auth_dir 的配置，端口回落默认值、账号池为空 —— 正是 2026-10-19 那次
// 「dist 里的 exe 却监听 7863 且 0 账号」的事故。
func looksLikeDeploy(dir string) bool {
	if entries, err := os.ReadDir(filepath.Join(dir, "auths")); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
				return true
			}
		}
	}
	return isFree2APIConfig(filepath.Join(dir, "config.json"))
}

// isFree2APIConfig 判断一份 config.json 是不是 free2api 的配置。
//
// 判据是「至少出现一个本程序独有的键」。不要求 listen 一定存在：老版本配置、
// 手动精简过的配置都可能没有它，但不会连 auth_dir / state_file / admin /
// output / schedule 一个都没有。这样既不会误接别家项目的同名文件，也不会
// 把合法的旧部署挡在外面。
func isFree2APIConfig(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		return false
	}
	for _, key := range []string{"listen", "auth_dir", "state_dir", "state_file", "api_key", "admin", "output", "schedule", "global"} {
		if _, ok := probe[key]; ok {
			return true
		}
	}
	return false
}

// findExistingDeploy 在 deploySearchRoots 里找最近使用过的部署。
//
// 只看固定深度（root 下一层、两层），按 config.json 的修改时间倒序；
// 修改时间相同时用 desktop.log 的活跃度兜底，避免备份目录「同秒创建」时
// 选错那一份。
func findExistingDeploy(exeDir, nearDir string) (deployCandidate, bool) {
	var cands []deployCandidate
	seen := map[string]bool{filepath.Clean(exeDir): true}
	nearVolume := filepath.VolumeName(nearDir)
	for _, root := range deploySearchRootsHook() {
		// 根目录不在本程序所在的盘、也不在用户目录下：跳过。
		// 这样 C: 上的 exe 不会去 D:/E: 找另一套部署。
		if !sameVolume(root, nearVolume) && !underUserHome(root) {
			continue
		}
		// 深度 4：覆盖 work\dist、projects\app\deploy 这类常见布局，
		// 又不至于把整盘都翻一遍。
		walkDeployCandidates(root, 4, seen, &cands)
	}
	if len(cands) == 0 {
		return deployCandidate{}, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		// 有账号的部署优先：升级要接手的是「用户真正在用的那份」。
		// 只有配置、没有 auths/ 的目录很可能是刚生成的空壳或别人的工程，
		// 哪怕它的 config.json 更新时间更晚，也不该被选中。
		if cands[i].hasAuth != cands[j].hasAuth {
			return cands[i].hasAuth
		}
		if !cands[i].cfgMod.Equal(cands[j].cfgMod) {
			return cands[i].cfgMod.After(cands[j].cfgMod)
		}
		return cands[i].logMod.After(cands[j].logMod)
	})
	return cands[0], true
}

// walkDeployCandidates 限定深度地找 config.json，不跟随符号链接。
func walkDeployCandidates(dir string, depth int, seen map[string]bool, out *[]deployCandidate) {
	dir = filepath.Clean(dir)
	dir = strings.TrimSuffix(dir, string(filepath.Separator)) + string(filepath.Separator)
	dir = filepath.Clean(dir)
	if seen[dir] {
		return
	}
	seen[dir] = true

	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil && looksLikeDeploy(dir) {
		// 备份/历史版本目录不是「当前部署」：名字里带 bak、backup、old、
		// dist-v<数字> 的一律跳过，避免升级时接到一份旧副本。
		if looksLikeBackupDir(dir) {
			return
		}
		c := deployCandidate{dir: dir, hasCfg: true, hasAuth: dirHasAuths(dir)}
		if fi, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
			c.cfgMod = fi.ModTime()
		}
		if fi, err := os.Stat(filepath.Join(dir, "data", "desktop.log")); err == nil {
			c.logMod = fi.ModTime()
		}
		*out = append(*out, c)
	}
	if depth <= 1 {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := strings.ToLower(e.Name())
		// 跳过明显的缓存/回收站，避免扫描噪音与权限错误。
		switch name {
		case "node_modules", "vendor", "$recycle.bin", "system volume information",
			"appdata", "windows", "program files", "program files (x86)":
			continue
		}
		walkDeployCandidates(filepath.Join(dir, e.Name()), depth-1, seen, out)
	}
}

// looksLikeBackupDir 判断目录名是否像备份/历史版本。
func looksLikeBackupDir(dir string) bool {
	base := strings.ToLower(filepath.Base(filepath.Clean(dir)))
	if base == "" {
		return false
	}
	for _, marker := range []string{"bak", "backup", "old", ".prev", "dist-v"} {
		if strings.Contains(base, marker) {
			return true
		}
	}
	// 形如 dist-v113 / v1.1.3 / 20261019 的版本目录。
	if strings.HasPrefix(base, "v") && len(base) > 1 && base[1] >= '0' && base[1] <= '9' {
		return true
	}
	if len(base) >= 8 && isDigits(base[:8]) {
		return true
	}
	return false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func dirHasAuths(dir string) bool {
	entries, err := os.ReadDir(filepath.Join(dir, "auths"))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			return true
		}
	}
	return false
}

// sameVolume 两个路径是否在同一盘符（Windows 下不区分大小写）。
func sameVolume(a, b string) bool {
	va := filepath.VolumeName(a)
	vb := filepath.VolumeName(b)
	if va == "" || vb == "" {
		return true // 相对路径等无法判断时放行，由调用方的根目录列表兜底
	}
	return strings.EqualFold(va, vb)
}

// underUserHome 路径是否位于当前用户目录下。
func underUserHome(p string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	home = strings.ToLower(filepath.Clean(home))
	p = strings.ToLower(filepath.Clean(p))
	return p == home || strings.HasPrefix(p, home+string(filepath.Separator))
}

// school.go 开学季任务与夜猫子任务的脚本类排程：从系统 crontab 迁入 Go scheduler。
//
// 背景：school（12:00）与 cat（01:00 夜猫窗口）原由系统 crontab 调
// scripts/school_open_day_cron.sh 执行——依赖外部系统 cron、容器重建可能丢失、
// 不在 config 里配置。迁入后成为第五、第六类任务，时点由 schedule.school_hours /
// schedule.cat_hours 配置，school_open_day_cron.sh 保留为手动触发入口。
package scheduler

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// repoRoot 定位脚本根（容器内 /app、宿主 /root/free2api）。
//
// 策略（按优先级）：
//  1. 从当前工作目录逐级向上找 scripts/school_open_day_2026.py；
//  2. 从可执行文件所在目录逐级向上找（Windows 便携包常见：exe 与 scripts/ 同级，
//     而进程工作目录可能是别处）；
//  3. 都找不到回落 os.Getwd()（Run 会因脚本缺失打 WARN，不 panic）。
//
// 第 2 条是 Windows 实测补的：exe 放在 work\demo 下、scripts/ 也复制到同级时，
// 只要进程是从别处拉起的，光靠 cwd 上溯就找不到脚本，任务静默不跑。
func repoRoot() string {
	if dir, ok := findScriptRoot(os.Getwd); ok {
		return dir
	}
	if dir, ok := findScriptRoot(executableDir); ok {
		return dir
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// findScriptRoot 从 base 给的起点逐级向上找含 scripts/school_open_day_2026.py 的目录。
func findScriptRoot(base func() (string, error)) (string, bool) {
	start, err := base()
	if err != nil {
		return "", false
	}
	dir := start
	for {
		if _, err := os.Stat(filepath.Join(dir, "scripts", "school_open_day_2026.py")); err == nil {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// executableDir 返回当前可执行文件所在目录（测试里是临时测试二进制目录，无害）。
func executableDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(exe), nil
}

// scriptRunner 脚本子进程的最小执行面：可被测试替换，避免测试真正拉起 python3。
type scriptRunner interface {
	SetDir(string)
	Run() error
}

// scriptCmd exec.Cmd 适配器：把 exec.Cmd 的 Dir 字段包装成 SetDir 方法，
// 满足 scriptRunner 接口（exec.Cmd 本身只有字段没有方法）。
type scriptCmd struct{ cmd *exec.Cmd }

func (c *scriptCmd) SetDir(dir string) { c.cmd.Dir = dir }
func (c *scriptCmd) Run() error        { return c.cmd.Run() }

// newScriptCmd 构建脚本子进程。包级变量便于测试注入 fake（installFakeExec 覆盖）。
// 工作目录由调用方 SetDir 显式设置脚本根。
//
// 子进程强制 UTF-8 输出：脚本 print 的账号昵称含非 ASCII（本机实测有 `ㅤ` 这种
// 韩文空白字符），Windows 中文环境下 Python 默认按 GBK 编码 stdout，直接
// UnicodeEncodeError 中断整批任务。PYTHONIOENCODING/PYTHONUTF8 两个变量同时设，
// 覆盖 3.7+ 全部版本口径；Linux 上设了也无副作用（本来就是 UTF-8）。
var newScriptCmd = func(program string, args ...string) scriptRunner {
	cmd := exec.Command(program, args...)
	cmd.Env = append(os.Environ(), "PYTHONIOENCODING=utf-8", "PYTHONUTF8=1")
	return &scriptCmd{cmd: cmd}
}

// pythonCmd 返回执行 scripts/*.py 的解释器名。
//
// 优先级：WB2A_PYTHON 显式指定 > PATH 上的 python3 > PATH 上的 python > "python3"。
//
// 为什么要自动探测：容器/Linux 只有 python3（行为零变更）；而 Windows 官方安装器
// 只提供 python.exe，PATH 上还可能存在 Microsoft Store 的 python3.exe App Execution
// Alias 存根——exec.Command 能找到它却无法真正执行，脚本类任务统一报
// `exit status 9009`（本机实测：`python3` 不存在、`python` 存在）。
// 显式设置 WB2A_PYTHON 仍最优先，供特殊环境覆盖。
func pythonCmd() string {
	if v := strings.TrimSpace(os.Getenv("WB2A_PYTHON")); v != "" {
		return v
	}
	if _, err := exec.LookPath("python3"); err == nil {
		return "python3"
	}
	if _, err := exec.LookPath("python"); err == nil {
		return "python"
	}
	return "python3"
}

// runScript 依次执行若干脚本命令：任一命令失败只记一行 WARN，不向上抛、
// 不影响调度主循环继续跑下一个时点。单命令失败不中断后续命令。
func runScript(name, root string, commands [][]string) {
	for _, cmdArgs := range commands {
		c := newScriptCmd(cmdArgs[0], cmdArgs[1:]...)
		c.SetDir(root)
		if err := c.Run(); err != nil {
			log.Printf("WARN: %s (%s): %v", name, cmdArgs[1], err)
			continue
		}
		log.Printf("%s: ok (%s)", name, cmdArgs[1])
	}
}

// RunSchoolNow 立即执行开学季任务：school_open_day_2026.py ALL --run --yes。
// 全量跑任务点亮 + 领奖 + 自动抽空抽奖余额。活动下线（in_period=false）时脚本
// 各段全量跳过、正常退出，不视为失败。失败只记 WARN。
func (s *Scheduler) RunSchoolNow() {
	root := repoRoot()
	runScript("school", root, [][]string{
		{pythonCmd(), "scripts/school_open_day_2026.py", "ALL", "--run", "--yes"},
	})
}

// RunCatNow 立即执行夜猫子任务：task_runner.py ALL --yes --only black_cat。
// black_cat 时段敏感：夜猫窗口 23:00–08:00 CST 内最多补 1 次（task_runner 内部
// 判定，非窗口期打印 skip 正常退出）。失败只记 WARN。
func (s *Scheduler) RunCatNow() {
	root := repoRoot()
	runScript("cat", root, [][]string{
		{pythonCmd(), "scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"},
	})
}

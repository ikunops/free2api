package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeScriptExec 记录命令构建参数并按需模拟执行失败，替代真实 exec 拉起 python3 子进程。
type fakeScriptExec struct {
	lastName string
	lastArgs []string
	lastDir  string
	runN     int
	err      error
}

func (f *fakeScriptExec) SetDir(dir string) { f.lastDir = dir }
func (f *fakeScriptExec) Run() error        { f.runN++; return f.err }

// installFakeExec 替换 newScriptCmd，测试结束还原。
func installFakeExec(t *testing.T) *fakeScriptExec {
	t.Helper()
	f := &fakeScriptExec{}
	orig := newScriptCmd
	newScriptCmd = func(name string, args ...string) scriptRunner {
		f.lastName, f.lastArgs = name, args
		return f
	}
	t.Cleanup(func() { newScriptCmd = orig })
	return f
}

func equalArgs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestNextWakeSchoolSlot 开学季任务在 school_hours（默认 12 点）处有独立时点。
func TestNextWakeSchoolSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 11, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 12, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school 12:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskSchool {
		t.Errorf("kinds=%v want [school]", kinds)
	}
}

// TestNextWakeCatSlot 夜猫子任务在 cat_hours（默认 1 点）处有独立时点。
func TestNextWakeCatSlot(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{9},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		CatHours:          []int{1},
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 23, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 15, 1, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（cat 01:00 独立时点）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCat {
		t.Errorf("kinds=%v want [cat]", kinds)
	}
}

// TestNextWakeSchoolCatDisabled 显式禁用 school/cat 后排程只剩签到时点（互不影响）。
func TestNextWakeSchoolCatDisabled(t *testing.T) {
	s := New(Config{
		CheckinHours:      []int{21},
		TravelDisabled:    true,
		ActivityDisabled:  true,
		KeepaliveDisabled: true,
		SchoolHours:       []int{12},
		CatHours:          []int{1},
		SchoolDisabled:    true,
		CatDisabled:       true,
	})
	at, kinds := s.nextWake(time.Date(2026, 9, 14, 8, 0, 0, 0, time.Local))
	if want := time.Date(2026, 9, 14, 21, 0, 0, 0, time.Local); !at.Equal(want) {
		t.Errorf("next=%v want %v（school/cat 禁用 → 只有签到 21:00）", at, want)
	}
	if len(kinds) != 1 || kinds[0] != taskCheckin {
		t.Errorf("kinds=%v want [checkin]", kinds)
	}
}

// TestRunSchoolNowBuildsCommand RunSchoolNow 构造
// python3 scripts/school_open_day_2026.py ALL --run --yes，工作目录设为仓库根。
func TestRunSchoolNowBuildsCommand(t *testing.T) {
	// 防环境泄漏：WB2A_PYTHON 若在测试机已设置会改写 pythonCmd()，使默认值断言失败。
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunSchoolNow()
	// 解释器名由 pythonCmd() 决定（Windows 上自动回落 python），这里断言与之一致，
	// 不写死 "python3"，否则 Linux/Windows 两种环境的期望值不同。
	if want := pythonCmd(); f.lastName != want {
		t.Errorf("name=%q want %q", f.lastName, want)
	}
	want := []string{"scripts/school_open_day_2026.py", "ALL", "--run", "--yes"}
	if !equalArgs(f.lastArgs, want) {
		t.Errorf("args=%v want %v", f.lastArgs, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
	if _, err := os.Stat(filepath.Join(f.lastDir, "scripts", "school_open_day_2026.py")); err != nil {
		t.Errorf("仓库根 %q 内应有 scripts/school_open_day_2026.py: %v", f.lastDir, err)
	}
}

// TestRunCatNowBuildsCommand RunCatNow 构造
// python3 scripts/task_runner.py ALL --yes --only black_cat，工作目录为仓库根。
func TestRunCatNowBuildsCommand(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	s := New(Config{})
	s.RunCatNow()
	want := []string{"scripts/task_runner.py", "ALL", "--yes", "--only", "black_cat"}
	if wantPy := pythonCmd(); f.lastName != wantPy || !equalArgs(f.lastArgs, want) {
		t.Errorf("cmd=%s %v want %s %v", f.lastName, f.lastArgs, wantPy, want)
	}
	if f.lastDir != repoRoot() {
		t.Errorf("dir=%q want repo root %q", f.lastDir, repoRoot())
	}
}

// TestDispatchSchoolCatAndFailureWarnsOnly dispatch 把 school/cat 分发给对应脚本；
// 脚本失败只记 WARN（不 panic/不向上抛），且不影响后续任务继续分发。
func TestDispatchSchoolCatAndFailureWarnsOnly(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	f := installFakeExec(t)
	f.err = errors.New("boom boom")
	s := New(Config{})

	var buf bytes.Buffer
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	})

	s.dispatch(context.Background(), taskSchool)
	if f.runN != 1 || f.lastArgs[0] != "scripts/school_open_day_2026.py" {
		t.Errorf("dispatch(school) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	s.dispatch(context.Background(), taskCat)
	if f.runN != 2 || f.lastArgs[0] != "scripts/task_runner.py" {
		t.Errorf("dispatch(cat) 未执行: runN=%d last=%v", f.runN, f.lastArgs)
	}
	out := buf.String()
	if !strings.Contains(out, "WARN") || !strings.Contains(out, "scripts/school_open_day_2026.py") {
		t.Errorf("school 失败未按 WARN 记录:\n%s", out)
	}
	if !strings.Contains(out, "scripts/task_runner.py") {
		t.Errorf("cat 失败未按 WARN 记录:\n%s", out)
	}
}

// TestPythonCmd 解释器解析优先级：
//  1. WB2A_PYTHON 显式设置（去空白）时取其值——最高优先，任何环境一致；
//  2. 未设置时自动探测：PATH 有 python3 用 python3（Linux/容器行为零变更），
//     否则用 python（Windows 官方安装器只有 python.exe）；
//  3. 都没有时回落 "python3"（让 exec 报出可诊断的错误）。
//
// 不写死探测结果：探测依赖测试机 PATH，断言按 LookPath 的实际结果推导。
func TestPythonCmd(t *testing.T) {
	t.Setenv("WB2A_PYTHON", "")
	_, errPy3 := exec.LookPath("python3")
	_, errPy := exec.LookPath("python")
	hasPy3, hasPy := errPy3 == nil, errPy == nil
	wantAuto := "python3"
	if !hasPy3 && hasPy {
		wantAuto = "python"
	}
	if got := pythonCmd(); got != wantAuto {
		t.Errorf("auto pythonCmd()=%q want %q (python3=%v python=%v)", got, wantAuto, hasPy3, hasPy)
	}

	t.Setenv("WB2A_PYTHON", "   ")
	if got := pythonCmd(); got != wantAuto {
		t.Errorf("blank pythonCmd()=%q want %q", got, wantAuto)
	}

	t.Setenv("WB2A_PYTHON", "python")
	if got := pythonCmd(); got != "python" {
		t.Errorf("override pythonCmd()=%q want python", got)
	}

	t.Setenv("WB2A_PYTHON", "  /usr/bin/python3.10  ")
	if got := pythonCmd(); got != "/usr/bin/python3.10" {
		t.Errorf("trim pythonCmd()=%q want /usr/bin/python3.10", got)
	}
}

// TestRepoRootFindsScriptsViaExecutableDir 可执行文件目录也能作为脚本根起点：
// Windows 便携包常见布局是 exe 与 scripts/ 同级，而进程工作目录可能在别处
// （本机实测：cwd=work\demo 上溯找不到 scripts/，任务静默不跑）。
func TestRepoRootFindsScriptsViaExecutableDir(t *testing.T) {
	// 用临时目录造一个「exe 同级有 scripts/」的布局，验证 findScriptRoot 能命中。
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeScript := filepath.Join(dir, "scripts", "school_open_day_2026.py")
	if err := os.WriteFile(fakeScript, []byte("# stub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := findScriptRoot(func() (string, error) { return dir, nil })
	if !ok {
		t.Fatalf("findScriptRoot 未在 %s 命中 scripts/", dir)
	}
	if got != dir {
		t.Errorf("findScriptRoot=%q want %q", got, dir)
	}

	// 起点自己就是仓库根也要命中。
	if got2, ok2 := findScriptRoot(func() (string, error) {
		return filepath.Join(dir, "scripts"), nil
	}); !ok2 || got2 != dir {
		t.Errorf("从 scripts/ 上溯: got=%q ok=%v want %q", got2, ok2, dir)
	}

	// 完全找不到时返回 false，不 panic。
	empty := t.TempDir()
	if _, ok3 := findScriptRoot(func() (string, error) { return empty, nil }); ok3 {
		t.Errorf("空目录不应命中 scripts/")
	}
}

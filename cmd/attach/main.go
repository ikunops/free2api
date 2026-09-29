// attach — 把 free2api 反代出来的模型接入本机 AI 客户端（目前支持 Codex）。
//
// 为什么需要它：Codex 只有一个全局 provider（顶层 `model_provider` + `[model_providers.<id>]`
// 表），桌面端每次重写 config.toml 都会剪掉它不认识的顶层标量（表会保留），所以"接上"
// 这件事必须能被反复执行 —— 本命令幂等：跑几次都只保证最终形态正确。
//
// 用法：
//
//	attach codex [--home DIR] [--gateway URL] [--model SLUG] [--dry-run]
//	    写入/修复：顶层 model_provider = "<id>" + model = "<网关模型名>" + [model_providers.<id>] 表
//	attach detach [--home DIR] [--dry-run]
//	    卸掉：删除顶层 model_provider（恢复官方默认 provider）；顶层 model 若指向网关模型一并删除
//	attach status [--home DIR] [--gateway URL]
//	    只看当前挂没挂、挂的是哪个地址、哪些模型在列（不改任何文件）
//
// 备份：任何写操作前都会把 config.toml 复制成 config.toml.bak-attach-<时间戳>。
//
// 默认值：--home 取 $CODEX_HOME，未设则 ~/.codex；--gateway 取 http://127.0.0.1:7864。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	providerID   = "wbhub"        // [model_providers.<id>]，与控制台文档一致
	providerName = "WBHub"        // 表里的 name 字段（仅展示用）
	defaultGW    = "http://127.0.0.1:7864"
	wireAPI      = "responses"    // Codex 0.151+ 只认 responses（chat 已被官方下线）
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		fatal("usage: attach <codex|detach|status> [flags]")
	}
	cmd := args[0]
	rest := args[1:]

	opts := parseFlags(rest)

	switch cmd {
	case "codex":
		doAttach(opts)
	case "detach":
		doDetach(opts)
	case "status":
		doStatus(opts)
	default:
		fatal("unknown subcommand %q (want codex|detach|status)", cmd)
	}
}

type options struct {
	home    string
	gateway string
	model   string
	dryRun  bool
}

func parseFlags(args []string) options {
	o := options{gateway: defaultGW}
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() string {
			if i+1 >= len(args) {
				fatal("%s 需要一个值", a)
			}
			i++
			return args[i]
		}
		switch {
		case a == "--home":
			o.home = val()
		case strings.HasPrefix(a, "--home="):
			o.home = strings.TrimPrefix(a, "--home=")
		case a == "--gateway":
			o.gateway = strings.TrimRight(val(), "/")
		case strings.HasPrefix(a, "--gateway="):
			o.gateway = strings.TrimRight(strings.TrimPrefix(a, "--gateway="), "/")
		case a == "--model":
			o.model = val()
		case strings.HasPrefix(a, "--model="):
			o.model = strings.TrimPrefix(a, "--model=")
		case a == "--dry-run":
			o.dryRun = true
		default:
			fatal("未知参数 %q", a)
		}
	}
	if o.home == "" {
		if h := os.Getenv("CODEX_HOME"); h != "" {
			o.home = h
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				fatal("无法定位用户目录：%v", err)
			}
			o.home = filepath.Join(home, ".codex")
		}
	}
	return o
}

// ── 网关侧：拉发布清单 ────────────────────────────────────────────────

// publishedModels 拉网关 /v1/models 的通用形态（data[].id 即客户端要填的模型名）。
// 拿不到不致命：attach 仍可写 provider 指向，只是无法校验/挑选模型名。
func publishedModels(gw string) ([]string, error) {
	cli := &http.Client{Timeout: 8 * time.Second}
	resp, err := cli.Get(gw + "/v1/models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("解析 /v1/models 失败：%w", err)
	}
	out := make([]string, 0, len(doc.Data))
	for _, m := range doc.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// inList 模型名是否在发布清单里（含裸名兜底：客户端可以只写裸名，网关会按唯一归属路由）。
func inList(list []string, model string) bool {
	for _, id := range list {
		if id == model {
			return true
		}
		if bare := bareName(id); bare == model {
			return true
		}
	}
	return false
}

// bareName 去掉 realm[/producer] 前缀与最后一个 "-x0.11"/"-free" 费率后缀（仅用于裸名比对）。
func bareName(id string) string {
	s := id
	if i := strings.LastIndex(s, ":"); i >= 0 {
		s = s[i+1:]
	}
	if m := regexp.MustCompile(`-(x[0-9.]+|free)$`).FindString(s); m != "" {
		s = strings.TrimSuffix(s, m)
	}
	return s
}

// ── config.toml 的行级编辑 ────────────────────────────────────────────
//
// 只做"顶层标量 + 一个 provider 表"的增删改，不引入 TOML 依赖、不动用户其余内容：
// 注释、顺序、其它表全部原样保留。

type toml struct {
	lines []string // 保持原文件行序；写回时用 "\n" 连接
}

func loadToml(path string) (*toml, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &toml{}, nil // 不存在 = 空文件起步
		}
		return nil, err
	}
	return &toml{lines: strings.Split(strings.TrimRight(string(raw), "\n"), "\n")}, nil
}

func (t *toml) String() string {
	if len(t.lines) == 0 {
		return ""
	}
	return strings.Join(t.lines, "\n") + "\n"
}

// topEnd 顶层标量区结束下标（第一个表头之前）。
func (t *toml) topEnd() int {
	for i, l := range t.lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			return i
		}
	}
	return len(t.lines)
}

var kvRe = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*=`)

// valueOf 取一行 `k = v` 的值：去行尾注释、去引号。
// 本工具自己写出的行带 `# ...`，不剥掉会把注释当成值的一部分。
// （模型名/地址里不会出现 '#'，够用；真要支持带 '#' 的字符串再引 TOML 解析器。）
func valueOf(line string) string {
	v := strings.TrimSpace(strings.SplitN(line, "=", 2)[1])
	if j := strings.Index(v, "#"); j >= 0 {
		v = strings.TrimSpace(v[:j])
	}
	return strings.Trim(v, `"'`)
}

func (t *toml) topKeyIndex(key string) int {
	for i := 0; i < t.topEnd(); i++ {
		if m := kvRe.FindStringSubmatch(strings.TrimSpace(t.lines[i])); m != nil && m[1] == key {
			return i
		}
	}
	return -1
}

func (t *toml) topKey(key string) string {
	i := t.topKeyIndex(key)
	if i < 0 {
		return ""
	}
	return valueOf(t.lines[i])
}

// setTopKey 有则替换、无则在顶层标量区末尾插入（保证在任何表头之前）。
// 幂等按**值**判定：值相同就不动那一行（注释差异不算改动），免得每次跑都无谓重写用户文件。
func (t *toml) setTopKey(key, val string, comment string) bool {
	line := fmt.Sprintf("%s = %q", key, val)
	if comment != "" {
		line += "   # " + comment
	}
	if i := t.topKeyIndex(key); i >= 0 {
		if t.topKey(key) == val {
			return false
		}
		t.lines[i] = line
		return true
	}
	idx := t.topEnd()
	// 跳过顶层区末尾的空行，插在最后一条标量之后
	for idx > 0 && strings.TrimSpace(t.lines[idx-1]) == "" {
		idx--
	}
	t.lines = append(t.lines[:idx], append([]string{line}, t.lines[idx:]...)...)
	return true
}

func (t *toml) removeTopKey(key string) bool {
	if i := t.topKeyIndex(key); i >= 0 {
		t.lines = append(t.lines[:i], t.lines[i+1:]...)
		return true
	}
	return false
}

// upsertProvider 维护 [model_providers.<id>] 表：存在则补齐/覆盖三个键，不存在则追加。
func (t *toml) upsertProvider(id, name, baseURL string) bool {
	head := "[model_providers." + id + "]"
	start := -1
	for i, l := range t.lines {
		if strings.TrimSpace(l) == head {
			start = i
			break
		}
	}
	want := map[string]string{
		"name":      fmt.Sprintf("%q", name),
		"base_url":  fmt.Sprintf("%q", baseURL),
		"wire_api":  fmt.Sprintf("%q", wireAPI),
	}
	if start < 0 {
		block := []string{"", fmt.Sprintf("# 由 free2api attach 写入；手工改也行，重跑 attach 会补齐（%s）", time.Now().Format("2006-01-02")), head}
		for _, k := range []string{"name", "base_url", "wire_api"} {
			block = append(block, k+" = "+want[k])
		}
		t.lines = append(t.lines, block...)
		return true
	}
	// 表内已有的键就地替换，缺的补在表头后
	end := len(t.lines)
	for i := start + 1; i < len(t.lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(t.lines[i]), "[") {
			end = i
			break
		}
	}
	changed := false
	for _, k := range []string{"name", "base_url", "wire_api"} {
		found := false
		for i := start + 1; i < end; i++ {
			if m := kvRe.FindStringSubmatch(strings.TrimSpace(t.lines[i])); m != nil && m[1] == k {
				// 幂等按值判定：值一样就不动这一行（注释/引号风格差异不算改动）。
				if valueOf(t.lines[i]) != strings.Trim(want[k], `"`) {
					t.lines[i] = k + " = " + want[k]
					changed = true
				}
				found = true
				break
			}
		}
		if !found {
			t.lines = append(t.lines[:start+1], append([]string{k + " = " + want[k]}, t.lines[start+1:]...)...)
			end++
			changed = true
		}
	}
	return changed
}

func (t *toml) hasProvider(id string) bool {
	head := "[model_providers." + id + "]"
	for _, l := range t.lines {
		if strings.TrimSpace(l) == head {
			return true
		}
	}
	return false
}

// ── 三个子命令 ──────────────────────────────────────────────────────

func configPath(home string) string { return filepath.Join(home, "config.toml") }

func backup(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	dst := fmt.Sprintf("%s.bak-attach-%s", path, time.Now().Format("20060102-150405"))
	return dst, os.WriteFile(dst, raw, 0o644)
}

func doAttach(o options) {
	path := configPath(o.home)

	list, lerr := publishedModels(o.gateway)
	if lerr != nil {
		fmt.Printf("⚠️  读网关发布清单失败（%v）——仍会写入 provider 指向，但跳过模型名校验\n", lerr)
	}

	t, err := loadToml(path)
	if err != nil {
		fatal("读取 %s 失败：%v", path, err)
	}

	// 模型选择：--model 优先；否则沿用现有（必须在发布清单里）；再否则挑一个默认
	model := o.model
	cur := t.topKey("model")
	switch {
	case model != "":
		if len(list) > 0 && !inList(list, model) {
			fmt.Printf("⚠️  --model %q 不在网关发布清单里（可能调不通）。可选项：\n%s\n", model, indentList(list))
		}
	case cur != "" && (len(list) == 0 || inList(list, cur)):
		model = cur
	case len(list) > 0:
		model = pickDefault(list)
		fmt.Printf("ℹ️  现有模型 %q 不是网关模型，自动改用 %q（可用 --model 指定）\n", cur, model)
	default:
		fmt.Println("⚠️  拿不到发布清单且未指定 --model：只写 provider 指向，模型名保持原样（可能调不通）")
	}

	changed := false
	changed = t.setTopKey("model_provider", providerID, "free2api attach 写入；桌面端可能剪掉此行，重跑 attach 修复") || changed
	if model != "" {
		changed = t.setTopKey("model", model, "free2api 网关模型") || changed
	}
	changed = t.upsertProvider(providerID, providerName, o.gateway+"/v1") || changed

	fmt.Printf("目标：%s\n网关：%s/v1\n模型：%s\n", path, o.gateway, model)
	if !changed {
		fmt.Println("✅ 已是目标形态，无需改动（幂等）")
		reportGatewayState(list)
		return
	}
	if o.dryRun {
		fmt.Println("— dry-run，未写入。将变成：")
		fmt.Println(t.String())
		return
	}
	if dst, err := backup(path); err != nil {
		fatal("备份失败：%v", err)
	} else if dst != "" {
		fmt.Println("备份：", dst)
	}
	if err := os.MkdirAll(o.home, 0o755); err != nil {
		fatal("创建 %s 失败：%v", o.home, err)
	}
	if err := os.WriteFile(path, []byte(t.String()), 0o644); err != nil {
		fatal("写入失败：%v", err)
	}
	fmt.Println("✅ 已写入。重启 Codex 桌面端后，模型选择器会列出网关发布的模型。")
	reportGatewayState(list)
}

func doDetach(o options) {
	path := configPath(o.home)
	t, err := loadToml(path)
	if err != nil {
		fatal("读取 %s 失败：%v", path, err)
	}
	cur := t.topKey("model")
	rmProvider := t.removeTopKey("model_provider")
	rmModel := false
	if cur != "" && strings.Contains(cur, ":") { // 网关模型名带 realm 前缀，官方模型名不含冒号
		rmModel = t.removeTopKey("model")
	}
	switch {
	case !rmProvider && !rmModel:
		fmt.Println("✅ 顶层没有 model_provider/model，已是官方默认形态（provider 表保留，随时可再 attach）")
		return
	}
	if o.dryRun {
		fmt.Println("— dry-run，未写入。将变成：")
		fmt.Println(t.String())
		return
	}
	if dst, err := backup(path); err != nil {
		fatal("备份失败：%v", err)
	} else if dst != "" {
		fmt.Println("备份：", dst)
	}
	if err := os.WriteFile(path, []byte(t.String()), 0o644); err != nil {
		fatal("写入失败：%v", err)
	}
	if rmProvider {
		fmt.Println("✅ 已删除顶层 model_provider")
	}
	if rmModel {
		fmt.Printf("✅ 顶层 model（%s）指向网关，已一并删除，回落到 Codex 默认模型\n", cur)
	}
	fmt.Println("   重启 Codex 后即恢复官方 provider（[model_providers." + providerID + "] 表保留，不影响官方使用）")
}

func doStatus(o options) {
	path := configPath(o.home)
	t, err := loadToml(path)
	if err != nil {
		fatal("读取 %s 失败：%v", path, err)
	}
	haveScalar := t.topKeyIndex("model_provider") >= 0 || t.topKeyIndex("model_provider ") >= 0
	cur := t.topKey("model")
	fmt.Printf("配置文件：%s\n", path)
	fmt.Printf("顶层 model_provider ：%s\n", yesNo(haveScalar, t.topKey("model_provider")))
	fmt.Printf("顶层 model          ：%s\n", yesNo(cur != "", cur))
	fmt.Printf("provider 表         ：%s\n", yesNo(t.hasProvider(providerID), "[model_providers."+providerID+"]"))
	list, err := publishedModels(o.gateway)
	if err != nil {
		fmt.Printf("网关发布清单        ：读取失败（%v）\n", err)
		return
	}
	fmt.Printf("网关发布模型        ：%d 个\n%s\n", len(list), indentList(list))
	if haveScalar && cur != "" && inList(list, cur) {
		fmt.Println("结论：已挂网关 ✅")
	} else if haveScalar {
		fmt.Println("结论：挂了网关，但顶层 model 不在发布清单里 ⚠️（跑 attach codex 修）")
	} else {
		fmt.Println("结论：走官方默认（想挂网关跑 attach codex）")
	}
}

// pickDefault 默认模型：优先 cn:auto（网关的自动路由），否则清单第一个。
func pickDefault(list []string) string {
	for _, id := range list {
		if id == "cn:auto" {
			return id
		}
	}
	sorted := append([]string(nil), list...)
	sort.Strings(sorted)
	for _, id := range sorted {
		if strings.HasSuffix(id, "-free") {
			return id // 退而求其次：先挑免费的
		}
	}
	if len(sorted) > 0 {
		return sorted[0]
	}
	return ""
}

func reportGatewayState(list []string) {
	if len(list) == 0 {
		return
	}
	fmt.Printf("网关当前发布 %d 个模型（选一个填 --model 可改默认）：\n%s\n", len(list), indentList(list))
}

func indentList(list []string) string {
	var b strings.Builder
	for _, s := range list {
		b.WriteString("    " + s + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func yesNo(ok bool, detail string) string {
	if !ok {
		return "（无）"
	}
	if detail == "" {
		return "（空值）"
	}
	return detail
}

func fatal(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "attach: "+format+"\n", a...)
	os.Exit(1)
}

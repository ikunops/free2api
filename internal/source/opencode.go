// opencode.go OpenCode（CLI/TUI）账号的「取源」侧：读 ~/.local/share/opencode/auth.json，
// 把账号自带的 OpenCode Zen / OpenCode Go 上游凭据抽出来，归一化成网关认得的扁平形 auth。
//
// 为什么单写一条链路：OpenCode 的 auth.json 形态极简——
//
//	{"<providerID>": {"type":"api", "key":"sk-..."}}
//
// 顶层键就是 provider 名，值是 {type,key}。它既不是 workbuddy 的嵌套 OAuth 形，也不是
// zcode 的 credentials 字典形，通用解析器一个字段都认不出来（键名不带 token/credential）。
//
// 两条硬线（与 zcode 侧同口径，用户明确要求过「要账号自带的能力，不是 agent 上的模型」）：
//
//  1. 只取 OpenCode 自家上游。判据是 providerID 属于 {opencode, opencode-go}——
//     这两个是 OpenCode 官方托管（zen / zen go）。用户自己在 OpenCode 里配的第三方
//     provider（openrouter / deepseek / zhipuai-coding-plan / tencent-tokenhub …）
//     一律排除：那些是「agent 上的模型」，收进来会变成拿别家 key 打 OpenCode。
//
//  2. 免费层（zen 的 *-free / big-pickle 等）同样直连 zen 端点即可：2026-09-30 实测
//     免费层闸门是「stream=true」与「tools 含 bash/read」两条的与，都在请求体里，
//     与 UA / TLS 指纹无关（详见 internal/upstream/opencode.go 文件头更正）。故 zen
//     账号的 upstream_base 写成 **直连 zen 端点**，付费层与免费层一并反代（出站自动
//     stream 强制 true 并补 bash/read 占位工具）。
//     opencode-go（免费额度）直连 zen/go 端点即可。
package source

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// OpenCode 自家上游的 providerID 与缺省 base。其余 providerID 一律视为第三方，不取。
const (
	// OpenCodeZenProvider OpenCode Zen（含免费层）。
	OpenCodeZenProvider = "opencode"
	// OpenCodeGoProvider OpenCode Go（付费额度）。
	OpenCodeGoProvider = "opencode-go"

	// OpenCodeZenBase 直连 Zen 端点（付费层与免费层都可反代；免费层出站由
	// stream=true + bash/read 两条闸门放行）。
	OpenCodeZenBase = "https://opencode.ai/zen/v1"
	// OpenCodeGoBase 直连 Go 端点（需 x-opencode-session 头，适配器会补）。
	OpenCodeGoBase = "https://opencode.ai/zen/go/v1"
	// OpenCodeServeBase 本机 opencode serve 的缺省地址。**本网关不使用**：serve 会走
	// 宿主机命令执行（RCE），本适配器刻意不做，直连 zen 端点即可（免费层亦然）。
	OpenCodeServeBase = "http://127.0.0.1:4096"
)

// DefaultOpenCodeAuthPath OpenCode CLI/TUI 的凭证文件路径。
func DefaultOpenCodeAuthPath() string {
	return filepath.Join(HomeDir(), ".local", "share", "opencode", "auth.json")
}

// OpenCodeAccount 一个从 auth.json 里抽出来的 OpenCode 账号。
type OpenCodeAccount struct {
	Provider string // providerID：opencode / opencode-go
	Key      string // 明文 api key（不序列化，只在进程内流转）
	Base     string // 建议上游 base（zen 直连；go 直连）
	// Free 该 provider 是否含免费层（zen 有、go 没有），前端据此标「免费额度」。
	Free bool
}

// openCodeAuthFile auth.json 的解析形态：providerID → {type,key}。
type openCodeAuthFile map[string]struct {
	Type string `json:"type"`
	Key  string `json:"key"`
}

// ListOpenCodeAccounts 读 OpenCode auth.json，返回自家上游账号（按 provider 排序）。
// 文件不存在 / 解析失败一律返回错误——OpenCode 是否装了、登没登录，调用方要能区分。
func ListOpenCodeAccounts(path string) ([]OpenCodeAccount, error) {
	if strings.TrimSpace(path) == "" {
		path = DefaultOpenCodeAuthPath()
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f openCodeAuthFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	out := make([]OpenCodeAccount, 0, len(f))
	for id, ent := range f {
		if !isOpenCodeOwnProvider(id) {
			continue
		}
		key := strings.TrimSpace(ent.Key)
		if key == "" || strings.HasPrefix(key, "enc:v1:") {
			continue
		}
		acc := OpenCodeAccount{Provider: id, Key: key}
		switch id {
		case OpenCodeGoProvider:
			acc.Base = OpenCodeGoBase
		default:
			// zen：直连 zen 端点，付费层与免费层都反代（免费层出站补 stream + bash/read）。
			acc.Base = OpenCodeZenBase
			acc.Free = true
		}
		out = append(out, acc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Provider < out[j].Provider })
	return out, nil
}

// isOpenCodeOwnProvider 报告 providerID 是否为 OpenCode 官方托管（zen / zen go）。
func isOpenCodeOwnProvider(id string) bool {
	switch id {
	case OpenCodeZenProvider, OpenCodeGoProvider:
		return true
	}
	return false
}

// OpenCodeUID OpenCode 账号在号池里的 uid 形态（"opencode-"+providerID）。
func OpenCodeUID(provider string) string { return "opencode-" + sanitizeUID(provider) }

// ImportOpenCodeAs 把 OpenCode 账号导入号池。ids 命中 providerID；空 = 全量。
// 每个 provider 只写一条凭据（一号一行），文件名沿用 workbuddy-<uid>.json 以便被
// auth.LoadDir 的 glob 收走；重复导入幂等（uid 稳定）。
func ImportOpenCodeAs(path, authDir string, ids []string, opt ImportOptions) (ImportResult, error) {
	accounts, err := ListOpenCodeAccounts(path)
	if err != nil {
		return ImportResult{}, err
	}
	opt = opt.normalized()
	opt.Producer = ProducerOpenCode
	if opt.Detail == "" {
		opt.Detail = path
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Kind: "opencode", AuthDir: authDir}
	labels := map[string]string{}
	for _, acc := range accounts {
		if len(want) > 0 && !want[acc.Provider] {
			continue
		}
		uid := OpenCodeUID(acc.Provider)
		dest := filepath.Join(authDir, AuthFileName(uid))
		if _, serr := os.Stat(dest); serr == nil {
			res.Skipped = append(res.Skipped, uid)
			continue
		}
		base := opt.UpstreamBase
		if base == "" {
			base = acc.Base
		}
		f := Flat{
			// accessToken 承载上游 Bearer：OpenCode 的 api key 直接当 Bearer 用。
			AccessToken: acc.Key,
			// realm 显式写 cn：OpenCode 无双域概念；写死避免 realm backfill 把文件
			// 重写成嵌套形（keepalive/refresh 对 OpenCode 账号无意义）。
			Realm:        "cn",
			Domain:       base,
			UID:          uid,
			Nickname:     openCodeLabel(acc.Provider),
			Producer:     ProducerOpenCode,
			UpstreamBase: base,
		}
		if werr := writeFlatAtomic(dest, f); werr != nil {
			res.Errors = append(res.Errors, uid+": "+werr.Error())
			continue
		}
		res.Imported = append(res.Imported, uid)
		labels[uid] = openCodeLabel(acc.Provider)
	}
	sort.Strings(res.Imported)
	sort.Strings(res.Skipped)
	if len(res.Imported) > 0 {
		mark := make(map[string]Origin, len(res.Imported))
		for _, uid := range res.Imported {
			mark[uid] = Origin{Method: opt.Method, Producer: ProducerOpenCode, Label: labels[uid], Detail: opt.Detail}
		}
		_ = LoadRegistry(authDir).Mark(mark)
	}
	return res, nil
}

// openCodeLabel 账号显示名。
func openCodeLabel(provider string) string {
	switch provider {
	case OpenCodeZenProvider:
		return "OpenCode Zen"
	case OpenCodeGoProvider:
		return "OpenCode Go"
	}
	return provider
}

// OpenCodeCandidates 管理页用：把可取用账号摊成 Candidate 列表（带凭据指纹与建议上游）。
func OpenCodeCandidates(path, authDir string) ([]Candidate, error) {
	accounts, err := ListOpenCodeAccounts(path)
	if err != nil {
		return nil, err
	}
	existing := importedIndex(authDir)
	out := make([]Candidate, 0, len(accounts))
	for _, acc := range accounts {
		uid := OpenCodeUID(acc.Provider)
		detail := "provider=" + acc.Provider + " · key=" + keyHint(acc.Key)
		if acc.Free {
			detail += " · 含免费层模型（出站 stream=true + 补 bash/read 占位工具即可直连反代）"
		}
		c := Candidate{
			ID:         acc.Provider,
			Label:      openCodeLabel(acc.Provider),
			Realm:      "opencode",
			HasRefresh: true,
			Producer:   ProducerOpenCode,
			Usable:     true,
			Detail:     detail,
			Upstream:   acc.Base,
			APIKeyHint: keyHint(acc.Key),
		}
		if fn, ok := existing[uid]; ok {
			c.Imported = true
			c.ImportedAs = fn
		}
		out = append(out, c)
	}
	return out, nil
}

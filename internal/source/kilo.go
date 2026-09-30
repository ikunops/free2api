// kilo.go Kilo Code 账号的「取源」侧。
//
// 与 workbuddy / zcode / opencode 三家都不一样：那三家要先去工具自己的登录态里
// 挖凭据（OAuth token / API key），Kilo 免费层**不需要任何账号**——它的
// OpenRouter 兼容网关对匿名调用直接放行（带不带 Authorization 都 200）。所以这里
// 不读任何本机文件，直接合成一条「匿名」占位号：accessToken 写 anonymous，
// upstream_base 写缺省 chat base。
//
// 这样做的意义是把 Kilo 接进同一套号池 / 发布清单 / 出口模型叙事：管理页里它和
// 别家一样是一条 producer、一组可选模型、一个可开关的出口，而不是「另开一个口子」
// 的特例。用户看到的行为也一致：勾上 Kilo 免费模型，出口就多出这些模型。
//
// 实测依据（2026-09-30，见 internal/upstream/kilo.go 文件头）：模型目录
// https://api.kilo.ai/api/gateway/models 免鉴权可读（397 个），按 pricing 双零
// 过滤后约 19 个免费模型；chat 端点匿名 200。
package source

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// KiloAnonymousUID Kilo 匿名占位号在号池里的 uid。固定值：Kilo 没有账号维度，
	// 一条就够，重复导入幂等（不会被拆成多条）。
	KiloAnonymousUID = "kilo-anonymous"
	// KiloAnonymousToken 匿名通道的占位凭据（与 upstream.kiloAnonymousKey 同值）。
	KiloAnonymousToken = "anonymous"
	// KiloChatBase Kilo 的 OpenAI 兼容对话前缀（缺省上游 base）。
	KiloChatBase = "https://api.kilo.ai/api/openrouter"
)

// KiloAvailable 本机是否「能用」Kilo。Kilo 不需要账号，所以只要网络可达就算可用；
// 这里不做网络探测（取源页的探测是同步路径，不该因外网抖动把整页拖住），
// 一律返回 true，真正可不可用由上游目录拉取/探活结果体现。
func KiloAvailable() bool { return true }

// KiloLabel 账号显示名。
func KiloLabel() string { return "Kilo Code（匿名免费层）" }

// KiloFlat 合成 Kilo 匿名号要写的扁平形凭据。
// realm 写死 cn：Kilo 无双域概念，写死避免 realm backfill 把文件改写成嵌套形
// （keepalive / refresh 对 Kilo 无意义）。
func KiloFlat() Flat {
	return Flat{
		AccessToken:  KiloAnonymousToken,
		Realm:        "cn",
		Domain:       KiloChatBase,
		UID:          KiloAnonymousUID,
		Nickname:     KiloLabel(),
		Producer:     ProducerKilo,
		UpstreamBase: KiloChatBase,
	}
}

// ImportKilo 把 Kilo 匿名号写进号池。Kilo 只有一条号，ids 为空 = 就导这一条；
// 非空时只在命中 KiloAnonymousUID 时导。重复导入幂等（uid 稳定、已存在则跳过）。
func ImportKilo(authDir string, ids []string, opt ImportOptions) (ImportResult, error) {
	opt = opt.normalized()
	opt.Producer = ProducerKilo
	if opt.Detail == "" {
		opt.Detail = "kilo 匿名免费层（无需账号）"
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Kind: "kilo", AuthDir: authDir}
	if len(want) > 0 && !want[KiloAnonymousUID] {
		return res, nil
	}
	dest := filepath.Join(authDir, AuthFileName(KiloAnonymousUID))
	if _, serr := os.Stat(dest); serr == nil {
		res.Skipped = append(res.Skipped, KiloAnonymousUID)
		return res, nil
	}
	f := KiloFlat()
	if base := strings.TrimSpace(opt.UpstreamBase); base != "" {
		f.UpstreamBase, f.Domain = base, base
	}
	if werr := writeFlatAtomic(dest, f); werr != nil {
		res.Errors = append(res.Errors, KiloAnonymousUID+": "+werr.Error())
		return res, nil
	}
	res.Imported = append(res.Imported, KiloAnonymousUID)
	sort.Strings(res.Imported)
	_ = LoadRegistry(authDir).Mark(map[string]Origin{
		KiloAnonymousUID: {Method: opt.Method, Producer: ProducerKilo, Label: KiloLabel(), Detail: opt.Detail},
	})
	return res, nil
}

// KiloCandidates 管理页用：Kilo 只有一条「匿名」候选。Imported 标出号池里是否已有。
func KiloCandidates(authDir string) ([]Candidate, error) {
	c := Candidate{
		ID:         KiloAnonymousUID,
		Label:      KiloLabel(),
		Realm:      "kilo",
		Producer:   ProducerKilo,
		Usable:     true,
		Detail:     "无需账号：Kilo 的 OpenAI 兼容网关对匿名调用直接放行；免费层按上游目录 pricing 双零判定",
		Upstream:   KiloChatBase,
		APIKeyHint: KiloAnonymousToken,
	}
	if fn, ok := importedIndex(authDir)[KiloAnonymousUID]; ok {
		c.Imported = true
		c.ImportedAs = fn
	}
	return []Candidate{c}, nil
}

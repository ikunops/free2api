// zcode.go zcode 账号的「取源」侧：读 ~/.zcode-switch/accounts/*.json，把账号**自带**的
// 智谱（BigModel / Z.ai）上游凭据抽出来，归一化成网关认得的扁平形 auth。
//
// 存在的理由：zcode-switch 已经是现成的账号管理轮子（zcode 客户端自己维护的账号快照），
// 里面躺着每种 provider 的 apiKey。原先本包只做「只读发现」——因为当时没摸清哪把 key
// 真能反代。现在实测清楚了，这条链路可以直接接进号池：
//
//	账号文件 config.provider["builtin:bigmodel-coding-plan"].options.apiKey
//	  └─ 49 位 id.secret（<32hex>.<16base62>），Bearer 打
//	     https://open.bigmodel.cn/api/paas/v4/chat/completions → 200 真回复
//
// 划两条硬线（都是用户明确要求的口径）：
//
//  1. 只取**账号自带**的上游凭据。判据是 provider 的 baseURL 指向真实的智谱上游
//     （open.bigmodel.cn / api.z.ai）。用户自己在 zcode 里加的第三方聚合网关
//     （openrouter / opencode / kilo）和指向本机 127.0.0.1:xxxx 的自建网关一律**排除**——
//     那些是「agent 上的模型」，不是账号能力，收进来会变成拿别家 key 打智谱。
//
//  2. 绝不抽 JWT 形态的凭据。builtin:*-start-plan 的 255 位 JWT 打的是
//     https://zcode.z.ai/api/v1/zcode-plan/... —— 那条路被阿里云风险识别（captcha）
//     挡在业务逻辑之前（实测：合法体与空体都返回 3007 captcha verify failed），
//     属红线段，不碰。looksLikeJWT 把这批直接滤掉。
//
// 与 ListZCodeSwitch 的分工：那个是**只读清单**（老口径，保留给「看看本机有几个号」），
// 本文件是**可取用清单**（带 key、带模型、能被 ImportZCode 写进号池）。
package source

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"free2api/internal/auth"
)

// zcodeUpstream 一条允许使用的上游：host → 缺省 base（不带路径尾巴）。
// 只有出现在这里的 host 才被认为是「账号自带能力」；其余一律排除（见文件头硬线 1）。
var zcodeUpstream = map[string]string{
	"open.bigmodel.cn": "https://open.bigmodel.cn/api/paas/v4", // 智谱 BigModel（国内）
	"api.z.ai":         "https://api.z.ai/api/paas/v4",         // Z.ai（国际）
}

// ZCodeKey 一条从 zcode 账号里抽出来的上游凭据候选。
type ZCodeKey struct {
	Provider string `json:"provider"` // config.provider 的键名，如 builtin:bigmodel-coding-plan
	BaseURL  string `json:"base_url"` // 归一化后的上游 base（不含 /chat/completions）
	APIKey   string `json:"-"`        // 明文凭据：**不序列化**，只在本进程内流转
	Kind     string `json:"kind,omitempty"`
	Builtin  bool   `json:"builtin"` // 是否 zcode 内置 provider（账号自带；false = 用户自定义）
	// IDSecret 该 key 是否为完整的 id.secret 形态（<32hex>.<secret>）。
	// 只有它实测能过鉴权；裸 32 位 hex 是被截断的前半段（实测 401 已过期）。
	IDSecret bool `json:"id_secret"`
}

// ZCodeModel 账号自带的模型能力（config.provider[].models 里声明的，非 agent 侧配置）。
type ZCodeModel struct {
	ID        string `json:"id"`
	Context   int64  `json:"context,omitempty"`
	Output    int64  `json:"output,omitempty"`
	Image     bool   `json:"image,omitempty"`
	Reasoning bool   `json:"reasoning,omitempty"`
}

// ZCodeAccount 一个 zcode 账号快照的可用部分。
type ZCodeAccount struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	UpdatedAt string       `json:"updated_at,omitempty"`
	Provider  string       `json:"provider,omitempty"` // oauth:active_provider（bigmodel / zai）
	Encrypted bool         `json:"encrypted"`          // active_provider 被 enc:v1 封了（回原机器取）
	Keys      []ZCodeKey   `json:"keys"`               // 抽到的上游凭据候选（已按可用性排序）
	Models    []ZCodeModel `json:"models"`             // 账号自带模型清单（多 provider 合并去重）
	JWT       string       `json:"-"`                  // zcodejwttoken：读额度用，不序列化
	DeviceMID string       `json:"-"`                  // virtual_device_mid：读额度要带
	// JWTEncrypted 账本里这条 jwt 是 enc:v1: 密文、且本机当前解不开。
	// 用途是如实上报「为什么读不到额度」，不是「加密=不可用」的唯一判据（同机能解）。
	JWTEncrypted bool   `json:"jwt_encrypted,omitempty"`
	SourceFile   string `json:"source_file,omitempty"`
}

// UsableKey 返回该账号最值得先用的一把凭据（无 → ok=false）。导出供 server 层
// 探活/额度查询取 key（明文只在进程内流转，不进响应体）。
func (a ZCodeAccount) UsableKey() (ZCodeKey, bool) { return a.usableKey() }

// usableKey 返回该账号最值得先用的一把凭据（无 → ok=false）。
func (a ZCodeAccount) usableKey() (ZCodeKey, bool) {
	for _, k := range a.Keys {
		if k.IDSecret {
			return k, true
		}
	}
	if len(a.Keys) > 0 {
		return a.Keys[0], true
	}
	return ZCodeKey{}, false
}

// zcodeProviderSpec config.provider 的单条解析形态。
type zcodeProviderSpec struct {
	Enabled bool   `json:"enabled"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Options struct {
		APIKey  string `json:"apiKey"`
		BaseURL string `json:"baseURL"`
	} `json:"options"`
	Models map[string]zcodeModelSpec `json:"models"`
}

type zcodeModelSpec struct {
	Limit struct {
		Context int64 `json:"context"`
		Output  int64 `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Reasoning struct {
		Enabled bool `json:"enabled"`
	} `json:"reasoning"`
}

// zcodeAccountFile zcode-switch 单账号快照的完整解析形态（只取用得到的字段）。
type zcodeAccountFile struct {
	ID          string                     `json:"id"`
	Name        string                     `json:"name"`
	UpdatedAt   string                     `json:"updated_at"`
	Credentials map[string]json.RawMessage `json:"credentials"`
	Config      struct {
		Provider map[string]zcodeProviderSpec `json:"provider"`
	} `json:"config"`
	VirtualDeviceMID string `json:"virtual_device_mid"`
}

// looksLikeJWT 判定 JWT 形态（三段点分 base64url）。start-plan 凭据即此形态，
// 走的是被 captcha 挡住的 zcode.z.ai 计划链路，一律不取（见文件头硬线 2）。
func looksLikeJWT(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				return false
			}
		}
	}
	return true
}

// zcodeBaseFromURL 把 provider 的 baseURL 归一化成「允许的上游 base」。
// 不在白名单 host 上 → ok=false（第三方聚合网关 / 本机自建网关，见文件头硬线 1）。
// baseURL 可能带路径（.../api/anthropic、.../api/paas/v4），统一替换成该 host 的
// OpenAI 兼容前缀：BigModel 的 /api/paas/v4。
func zcodeBaseFromURL(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	base, ok := zcodeUpstream[host]
	if !ok {
		return "", false
	}
	return base, true
}

// zcodeKeyScore 凭据排序权重（越大越先用）。实测口径：
//   - id.secret（49 位带点）能过鉴权；裸 32 位 hex 是被截断的前半段（401）。
//   - 内置 coding-plan > 内置普通 > 用户自定义。
func zcodeKeyScore(k ZCodeKey) int {
	s := 0
	if k.IDSecret {
		s += 100
	}
	if k.Builtin {
		s += 10
	}
	if strings.Contains(k.Provider, "coding-plan") {
		s += 5
	}
	return s
}

// ParseZCodeAccount 解析一个 zcode 账号快照文件（纯函数，不碰磁盘）。
func ParseZCodeAccount(raw []byte, sourceFile string) (ZCodeAccount, error) {
	var f zcodeAccountFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return ZCodeAccount{}, fmt.Errorf("parse zcode account: %w", err)
	}
	acc := ZCodeAccount{
		ID:         f.ID,
		Name:       strings.TrimSpace(f.Name),
		UpdatedAt:  f.UpdatedAt,
		DeviceMID:  f.VirtualDeviceMID,
		SourceFile: sourceFile,
	}
	if acc.ID == "" && sourceFile != "" {
		acc.ID = strings.TrimSuffix(filepath.Base(sourceFile), filepath.Ext(sourceFile))
	}
	if acc.Name == "" {
		acc.Name = acc.ID
	}
	if raw, ok := f.Credentials[zcodeCredActiveProvider]; ok {
		if zcodePlainCred(raw) {
			acc.Provider = strings.Trim(strings.TrimSpace(string(raw)), "\"")
		} else {
			acc.Encrypted = true
		}
	}
	if raw, ok := f.Credentials[zcodeCredJWTToken]; ok {
		val := zcodeCredTrim(string(raw))
		switch {
		case zcodePlainCred(raw):
			acc.JWT = val
		default:
			// enc:v1: 字段级加密。密钥按「机器 + 用户名」派生——同机能解、换机器解不开；
			// 解不开只标注，不失败（号本身还能反代，只是额度读不到）。
			if pt, _, derr := DecryptZCodeCredAuto(val); derr == nil {
				acc.JWT = strings.TrimSpace(pt)
			} else if strings.HasPrefix(val, zcodeCredPrefix) {
				acc.JWTEncrypted = true
			}
		}
	}

	seenKey := map[string]bool{}
	seenModel := map[string]ZCodeModel{}
	for name, spec := range f.Config.Provider {
		key := strings.TrimSpace(spec.Options.APIKey)
		base, baseOK := zcodeBaseFromURL(spec.Options.BaseURL)
		// 模型清单：只要 provider 指向允许的上游就收（即使这把 key 空/不可用，
		// 模型能力是账号声明的，与某一把临时 key 无关）。
		if baseOK {
			for id, ms := range spec.Models {
				id = strings.TrimSpace(id)
				if id == "" {
					continue
				}
				m := ZCodeModel{
					ID:        id,
					Context:   ms.Limit.Context,
					Output:    ms.Limit.Output,
					Reasoning: ms.Reasoning.Enabled,
				}
				for _, in := range ms.Modalities.Input {
					if strings.EqualFold(in, "image") {
						m.Image = true
					}
				}
				if prev, ok := seenModel[id]; !ok || prev.Context < m.Context {
					seenModel[id] = m
				}
			}
		}
		if !baseOK || key == "" {
			continue
		}
		// 加密凭据（enc:v1:）读不出、JWT 走 captcha 链路——都不取，不猜。
		if strings.HasPrefix(key, zcodeCredPrefix) || looksLikeJWT(key) {
			continue
		}
		if seenKey[base+"|"+key] {
			continue
		}
		seenKey[base+"|"+key] = true
		acc.Keys = append(acc.Keys, ZCodeKey{
			Provider: name,
			BaseURL:  base,
			APIKey:   key,
			Kind:     spec.Kind,
			Builtin:  strings.HasPrefix(name, "builtin:"),
			IDSecret: strings.Contains(key, ".") && len(key) >= 40,
		})
	}
	sort.Slice(acc.Keys, func(i, j int) bool {
		si, sj := zcodeKeyScore(acc.Keys[i]), zcodeKeyScore(acc.Keys[j])
		if si != sj {
			return si > sj
		}
		return acc.Keys[i].Provider < acc.Keys[j].Provider
	})
	ids := make([]string, 0, len(seenModel))
	for id := range seenModel {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		acc.Models = append(acc.Models, seenModel[id])
	}
	return acc, nil
}

// ListZCodeAccounts 读 zcode-switch 账号目录，返回**可取用**的账号清单（按名字排序）。
// 目录不存在 / 单个文件损坏一律跳过，不报错——这条链路是加分项，不该拖垮管理页。
func ListZCodeAccounts(dir string) ([]ZCodeAccount, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]ZCodeAccount, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		raw, rerr := os.ReadFile(full)
		if rerr != nil {
			continue
		}
		acc, perr := ParseZCodeAccount(raw, full)
		if perr != nil {
			continue
		}
		out = append(out, acc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ImportZCodeAs 把 zcode 账号导入号池。ids 为空 = 全量；非空 = 只导 id/名字命中的。
// 每个账号只写**一把**凭据（该账号得分最高的那把），文件名沿用 workbuddy-<uid>.json
// 以便被 auth.LoadDir 的 glob 收走；uid 用账号 id，重复导入幂等。
//
// 为什么只写一把：一号多 key 会让「余额/额度」归属变得含糊（几把 key 可能属于不同
// 子账号），号池维度应当一号一行。要多号就多导几条，粒度更清楚。
func ImportZCodeAs(dir, authDir string, ids []string, opt ImportOptions) (ImportResult, error) {
	accounts, err := ListZCodeAccounts(dir)
	if err != nil {
		return ImportResult{}, err
	}
	opt = opt.normalized()
	opt.Producer = ProducerZCode
	if opt.Detail == "" {
		opt.Detail = dir
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		return ImportResult{}, err
	}
	res := ImportResult{Kind: "zcode-switch", AuthDir: authDir}
	labels := map[string]string{}
	for _, acc := range accounts {
		if len(want) > 0 && !want[acc.ID] && !want[acc.Name] {
			continue
		}
		key, ok := acc.usableKey()
		if !ok {
			res.Errors = append(res.Errors, acc.Name+": 没有可用的上游凭据（加密或未登录）")
			continue
		}
		uid := ZCodeUID(acc.ID)
		dest := filepath.Join(authDir, AuthFileName(uid))
		if _, serr := os.Stat(dest); serr == nil {
			res.Skipped = append(res.Skipped, uid)
			continue
		}
		f := Flat{
			// accessToken 承载上游 Bearer 凭据：对 BigModel 来说这把 id.secret 就是
			// Bearer Token 本身（ChatHeaders 会原样放进 Authorization 头）。
			AccessToken: key.APIKey,
			// realm 显式写 cn：避免 auth.LoadDir 的 realm backfill 把 zcode 文件
			// 重写成嵌套形（keepalive/refresh 对 zcode 账号无意义，也不该触发）。
			Realm:        "cn",
			Domain:       key.BaseURL,
			UID:          uid,
			Nickname:     acc.Name,
			Producer:     ProducerZCode,
			UpstreamBase: key.BaseURL,
			// 观测凭据一并落盘：zcodejwttoken + device mid 在 zcode 生态里原本只躺在
			// 账本或应用私有目录里，号池文件不带的话，换机器 / 删账本后就再也读不到
			// 这个号的套餐到期与剩余额度了。device mid 账本没带时用本机应用级的兜底。
			ZCodeJWT:       acc.JWT,
			ZCodeDeviceMID: firstNonEmpty(acc.DeviceMID, ZCodeAppDeviceMID("")),
		}
		if werr := writeFlatAtomic(dest, f); werr != nil {
			res.Errors = append(res.Errors, uid+": "+werr.Error())
			continue
		}
		res.Imported = append(res.Imported, uid)
		labels[uid] = acc.Name
	}
	sort.Strings(res.Imported)
	sort.Strings(res.Skipped)
	if len(res.Imported) > 0 {
		mark := make(map[string]Origin, len(res.Imported))
		for _, uid := range res.Imported {
			mark[uid] = Origin{Method: opt.Method, Producer: ProducerZCode, Label: labels[uid], Detail: opt.Detail}
		}
		_ = LoadRegistry(authDir).Mark(mark)
	}
	return res, nil
}

// ImportZCode 便捷入口：来源记为 switch 账本（zcode-switch 目录本身就是账本形态）。
func ImportZCode(dir, authDir string, ids []string) (ImportResult, error) {
	return ImportZCodeAs(dir, authDir, ids, ImportOptions{Method: MethodSwitch, Detail: dir})
}

// ZCodeCandidates 管理页用：把可取用账号摊成 Candidate 列表（带模型清单与凭据指纹）。
// authDir 非空时标注「已在号池」。probe 由调用方（server 层）另行补充：本包不做网络 I/O。
func ZCodeCandidates(dir, authDir string) ([]Candidate, error) {
	accounts, err := ListZCodeAccounts(dir)
	if err != nil {
		return nil, err
	}
	existing := importedIndex(authDir)
	out := make([]Candidate, 0, len(accounts))
	for _, acc := range accounts {
		key, hasKey := acc.usableKey()
		models := make([]string, 0, len(acc.Models))
		for _, m := range acc.Models {
			models = append(models, m.ID)
		}
		uid := ZCodeUID(acc.ID)
		c := Candidate{
			ID:         acc.ID,
			Label:      acc.Name,
			Realm:      "zcode",
			HasRefresh: hasKey,
			Producer:   ProducerZCode,
			Models:     models,
			Usable:     hasKey,
		}
		switch {
		case acc.Encrypted && !hasKey:
			c.Detail = "上游凭据被 enc:v1 加密，本机解不开（密钥按机器 + 用户名派生，换机器必然解不开）"
		case !hasKey:
			c.Detail = "没有指向智谱上游的可用凭据（只找到第三方网关或 start-plan JWT）"
		default:
			c.Detail = "provider=" + acc.Provider + " · key=" + key.Provider
			if key.IDSecret {
				c.Detail += "（id.secret，实测可用）"
			} else {
				c.Detail += "（裸 32 位，实测凭证已过期/不完整）"
			}
			c.Note = "上游 " + key.BaseURL
			c.Upstream = key.BaseURL
			c.APIKeyHint = keyHint(key.APIKey)
		}
		if fn, ok := existing[uid]; ok {
			c.Imported = true
			c.ImportedAs = fn
		}
		out = append(out, c)
	}
	return out, nil
}

// ZCodeUID zcode 账号在号池里的 uid 形态（"zcode-"+账号 id）。单独导出，
// 好让「导入」与「回显已在号池」两侧用同一个派生规则（写死两处必然漂移）。
func ZCodeUID(accountID string) string { return "zcode-" + sanitizeUID(accountID) }

// KeyHint 是 keyHint 的导出面（server 层回显指纹用；明文永不外传）。
func KeyHint(k string) string { return keyHint(k) }

// keyHint 凭据指纹：前 6 + 后 4，中间省略。管理页要能对账「是不是同一把 key」，
// 但绝不能把明文全量回显到浏览器（页面截图/日志都会带出去）。
func keyHint(k string) string {
	k = strings.TrimSpace(k)
	if k == "" {
		return ""
	}
	if len(k) <= 12 {
		return k[:2] + "…" + k[len(k)-2:]
	}
	return k[:6] + "…" + k[len(k)-4:]
}

// zcodeAccountFile 里 credentials 的键名常量（避免散落的字符串字面量拼错）。
const (
	zcodeCredActiveProvider = "oauth:active_provider"
	zcodeCredJWTToken       = "zcodejwttoken"
)

var _ = auth.AuthFileGlob // 保底引用：本文件的导入产物必须匹配该 glob

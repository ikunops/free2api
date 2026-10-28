package source

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LoadZCodeAppAccount 读 ZCode 应用自己的当前登录态（~/.zcode/v2）。
//
// 这与 zcode-switch 的账号快照不是同一种文件：应用把身份、JWT 与静态 API key
// 分散在 config.json / credentials.json / telemetry-state.json 三处，且后者常是
// enc:v1 字段级加密。这里只抽出当前登录的那一个号，作为 method=app 的可导入候选。
//
// 关键边界：oauth:zai:access_token / zcodejwttoken 是官方客户端登录态，不是静态
// OpenAI 兼容 API 凭据；只有 account-provider:*:api-key 解出来的值才允许进入 Keys。
func LoadZCodeAppAccount(appDir string) (ZCodeAccount, error) {
	if strings.TrimSpace(appDir) == "" {
		appDir = ZCodeAppCredDir()
	}
	acc := ZCodeAccount{
		SourceFile: filepath.Join(appDir, "credentials.json"),
		DeviceMID:  readZCodeDeviceMID(filepath.Join(appDir, "telemetry-state.json")),
	}
	credRaw, err := os.ReadFile(filepath.Join(appDir, "credentials.json"))
	if err != nil {
		return ZCodeAccount{}, fmt.Errorf("读 zcode credentials.json 失败：%w", err)
	}
	var creds map[string]json.RawMessage
	if err := json.Unmarshal(credRaw, &creds); err != nil {
		return ZCodeAccount{}, fmt.Errorf("zcode credentials.json 不是合法 json：%w", err)
	}
	if raw, ok := creds[zcodeCredActiveProvider]; ok {
		acc.Provider = strings.Trim(strings.TrimSpace(string(raw)), "\"")
	}
	// user_info 是登录身份；不同版本是 JSON 字符串，也兼容直接对象。
	if raw, ok := creds["oauth:"+acc.Provider+":user_info"]; ok {
		var ui struct {
			DisplayName string `json:"displayName"`
			Username    string `json:"username"`
			ID          string `json:"id"`
		}
		if uerr := decodeJSONMaybeString(raw, &ui); uerr == nil {
			acc.ID = strings.TrimSpace(ui.ID)
			acc.Name = strings.TrimSpace(ui.DisplayName)
			if acc.Name == "" {
				acc.Name = strings.TrimSpace(ui.Username)
			}
		}
	}
	if acc.ID == "" {
		acc.ID = "zcode-app"
	}
	if acc.Name == "" {
		acc.Name = acc.ID
	}
	// 静态 key：只收智谱白名单 + account-provider:*:api-key。
	seen := map[string]bool{}
	for name, raw := range creds {
		if !strings.HasPrefix(name, "account-provider:") || !strings.HasSuffix(name, ":api-key") {
			continue
		}
		val, _, derr := DecryptZCodeCredAuto(string(raw))
		if derr != nil {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" || looksLikeJWT(val) {
			continue
		}
		base := zcodeBaseFromCredentialName(name)
		if base == "" {
			continue
		}
		k := base + "|" + val
		if seen[k] {
			continue
		}
		seen[k] = true
		acc.Keys = append(acc.Keys, ZCodeKey{
			Provider: name,
			BaseURL:  base,
			APIKey:   val,
			Kind:     "api-key",
			Builtin:  strings.Contains(name, ":coding-plan:"),
			IDSecret: strings.Contains(val, ".") && len(val) >= 40,
		})
	}
	// 读额度专用 JWT：独立于静态 key，不写进 Keys。
	if raw, ok := creds[zcodeCredJWTToken]; ok {
		if val, _, derr := DecryptZCodeCredAuto(string(raw)); derr == nil {
			acc.JWT = strings.TrimSpace(val)
		} else {
			acc.JWTEncrypted = true
		}
	}
	// 模型能力来自 config.json（不是 credentials.json）。
	acc.Models = loadZCodeAppModels(filepath.Join(appDir, "config.json"))
	sort.Slice(acc.Keys, func(i, j int) bool {
		si, sj := zcodeKeyScore(acc.Keys[i]), zcodeKeyScore(acc.Keys[j])
		if si != sj {
			return si > sj
		}
		return acc.Keys[i].Provider < acc.Keys[j].Provider
	})
	return acc, nil
}

func decodeJSONMaybeString(raw json.RawMessage, dst any) error {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return json.Unmarshal([]byte(s), dst)
	}
	return json.Unmarshal(raw, dst)
}

func loadZCodeAppModels(path string) []ZCodeModel {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var appFile struct {
		Provider map[string]zcodeProviderSpec `json:"provider"`
		Config   struct {
			Provider map[string]zcodeProviderSpec `json:"provider"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &appFile); err != nil {
		return nil
	}
	seen := map[string]ZCodeModel{}
	providers := appFile.Provider
	if len(providers) == 0 {
		providers = appFile.Config.Provider
	}
	for _, spec := range providers {
		for id, ms := range spec.Models {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			m := ZCodeModel{ID: id, Context: ms.Limit.Context, Output: ms.Limit.Output, Reasoning: ms.Reasoning.Enabled}
			for _, in := range ms.Modalities.Input {
				if strings.EqualFold(in, "image") {
					m.Image = true
				}
			}
			if prev, ok := seen[id]; !ok || prev.Context < m.Context {
				seen[id] = m
			}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ZCodeModel, 0, len(ids))
	for _, id := range ids {
		out = append(out, seen[id])
	}
	return out
}

// ZCodeAppCandidates 管理页用：应用当前登录态最多产生一个候选。
// 历史函数保留给直接读候选的调用方；管理端点走 server 的增强视图。
func ZCodeAppCandidates(appDir, authDir string) ([]Candidate, error) {
	acc, err := LoadZCodeAppAccount(appDir)
	if err != nil {
		return nil, err
	}
	key, hasKey := acc.usableKey()
	c := Candidate{ID: acc.ID, Label: acc.Name, Realm: "zcode", HasRefresh: hasKey, Producer: ProducerZCode, Usable: hasKey}
	for _, m := range acc.Models {
		c.Models = append(c.Models, m.ID)
	}
	if hasKey {
		c.Detail = "应用登录态 · provider=" + acc.Provider + " · key=" + key.Provider + "（静态 key 形态完整；不等于有余额）"
		c.Note = "上游 " + key.BaseURL
		c.Upstream = key.BaseURL
		c.APIKeyHint = keyHint(key.APIKey)
	} else {
		c.Detail = "应用登录态里没有可解出的静态 API key（JWT/登录 token 不能当静态 key 使用）"
	}
	if existing := importedIndex(authDir); existing[ZCodeUID(acc.ID)] != "" {
		c.Imported = true
		c.ImportedAs = existing[ZCodeUID(acc.ID)]
	}
	return []Candidate{c}, nil
}

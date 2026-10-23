// growth_expert_local.go 从 WorkBuddy 本机专家缓存解析真实专家元数据。
//
// 缓存是客户端安装/使用专家时落盘的公开元数据（expertId/plugin/agent/prompt），
// 不读取登录 token。市场接口不可用时，它是避免编造专家 id 的可靠兜底；调用方仍
// 必须完成一次真实会话，不能把“找到 id”当成任务完成。
package upstream

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const (
	growthExpertCacheMaxPromptBytes = 64 * 1024
	growthExpertCacheMaxFiles       = 64
)

var growthExpertCacheOnce sync.Once
var growthExpertCacheSnapshot []GrowthMarketExpert
var growthExpertCacheErr error

// FindLocalGrowthExpert 在本机 WorkBuddy 专家缓存中按任务语义查找真实专家。
// aliases 通常是任务名/任务描述中的专有名词；空 aliases 只允许唯一候选。
func FindLocalGrowthExpert(aliases ...string) (GrowthMarketExpert, bool) {
	experts := localGrowthExperts()
	if len(experts) == 0 {
		return GrowthMarketExpert{}, false
	}
	for _, alias := range aliases {
		if e, ok := FindGrowthMarketExpert(experts, alias); ok {
			return e, true
		}
	}
	if len(experts) == 1 {
		return experts[0], true
	}
	return GrowthMarketExpert{}, false
}

// LocalGrowthExpertByID 精确取缓存内的专家，用于市场查询结果补 prompt/plugin 信息。
func LocalGrowthExpertByID(id string) (GrowthMarketExpert, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return GrowthMarketExpert{}, false
	}
	for _, e := range localGrowthExperts() {
		if strings.EqualFold(e.ExpertID, id) || strings.EqualFold(e.SourceID, id) {
			return e, true
		}
	}
	return GrowthMarketExpert{}, false
}

func localGrowthExperts() []GrowthMarketExpert {
	growthExpertCacheOnce.Do(func() {
		growthExpertCacheSnapshot, growthExpertCacheErr = loadLocalGrowthExperts()
	})
	if growthExpertCacheErr != nil {
		return nil
	}
	return growthExpertCacheSnapshot
}

func loadLocalGrowthExperts() ([]GrowthMarketExpert, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	experts, ok := loadLocalGrowthExpertsFromHome(home)
	if ok {
		return experts, nil
	}
	return nil, fmt.Errorf("local WorkBuddy expert cache is empty")
}

func loadLocalGrowthExpertsFromHome(home string) ([]GrowthMarketExpert, bool) {
	experts, err := loadLocalGrowthExpertsAt(filepath.Join(home, ".workbuddy"))
	return experts, err == nil && len(experts) > 0
}

func loadLocalGrowthExpertsAt(root string) ([]GrowthMarketExpert, error) {
	files := make([]string, 0, 8)
	// 先读每个用户目录根部的 expert.json，再读 personal/global 下的最新缓存。
	patterns := []string{
		filepath.Join(root, "storage", "user-*", "expert.json"),
		filepath.Join(root, "storage", "user-*-personal", "global", "expert.json"),
	}
	for _, pattern := range patterns {
		matches, _ := filepath.Glob(pattern)
		sort.Strings(matches)
		files = append(files, matches...)
	}
	seenFile := map[string]bool{}
	byID := map[string]GrowthMarketExpert{}
	for _, path := range files {
		if seenFile[path] || len(seenFile) >= growthExpertCacheMaxFiles {
			continue
		}
		seenFile[path] = true
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var wrapper struct {
			Recents []localGrowthExpertJSON `json:"recents"`
		}
		if err := json.Unmarshal(raw, &wrapper); err != nil {
			continue
		}
		for _, item := range wrapper.Recents {
			e := item.toMarketExpert()
			if e.ExpertID == "" {
				continue
			}
			// 后读到的 personal/global 覆盖根目录旧缓存。
			if e.Prompt == "" {
				if old, ok := byID[e.ExpertID]; ok {
					e.Prompt = old.Prompt
				}
			}
			byID[e.ExpertID] = e
		}
	}
	out := make([]GrowthMarketExpert, 0, len(byID))
	for _, e := range byID {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayNameZH != out[j].DisplayNameZH {
			return out[i].DisplayNameZH < out[j].DisplayNameZH
		}
		return out[i].ExpertID < out[j].ExpertID
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("local WorkBuddy expert cache is empty")
	}
	return out, nil
}

type localGrowthExpertJSON struct {
	ID             string `json:"id"`
	MarketExpertID string `json:"marketExpertId"`
	Name           string `json:"name"`
	Profession     string `json:"profession"`
	Version        string `json:"version"`
	PluginName     string `json:"pluginName"`
	ExpertType     string `json:"expertType"`
	AgentName      string `json:"agentName"`
	LocalPath      string `json:"localPath"`
	DefaultPrompt  string `json:"defaultInitPrompt"`
}

func (e localGrowthExpertJSON) toMarketExpert() GrowthMarketExpert {
	id := strings.TrimSpace(e.MarketExpertID)
	if id == "" {
		// 有些缓存用语义 id（如 DouyinStrategist）作为 id，市场 id 才是 ex_*。
		id = strings.TrimSpace(e.ID)
	}
	typ := strings.TrimSpace(e.ExpertType)
	if typ == "" {
		typ = "agent"
	}
	prompt := readGrowthExpertPrompt(e.LocalPath, e.PluginName, e.AgentName)
	if prompt == "" {
		prompt = strings.TrimSpace(e.DefaultPrompt)
	}
	return GrowthMarketExpert{
		ExpertID: id, SourceID: strings.TrimSpace(e.ID), ExpertType: typ,
		DisplayNameZH: e.Name, ProfessionZH: e.Profession, Version: e.Version,
		PluginName: e.PluginName, AgentName: e.AgentName, LocalPath: e.LocalPath,
		Prompt: prompt,
	}
}

func readGrowthExpertPrompt(localPath, pluginName, agentName string) string {
	localPath = strings.TrimSpace(localPath)
	agentName = strings.TrimSpace(agentName)
	pluginName = strings.TrimSpace(pluginName)
	candidates := make([]string, 0, 3)
	if localPath != "" && agentName != "" {
		candidates = append(candidates, filepath.Join(localPath, "agents", agentName+".md"))
	}
	if localPath != "" && pluginName != "" {
		candidates = append(candidates, filepath.Join(localPath, "agents", pluginName+".md"))
	}
	if localPath != "" {
		matches, _ := filepath.Glob(filepath.Join(localPath, "agents", "*.md"))
		sort.Strings(matches)
		candidates = append(candidates, matches...)
	}
	seen := map[string]bool{}
	for _, path := range candidates {
		if seen[path] {
			continue
		}
		seen[path] = true
		st, err := os.Stat(path)
		if err != nil || st.IsDir() || st.Size() <= 0 || st.Size() > growthExpertCacheMaxPromptBytes {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if text := strings.TrimSpace(string(raw)); text != "" {
			return text
		}
	}
	return ""
}

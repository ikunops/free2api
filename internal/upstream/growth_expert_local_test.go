package upstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalGrowthExpertsPreferMarketIDAndReadPrompt(t *testing.T) {
	home := t.TempDir()
	pluginDir := filepath.Join(home, ".workbuddy", "plugins", "marketplaces", "experts", "plugins", "demo-expert")
	if err := os.MkdirAll(filepath.Join(pluginDir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "agents", "demo-agent.md"), []byte("# Demo\n真实专家提示词"), 0o644); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(home, ".workbuddy", "storage", "user-test-personal", "global")
	if err := os.MkdirAll(storage, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := `{"recents":[{"id":"DemoExpert","marketExpertId":"ex_real_demo","name":"真实专家","profession":"演示职业","version":"1.2.3","pluginName":"demo-plugin","expertType":"agent","agentName":"demo-agent","localPath":` + quoteJSON(pluginDir) + `}]}`
	if err := os.WriteFile(filepath.Join(storage, "expert.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}

	got, ok := loadLocalGrowthExpertsFromHome(home)
	if !ok {
		t.Fatal("local expert cache not parsed")
	}
	if len(got) != 1 || got[0].ExpertID != "ex_real_demo" || got[0].SourceID != "DemoExpert" {
		t.Fatalf("experts=%+v", got)
	}
	if !strings.Contains(got[0].Prompt, "真实专家提示词") {
		t.Fatalf("prompt=%q", got[0].Prompt)
	}
}

func TestGrowthExpertChatMetaUsesStringGrowthEvent(t *testing.T) {
	meta, err := GrowthExpertChatMeta(GrowthMarketExpert{
		ExpertID: "ex_real", DisplayNameZH: "专家", ProfessionZH: "职业",
		PluginName: "plugin", AgentName: "agent", ExpertType: "agent", Version: "1.0",
	}, "conv", "req", "glm-5.2", "GLM-5.2", []map[string]any{{"eventCode": "expert_actual_use"}})
	if err != nil {
		t.Fatal(err)
	}
	ai, ok := meta["codebuddy.ai"].(map[string]any)
	if !ok || ai["expertId"] != "ex_real" || ai["conversationId"] != "conv" {
		t.Fatalf("meta=%#v", meta)
	}
	raw, ok := ai["growthEvent"].(string)
	if !ok || !strings.Contains(raw, "expert_actual_use") {
		t.Fatalf("growthEvent must be JSON string, meta=%#v", ai)
	}
}

func quoteJSON(s string) string {
	b, _ := jsonMarshal([]string{s})
	return strings.TrimSuffix(strings.TrimPrefix(string(b), "["), "]")
}

func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

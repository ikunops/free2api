package upstream

import "testing"

// TestContextWindowListingRemoteWins 上游动态值（maxInputTokens>0）为权威：
// 无视知识表直接透出（包括与知识表不同的值）。
func TestContextWindowListingRemoteWins(t *testing.T) {
	if got := ContextWindowListing("glm-5.2", 262144); got != 262144 {
		t.Errorf("remote wins: context_length=%d want 262144 (remote authoritative)", got)
	}
	// 远端小值也权威——不「纠正」上游。
	if got := ContextWindowListing("glm-5.2", 32000); got != 32000 {
		t.Errorf("small remote: context_length=%d want 32000 (remote authoritative)", got)
	}
}

// TestContextWindowListingKnowledgeTable 知识表命中：远端零值 → 按模型补齐真实量级，
// 不再透出假 131072。
func TestContextWindowListingKnowledgeTable(t *testing.T) {
	cases := []struct {
		model, source string
		want          int64
	}{
		{"glm-5.2", "fork 实测 CN 1M", 1000000},
		{"glm-5.1", "fork 实测 200K", 200000},
		{"kimi-k2.7", "fork 实测 256K", 256000},
		{"minimax-m3", "fork 实测 512K", 512000},
		{"deepseek-v4-pro", "fork 实测 1M", 1000000},
		{"deepseek-v4.1-flash", "实测外推 + models.dev", 1000000},
		{"hy3", "fork 实测 192K", 192000},
		{"gpt-6-astra", "models.dev", 1050000},
		{"gpt-5.3-codex", "models.dev", 400000},
		{"auto", "fork global 外推", 168000},
	}
	for _, c := range cases {
		if got := ContextWindowListing(c.model, 0); got != c.want {
			t.Errorf("%s (%s): context_length=%d want %d", c.model, c.source, got, c.want)
		}
	}
}

// TestContextWindowListingUnknownFallback1M 知识表也未收录 → 1M 兜底
// （宁可高估不低估：高估代价=客户端不截断、上游报错重试；低估代价=丢上下文）。
func TestContextWindowListingUnknownFallback1M(t *testing.T) {
	if got := ContextWindowListing("totally-unknown-model", 0); got != 1000000 {
		t.Errorf("unknown model: context_length=%d want 1000000 (1M fallback)", got)
	}
	if got := ContextWindowListing("", 0); got != 1000000 {
		t.Errorf("empty model: context_length=%d want 1000000", got)
	}
}

// TestMaxOutputTokensListing max_output_tokens 三级查找口径与 context_length 不同：
// 未知 → 省略（ok=false），没有 1M 兜底（输出上限无安全侧可估）。
func TestMaxOutputTokensListing(t *testing.T) {
	// remote 权威。
	if got, ok := MaxOutputTokensListing("glm-5.2", 64000); !ok || got != 64000 {
		t.Errorf("remote: max_output_tokens=%d,%v want 64000,true", got, ok)
	}
	// 知识表命中。
	if got, ok := MaxOutputTokensListing("deepseek-v4-pro", 0); !ok || got != 384000 {
		t.Errorf("table: max_output_tokens=%d,%v want 384000,true", got, ok)
	}
	// 知识表条目但输出上限未知（kimi-k2.8-preview/auto）→ 省略。
	if got, ok := MaxOutputTokensListing("auto", 0); ok || got != 0 {
		t.Errorf("auto: max_output_tokens=%d,%v want 0,false (unknown → omit)", got, ok)
	}
	// 完全未知 → 省略。
	if _, ok := MaxOutputTokensListing("totally-unknown-model", 0); ok {
		t.Error("unknown model max_output_tokens should be omitted")
	}
}

// TestContextCatalogNoLegacy131072 回归锚点：131072 假兜底已退役——知识表任意条目
// 不得再以 131072 作为 context_length（除非真值恰为 128K，本表当前无此值）。
func TestContextCatalogNoLegacy131072(t *testing.T) {
	for model, cap := range contextCapFallback {
		if cap.context == 131072 {
			t.Errorf("%s: knowledge table context=131072 (legacy fake fallback leaked)", model)
		}
	}
}

// TestContextCatalogValuesPositive 表完整性：每条 context 必为正（0/负条目无意义，
// 会静默落到 1M 兜底）；maxOutput 为 0 语义是「未知省略」，不得为负。
func TestContextCatalogValuesPositive(t *testing.T) {
	for model, cap := range contextCapFallback {
		if cap.context <= 0 {
			t.Errorf("%s: context=%d must be positive", model, cap.context)
		}
		if cap.maxOutput < 0 {
			t.Errorf("%s: maxOutput=%d must be >=0", model, cap.maxOutput)
		}
	}
}

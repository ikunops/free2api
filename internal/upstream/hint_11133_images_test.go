package upstream

import "testing"

// TestHint11133ImageWithCatalogSupport 上游目录声明「支持图片」但仍 11133 时，
// hint 必须指向「换模型/换会话」，而不是让用户以为自己 body 写错了。
//
// 这条分支以前不存在：早期实现只在 supports_images=false 时才给具体指向，而实测
// deepseek-v4.1-flash 在 /v1/models 里 supports_images=true、1x1 极小图却稳定
// 11133（换 64px 正常图同一个号又能过）。结果是拿不到这条分支的人只看到中性文案
// 「check message format and model capabilities」，怎么改 body 都没用。
func TestHint11133ImageWithCatalogSupport(t *testing.T) {
	ctx := HintContext{
		Model:               "deepseek-v4.1-flash",
		HasImage:            true,
		ModelInCatalog:      true,
		ModelSupportsImages: true,
	}
	got := GatewayHint(ErrModelParamInvalid, `{"code":11133,"extError":{"code":"model_param_invalid"}}`, ctx)
	want := "image was rejected by this account's model backend even though the catalog marks it multimodal; retry with a different model or start a new conversation"
	if got != want {
		t.Errorf("hint=%q\n期望 %q", got, want)
	}
}

// TestHint11133ImageWithoutCatalogSupport 目录声明不支持图片 → 指向换多模态模型。
func TestHint11133ImageWithoutCatalogSupport(t *testing.T) {
	ctx := HintContext{Model: "deepseek-v4.1-flash", HasImage: true, ModelInCatalog: true, ModelSupportsImages: false}
	got := GatewayHint(ErrModelParamInvalid, `{"code":11133}`, ctx)
	want := "model deepseek-v4.1-flash does not support images; pick one with supports_images=true from /v1/models"
	if got != want {
		t.Errorf("hint=%q\n期望 %q", got, want)
	}
}

// TestHint11133ImageCatalogUnknown 目录没收录 → 不做能力判定，退中性文案（宁缺勿滥）。
func TestHint11133ImageCatalogUnknown(t *testing.T) {
	ctx := HintContext{Model: "mystery", HasImage: true, ModelInCatalog: false}
	got := GatewayHint(ErrModelParamInvalid, `{"code":11133}`, ctx)
	want := "request parameters were rejected by the model provider; check message format and model capabilities"
	if got != want {
		t.Errorf("hint=%q\n期望 %q", got, want)
	}
}

// TestHint11133NoImage 不带图 → 一律中性文案，不提模型能力。
func TestHint11133NoImage(t *testing.T) {
	for _, supports := range []bool{true, false} {
		ctx := HintContext{Model: "m", HasImage: false, ModelInCatalog: true, ModelSupportsImages: supports}
		got := GatewayHint(ErrModelParamInvalid, `{"code":11133}`, ctx)
		if got != "request parameters were rejected by the model provider; check message format and model capabilities" {
			t.Errorf("supportsImages=%v 时不该提模型能力，got %q", supports, got)
		}
	}
}
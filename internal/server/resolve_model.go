package server

import "strings"

// resolveModel 解析模型名协议（PLAN D6）：
//
//	[realm:][producer:]model      realm ∈ {cn, global}，producer ∈ {workbuddy, zcode, qoder}
//
// 两段前缀都可选、且可以只出现其一（顺序不限，最多各一段）。不认识的段一律
// 当裸名的一部分——「前缀必须是精确的小写枚举」这条大小写敏感规则不变，所以
// "GLM-4.6" 这种带大写的裸名不会被误剥。
//
// producer 前缀的存在理由：同一批模型名可能同时出现在两个客户端的上游目录里
// （比如 glm 系列），光看名字分不清该用谁的号。显式写 "zcode:glm-4.6" 就把
// 路由钉死在 zcode 号池上；不写则由 modelOwner 按「谁家目录里有」推断。
//
// bare 即出站/选号/账本使用的裸模型名。
func resolveModelRoute(model string) (realm, producer, bare string) {
	realm, producer, rest := "", "", model
	for i := 0; i < 2; i++ {
		idx := strings.IndexByte(rest, ':')
		if idx < 0 {
			break
		}
		head := rest[:idx]
		if realm == "" && isRealmPrefix(head) {
			realm = head
			rest = rest[idx+1:]
			continue
		}
		if producer == "" && isProducerPrefix(head) {
			producer = head
			rest = rest[idx+1:]
			continue
		}
		break
	}
	if realm == "" {
		realm = "cn"
	}
	return realm, producer, rest
}

// resolveModel 二值形态（realm + bare），保持既有调用方签名不变。
func resolveModel(model string) (realm, bare string) {
	r, _, b := resolveModelRoute(model)
	return r, b
}

func isRealmPrefix(s string) bool { return s == "cn" || s == "global" }

// isProducerPrefix 生产者前缀枚举。与 internal/source 的 Producer* 常量同值
// （两处同值由 server 侧测试锚定，见 resolve_model_test.go）。
func isProducerPrefix(s string) bool {
	return s == "workbuddy" || s == "zcode" || s == "qoder" || s == "opencode" || s == "kilo"
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// ResolveModelRoute 是 resolveModelRoute 的导出面（跨包调用，带 producer 维）。
func ResolveModelRoute(model string) (realm, producer, bare string) { return resolveModelRoute(model) }

// resolveModelPrefixed 在 resolveModel 之上再剥两层「输出侧」修饰：模型前缀 + 费率后缀。
//
// 两层都只出现在网关**对外**的模型名里（GET /v1/models 的 id）：
//   - 前缀来自 output.model_prefix，用途是让客户端一眼区分模型来自哪个网关；
//   - 后缀来自 output.rate_hint（RateSuffix），用途是让客户端看名字就知道费率。
//
// 入站请求带着它们回来时必须在 realm 解析与裸名比对之前剥干净，两端口径严格对称
// （拼装侧见 modelIDFor，剥除侧见 StripRateSuffix）。
func resolveModelPrefixed(model, prefix string) (realm, producer, bare string) {
	realm, producer, bare = resolveModelRoute(model)
	if prefix != "" {
		bare = strings.TrimPrefix(bare, prefix)
	}
	bare = StripRateSuffix(bare)
	return realm, producer, bare
}

// ResolveModelWithPrefix 是 resolveModelPrefixed 的导出面（cmd/server 的粘性闭包用）。
// 二值形态（realm + bare）：粘性闭包只按 realm 分池，producer 维度由 handler
// 在粘性命中后校验（见 handler.chatCompletions 的 producer 谓词）。
func ResolveModelWithPrefix(model, prefix string) (realm, bare string) {
	r, _, b := resolveModelPrefixed(model, prefix)
	return r, b
}

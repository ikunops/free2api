// 分池选号域：按 realm（cn/global）过滤选号与可用集合。realm=="" 退化为现状。
package pool

import (
	"time"

	"free2api/internal/auth"
)

// PickExcludingForRealm 按 realm 过滤的轮换选号：候选仅限 Realm()==realm 的账号。
// realm=="" 退化为 PickExcluding（现状语义，老调用零改动）。
// 可选做请求级轮换（tried）与模型感知（reqModel，6004 模型豁免照常生效）；
// reqModel 非空时健康口径换成 healthyForModel。realm 不匹配的全冷却兜底同样排除。
func (p *Pool) PickExcludingForRealm(tried map[string]bool, reqModel, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm, "", false)
}

// PickExcludingForProducerRealm 按 (producer, realm) 双维度过滤的轮换选号：
// 候选仅限「台账/凭证上的 producer 精确匹配」且 Realm()==realm 的账号。
// producer=="" / realm=="" 各自退化为不过滤该维度（老调用零改动）。
//
// 存在的理由：请求头/模型名可以指明这次要打哪个客户端的上游（"zcode:glm-4.6"）。
// 各家凭据不同构、上游也不同，跨客户端顶替不是「降级」而是「错」，必须在选号层
// 就分开——与 servableProducer（能不能接流量）正交：那个说「这家的上游通不通」，
// 这个说「这次请求要打哪一家」。
func (p *Pool) PickExcludingForProducerRealm(tried map[string]bool, reqModel, producer, realm string) *auth.Auth {
	return p.pick(tried, reqModel, realm, producer, false)
}

// PickExcludingForProducerRealmFree 同 PickExcludingForProducerRealm，额外声明 reqModel
// 是不是零积分模型：为真则本次选号走免费分支（跳过成本分层 + 不按余额加权，见 pick）。
// 存在的理由：免费/收费的判定要查上游目录（server 侧缓存），pool 自己看不到目录，
// 只能由调用方把结论传进来；用独立方法名而不是加参数，是为了让既有调用点零改动、
// 也让「这一次选号到底按不按余额加权」在调用处一眼可读（对话主路径用这个，
// 目录拉取等无模型名的路径继续用上面那个 → 恒 false，零回归）。
func (p *Pool) PickExcludingForProducerRealmFree(tried map[string]bool, reqModel, producer, realm string, freeModel bool) *auth.Auth {
	return p.pick(tried, reqModel, realm, producer, freeModel)
}

// PickForProducer 在某生产者的号池里选一个号（无 realm 谓词、无 tried）。
//
// DeptestOnly: 生产侧（server.fetchZCodeCatalog）需要多号重试，走的是
// PickExcludingForProducerRealm(tried, ...)；本方法是「该生产者有没有健康号」的
// 最小原语，保留作 pool 侧契约锚点（producer_test.go）与后续接新上游的入口。
func (p *Pool) PickForProducer(producer, reqModel string) *auth.Auth {
	return p.pick(nil, reqModel, "", producer, false)
}

// AvailableUIDsForRealm 同 AvailableUIDs，但仅返回 Realm()==realm 的账号。
// DeptestOnly: 仅 realm_test.go 引用；生产经 wiring.go 走
// AvailableUIDsForModelRealm。保留作 ForModelRealm 的模型维度退化
// （model=""）语义锚点测试。
// realm=="" 退化为 AvailableUIDs（现状语义）。
func (p *Pool) AvailableUIDsForRealm(realm string) []string {
	return p.availableUIDsLocked(realm, func(e *entry, now time.Time) bool { return e.healthy(now) })
}

// AvailableUIDsForModelRealm 同 AvailableUIDsForModel，但仅返回 Realm()==realm 的账号
// （6004 模型豁免照常生效）。realm=="" 退化为 AvailableUIDsForModel。
func (p *Pool) AvailableUIDsForModelRealm(model, realm string) []string {
	return p.availableUIDsLocked(realm,
		func(e *entry, now time.Time) bool { return e.healthyForModel(now, model) })
}

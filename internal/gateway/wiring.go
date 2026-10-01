package gateway

import (
	"free2api/internal/pool"
	"free2api/internal/server"
)

// RealmAwareAvailableForModel 构造会话粘性路由按模型可用口径的 realm 感知闭包。
//
// 粘性分配的模型名可能带 realm 前缀（"global:gpt-5.4" / "cn:glm-5.2"）：必须按前缀剥出
// realm + bareModel，再交给分池选号域过滤——否则裸名取池子全集，global 号会被粘性分配给
// CN 前缀请求（跨 realm 泄漏）。裸名/显式 cn → cn 集合；global: → global 集合。
//
// realm 为空串时 pool.AvailableUIDsForModelRealm 退化为现状（AvailableUIDsForModel），
// 老调用（无前缀模型名）语义零改动。
func RealmAwareAvailableForModel(p *pool.Pool, out *server.OutputStore) func(model string) []string {
	return func(model string) []string {
		// 前缀从 store 现读（不是启动时快照）：管理页改了前缀，粘性路由立刻同口径，
		// 否则会出现「/v1/models 列出带前缀的名字，粘性却按无前缀解析」的错位。
		prefix := ""
		if out != nil {
			prefix = out.Prefix()
		}
		realm, bare := server.ResolveModelWithPrefix(model, prefix)
		return p.AvailableUIDsForModelRealm(bare, realm)
	}
}

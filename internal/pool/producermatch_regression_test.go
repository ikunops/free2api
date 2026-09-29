package pool

import (
	"testing"

	"free2api/internal/auth"
)

// TestProducerMatchEmptyMeansWorkbuddy 裸名回落（请求 producer 为空）不得把 zcode 号放进候选。
//
// 回归背景（2026-09-29 实测，客户端侧表现为「模型已被标记为池级不可用」503）：
// workbuddy 独有的模型（如 deepseek-v4.1-flash、hy3）走裸名时，resolveRoute 依设计返回
// producer=""（注释口径：「空串 = CN 默认那家，与 workbuddy 同义」），而 producerMatch
// 当时对空串**直接放行**（旧注释写作「不过滤」）。于是 realm 同为 cn 的 zcode 号进入候选
// —— zcode 号的 realm 导入时写死为 cn —— 智谱凭据拿着 workbuddy 的模型名打上游，回
// 11102「该后端无此模型」；逐号封 6h 后轮转末态变 ErrModelBlocked，该模型被错标
// 「池级不可用」24h（`cn/workbuddy/deepseek-v4.1-flash` 的台账即为现场痕迹）。
//
// 契约：空串归一为 workbuddy —— 与 producerAllowed / deadKey / producerFor 第三级回落
// 同口径，四处必须一致，否则就是「池里算它 workbuddy、出站却打智谱」的口径裂缝。
func TestProducerMatchEmptyMeansWorkbuddy(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "wb1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	zc := &auth.Auth{UID: "zc1", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "key.secret"}
	zc.SetProducer("zcode") // 凭据自带 producer：单文件整体迁移也知道该打哪家上游
	p.Add(zc)
	producers := map[string]string{"wb1": "workbuddy", "zc1": "zcode"}
	p.SetProducerOf(func(uid string) string { return producers[uid] })

	// 单元层：空串 = workbuddy。zcode 号必须被挡，workbuddy 号必须放行。
	if p.producerMatch("zc1", "") {
		t.Fatal(`producerMatch("zc1", "") = true：空串被当成"不过滤"，zcode 号会进 workbuddy 模型的候选`)
	}
	if !p.producerMatch("wb1", "") {
		t.Fatal(`producerMatch("wb1", "") = false：空串应归一为 workbuddy（历史凭证零回归）`)
	}

	// 选号层：裸名回落（producer==""）连续 20 次都必须落在 workbuddy 号上。
	// 旧实现在此会以约一半概率选中 zc1，故 20 次足以稳定复现回归。
	for i := 0; i < 20; i++ {
		a := p.PickExcludingForProducerRealm(nil, "", "", "")
		if a == nil || a.UID != "wb1" {
			t.Fatalf("第 %d 次裸名回落选号 = %v, want 恒为 wb1（zcode 号不得进候选）", i, a)
		}
		p.NoteSuccess(a.UID)
	}

	// 显式指定 producer 时仍然严格分池（不能被归一化改动带偏）。
	if a := p.PickExcludingForProducerRealm(nil, "", "zcode", ""); a == nil || a.UID != "zc1" {
		t.Fatalf("显式 producer=zcode 选号 = %v, want zc1", a)
	}
	if a := p.PickExcludingForProducerRealm(nil, "", "workbuddy", ""); a == nil || a.UID != "wb1" {
		t.Fatalf("显式 producer=workbuddy 选号 = %v, want wb1", a)
	}
}

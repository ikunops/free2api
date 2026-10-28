package pool

import (
	"testing"

	"free2api/internal/auth"
)

// TestProducerRoutingFilter æçäº§èï¼AI å®¢æ·ç«¯ï¼è¿æ»¤éå·ï¼
// producer è°è¯ï¼producerMatchï¼åªç®¡ãæ¬æ¬¡è¯·æ±æå®æåªä¸å®¶ãï¼
// ä¸ servableProducerï¼è¿å®¶ä¸æ¸¸ææ²¡æåä»£å®ç°ï¼æ­£äº¤ã
// workbuddy ä¸ zcode é½å·²æ¥åä»£ â ä¸¤å®¶é½è¿å¯ç¨éï¼
// è¯·æ±æ¾å¼æå® producer æ¶ä¸¥æ ¼åæ± ï¼æ¿ zcode å­æ®æ workbuddy ä¸æ¸¸æ¯éï¼ä¸æ¯éçº§ï¼ã
func TestProducerRoutingFilter(t *testing.T) {
	withNoPickGap(t)
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	p := New("")
	p.Add(&auth.Auth{UID: "wb1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	zc := &auth.Auth{UID: "zc1", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "key.secret"}
	zc.SetProducer("zcode") // å­æ®èªå¸¦ producerï¼åæä»¶æ´ä½è¿ç§»ä¹ç¥éè¯¥æåªå®¶ä¸æ¸¸
	p.Add(zc)
	producers := map[string]string{"wb1": "workbuddy", "zc1": "zcode"}
	p.SetProducerOf(func(uid string) string { return producers[uid] })

	// ä¸¤å®¶é½ servable â å¯ç¨éä¸¤ä¸ªå·é½å¨ï¼ç²æ§è·¯ç±/æ¢æ´»ä¸éå·åå£å¾ï¼ã
	if got := p.AvailableUIDs(); len(got) != 2 {
		t.Fatalf("AvailableUIDs() = %v, want 2 ä¸ªï¼workbuddy + zcode é½ servableï¼", got)
	}
	// æå® producer æ¶ä¸¥æ ¼åæ± ï¼å zcode ç»ä¸è½å° workbuddy å·ä¸ï¼åä¹äº¦ç¶ã
	for i := 0; i < 20; i++ {
		if a := p.PickExcludingForProducerRealm(nil, "", "zcode", ""); a == nil || a.UID != "zc1" {
			t.Fatalf("ç¬¬ %d æ¬¡ zcode éå· = %v, want æä¸º zc1", i, a)
		} else {
			p.NoteSuccess(a.UID)
		}
		if a := p.PickExcludingForProducerRealm(nil, "", "workbuddy", ""); a == nil || a.UID != "wb1" {
			t.Fatalf("ç¬¬ %d æ¬¡ workbuddy éå· = %v, want æä¸º wb1", i, a)
		} else {
			p.NoteSuccess(a.UID)
		}
	}
	// æ± éç¡®å®æä¸¤ä¸ªå·ï¼è¿æ»¤åªä½ç¨äºéå·ï¼ä¸ä½åºè´¦å·ã
	if total, healthy, _, _, _ := p.CountsDetailed(); total != 2 || healthy != 2 {
		t.Errorf("CountsDetailed() = (total=%d healthy=%d), want (2,2)ï¼è¿æ»¤ä¸è¯¥æå·ï¼", total, healthy)
	}

	tot := p.ProducerTotals()
	if len(tot) != 2 {
		t.Fatalf("ProducerTotals() åç»æ° = %d, want 2ï¼workbuddy + zcodeï¼: %+v", len(tot), tot)
	}
	byKey := map[string]ProducerStat{}
	for _, s := range tot {
		byKey[s.Producer] = s
	}
	if wb := byKey["workbuddy"]; wb.Total != 1 || wb.Healthy != 1 || !wb.Servable {
		t.Errorf("workbuddy åç» = %+v, want total=1 healthy=1 servable=true", wb)
	}
	if zc := byKey["zcode"]; zc.Total != 1 || zc.Healthy != 1 || !zc.Servable {
		t.Errorf("zcode åç» = %+v, want total=1 healthy=1 servable=trueï¼zcode å·²æ¥åä»£ï¼", zc)
	}
}

// TestProducerPickForQoder qoder 已接反代：池里只有 qoder 号时 chat 能选到号，
// 且按 producer 严格分池。回归背景：接入 Qoder COSY 协议前此函数把 qoder 挡在
// 候选之外，接入后忘记放开 servableProducer 会让整条链路 503（"再接入一家上游
// 只改 servableProducer 一个函数"的契约点）。
func TestProducerPickForQoder(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	qd := &auth.Auth{UID: "qd1", Domain: "https://gateway.qoder.com.cn", AccessToken: "dt"}
	qd.SetProducer("qoder")
	p.Add(qd)

	if got := p.AvailableUIDs(); len(got) != 1 || got[0] != "qd1" {
		t.Fatalf("AvailableUIDs() = %v, want [qd1]（qoder 已 servable）", got)
	}
	if a := p.PickForProducer("qoder", ""); a == nil || a.UID != "qd1" {
		t.Fatalf("PickForProducer(qoder) = %v, want qd1（拉模型目录用）", a)
	}
	if a := p.PickExcludingForProducerRealm(nil, "gfmodel", "qoder", "cn"); a == nil || a.UID != "qd1" {
		t.Fatalf("PickExcludingForProducerRealm(qoder, cn) = %v, want qd1", a)
	}
	if !p.ServableNow() {
		t.Error("ServableNow() = false, want true（qoder 已 servable）")
	}
}

// TestProducerPickForZCode zcode æ¯ servable çäº§èï¼æ± éåªæ zcode å·æ¶ chat è½éå°å·ã
// åå½èæ¯ï¼éæå¯é¥åå­æ®ï¼ExpiresAt æ 0ï¼æ¾è¢« NeedsRefresh
// å¤ä¸ºãéå·æ°ãèæ´æ± è½®ç©º â /v1/chat/completions æ 503ã
func TestProducerPickForZCode(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	zc := &auth.Auth{UID: "zc1", Domain: "https://open.bigmodel.cn/api/paas/v4", AccessToken: "key.secret"}
	zc.SetProducer("zcode")
	p.Add(zc)

	if got := p.AvailableUIDs(); len(got) != 1 || got[0] != "zc1" {
		t.Fatalf("AvailableUIDs() = %v, want [zc1]", got)
	}
	if a := p.PickForProducer("zcode", ""); a == nil || a.UID != "zc1" {
		t.Fatalf("PickForProducer(zcode) = %v, want zc1ï¼æ zcode æ¨¡åç®å½ç¨ï¼", a)
	}
	// å¸¦æ¨¡å + realm + producer ä¸ç»´ç chat éå·è·¯å¾ï¼"cn:zcode:glm-4.6" çåé¨å½¢æï¼ã
	if a := p.PickExcludingForProducerRealm(nil, "glm-4.6", "zcode", "cn"); a == nil || a.UID != "zc1" {
		t.Fatalf("PickExcludingForProducerRealm(zcode, cn) = %v, want zc1", a)
	}
	if !p.ServableNow() {
		t.Error("ServableNow() = false, want trueï¼zcode å·² servableï¼")
	}
}

// TestProducerTotalsUnannotated 未注入解析器/台账无标注时回落到 workbuddy：
// 历史凭证（没有 .origins.json 标注）必须零回归地照常承接流量。
func TestProducerTotalsUnannotated(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "legacy1", Domain: "www.codebuddy.cn", AccessToken: "at"})

	tot := p.ProducerTotals()
	if len(tot) != 1 || tot[0].Producer != "workbuddy" || !tot[0].Servable {
		t.Fatalf("ProducerTotals() = %+v, want 单组 workbuddy servable=true", tot)
	}
	if a := p.Pick(""); a == nil || a.UID != "legacy1" {
		t.Fatalf("Pick() = %v, want legacy1（无标注按 workbuddy，照常可服务）", a)
	}
}

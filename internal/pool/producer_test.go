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

// TestProducerFilterUnservable è¿æ²¡æ¥åä»£çå®¢æ·ç«¯ï¼qoderï¼çå·ä¸è¿åéï¼
// å®å¯ /healthz æ¥ 503ãchat å 503ï¼ä¹ä¸è½æ¿å«å®¶å­æ®å»æ workbuddy ä¸æ¸¸ã
// è¿æ¯ãåæ¥ä¸å®¶ä¸æ¸¸åªæ¹ servableProducer ä¸ä¸ªå½æ°ãå¥çº¦çéå®ç¹ã
func TestProducerFilterUnservable(t *testing.T) {
	withNoPickGap(t)
	p := New("")
	p.Add(&auth.Auth{UID: "qd1", Domain: "www.codebuddy.cn", AccessToken: "at"})
	p.SetProducerOf(func(string) string { return "qoder" })

	if got := p.AvailableUIDs(); len(got) != 0 {
		t.Errorf("AvailableUIDs() = %v, want ç©ºï¼qoder æªæ¥åä»£ï¼ä¸æ¿æ¥æµéï¼", got)
	}
	if a := p.Pick(""); a != nil {
		t.Errorf("Pick() = %v, want nilï¼æ  servable çäº§èåéï¼", a)
	}
	if p.ServableNow() {
		t.Error("ServableNow() = true, want falseï¼åªæä¸å¯æå¡çå®¢æ·ç«¯ï¼")
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

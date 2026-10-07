// channels_test.go 多出口的契约测试：出口范围过滤、跨出口拦截、配置校验、
// 监听器生命周期（起/停/端口冲突）。
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// TestProducerAllowed 出口范围判定的三种形态：不设 = 全放；设了 = 只放列进去的；
// 裸名解析出的空 producer 与 workbuddy 同义（网关里 CN 默认那家）。
func TestProducerAllowed(t *testing.T) {
	all := NewHandler(Config{Pool: testPoolWith()})
	for _, p := range []string{"workbuddy", "zcode", "", "qoder"} {
		if !all.producerAllowed(p) {
			t.Fatalf("空名单应当全放，producer=%q 被判成不允许", p)
		}
	}
	zc := NewHandler(Config{Pool: testPoolWith(), ProducerAllow: []string{"zcode"}})
	if !zc.producerAllowed("zcode") {
		t.Fatal("zcode 专用口应当允许 zcode")
	}
	for _, p := range []string{"workbuddy", "", "qoder"} {
		if zc.producerAllowed(p) {
			t.Fatalf("zcode 专用口不该允许 producer=%q", p)
		}
	}
}

// TestChannelGuardRejectsCrossProducer 跨出口请求必须 404（不是放过再失败）：
// 只有一个 zcode 号池口时，拿 workbuddy 的模型名调它要被明确挡住。
func TestChannelGuardRejectsCrossProducer(t *testing.T) {
	h := NewHandler(Config{
		Pool:          testPoolWith(&auth.Auth{UID: "u1"}),
		ProducerAllow: []string{"zcode"},
	})
	body := `{"model":"cn:workbuddy:auto","messages":[]}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader([]byte(body))))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code=%d body=%s want 404", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "model_not_found")
}

// TestNormalizeChannels 出口配置校验：id 字符集、id/端口去重、格式白名单。
func TestNormalizeChannels(t *testing.T) {
	ok := []OutputChannel{
		{ID: "zcode", Listen: "127.0.0.1:7870"},
		{ID: "wb-2", Listen: ":7871", Format: FormatOpenAI, RateHint: RateFree},
	}
	out, err := normalizeChannels(ok)
	if err != nil {
		t.Fatalf("合法配置被拒：%v", err)
	}
	if len(out) != 2 || out[1].Listen != ":7871" || out[1].RateHint != RateFree {
		t.Fatalf("归一化结果 = %+v", out)
	}
	// id 大小写不敏感：统一归一成小写（面板也按小写提交），不算错。
	if up, err := normalizeChannels([]OutputChannel{{ID: "ZCode", Listen: ":7872"}}); err != nil || up[0].ID != "zcode" {
		t.Fatalf("大写 id 应当归一成小写：%+v err=%v", up, err)
	}

	bad := []struct {
		name string
		in   []OutputChannel
	}{
		{"空 id", []OutputChannel{{Listen: ":7870"}}},
		{"id 重复", []OutputChannel{{ID: "a", Listen: ":7870"}, {ID: "a", Listen: ":7871"}}},
		{"端口重复", []OutputChannel{{ID: "a", Listen: ":7870"}, {ID: "b", Listen: ":7870"}}},
		{"通配与回环撞同一口", []OutputChannel{{ID: "a", Listen: ":7870"}, {ID: "b", Listen: "127.0.0.1:7870"}}},
		{"端口非法", []OutputChannel{{ID: "a", Listen: ":99999"}}},
		{"格式不支持", []OutputChannel{{ID: "a", Listen: ":7870", Format: "anthropic"}}},
	}
	for _, c := range bad {
		if _, err := normalizeChannels(c.in); err == nil {
			t.Errorf("%s：应当报错但通过了", c.name)
		}
	}
}

// TestAdminOutputPutChannels 出口经 PUT /admin/output 整份替换：撞主口端口要挡，
// 合法配置要回显 channels / channel_status / channel_default。
func TestAdminOutputPutChannels(t *testing.T) {
	h := NewHandler(Config{
		Pool:         testPoolWith(),
		AdminEnabled: true,
		Output:       NewOutputStore(""),
		Listen:       "127.0.0.1:7864",
	})
	put := func(body string) (*httptest.ResponseRecorder, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("PUT", "/admin/output", bytes.NewReader([]byte(body))))
		var m map[string]any
		if rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
				t.Fatalf("decode: %v body=%s", err, rec.Body)
			}
		}
		return rec, m
	}

	rec, _ := put(`{"channels":[{"id":"zcode","listen":"127.0.0.1:7864"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("撞主口端口应当 400，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	assertJSONErrorCode(t, rec.Body.String(), "listen_conflict")

	rec, m := put(`{"channels":[{"id":"zcode","listen":"127.0.0.1:7869","producers":["zcode"]}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("合法出口应当 200，实际 code=%d body=%s", rec.Code, rec.Body)
	}
	if _, ok := m["channels"]; !ok {
		t.Fatalf("响应缺 channels：%v", m)
	}
	if _, ok := m["channel_default"]; !ok {
		t.Fatalf("响应缺 channel_default：%v", m)
	}
	got := h.outputCfg().Channels
	if len(got) != 1 || got[0].ID != "zcode" || len(got[0].Producers) != 1 {
		t.Fatalf("通道未落到配置：%+v", got)
	}
}

// TestChannelRunnerLifecycle 监听器随配置起停：加一个出口就多一个 listener，
// 删掉就释放端口（释放的判据是「同一地址可以再绑一次」）。
func TestChannelRunnerLifecycle(t *testing.T) {
	addr := freeAddr(t)
	base := Config{Pool: testPoolWith(), AdminEnabled: false}
	r := NewChannelRunner(base)
	defer r.Close()

	r.Reconcile(OutputConfig{Channels: []OutputChannel{{ID: "zcode", Listen: addr, Producers: []string{"zcode"}}}})
	sts := r.Status()
	if len(sts) != 1 || !sts[0].Running || sts[0].ID != "zcode" {
		t.Fatalf("通道没起来：%+v", sts)
	}
	if sts[0].BaseURL == "" || sts[0].Error != "" {
		t.Fatalf("通道状态不完整：%+v", sts[0])
	}
	// 配置里去掉这个通道 → 监听器关闭、端口释放。
	r.Reconcile(OutputConfig{})
	if sts := r.Status(); len(sts) != 0 {
		t.Fatalf("通道没停：%+v", sts)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("端口没释放：%v", err)
	}
	_ = ln.Close()
}

// TestChannelRunnerBindErrorReported 端口被占时只在状态里报错，不 panic、
// 不影响其他通道（面板据此显示为什么没起来）。
func TestChannelRunnerBindErrorReported(t *testing.T) {
	addr := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	r := NewChannelRunner(Config{Pool: testPoolWith()})
	defer r.Close()
	r.Reconcile(OutputConfig{Channels: []OutputChannel{{ID: "busy", Listen: addr}}})
	sts := r.Status()
	if len(sts) != 1 || sts[0].Running || sts[0].Error == "" {
		t.Fatalf("端口被占应当在状态里报错：%+v", sts)
	}
}

// freeAddr 取一个当前空闲的 127.0.0.1 端口（立刻关掉，交给调用方去绑）。
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

// TestChannelStatusEmptyIsArray 主口没有额外出口时 channel_status 必须是空数组
// 而不是 nil（JSON 里 null 会让面板渲染出错）。
func TestChannelStatusEmptyIsArray(t *testing.T) {
	h := NewHandler(Config{Pool: testPoolWith(), ChannelStatus: func() []ChannelStatus { return []ChannelStatus{} }})
	if got := h.channelStatus(); got == nil {
		t.Fatal("channelStatus 不应返回 nil（JSON 会变 null）")
	}
}

// seedCatalogs 直接铺两家的模型目录缓存（1h 缓存的命中路径，不发任何上游请求），
// 结束后还原——包级缓存是共享的，不能留给下一个测试。
func seedCatalogs(t *testing.T, wb, zc []upstream.ModelInfo) {
	t.Helper()
	dynamicModelsCache.Lock()
	oldWB, oldWBFetch, oldWBFail := dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail
	dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = wb, time.Now(), time.Time{}
	dynamicModelsCache.Unlock()
	zcodeCatalogCache.Lock()
	oldZC, oldZCFetch, oldZCFail := zcodeCatalogCache.infos, zcodeCatalogCache.fetched, zcodeCatalogCache.lastFail
	zcodeCatalogCache.infos, zcodeCatalogCache.fetched, zcodeCatalogCache.lastFail = zc, time.Now(), time.Time{}
	zcodeCatalogCache.Unlock()
	t.Cleanup(func() {
		dynamicModelsCache.Lock()
		dynamicModelsCache.ids, dynamicModelsCache.fetched, dynamicModelsCache.lastFail = oldWB, oldWBFetch, oldWBFail
		dynamicModelsCache.Unlock()
		zcodeCatalogCache.Lock()
		zcodeCatalogCache.infos, zcodeCatalogCache.fetched, zcodeCatalogCache.lastFail = oldZC, oldZCFetch, oldZCFail
		zcodeCatalogCache.Unlock()
	})
}

// TestSingleProducerResolvesAmbiguousBareName 裸名在两家的目录里都有（真歧义）时：
// 单来源出口按该来源落地（客户端只连这个口）；不设范围的口保持「歧义回落 workbuddy」
// 的老语义；显式前缀仍然钉死来源。
func TestSingleProducerResolvesAmbiguousBareName(t *testing.T) {
	seedCatalogs(t,
		[]upstream.ModelInfo{{ID: "glm-4.6"}, {ID: "auto"}},
		[]upstream.ModelInfo{{ID: "glm-4.6"}})

	zc := NewHandler(Config{Pool: testPoolWith(), ProducerAllow: []string{"zcode"}})
	if _, p, _ := zc.resolveRoute("glm-4.6"); p != upstream.ProducerZCode {
		t.Fatalf("zcode 专用口解析裸名 glm-4.6 应落 zcode，实际 %q", p)
	}
	// auto 只在 workbuddy 目录里 → 不是歧义，不该被“按唯一来源落地”的规则救回来。
	if _, p, _ := zc.resolveRoute("cn:auto"); p != "" {
		t.Fatalf("auto 不该被算成歧义：%q", p)
	}
	// 歧义 + 单来源：出口范围检查必须放行（而不是 404）。
	if _, p, _ := zc.resolveRoute("glm-4.6"); !zc.producerAllowed(p) {
		t.Fatalf("zcode 专用口应当放行歧义裸名 glm-4.6，实际 producer=%q", p)
	}

	all := NewHandler(Config{Pool: testPoolWith()})
	if _, p, _ := all.resolveRoute("glm-4.6"); p != "" {
		t.Fatalf("不设范围时歧义裸名应回落空（= workbuddy），实际 %q", p)
	}
	if _, p, _ := all.resolveRoute("zcode:glm-4.6"); p != upstream.ProducerZCode {
		t.Fatalf("显式前缀应钉死 zcode，实际 %q", p)
	}
}

// TestChannelStopKeepsStatusClean 主动停掉的通道不能把 net.ErrClosed 写回面板。
// 回归的是这个竞态：stop() 关掉 listener 后 Serve 立刻返回「use of closed network connection」，
// 异步 goroutine 抢在 delete(stat) 之后把这条当故障写回去，于是删掉/重建过的出口永远显示
// 「未运行 + 服务退出」，看着像出口起不来。
func TestChannelStopKeepsStatusClean(t *testing.T) {
	addr := freeAddr(t)
	r := NewChannelRunner(Config{Pool: testPoolWith()})
	defer r.Close()
	r.Reconcile(OutputConfig{Channels: []OutputChannel{{ID: "a", Listen: addr}}})
	if sts := r.Status(); len(sts) != 1 || !sts[0].Running {
		t.Fatalf("通道没起来：%+v", sts)
	}
	r.Reconcile(OutputConfig{})
	// 异步收工要一点时间才跑完，这段窗口正是以前会把状态写回来的地方。
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sts := r.Status(); len(sts) != 0 {
			t.Fatalf("删掉通道后状态又被写回：%+v", sts)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// TestChannelInheritsPrimaryOutput 出口留空 = 继承主口：主口改了格式/前缀/费率，
// 没单独覆盖过的出口要跟着重建并生效，不能停在启动时那一份。
func TestChannelInheritsPrimaryOutput(t *testing.T) {
	addr := freeAddr(t)
	store := NewOutputStore("")
	if _, err := store.Set(OutputConfig{Format: FormatOpenAI, RateHint: RateCredit}); err != nil {
		t.Fatalf("seed store: %v", err)
	}
	r := NewChannelRunner(Config{Pool: testPoolWith(), Output: store})
	defer r.Close()

	r.Reconcile(OutputConfig{Channels: []OutputChannel{{ID: "z", Listen: addr, Producers: []string{"zcode"}}}})
	sts := r.Status()
	if len(sts) != 1 || sts[0].Format != FormatOpenAI || sts[0].RateHint != RateCredit {
		t.Fatalf("出口应先继承主口当前值：%+v", sts)
	}

	// 主口改成 responses + free + 前缀 zc-；出口一个字没改，应当自动跟随。
	if _, err := store.Set(OutputConfig{Format: FormatResponses, ModelPrefix: "zc-", RateHint: RateFree}); err != nil {
		t.Fatalf("update store: %v", err)
	}
	// 注意：Reconcile 收的是「期望的通道清单」，store.Get() 里没有 Channels（主口配置不带通道），
	// 这里要显式带上，否则等于「把出口删光」。
	chans := []OutputChannel{{ID: "z", Listen: addr, Producers: []string{"zcode"}}}
	r.Reconcile(OutputConfig{Channels: chans})
	sts = r.Status()
	if len(sts) != 1 {
		t.Fatalf("主口改参数后出口不该消失：%+v", sts)
	}
	if sts[0].Format != FormatResponses || sts[0].RateHint != RateFree || sts[0].ModelPrefix != "zc-" {
		t.Fatalf("主口改了输出参数，继承的出口没跟上：%+v", sts[0])
	}

	// 出口显式覆盖格式后，主口再改不应波及它。
	if _, err := store.Set(OutputConfig{Format: FormatOpenAI, ModelPrefix: "zc-", RateHint: RateFree}); err != nil {
		t.Fatalf("update2: %v", err)
	}
	r.Reconcile(OutputConfig{Channels: []OutputChannel{{ID: "z", Listen: addr, Format: FormatResponses, Producers: []string{"zcode"}}}})
	if sts := r.Status(); sts[0].Format != FormatResponses {
		t.Fatalf("出口显式覆盖的格式被主口顶掉了：%+v", sts[0])
	}
}

// TestShouldReportServeExit 判定表：正常收工 / 我们主动停 / listener 被关都不算故障，
// 只有真异常才该写回面板。以前这条判据写成「Serve 返回的不是 ErrServerClosed 就报」，
// 多核下 listener 先关会让 Serve 返回 net.ErrClosed，于是删掉或重建过的出口在面板上
// 永远显示「未运行 + 服务退出：use of closed network connection」。
func TestShouldReportServeExit(t *testing.T) {
	closed := &net.OpError{Op: "accept", Net: "tcp", Err: net.ErrClosed}
	cases := []struct {
		name    string
		err     error
		stopped bool
		want    bool
	}{
		{"nil（正常收工）", nil, false, false},
		{"ErrServerClosed", http.ErrServerClosed, false, false},
		{"listener 已被关掉（net.ErrClosed）", closed, false, false},
		{"主动停之后不管拿到什么错误", errors.New("boom"), true, false},
		{"真正的意外退出", errors.New("boom"), false, true},
	}
	for _, c := range cases {
		if got := shouldReportServeExit(c.err, c.stopped); got != c.want {
			t.Errorf("%s：shouldReportServeExit(%v, stopped=%v) = %v，应为 %v",
				c.name, c.err, c.stopped, got, c.want)
		}
	}
}

// TestNormalizeOutputFoldsChannelsEqualToPrimary 出口字段与主口取值相同 = 当初并没有特意
// 覆盖它（早期配置会留一份冗余同值）。归一化时应折成空串，让「继承主口」真正成立；
// 不折的话主口改了格式/费率，出口会因为「显式值不相等」而不跟随。
func TestNormalizeOutputFoldsChannelsEqualToPrimary(t *testing.T) {
	in := OutputConfig{
		Format: FormatOpenAI, ModelPrefix: "zc-", RateHint: RateCredit,
		Channels: []OutputChannel{{
			ID: "zcode", Listen: "127.0.0.1:7870",
			Format: FormatOpenAI, ModelPrefix: "zc-", RateHint: RateCredit, Producers: []string{"zcode"},
		}},
	}
	out, err := normalizeOutput(in)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if ch := out.Channels[0]; ch.Format != "" || ch.ModelPrefix != "" || ch.RateHint != "" {
		t.Fatalf("与主口同值的字段应折成空（=继承）：%+v", ch)
	}

	// 显式不同于主口的值 = 真的要覆盖，必须原样保留。
	in2 := OutputConfig{Format: FormatOpenAI, RateHint: RateCredit,
		Channels: []OutputChannel{{ID: "z", Listen: "127.0.0.1:7871", Format: FormatResponses, RateHint: RateFree}}}
	out2, err := normalizeOutput(in2)
	if err != nil {
		t.Fatalf("normalize2: %v", err)
	}
	if ch := out2.Channels[0]; ch.Format != FormatResponses || ch.RateHint != RateFree {
		t.Fatalf("显式覆盖的值被误折：%+v", ch)
	}
}

// TestOutputStoreFoldsLegacyChannelsOnLoad 老配置（通道里写着与主口同值的 format/rate_hint）
// 在加载时会被归一化折成「继承」；既然内存里已经折过，磁盘上也顺手落一份干净形态，
// 免得下次重启又要再折一遍、也让排查的人看到文件与面板一致。
func TestOutputStoreFoldsLegacyChannelsOnLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "output.json")
	legacy := `{
  "format": "openai",
  "model_prefix": "",
  "rate_hint": "credit",
  "models": null,
  "channels": [
    {"id":"zcode","name":"ZCode 专用","listen":"127.0.0.1:7870","format":"openai","rate_hint":"credit","producers":["zcode"]}
  ]
}`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st := NewOutputStore(path)
	got := st.Get()
	if len(got.Channels) != 1 {
		t.Fatalf("通道数 = %d，应为 1", len(got.Channels))
	}
	if ch := got.Channels[0]; ch.Format != "" || ch.RateHint != "" {
		t.Fatalf("内存里应与主口同值的字段已折成空：%+v", ch)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var onDisk OutputConfig
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("unmarshal on-disk: %v", err)
	}
	if ch := onDisk.Channels[0]; ch.Format != "" || ch.RateHint != "" {
		t.Fatalf("磁盘上应已落成继承形态：%+v（原始 %s）", ch, string(raw))
	}
	if onDisk.Format != FormatOpenAI || onDisk.RateHint != RateCredit {
		t.Fatalf("主口字段不该被动：%+v", onDisk)
	}
	// 通道的身份字段不能被这次重写弄丢。
	if ch := onDisk.Channels[0]; ch.ID != "zcode" || ch.Listen != "127.0.0.1:7870" || len(ch.Producers) != 1 || ch.Producers[0] != "zcode" {
		t.Fatalf("重写丢了通道身份字段：%+v", ch)
	}
}

// TestChannelRunnerRetriesFailedBind 启动瞬间端口被占（旧进程没退、TIME_WAIT 等）时，
// 通道不能永久挂在「未运行」：后台重试循环要等端口一释放就自动补起。
// 这正是线上 7870/7871 重启后一直未运行、点一次保存才恢复的根因。
func TestChannelRunnerRetriesFailedBind(t *testing.T) {
	addr := freeAddr(t)
	blocker, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("blocker listen: %v", err)
	}

	// 50ms 重试间隔：把「等端口释放」压缩到单测可承受的时长。
	r := newChannelRunner(Config{Pool: testPoolWith()}, 50*time.Millisecond)
	defer r.Close()
	r.Reconcile(OutputConfig{Channels: []OutputChannel{{ID: "late", Listen: addr, Producers: []string{"zcode"}}}})
	if sts := r.Status(); len(sts) != 1 || sts[0].Running || sts[0].Error == "" {
		t.Fatalf("端口被占时应报未运行：%+v", sts)
	}

	// 旧进程退出 = 端口释放。此后不再动配置，只靠后台重试自愈。
	_ = blocker.Close()
	deadline := time.Now().Add(3 * time.Second)
	for {
		sts := r.Status()
		if len(sts) == 1 && sts[0].Running && sts[0].Error == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("端口释放后通道没有自动恢复：%+v", sts)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 恢复后的监听器必须真能服务：能建 TCP + 完成一次 HTTP 往返即可（404 也算活着）。
	cl := &http.Client{Timeout: 3 * time.Second}
	resp, err := cl.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("恢复后端口不可服务: %v", err)
	}
	_ = resp.Body.Close()
}

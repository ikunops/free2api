package session

import (
	"encoding/hex"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"free2api/internal/redisstore"
)

// countingStore 记录镜像调用次数的假 Store（不联网）。
type countingStore struct {
	redisstore.Noop
	mu       sync.Mutex
	setBinds int
	delBinds int
	binds    map[string]string
}

func newCountingStore() *countingStore {
	return &countingStore{binds: map[string]string{}}
}

func (c *countingStore) SetBind(key, uid string, ttl time.Duration) {
	c.mu.Lock()
	c.setBinds++
	c.binds[key] = uid
	c.mu.Unlock()
}
func (c *countingStore) DelBind(key string) {
	c.mu.Lock()
	c.delBinds++
	delete(c.binds, key)
	c.mu.Unlock()
}
func (c *countingStore) LoadBinds() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for k, v := range c.binds {
		out[k] = v
	}
	return out
}

func routerWith(store redisstore.Store, avail []string, ttl time.Duration) *Router {
	return routerWithGC(store, avail, ttl, 0) // GCInterval 0 → New 兜底默认（5m）
}

// routerWithGC 同 routerWith，但显式注入 GCInterval。GCInterval 必须在 StartGC
// **之前**注入：StartGC 之后写 r.cfg.GCInterval 会与 GC goroutine 里的
// time.NewTicker(r.cfg.GCInterval) 竞争（cfg 非并发安全），测试自身即犯规。
func routerWithGC(store redisstore.Store, avail []string, ttl, gcInterval time.Duration) *Router {
	return New(Config{
		TTL:        ttl,
		GCInterval: gcInterval,
		Store:      store,
		Available:  func() []string { return avail },
	})
}

func TestSameKeySameAccount(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1", "a2"}, time.Minute)
	u1, ok1 := r.Resolve("c1")
	u2, ok2 := r.Resolve("c1")
	if !ok1 || !ok2 || u1 != u2 {
		t.Fatalf("same key should map to same account: %s vs %s", u1, u2)
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1", r.Count())
	}
}

func TestTTLExpiryReassigns(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, 10*time.Millisecond)
	u1, _ := r.Resolve("c1")
	time.Sleep(20 * time.Millisecond)
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve after expiry should still succeed")
	}
	// 过期后可重新分配（可能巧合同号，但至少返回有效账号）。
	_ = u1
	_ = u2
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (reassigned, not duplicated)", r.Count())
	}
}

func TestBoundAccountCooldownReassigns(t *testing.T) {
	r := routerWith(newCountingStore(), []string{"a1"}, time.Minute)
	u1, _ := r.Resolve("c1")
	if u1 != "a1" {
		t.Fatalf("initial bind=%s want a1", u1)
	}
	// a1 冷却 → 可用列表只剩 a2 → 重新分配必须换到 a2。
	r.cfg.Available = func() []string { return []string{"a2"} }
	u2, ok := r.Resolve("c1")
	if !ok {
		t.Fatal("resolve should succeed with fallback account")
	}
	if u2 == u1 {
		t.Fatalf("bound account %s cooled but still assigned", u1)
	}
	if u2 != "a2" {
		t.Fatalf("reassigned to %s want a2", u2)
	}
}

func TestNoSessionKeyPassthrough(t *testing.T) {
	// ExtractKey 找不到任何会话键 → 空串（调用方据空串走普通 Pick；router 不会被调用）。
	got := ExtractKey([]byte(`{"model":"x","messages":[]}`))
	if got != "" {
		t.Errorf("ExtractKey should return empty, got %q", got)
	}
}

func TestExtractKeyPriority(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"metadata":{"conversation_id":"mc","user_id":"mu"},"conversation_id":"top"}`, "mc"}, // metadata.conversation_id 优先
		{`{"conversation_id":"top"}`, "top"},                                                   // 顶层 conversation_id
		// P1-anti-monopoly：user_id 不再是粘性键（对话级粒度修正，键序四键全 conversation 维度）。
		{`{"metadata":{"user_id":"mu"}}`, ""},    // user_id 单独出现 → 空（不生成粘性）
		{`{"user_id":"mu"}}`, ""},                // 顶层 user_id 从未支持，保持空
		{`{"metadata":{"conversation_id":123}}`, ""}, // 非字符串 → 空
		{`not-json`, ""}, // 非法 JSON → 空
		// issue #35：客户端实际发 camelCase conversationId，ExtractKey 必须识别。
		{`{"conversationId":"abc"}`, "abc"},                                            // 顶层 camelCase
		{`{"metadata":{"conversationId":"abc"}}`, "abc"},                               // metadata.camelCase
		{`{"metadata":{"conversation_id":"snake","conversationId":"camel"}}`, "snake"}, // snake 优先于 camel
		{`{"conversation_id":"snake","conversationId":"camel"}`, "snake"},              // 顶层 snake 优先于 camel
		{`{"conversationId":123}`, ""},                                                 // 数字 conversationId → 空
		{`{"metadata":{"conversationId":456}}`, ""},                                    // metadata 数字 conversationId → 空
		{`{"metadata":{"conversationId":"abc","user_id":"mu"}}`, "abc"},                // camel conversationId 生效（user_id 不再抢占）
		// 剔除前 user_id 抢占顶层 conversation_id（session.go 旧键序 3 在 4 之前）；
		// 剔除后顶层 conversation_id 正常生效。
		{`{"metadata":{"user_id":"mu"},"conversation_id":"top"}`, "top"},
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

// TestUserIdNoLongerSticky P1-anti-monopoly：user_id 不再生成粘性——只发
// metadata.user_id 的客户端 ExtractKey 返回空（无粘性键），handler 侧 gate
// （sessKey != ""）不成立，Router 不会被咨询，同一 user 的并行对话不再钉同一
// 账号（回落加权轮换）。键序断言由 TestExtractKeyPriority 覆盖（user_id → ""）。
func TestUserIdNoLongerSticky(t *testing.T) {
	bodies := []string{
		`{"model":"m","metadata":{"user_id":"u-42"},"messages":[]}`,
		`{"model":"m","metadata":{"user_id":"u-42"},"conversation_id":"c1"}`, // user_id 不再抢占顶层键
	}
	for _, body := range bodies {
		if key := ExtractKey([]byte(body)); key == "" && strings.Contains(body, `"conversation_id":"c1"`) {
			// 第二条应取 conversation_id（非空）——防御本测试自身的构造错误。
			t.Fatalf("构造错误：含 conversation_id 的 body 不应返回空: %s", body)
		}
	}
	// 主断言：只发 user_id 的 body 无粘性键。
	if key := ExtractKey([]byte(bodies[0])); key != "" {
		t.Fatalf("user_id 不应再生成粘性键, got %q", key)
	}
	// conversation 变体不受影响：四种键形态照常提取。
	for _, body := range []string{
		`{"conversation_id":"c1"}`,
		`{"conversationId":"c1"}`,
		`{"metadata":{"conversation_id":"c1"}}`,
		`{"metadata":{"conversationId":"c1"}}`,
	} {
		if key := ExtractKey([]byte(body)); key != "c1" {
			t.Errorf("conversation 变体应照常提取: %s got %q", body, key)
		}
	}
}

// TestExtractKeyPromptCacheKey 第 5 键 prompt_cache_key：pi-ai 驱动的客户端
// （dsh 等）把会话 ID 放在该 OpenAI 前缀缓存字段里而非 conversation_id。
// 必须置于最后：conversation 维度四键任一在场时，prompt_cache_key 不得抢占。
func TestExtractKeyPromptCacheKey(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		// 只有 prompt_cache_key → 取它（本键存在的唯一目的）。
		{`{"prompt_cache_key":"pck-1"}`, "pck-1"},
		// 与 conversation 维度并存 → conversation 优先，prompt_cache_key 绝不抢占。
		{`{"prompt_cache_key":"pck-1","conversation_id":"conv"}`, "conv"},
		{`{"prompt_cache_key":"pck-1","conversationId":"conv"}`, "conv"},
		{`{"prompt_cache_key":"pck-1","metadata":{"conversation_id":"mc"}}`, "mc"},
		{`{"prompt_cache_key":"pck-1","metadata":{"conversationId":"mc"}}`, "mc"},
		// 非字符串 / 空串 → 不生成键（strOrEmpty 口径）。
		{`{"prompt_cache_key":123}`, ""},
		{`{"prompt_cache_key":""}`, ""},
	}
	for _, c := range cases {
		if got := ExtractKey([]byte(c.body)); got != c.want {
			t.Errorf("ExtractKey(%s)=%q want %q", c.body, got, c.want)
		}
	}
}

// TestStickyFallbackKeyStable 同一条首条 user 消息 → 恒同键（会话级稳定）。
// 这是"不动客户端也能命中粘性"的核心契约：会话内历史增长不影响本函数结果。
func TestStickyFallbackKeyStable(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":"首条消息"}]}`
	a := StickyFallbackKey([]byte(body))
	b := StickyFallbackKey([]byte(body))
	if a == "" {
		t.Fatal("StickyFallbackKey 不应为空（有 user 消息）")
	}
	if a != b {
		t.Fatalf("同 body 应派生同键: %q vs %q", a, b)
	}
	// 形态：前缀 fb: + 32 hex（与 ids.go 的 16 字节 hex 约定一致）。
	if !strings.HasPrefix(a, "fb:") || len(a) != len("fb:")+32 {
		t.Fatalf("键形态错误: %q", a)
	}
	if _, err := hex.DecodeString(a[len("fb:"):]); err != nil {
		t.Fatalf("键体应为 hex: %q (%v)", a, err)
	}
}

// TestStickyFallbackKeyIgnoresLaterHistory 追加历史后键不变——证明取的是
// **首条**（会话级）而非**最后一条**（轮级，见 TurnKey）。若误取最后一条，
// agent 每轮 tool result 回填都会换键 → 每轮换号，粘性名存实亡。
func TestStickyFallbackKeyIgnoresLaterHistory(t *testing.T) {
	first := `{"model":"m","messages":[{"role":"user","content":"开场白"}]}`
	longer := `{"model":"m","messages":[` +
		`{"role":"user","content":"开场白"},` +
		`{"role":"assistant","content":"好的"},` +
		`{"role":"user","content":"继续"},` +
		`{"role":"assistant","content":"收到"},` +
		`{"role":"user","content":"再继续"}]}`
	if a, b := StickyFallbackKey([]byte(first)), StickyFallbackKey([]byte(longer)); a != b {
		t.Fatalf("会话推进不应换键（应取首条 user）: %q vs %q", a, b)
	}
}

// TestStickyFallbackKeyDiffersByFirstMessage 不同首条消息 → 不同键（新会话不混绑）。
func TestStickyFallbackKeyDiffersByFirstMessage(t *testing.T) {
	a := StickyFallbackKey([]byte(`{"messages":[{"role":"user","content":"会话甲"}]}`))
	b := StickyFallbackKey([]byte(`{"messages":[{"role":"user","content":"会话乙"}]}`))
	if a == "" || b == "" {
		t.Fatal("两条都应派生非空键")
	}
	if a == b {
		t.Fatalf("不同首条消息应得不同键，got %q", a)
	}
}

// TestStickyFallbackKeyEmptyCases 无法派生时返回空串——调用方据此回落
// 「无粘性」旧行为（不伪造会话，避免脏绑定）。
func TestStickyFallbackKeyEmptyCases(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"空 body", ``},
		{"无 messages", `{"model":"m"}`},
		{"messages 为空", `{"messages":[]}`},
		{"无 user 消息", `{"messages":[{"role":"system","content":"sys"},{"role":"assistant","content":"a"}]}`},
		{"首条 user 无文本", `{"messages":[{"role":"user","content":null}]}`},
		{"首条 user 空串", `{"messages":[{"role":"user","content":"   "}]}`},
		{"非法 JSON", `not-json`},
	}
	for _, c := range cases {
		if got := StickyFallbackKey([]byte(c.body)); got != "" {
			t.Errorf("%s: 应返回空串, got %q", c.name, got)
		}
	}
}

// TestStickyFallbackKeyMultimodal 多模态 content 数组：拼接各 part 的 text
// （复用 ids.go contentText 口径），有文本即派生键。
func TestStickyFallbackKeyMultimodal(t *testing.T) {
	body := `{"messages":[{"role":"user","content":[{"type":"text","text":"图片说明文字"}]}]}`
	if got := StickyFallbackKey([]byte(body)); got == "" {
		t.Fatal("多模态文本部分应能派生键")
	}
}

// TestStickyFallbackKeyTrimsWhitespace 首尾空白归一：'hi' 与 '  hi  ' 同键。
func TestStickyFallbackKeyTrimsWhitespace(t *testing.T) {
	a := StickyFallbackKey([]byte(`{"messages":[{"role":"user","content":"hi"}]}`))
	b := StickyFallbackKey([]byte(`{"messages":[{"role":"user","content":"  hi  "}]}`))
	if a != b {
		t.Fatalf("首尾空白应归一为同键: %q vs %q", a, b)
	}
}

// TestStickyFallbackKeySuppressedByUserID P1-anti-monopoly 契约在 fallback 路径的
// 延伸：携带 user_id 的请求恒不派生键。
//
// 为什么关键：ExtractKey 有意剔除 user_id 作粘性键（user 维度粒度过粗，一个 user
// 的并行对话会被钉到同一账号）。若 fallback 不设闸，只发 user_id 的请求会借首条
// prompt 重新获得粘性，使该契约在 handler 侧失效——正是 Copilot review 指出的回归。
// 注意本测试与 TestUserIdNoLongerSticky 互补：后者只覆盖 ExtractKey 返回空，
// 覆盖不到 handler 侧其后调用的 StickyFallbackKey。
func TestStickyFallbackKeySuppressedByUserID(t *testing.T) {
	bodies := []struct {
		name string
		body string
	}{
		{"metadata.user_id + 文本 user 消息",
			`{"model":"m","metadata":{"user_id":"u-42"},"messages":[{"role":"user","content":"首条消息"}]}`},
		{"顶层 user_id + 文本 user 消息",
			`{"model":"m","user_id":"u-42","messages":[{"role":"user","content":"首条消息"}]}`},
		{"metadata.user_id + prompt_cache_key 也没有",
			`{"metadata":{"user_id":"u-42"},"messages":[{"role":"user","content":"x"}]}`},
		// 非字符串 / 空串 user_id 不算标识（strOrEmpty 口径）。
		{"user_id 非字符串不抑制",
			`{"metadata":{"user_id":123},"messages":[{"role":"user","content":"首条消息"}]}`},
		{"user_id 空串不抑制",
			`{"metadata":{"user_id":""},"messages":[{"role":"user","content":"首条消息"}]}`},
	}
	for _, c := range bodies {
		got := StickyFallbackKey([]byte(c.body))
		switch c.name {
		case "user_id 非字符串不抑制", "user_id 空串不抑制":
			if got == "" {
				t.Errorf("%s: 非字符串/空 user_id 不应抑制 fallback", c.name)
			}
		default:
			if got != "" {
				t.Errorf("%s: 带 user_id 应返回空串（回落加权轮换）, got %q", c.name, got)
			}
		}
	}
}

// TestStickyFallbackKeyNoUserIDStillWorks 无 user_id 的请求不受抑制影响——
// 确保上面的闸门没有误伤真正需要 fallback 的客户端（dsh / Codex 等）。
func TestStickyFallbackKeyNoUserIDStillWorks(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"首条消息"}]}`,
		`{"model":"m","metadata":{"conversation":"c"},"messages":[{"role":"user","content":"首条消息"}]}`,
	}
	for _, body := range bodies {
		if got := StickyFallbackKey([]byte(body)); got == "" {
			t.Errorf("无 user_id 应正常派生键: %s", body)
		}
	}
}

func TestConcurrentSameKeyAssignsOnce(t *testing.T) {
	avail := []string{"a1", "a2", "a3", "a4", "a5"}
	r := routerWith(newCountingStore(), avail, time.Minute)

	const N = 100
	uids := make([]string, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			u, ok := r.Resolve("same-key")
			if ok {
				uids[idx] = u
			}
		}(i)
	}
	wg.Wait()

	// 所有 goroutine 必须拿到同一个账号（写锁 re-check 防重复分配）。
	first := ""
	for _, u := range uids {
		if u == "" {
			t.Fatal("some goroutine failed to resolve")
		}
		if first == "" {
			first = u
		}
		if u != first {
			t.Fatalf("concurrent resolve assigned different accounts: %s vs %s", first, u)
		}
	}
	if r.Count() != 1 {
		t.Errorf("count=%d want 1 (single binding)", r.Count())
	}
}

// boundUID 直接读绑定 uid（不触发 Resolve 的重分配），供 Bind 系列测试断言用（包内私有 helper）。
func (r *Router) boundUID(key string) (string, bool) {
	r.mu.RLock()
	e, ok := r.entries[key]
	r.mu.RUnlock()
	return e.uid, ok
}

func TestBindOverridesAndMirrors(t *testing.T) {
	// Bind 幂等覆盖旧值，并异步镜像 SetBind。
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Bind("c1", "a1")
	if u, ok := r.boundUID("c1"); !ok || u != "a1" {
		t.Fatalf("bind c1->a1 then bound=%s ok=%v", u, ok)
	}
	// 覆盖到 a2
	r.Bind("c1", "a2")
	if u, _ := r.boundUID("c1"); u != "a2" {
		t.Fatalf("bind override should map c1->a2, got %s", u)
	}
	if r.Count() != 1 {
		t.Errorf("bind override must not duplicate entries, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	binds := map[string]string{}
	for k, v := range st.binds {
		binds[k] = v
	}
	st.mu.Unlock()
	if n != 2 {
		t.Errorf("SetBind mirror count=%d want 2", n)
	}
	if binds["c1"] != "a2" {
		t.Errorf("mirrored bind should be a2, got %s", binds["c1"])
	}
}

func TestBindIgnoresEmptyKey(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("", "a1")
	r.Bind("c1", "")
	if r.Count() != 0 {
		t.Errorf("Bind with empty key/uid must be no-op, count=%d", r.Count())
	}
	st.mu.Lock()
	n := st.setBinds
	st.mu.Unlock()
	if n != 0 {
		t.Errorf("empty-key Bind must not mirror, setBinds=%d", n)
	}
}

func TestBindThenUnbindLifecycle(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, time.Minute)
	r.Bind("c1", "a1")
	if !r.Unbind("c1") {
		t.Fatal("Unbind should report found")
	}
	if r.Count() != 0 {
		t.Errorf("count after unbind=%d want 0", r.Count())
	}
	st.mu.Lock()
	del := st.delBinds
	st.mu.Unlock()
	if del != 1 {
		t.Errorf("DelBind mirror count=%d want 1", del)
	}
}

func TestRedisMirrorSetBindCount(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.Resolve("c1")
	r.Resolve("c1") // 快路径 touch → 又镜像一次
	if st.setBinds < 1 {
		t.Errorf("SetBind mirror count=%d want >=1", st.setBinds)
	}
	r.Unbind("c1")
	if st.delBinds != 1 {
		t.Errorf("DelBind mirror count=%d want 1", st.delBinds)
	}
}

func TestGCCleansExpired(t *testing.T) {
	st := newCountingStore()
	r := routerWith(st, []string{"a1"}, 10*time.Millisecond)
	r.Resolve("c1")
	r.Resolve("c2")
	time.Sleep(20 * time.Millisecond)
	removed := r.gcOnce(time.Now())
	if removed != 2 {
		t.Errorf("gc removed=%d want 2", removed)
	}
	if r.Count() != 0 {
		t.Errorf("count after gc=%d want 0", r.Count())
	}
}

func TestLoadFromStoreRestores(t *testing.T) {
	st := newCountingStore()
	st.binds["c1"] = "a1"
	st.binds["c2"] = "a2"
	r := routerWith(st, []string{"a1", "a2"}, time.Minute)
	r.LoadFromStore()
	if r.Count() != 2 {
		t.Fatalf("restored count=%d want 2", r.Count())
	}
	u, ok := r.Resolve("c1")
	if !ok || u != "a1" {
		t.Errorf("restored c1 -> %s want a1", u)
	}
}

// TestStopGCStopsGoroutine 关停语义回归：StopGC 后后台 GC goroutine 必须真的退出
// （N 轮「StartGC → StopGC」不泄漏 goroutine）。
//
// 原实现三处缺陷同时存在：① goroutine 在 select 里**每轮无锁重读** r.stop，
// 而 StopGC 持写锁把它置 nil —— 数据竞争（-race 报 Write@StopGC vs Read@StartGC.func1）；
// ② 一旦读到 nil，case <-r.stop 变成「select 里的 nil channel」永不就绪，关停信号
// 彻底丢失，goroutine 只能靠 ticker 无限空转；③ 于是 StopGC 之后 gcOnce 仍在跑，
// 每次 Start/Stop 循环净泄漏一个 goroutine。修复把 stop channel 捕获进局部变量，
// 让 close 与 select 观测同一个 channel。
//
// GCInterval 必须**构造时**注入（不能 StartGC 后再写 r.cfg）：goroutine 会读
// r.cfg.GCInterval 起 ticker，启动后写 cfg 本身就是数据竞争。
func TestStopGCStopsGoroutine(t *testing.T) {
	const rounds = 20
	baseline := runtime.NumGoroutine()

	for i := 0; i < rounds; i++ {
		r := routerWithGC(newCountingStore(), []string{"a1"}, time.Minute, time.Millisecond)
		r.StartGC()
		// 1ms tick：让 ticker 在 goroutine 退出前至少触发数轮，覆盖
		// 「select 重新求值 r.stop」的窗口（原缺陷的触发路径）。
		time.Sleep(2 * time.Millisecond)
		r.StopGC()
	}

	// 轮询等待所有 GC goroutine 退出（有界，防挂死）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Errorf("StopGC 后 goroutine 未退出（泄漏）: baseline=%d now=%d rounds=%d",
		baseline, runtime.NumGoroutine(), rounds)
}

// TestStartGCIdempotentAndRestartable StartGC 幂等、StopGC 后可重启：
// 重复 StartGC 不叠加 goroutine，StopGC 后再 StartGC 仍能恢复 GC
// （原实现 StopGC 把 r.stop 置 nil，goroutine 卡在 nil channel 上永不退出；
// 修复后 close 能被观测到，重启也能重新拉起一个可关停的 GC）。
func TestStartGCIdempotentAndRestartable(t *testing.T) {
	r := routerWithGC(newCountingStore(), []string{"a1"}, 10*time.Millisecond, 5*time.Millisecond)
	baseline := runtime.NumGoroutine()

	r.StartGC()
	r.StartGC() // 幂等：不应起第二个
	r.StartGC()
	time.Sleep(20 * time.Millisecond)
	if n := runtime.NumGoroutine(); n > baseline+2 {
		t.Errorf("重复 StartGC 叠加了 goroutine: baseline=%d now=%d", baseline, n)
	}

	r.StopGC()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > baseline+1 {
		time.Sleep(5 * time.Millisecond)
	}

	// 重启：StopGC 已把 r.stop 置 nil，StartGC 应能重新拉起可用的 GC。
	// TTL=10ms、GCInterval=5ms：绑定建立后必然被重启的 GC 清掉。
	r.Resolve("c1")
	r.StartGC()
	r.Resolve("c2")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && r.Count() != 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := r.Count(); got != 0 {
		t.Errorf("重启后 GC 未生效: count=%d want 0", got)
	}
	r.StopGC()
}

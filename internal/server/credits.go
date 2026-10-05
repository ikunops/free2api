// credits.go 「积分」只读查询端点：把每个账号的余额（含快过期子集）从上游直查回来。
//
// 为什么不直接用 /status 里的 credits 字段：池里的 credits 只在**每日签到**时写入
// （scheduler 的 checkin 任务调 SetCreditsDetailed）。新装的网关、或刚导完号还没到
// 签到点的实例，看到的余额全是 0——运维会误判成「账号没余额」，而这恰恰是最需要
// 看准的数字。所以这里按需直查上游 billing 接口（与 cmd/credit 同一口径），
// 并做 10 分钟缓存：余额是分钟级变化的展示量，没必要每次开页面都打一遍所有账号。
//
// 两条互补的路径，谁都不替代谁：
//   - 每日签到写池内 credits（参与选号权重、快过期优先消耗，是**运行态**）；
//   - /admin/credits 按需直查（是**观测态**，随时可看，不影响选号）。
//
// zcode 号另走一条链路：它没有 billing 积分，只有套餐权益（plans/balances），
// 而且要一对专门的凭据（zcodejwttoken + device mid）。这对凭据的解析见
// resolveZCodeCreds——号池文件优先，zcode 账本其次，应用登录态兜底。
package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"free2api/internal/auth"
	"free2api/internal/pool"
	"free2api/internal/source"
)

// creditsTTL 余额缓存有效期。手动「刷新余额」不受它限制（强制重查）。
const creditsTTL = 10 * time.Minute

// creditRow 单账号余额行（脱敏：只给 uid/昵称/域 + 数值，不含任何凭证）。
type creditRow struct {
	UID      string `json:"uid"`
	Nickname string `json:"nickname,omitempty"`
	Realm    string `json:"realm,omitempty"`
	// Producer 这行属于哪家客户端（workbuddy / zcode）。输出侧的发布清单要靠它
	// 把 zcode 号按套餐额度收敛模型（见 zcodeEntitledModels）：上游 /models 是整家
	// 厂商的目录，不等于这些号能跑什么。
	Producer string `json:"producer,omitempty"`
	// RealmStored 凭证文件里持久化的域（cn/global）；与 Realm 不同时说明该号被
	// 「逃生门」（config global.enabled=false）强制按 CN 路由——这时的上游失败
	// 不是余额问题，是路由/网络问题，必须让运维一眼看出来，故单独透出。
	RealmStored string `json:"realm_stored,omitempty"`
	// Remain 当前可花余额（上游权威），Expiring 其中在快过期窗口内、不用就作废的部分。
	Remain   int64 `json:"remain"`
	Expiring int64 `json:"expiring"`
	// Total 套餐总量 / Used 已用量 / Packages 套餐数；上游不给就省略（不编造 0）。
	Total    *int64 `json:"total,omitempty"`
	Used     *int64 `json:"used,omitempty"`
	Packages int    `json:"packages,omitempty"`
	// Unit 余额单位："token" = 按 token 计（zcode 套餐按 token 发），空 = 积分。
	// 300000000 当积分报会看错三个数量级，前端据此换单位显示。
	Unit string `json:"unit,omitempty"`
	// ExpiresAt 套餐/权益到期（Unix 秒）。与号池 auth 文件的 token 到期是两回事：
	// zcode 号没有 refresh token，能回答"用到什么时候"的只有套餐有效期。
	ExpiresAt int64 `json:"expires_at,omitempty"`
	// EntitledModels 该账号**当前真正能用**的模型。workbuddy 侧由上游目录决定（不重复
	// 塞进这一列），zcode 侧由套餐 capabilities 决定——这正是"账号自带模型"的真身。
	EntitledModels []string `json:"entitled_models,omitempty"`
	// PlanSummary 套餐人话名（如"ZCode 周末活动"），没有就留空。
	PlanSummary string `json:"plan_summary,omitempty"`
	// Note 上游答了但没给可用范围时的说明（宁可不报数，也不编造 0）。
	Note string `json:"note,omitempty"`
	// CredSource 读这份额度用的凭据是**哪一层**来的：号池文件 / zcode 账本 /
	// ZCode 应用登录态。运维要能一眼分清「这是这个号自己的数」还是「兜底来的数」。
	CredSource string `json:"cred_source,omitempty"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
}

// creditCache 余额查询缓存 + 单飞（同一时刻只允许一个刷新在跑，防抖动点爆上游）。
type creditCache struct {
	mu       sync.Mutex
	ts       time.Time
	rows     []creditRow
	inflight bool
}

// adminCredits 返回余额快照。缓存过期就在后台起一次刷新并**立即返回旧值**
// （refreshing=true）——不让管理页等上游：8 个账号里只要有一个跨域超时，
// 同步等下去就是十几秒的白屏。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	rows, ts, inflight := h.creditsSnapshot(false)
	writeJSON(w, http.StatusOK, map[string]any{
		"ts":         time.Now().Unix(),
		"fetched_at": unixOrZero(ts),
		"refreshing": inflight,
		"stale":      ts.IsZero() || time.Since(ts) > creditsTTL,
		"accounts":   rows,
		"ttl_sec":    int(creditsTTL.Seconds()),
	})
}

// adminCreditsRefresh 强制重查（管理页「刷新余额」按钮）。同样不阻塞：
// 起后台任务，本次响应立刻返回，结果由前端下一次轮询取到。
func (h *Handler) adminCreditsRefresh(w http.ResponseWriter, r *http.Request) {
	rows, ts, inflight := h.creditsSnapshot(true)
	writeJSON(w, http.StatusOK, map[string]any{
		"ts":         time.Now().Unix(),
		"fetched_at": unixOrZero(ts),
		"refreshing": inflight,
		"stale":      true,
		"accounts":   rows,
		"ttl_sec":    int(creditsTTL.Seconds()),
	})
}

// creditsSnapshot 取缓存；需要刷新时在后台起一次（单飞）。返回缓存副本 + 刷新时间 + 是否在刷。
func (h *Handler) creditsSnapshot(force bool) ([]creditRow, time.Time, bool) {
	c := &h.credits
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := !c.ts.IsZero() && time.Since(c.ts) < creditsTTL
	if (force || !fresh) && !c.inflight {
		c.inflight = true
		go h.refreshCredits()
	}
	rows := make([]creditRow, len(c.rows))
	copy(rows, c.rows)
	return rows, c.ts, c.inflight
}

// refreshCredits 后台刷新一次并写回缓存。
func (h *Handler) refreshCredits() {
	rows := h.collectCredits()
	h.credits.mu.Lock()
	h.credits.rows = rows
	h.credits.ts = time.Now()
	h.credits.inflight = false
	h.credits.mu.Unlock()
	storeZCodeEntitled(rows)
}

// collectCredits 并发查所有账号余额（单账号失败只影响它自己那一行）。
// 逐账号串行是 8×RTT；并发把总耗时压到最慢那一个（跨域超时是主要风险）。
func (h *Handler) collectCredits() []creditRow {
	if h.cfg.Pool == nil || h.cfg.Upstream == nil {
		return []creditRow{}
	}
	list := h.cfg.Pool.List()
	out := make([]creditRow, len(list))
	var wg sync.WaitGroup
	soon := h.cfg.ExpiringSoon
	// zcode 号的额度要靠 (zcodejwttoken, device mid) 这对凭据去读，而这对凭据不在
	// 反代凭据里——所以先把池里所有 zcode 号的凭据按三层兜底解析好；池里没有
	// zcode 号就完全不读盘。
	zcreds := h.resolveZCodeCreds(list)
	for i, st := range list {
		out[i] = creditRow{UID: st.UID, Nickname: st.Nickname, Realm: st.Realm, Producer: st.Producer}
		a := h.cfg.Pool.AuthByUID(st.UID)
		if a == nil || strings.TrimSpace(a.AccessTokenValue()) == "" {
			out[i].Error = "号池里这条没有 access token"
			continue
		}
		// zcode 走另一条链路：没有 billing 积分接口，只有套餐权益（plans/balances）。
		// 混用 workbuddy 的 UserResourceFull 只会拿到一个光秃秃的 401——那正是
		// 管理页上"余额查不到 + 一整页 HTML"的来源。
		// kilo 是匿名免费通道：没有账号、没有 billing 接口、没有到期时间，也无需探活
		// （上游放行即用）。这里给一行恒定的「免费通道」说明，不走下面 workbuddy 的
		// UserResourceFull（那会拿它的占位 token 去打 workbuddy 域，必然 401）。
		if st.Producer == source.ProducerKilo {
			out[i].OK = true
			out[i].Note = "匿名免费通道：无积分/额度概念，上游按模型 pricing 免费放行"
			out[i].CredSource = "内置"
			continue
		}
		if st.Producer == source.ProducerZCode {
			plan := zcreds[st.UID]
			wg.Add(1)
			go func(i int, plan zcodeCredPlan) {
				defer wg.Done()
				h.fillZCodeCredit(&out[i], plan, soon)
			}(i, plan)
			continue
		}
		// 国际版号 + 逃生门（global.enabled=false）→ 必然打不通：token 属于
		// workbuddy.ai，却被按 CN 路由发到 codebuddy.cn，上游只回一个光秃秃的 401。
		// 这里显式标注，避免把「路由/网络问题」误读成「这号没余额」。
		globalForcedCN := a.RealmStored() == "global" && a.Realm() != "global"
		if globalForcedCN {
			out[i].RealmStored = "global"
		}
		wg.Add(1)
		go func(i int, a *auth.Auth) {
			defer wg.Done()
			remain, used, size, packs, expiring, err := h.cfg.Upstream.UserResourceFull(a, soon)
			if err != nil {
				msg := humanizeUpstreamErr(err.Error())
				if globalForcedCN {
					msg = "国际版号（workbuddy.ai）被 global.enabled=false 强制按 CN 路由，上游必然拒绝：" + msg
				}
				out[i].Error = msg
				return
			}
			out[i].Remain = remain
			out[i].Expiring = expiring
			out[i].Packages = packs
			if size > 0 {
				s, u := size, used
				out[i].Total, out[i].Used = &s, &u
			}
			out[i].OK = true
		}(i, a)
	}
	wg.Wait()
	return out
}

// zcodeCredPlan 一个 zcode 号的读额度凭据解析结果（凭据 + 来源/未命中的说明）。
type zcodeCredPlan struct {
	cred source.ZCodeQuotaCred
	note string
}

// resolveZCodeCreds 给池里所有 zcode 号解析「读额度凭据」，三层兜底：
//
//  1. 号池凭证文件自带（导入时落盘）——最优，随文件跨机器走
//  2. zcode-switch 账本快照
//  3. ZCode 应用当前登录态（本机可解密，但**只覆盖应用里正登录的那一个号**）
//
// 第 3 层带防误标：若它的 JWT 已归属别的 uid（或本机应用登录的其实是另一个号），
// 就不给这一行用——宁可这一行报「读不到」，也不把 A 号的额度安到 B 号上。
// 池里没有 zcode 号时直接返回空 map：不读盘、不多花一次 IO。
func (h *Handler) resolveZCodeCreds(list []pool.Status) map[string]zcodeCredPlan {
	out := map[string]zcodeCredPlan{}
	var leftovers []string
	miss := map[string]string{}    // uid → A/B 两层没命中的原因
	claimed := map[string]string{} // 已归属的 JWT → uid
	any := false
	for _, st := range list {
		if st.Producer != source.ProducerZCode {
			continue
		}
		any = true
		own := source.ZCodeQuotaCred{}
		if a := h.cfg.Pool.AuthByUID(st.UID); a != nil {
			own = source.ZCodeQuotaCred{JWT: a.ZCodeJWT(), DeviceMID: a.ZCodeDeviceMID()}
		}
		c, ok, why := source.ResolveZCodeQuotaCred(own, st.UID, h.zcodeDir())
		if ok {
			out[st.UID] = zcodeCredPlan{cred: c}
			claimed[c.JWT] = st.UID
			continue
		}
		miss[st.UID] = why
		leftovers = append(leftovers, st.UID)
	}
	if !any || len(leftovers) == 0 {
		return out
	}

	app, err := source.ReadZCodeAppQuotaCred(h.zcodeAppDir())
	if err != nil || app.Empty() {
		for _, uid := range leftovers {
			note := "这个号读不到额度凭据：号池文件没带，" + miss[uid]
			if err != nil {
				note += "；本机 ZCode 应用登录态也读不到（" + firstLineText(err.Error()) + "）"
			}
			note += "。在这台机器上重新导入一次即可把凭据落到号池文件里（之后换机器也能读）"
			out[uid] = zcodeCredPlan{note: note}
		}
		return out
	}
	owner, taken := claimed[app.JWT]
	for _, uid := range leftovers {
		if taken && owner != uid {
			out[uid] = zcodeCredPlan{note: miss[uid] + "；本机 ZCode 应用当前登录的是其它号（" + owner +
				"），不把它的额度安到这一行上"}
			continue
		}
		c := app
		c.Source = "ZCode 应用登录态"
		out[uid] = zcodeCredPlan{cred: c}
		claimed[app.JWT] = uid
		owner, taken = uid, true
	}
	return out
}

// fillZCodeCredit 按 zcode 自己的口径填一行额度：套餐权益（plans/balances）而不是
// billing 积分。**关键是可用模型**——capabilities 里的 model:<id> 才是这个号当前
// 真正能调的东西（实测周末活动的号 /models 能列 11 个，可用的只有一个）。
//
// 凭据从 plan 里来（已在 collectCredits 里按三层兜底解析好）；plan.note 非空 =
// 没解析到凭据，如实报「读不到」，不返回空行让人误以为这号没额度。
func (h *Handler) fillZCodeCredit(row *creditRow, plan zcodeCredPlan, soon time.Duration) {
	if plan.note != "" {
		row.Error = plan.note
		return
	}
	cred := plan.cred
	if cred.Empty() {
		row.Error = "没解析到这个号的 zcodejwttoken，读不了额度"
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	bal, err := h.cfg.Upstream.FetchZCodeBalance(ctx, cred.JWT, cred.DeviceMID)
	if err != nil {
		row.Error = humanizeUpstreamErr(err.Error())
		return
	}
	row.OK = true
	row.CredSource = cred.Source
	row.Unit = "token"
	row.EntitledModels = bal.EntitledModels()
	row.PlanSummary = bal.PlanSummary()
	var total, used, remain, expiring, expires int64
	now := time.Now()
	for _, g := range bal.Balances {
		t, u := int64(g.TotalUnits), int64(g.UsedUnits)
		avail := int64(g.AvailableUnits)
		if avail == 0 {
			avail = int64(g.RemainingUnits)
		}
		total, used, remain = total+t, used+u, remain+avail
		if e := int64(g.ExpiresAt); e > 0 {
			if expires == 0 || e < expires {
				expires = e
			}
			if soon > 0 && time.Unix(e, 0).Sub(now) <= soon {
				expiring += avail
			}
		}
	}
	row.Remain, row.Expiring, row.ExpiresAt = remain, expiring, expires
	if total > 0 {
		tv, uv := total, used
		row.Total, row.Used = &tv, &uv
	}
	row.Packages = len(bal.Balances)
	// 上游答了 200 但一条权益都没给（实测有这种号）：报「未给可用范围」，不报 0。
	// 0 会被读成"这号没额度了"，而真相是"上游没说"——两者对运维的含义完全相反。
	if len(bal.Balances) == 0 && len(row.EntitledModels) == 0 {
		row.Note = "上游未给可用范围（读得到账号，读不到套餐）"
		// 同时撤掉 OK：留着 OK=true 会让人看到一个大写的「0」，而真相是上游没说。
		row.OK = false
	}
}

// poolHasZCode 池里有没有 zcode 号。「要不要为收敛模型表去刷一次额度」拿它当闸门：
// 一个 zcode 号都没接，就不该为这件事打任何上游。
func (h *Handler) poolHasZCode() bool {
	if h.cfg.Pool == nil {
		return false
	}
	for _, st := range h.cfg.Pool.List() {
		if st.Producer == source.ProducerZCode {
			return true
		}
	}
	return false
}

// zcodeEntitledCache 号池里 zcode 号「有额度」的模型并集（包级共享 —— 必须共享）。
//
// 主口与各出口是**各自独立的 Handler 实例**（channels.go 从同一份 baseCfg 复制），
// 每份都有自己的 credits 缓存；模型表却都要按同一份额度收敛。挂在 handler 上会出现
// 「主口收对了、zcode 专用口还列着整家目录」这种分裂，所以这份数据只能放包级。
//
// fetched / triedAt 都用 /admin/credits 的 TTL 口径：前者是数据新鲜度，后者是
// 「什么时候尝试去刷过」——主口与出口可能同时来问，靠它保证一个 TTL 内只打一次上游。
var zcodeEntitledCache struct {
	sync.RWMutex
	set map[string]bool
	// byUID 每个 zcode 号自己**有没有**至少一个可用模型（uid → true/false）。
	// 并集 set 只能回答「这些号整体能跑哪些模型」，回答不了「哪个号能跑」——
	// 概览页的「可用」列（健康 且 至少能跑一个模型）要的正是后者，故单独存一份。
	byUID   map[string]bool
	known   bool
	fetched time.Time
	triedAt time.Time
}

// zcodeEntitledPath 收敛快照的落盘路径（数据目录，与 output.json / model.json 同级）。
// 由 cmd/server 启动时 SetZCodeEntitledPath 注入；空 = 不落盘（纯内存 / 测试形态）。
var zcodeEntitledPath string

// zcodeEntitledSnapshot 收敛快照的落盘形态：模型并集 + 读到它的时刻。
type zcodeEntitledSnapshot struct {
	Models  []string        `json:"models"`
	ByUID   map[string]bool `json:"by_uid,omitempty"`
	Fetched int64           `json:"fetched_at"`
}

// SetZCodeEntitledPath 接线快照落盘路径（起服务前调用一次）。
func SetZCodeEntitledPath(path string) {
	zcodeEntitledCache.Lock()
	zcodeEntitledPath = path
	zcodeEntitledCache.Unlock()
}

// LoadZCodeEntitled 启动时把上次的收敛结果读回缓存（跨重启保留）。
//
// 为什么值得落盘：这份结果是「哪些 GLM 型号这些号真能跑」，上次已经问到过，跟
// output.json / model.json 一样是数据目录里的持久化物。不落盘的话，每次重启后的
// 头几秒 /v1/models 与发布清单都会先把整家 GLM 目录（11 个）当「可用」列出去，
// 再跳回收敛后的 1 个——用户看到的就是「怎么又变回 11 个了」。
// 读不到 / 损坏 / 空 → 什么都不做（下一次额度读取会重新写入）。
func LoadZCodeEntitled() {
	zcodeEntitledCache.Lock()
	path := zcodeEntitledPath
	zcodeEntitledCache.Unlock()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var snap zcodeEntitledSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil || len(snap.Models) == 0 {
		return
	}
	set := make(map[string]bool, len(snap.Models))
	for _, m := range snap.Models {
		if m != "" {
			set[m] = true
		}
	}
	fetched := time.Now()
	if snap.Fetched > 0 {
		fetched = time.Unix(snap.Fetched, 0)
	}
	byUID := make(map[string]bool, len(snap.ByUID))
	for uid, ok := range snap.ByUID {
		if uid != "" {
			byUID[uid] = ok
		}
	}
	zcodeEntitledCache.Lock()
	zcodeEntitledCache.set = set
	zcodeEntitledCache.byUID = byUID
	zcodeEntitledCache.known = true
	zcodeEntitledCache.fetched = fetched
	zcodeEntitledCache.Unlock()
}

// saveZCodeEntitledLocked 原子落盘（tmp + rename，pool state.json / model.json 同模式）。
// 调用方须持 zcodeEntitledCache 锁（路径也在锁内读）。空路径 / 写失败 → 静默
// （内存缓存仍生效，只是这次重启要重新问一遍上游）。
func saveZCodeEntitledLocked(set, byUID map[string]bool) {
	path := zcodeEntitledPath
	if path == "" {
		return
	}
	models := make([]string, 0, len(set))
	for m := range set {
		models = append(models, m)
	}
	sort.Strings(models)
	raw, err := json.MarshalIndent(zcodeEntitledSnapshot{Models: models, ByUID: byUID, Fetched: time.Now().Unix()}, "", "  ")
	if err != nil {
		return
	}
	if dir := filepath.Dir(path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// storeZCodeEntitled 把一行行额度折成「哪些模型真的有额度」写进包级缓存。
// 判据是套餐 capabilities（upstream.ZCodeBalance.EntitledModels），不是上游 /models
// 能列出什么。
//
// 三种情况必须分清楚，否则面板会「莫名其妙变回去」：
//   - 读到额度 → 写新集合（并落盘）；
//   - 读成功但确实没有额度（行 OK 而 entitled 为空：套餐到期 / 活动结束）→ 如实写空，
//     调用方不收敛（不假装没有，也不假装能用）；
//   - **一行都没读成功**（上游 401 / 超时 / 凭据解不开）→ 那是「读不到」，不是「没有」：
//     保留上次问到的集合。少了这一条，一次网络抖动就会把用户眼前已经收敛好的清单
//     掀回整家目录（11 个 GLM），而用户可能正勾着（旧实现就是 known=false 覆盖）。
//
// 池里一个 zcode 号都没有 → 清掉（别拿旧集合去收敛人家的模型表）。
func storeZCodeEntitled(rows []creditRow) {
	set := map[string]bool{}
	byUID := map[string]bool{}
	sawZCode := false
	readOK := false // 至少有一行真的从上游读回来了
	for _, r := range rows {
		if r.Producer != source.ProducerZCode {
			continue
		}
		sawZCode = true
		if !r.OK {
			// 这一行读不到：不给它下「没有模型」的结论（byUID 里不放 false），
			// 概览的「可用」判定会把它当作未知而不是不可用。
			continue
		}
		readOK = true
		entitled := false
		for _, m := range r.EntitledModels {
			if m != "" {
				set[m] = true
				entitled = true
			}
		}
		byUID[r.UID] = entitled
	}
	zcodeEntitledCache.Lock()
	defer zcodeEntitledCache.Unlock()
	if !sawZCode {
		zcodeEntitledCache.set = nil
		zcodeEntitledCache.byUID = nil
		zcodeEntitledCache.known = false
		zcodeEntitledCache.fetched = time.Now()
		return
	}
	if !readOK && zcodeEntitledCache.known {
		// 读不到 ≠ 没有：留旧集合，fetched 也不动（TTL 到点继续重试）。
		return
	}
	zcodeEntitledCache.set = set
	zcodeEntitledCache.byUID = byUID
	zcodeEntitledCache.known = len(set) > 0
	zcodeEntitledCache.fetched = time.Now()
	if zcodeEntitledCache.known {
		saveZCodeEntitledLocked(set, byUID)
	}
}

// zcodeAccountUsable 报告某个 zcode 号**现在是否至少能跑一个模型**（供概览「可用」列）。
// 返回 (usable, known)：known=false 表示还没问到过这个号的额度，调用方不要据此判死。
// 判据是该号自己的套餐 capabilities（byUID），不是号池并集——周末活动只发一个号时，
// 并集有模型不代表每个号都能跑。
func zcodeAccountUsable(uid string) (bool, bool) {
	zcodeEntitledCache.RLock()
	defer zcodeEntitledCache.RUnlock()
	if !zcodeEntitledCache.known {
		return false, false
	}
	if zcodeEntitledCache.byUID == nil {
		return false, false
	}
	ok, present := zcodeEntitledCache.byUID[uid]
	if !present {
		return false, false
	}
	return ok, true
}

// zcodeEntitledModels 号池里 zcode 账号**真正能用**的模型并集，以及「是否问到过」。
//
// 判据是套餐/额度的 capabilities（upstream.ZCodeBalance.EntitledModels），不是上游
// /models 能列出什么——实测：周末活动的号 /models 列 11 个，capabilities 只有
// glm-5.3-flash。数据来自包级缓存（由 refreshCredits / WarmCredits 写），不额外打上游；
// 冷或过期就在后台起一次刷新，本次按「不知道」返回。
//
// 过期时**照旧用旧值**（stale-while-revalidate）并在后台起一次刷新：收敛宁可按上次
// 问到的结果继续收，也不能中途退回「整家目录」——那会让用户眼皮底下的清单从 1 条
// 变回 11 条（旧实现就是这样：TTL 一到先返回「不知道」）。只有**从没问到过**
// （known=false，冷启动）才说不知道，调用方那时不要收敛：宁可按上游全量目录列出，
// 也不假装号池一个模型都没有。
func (h *Handler) zcodeEntitledModels() (map[string]bool, bool) {
	if !h.poolHasZCode() {
		return nil, false
	}
	now := time.Now()
	zcodeEntitledCache.Lock()
	set, known := zcodeEntitledCache.set, zcodeEntitledCache.known
	stale := !known || now.Sub(zcodeEntitledCache.fetched) >= creditsTTL
	trigger := stale && now.Sub(zcodeEntitledCache.triedAt) >= creditsTTL
	if trigger {
		zcodeEntitledCache.triedAt = now
	}
	zcodeEntitledCache.Unlock()
	if trigger {
		h.creditsSnapshot(false) // 后台重查；本次照旧用旧值，不回退
	}
	return set, known
}

// WarmCredits 预热余额/额度缓存（cmd/server 启动时后台调一次，不阻塞起服务）。
//
// 为什么要在启动时预一把：对外模型表与发布清单里 zcode 那一段是按套餐额度收敛的
// （见 zcodeEntitledModels），读的就是这份缓存。不预热的话，重启后的头几秒 /v1/models
// 会先把上游整家目录（如 11 个 GLM）列出去，得等前台点一下才收对。
// 池里没有 zcode 号就不预热：那些号的余额只在积分页用得上，按需查即可。
func (h *Handler) WarmCredits() {
	if !h.poolHasZCode() {
		return
	}
	h.creditsSnapshot(true)
}

// upstreamHTTPCode 从错误文本里抠出上游状态码（"upstream client (http 401)" /
// "zcode balance: HTTP 401" 两种写法都要认）。
var upstreamHTTPCode = regexp.MustCompile(`(?i)http[ /]?([1-5]\d\d)`)

// upstreamHTTPHint 状态码人话表（只覆盖真会遇到的；其余给通用文案）。
var upstreamHTTPHint = map[string]string{
	"401": "凭据不被上游接受",
	"403": "上游拒绝访问（可能风控或路由错域）",
	"404": "上游没有这个接口",
	"408": "上游超时",
	"429": "上游限流",
	"500": "上游内部错误",
	"502": "上游网关错误",
	"503": "上游暂不可用",
	"504": "上游网关超时",
}

// humanizeUpstreamErr 把上游原始报错压成一句能读的话。
// 上游 4xx/5xx 的响应体常常是整页 HTML（登录页/网关页），直接塞进表格既撑爆观感
// 又没有信息量——只留「哪个状态码、大概什么意思」。
func humanizeUpstreamErr(msg string) string {
	s := strings.TrimSpace(msg)
	if i := strings.Index(strings.ToLower(s), "<html"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(strings.Trim(s, ":： \t"))
	if m := upstreamHTTPCode.FindStringSubmatch(s); m != nil {
		hint := upstreamHTTPHint[m[1]]
		if hint == "" {
			hint = "上游返回该状态码"
		}
		return "上游 HTTP " + m[1] + "（" + hint + "）"
	}
	if s == "" {
		return "上游没有返回可读信息"
	}
	return firstLineText(s)
}

// unixOrZero 零值时间 → 0（前端按 0 判「还没查过」）。
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// firstLineText 错误文本只留首行（上游报文可能很长，塞进表格会撑爆观感）。
func firstLineText(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

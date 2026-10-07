// reset.go 运行态复位（POST /admin/accounts/reset）：把「冷却 + 失败计数」一次清干净，
// 不用停服、不用删 state.json。
//
// 为什么需要它：账号被 429 / 5xx / WAF 打成冷却或熔断之后，即使上游已经恢复，也得等
// 冷却自然到期才回池；排障时「停服 + 删 state.json」既不优雅，也会连带丢掉别的状态。
// 这里给一个不掉线的复位入口：清了立刻能承接流量。
//
// 刻意**不动**的字段（与 transition.go 的迁移矩阵保持一致的正交性）：
//   - disabled / manualDisabled：一个是「系统判定这号坏了」的终态，一个是运维意图。
//     复位不是解禁（解禁是 revive / enable 的活）——顺手清掉会把坏号静默放回选号池。
//   - credits / creditsExpiring / creditsUrgent：上游事实，不是本进程的运行时观测。
//   - modelCost：历史成本观测（选号权重用），与「失败了」无关。
package pool

import "time"

// ResetResult 一次复位的结果。
type ResetResult struct {
	Matched int `json:"matched"` // 命中的账号数
	Reset   int `json:"reset"`   // 真正清掉运行态的账号数（本来就干净的只计数不改写）
	// StillDisabled / StillManualDisable 复位后仍是不可选的账号数：复位只清运行态，
	// 要回池还得单独 revive（自动禁用）/ enable（手动停用）。面板据此提示，避免
	// 用户以为「点了清空冷却怎么还有号不干活」。
	StillDisabled      int `json:"still_disabled"`
	StillManualDisable int `json:"still_manual_disabled"`
	// NotFound 点名了但池里没有的 uid（uids 形式才有意义；全池复位时为空）。
	NotFound []string `json:"not_found,omitempty"`
}

// ResetRuntime 复位 uids 的运行态。**nil = 全池**；非 nil（含空切片）= 只处理列出的那些。
//
// 「空切片 ≠ 全池」是刻意的：按 producer 复位时「这个客户端一个号都没有」必须复位 0 个，
// 若按 len==0 判全池，一个拼错的生产者名就会把整池的冷却全清掉——那是灾难性的误伤。
// 全池请显式传 nil。
//
// 清的字段：until/coolKind/softStreak/modelCooldowns（冷却域）、
// breakerUntil/fails/retryCount（熔断器）、sessionDeadFails、consecutiveFails/degradeUntil
// （连败降权）。reason 只在「不是 disabled」时清——disabled 账号的 reason 是禁用原因，
// 那是要留着给运维看的。
func (p *Pool) ResetRuntime(uids []string) ResetResult {
	res := ResetResult{}
	p.mu.Lock()
	defer p.mu.Unlock()

	targets := uids
	if targets == nil {
		targets = make([]string, 0, len(p.byUID))
		for uid := range p.byUID {
			targets = append(targets, uid)
		}
	}
	dirty := false
	for _, uid := range targets {
		e, ok := p.byUID[uid]
		if !ok {
			if uids != nil {
				res.NotFound = append(res.NotFound, uid) // 点名了才报，全池遍历不可能缺
			}
			continue
		}
		res.Matched++
		if e.disabled {
			res.StillDisabled++
		}
		if e.manualDisabled {
			res.StillManualDisable++
		}
		// 「有没有东西要清」的判据：任一运行态字段非零。全零 = 幂等空操作，
		// 不写 dirty（避免面板反复点导致无意义的落盘）。
		// reason 只在非 disabled 时算「有东西要清」：disabled 账号的 reason 是禁用原因，
		// 下面刻意保留它，若也算改动就会出现「Reset=1 但其实一个字段都没动」的假账。
		changed := !e.until.IsZero() || e.coolKind != 0 || (!e.disabled && e.reason != "") ||
			e.softStreak != 0 || len(e.modelCooldowns) > 0 ||
			!e.breakerUntil.IsZero() || e.fails != 0 || e.retryCount != 0 ||
			e.sessionDeadFails != 0 || e.consecutiveFails != 0 || !e.degradeUntil.IsZero()
		e.until = time.Time{}
		e.coolKind = 0
		if !e.disabled {
			e.reason = ""
		}
		e.softStreak = 0
		e.modelCooldowns = nil
		e.breakerUntil = time.Time{}
		e.fails = 0
		e.retryCount = 0
		e.sessionDeadFails = 0
		e.consecutiveFails = 0
		e.degradeUntil = time.Time{}
		if changed {
			res.Reset++
			dirty = true
		}
	}
	if dirty {
		p.dirty.Store(true)
	}
	return res
}

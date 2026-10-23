package webui

import (
	"regexp"
	"strings"
	"testing"
)

// TestGrowthSummaryKeepsNameAndCountersOnOneRow 成长任务账号行必须是
// 「箭头 + 可收缩账号名 + 固定宽度计数」的同一行，不能靠猜 grid 列数排。
//
// 回归背景：DOM 一直是 3 项（::before、账号名、计数），旧样式却只给了 2 列；
// 窄窗口时第三项自动换行，账号名被推到最右，计数掉到下一行并与文字重叠。
// 这是同一 bug 反复出现的原因，因此这里同时约束结构和三项的布局角色。
func TestGrowthSummaryKeepsNameAndCountersOnOneRow(t *testing.T) {
	css := extractCSS(t)
	body := scriptBody(t)

	re := regexp.MustCompile(`(?s)\.growth-det>summary\{[^}]*display:flex[^}]*align-items:center`)
	if !re.MatchString(css) {
		t.Error("账号摘要行必须用 flex 横向排版，不能使用固定 grid 列数；否则 DOM 增项时窄屏会换行")
	}
	for _, want := range []string{
		`\.growth-det>summary::before\{[^}]*flex:0 0 14px`,
		`\.growth-name\{[^}]*flex:1 1 auto[^}]*min-width:0`,
		`\.growth-count\{[^}]*flex:0 0 auto[^}]*white-space:nowrap`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("账号行布局角色缺失：%s", want)
		}
	}
	if regexp.MustCompile(`\.growth-det>summary\{[^}]*grid-template-columns`).MatchString(css) {
		t.Error("账号摘要行又用回了 grid-template-columns：已出现 3 个逻辑项，列数漂移会重复挤压问题")
	}
	if !strings.Contains(body, `<span class="growth-name">`) || !strings.Contains(body, `<span class="growth-count">`) {
		t.Error("账号摘要的 DOM 结构变了：应保留 growth-name + growth-count 两个直接子项")
	}
}

// TestGrowthTaskShowsDynamicReward 任务奖励必须来自上游 reward_credit/reward_energy，
// 用紧凑徽标挂在任务标题后面；不能再加一列把任务行撑宽，也不能硬编码任务码表。
func TestGrowthTaskShowsDynamicReward(t *testing.T) {
	css := extractCSS(t)
	body := scriptBody(t)
	for _, want := range []string{
		`\.growth-reward\{[^}]*display:inline-flex[^}]*white-space:nowrap`,
		`\.growth-reward \.credit\{[^}]*var\(--warn`,
		`\.growth-reward \.energy\{[^}]*var\(--acc`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("奖励徽标样式缺失：%s", want)
		}
	}
	for _, want := range []string{
		`Number(t.reward_credit) > 0`,
		`Number(t.reward_energy) > 0`,
		`class="growth-reward"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("奖励徽标没有接到上游字段：%s", want)
		}
	}
	if strings.Contains(body, `"chat_5":100`) || strings.Contains(body, `taskRewardMap`) {
		t.Error("任务奖励不能硬编码任务码表")
	}
}

// TestGrowthTaskBusyKeepsRowContent 单项处理只能替换右侧按钮，任务名/描述/
// 进度/状态必须始终保留。否则用户展开后点「做这一项」，整行会变成
// “正在做这一项…”而看不出到底在处理什么。
func TestGrowthTaskBusyKeepsRowContent(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`var main = '<div class="tn">' + esc(taskTitle) + reward + '</div>' + taskDesc;`,
		`'<div class="prog">' + esc(prog) + '</div>'`,
		`'<div class="act ' + esc(action) + '">' + esc(actionText) + '</div>'`,
		`'<span class="growth-btn-spin" title="处理中">'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("处理中行内容被错误替换：缺少 %s", want)
		}
	}
	if strings.Contains(body, `正在做这一项…`) {
		t.Error("处理中不能覆盖任务名；加载态只允许出现在右侧操作位")
	}
}

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
// 作为独立列显示；数据缺失时显示占位符，不能硬编码任务码表。
func TestGrowthTaskShowsDynamicReward(t *testing.T) {
	css := extractCSS(t)
	body := scriptBody(t)
	for _, want := range []string{
		`\.growth-task-reward\{[^}]*justify-content:flex-end[^}]*font-variant-numeric:tabular-nums`,
		`\.growth-task-reward \.credit\{[^}]*var\(--warn`,
		`\.growth-task-reward \.energy\{[^}]*var\(--acc`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("奖励列样式缺失：%s", want)
		}
	}
	for _, want := range []string{
		`Number(t.reward_credit) > 0`,
		`Number(t.reward_energy) > 0`,
		`class="growth-task-reward"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("奖励列没有接到上游字段：%s", want)
		}
	}
	if strings.Contains(body, `"chat_5":100`) || strings.Contains(body, `taskRewardMap`) {
		t.Error("任务奖励不能硬编码任务码表")
	}
}

// TestGrowthTaskBusyKeepsRowContent 单项处理只能替换右侧按钮，任务名/描述/
// 进度/奖励/状态必须始终保留。否则用户展开后点「做这一项」，整行会变成
// “正在做这一项…”而看不出到底在处理什么。
func TestGrowthTaskBusyKeepsRowContent(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`var main = '<div class="tn">' + esc(taskTitle) + '</div>' + taskDesc;`,
		`'<div class="prog">' + esc(prog) + '</div>'`,
		`'<div class="growth-task-reward" title="完成并领取后到账">' + rewardCell + '</div>'`,
		`'<div class="act ' + esc(action) + '">' + esc(actionText) + '</div>'`,
		`class="tiny growth-task-btn"`,
		`'<span class="growth-btn-spin" title="处理中">'`,
		`'<span class="growth-task-btn-label">' + actionLabel + '</span>'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("处理中行内容被错误替换：缺少 %s", want)
		}
	}
	if strings.Contains(body, `正在做这一项…`) {
		t.Error("处理中不能覆盖任务名；加载态只允许出现在右侧操作位")
	}
}

// TestGrowthTaskButtonCSSUsesRealNewlines 回归：CSS 块之间误写成字面量 `\n`
// 时，浏览器会把后续规则当成同一选择器的一部分，spinner/禁用态一起失效。
func TestGrowthTaskButtonCSSUsesRealNewlines(t *testing.T) {
	css := extractCSS(t)
	if strings.Contains(css, `\n`) {
		t.Fatal("CSS 中出现了字面量 \\n；规则必须使用真实换行")
	}
	for _, want := range []string{
		`(?m)^\.growth-task-btn\{[^}]*display:inline-flex`,
		`(?m)^\.growth-task-btn:disabled\{[^}]*cursor:default`,
		`(?m)^\.growth-btn-spin\{[^}]*width:16px`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("按钮加载态样式缺失：%s", want)
		}
	}
}

// TestZCodeUnavailableCopyCoversRoute 发布清单把“未授权”和“不可路由”合并收进
// 折叠区后，摘要必须用统一口径“不可用”；不能只写“未授权”，否则 1113/401
// 导致的不可路由行会被错误解释成套餐没授权。
func TestZCodeUnavailableCopyCoversRoute(t *testing.T) {
	body := scriptBody(t)
	if !strings.Contains(body, `unent.length + ' 个不可用</span>'`) {
		t.Error("模型分组摘要仍把 route_available=false 的行称作“未授权”")
	}
	if strings.Contains(body, `unent.length + ' 个未授权</span>'`) {
		t.Error("不可用摘要不能漏掉不可路由这一层")
	}
}

// TestGrowthRunFailureIsVisible 单项任务没有任何上报/领取结果时，前端不能
// 继续显示“已处理该账号”；必须把后端 errors 或明确失败反馈给用户。
func TestGrowthRunFailureIsVisible(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`Array.isArray(r.errors)`,
		`这一项未完成：`,
		`上游没有返回可确认的结果`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("失败反馈链路缺失：%s", want)
		}
	}
}

// TestGrowthOpenSurvivesFullRender 轮询触发 render() 时，展开状态必须先从
// 真实 <details open> 回写到 S.growthOpen，再用于下一次渲染。否则用户展开
// 账号后最多 20 秒就会被后台轮询自动折叠。
func TestGrowthOpenSurvivesFullRender(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`$$("details.growth-det").forEach`,
		`if (d.open) S.growthOpen[uid] = true`,
		`saveGrowthOpen();`,
		`(isOpen ? " open" : "")`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("展开状态持久化链路缺失：%s", want)
		}
	}
}

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
		`class="growth-task-reward"`, `growth-task-head`, `<div>奖励</div>`, `var hasOp = tasks.some`, `(hasOp ? '<div>操作</div>' : '')`, `+ thead + trows`, `var actionText = action === "run" ? "可帮做"`,
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
// 奖励/状态必须始终保留。进度只有 target>0 时以内联小字显示，不单独占列。
func TestGrowthTaskBusyKeepsRowContent(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`var main = '<div class="tn">' + esc(taskTitle) + '</div>' + taskDesc + progressInline;`,
		`var progressInline = (t.progress && t.progress.target) ? '<div class="td progress-inline">进度 '`,
		`'<div class="growth-task-reward" title="完成并领取后到账">' + rewardCell + '</div>'`,
		`'<div class="act ' + esc(action) + '">' + esc(actionText) + '</div>'`,
		`class="tiny growth-task-btn' + (isTaskBusy ? ' is-loading' : '')`,
		`'<span class="mini-spin" title="处理中" aria-label="处理中"></span>'`,
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

// TestGrowthManualAcceptedDoesNotClaimUserMustAct 关注公众号这类 accepted
// 任务是在等上游计时，不是“还需用户再操作”；文案必须区分，不能继续写需你完成。
func TestGrowthManualAcceptedDoesNotClaimUserMustAct(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`function growthManualText(t)`,
		`if (st === "accepted") return "等待上游确认";`,
		`return "需你完成";`,
		`var actionText = action === "run" ? "可帮做" : action === "claim" ? "奖励可领取" : growthManualText(t);`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("手动任务状态文案缺失：%s", want)
		}
	}
}

// TestGrowthTaskBusyOnlyLeavesButtonSpinner 单项处理时按钮文字应被圆形 spinner
// 替换，而不是文字旁边再追加一个 spinner；任务行其它内容保持原位。
func TestGrowthTaskBusyOnlyLeavesButtonSpinner(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`var isTaskBusy = taskBusy === tk;`,
		`class="tiny growth-task-btn' + (isTaskBusy ? ' is-loading' : '')`,
		`? '<span class="mini-spin" title="处理中" aria-label="处理中"></span>'`,
		`'<div class="growth-task-main">' + main + '</div>'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("单项加载态结构缺失：%s", want)
		}
	}
	if strings.Contains(body, `'<span class="growth-task-btn-label">做这一项</span><span class="mini-spin"`) {
		t.Error("加载态必须用 spinner 替换「做这一项」，不能文字和 spinner 同时存在")
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
		`(?m)^\.growth-task-btn \.mini-spin\{[^}]*width:14px`,
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
		`var growthDets = $$("details.growth-det");`,
		`if (growthDets.length){`,
		`growthDets.forEach(function(d){`,
		`if (d.open) S.growthOpen[uid] = true`,
		`saveGrowthOpen();`,
		`(isOpen ? " open" : "")`, `if (!t.isConnected || S.growthRendering) return;`, `growthRendering: false`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("展开状态持久化链路缺失：%s", want)
		}
	}
}

// TestGrowthOpenSurvivesTaskRunAndPolling render() 前后必须保留用户展开集合；
// 任务完成/20 秒轮询替换 S.growth 时不能把 open 状态丢掉。
func TestGrowthOpenSurvivesTaskRunAndPolling(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`var growthDets = $$("details.growth-det");`,
		`if (growthDets.length){`,
		`var openBefore = {};`,
		`Object.keys(S.growthOpen || {}).forEach(function(k){ openBefore[k] = !!S.growthOpen[k]; });`,
		`S.growthOpen = openBefore;`,
		`S.growth = await api("/admin/growth/tasks");`,
		`S.growthOpen = openBefore;`,
		`S.growth = fresh;`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("展开状态在重画/轮询时丢失：%s", want)
		}
	}
}

// TestGrowthAccountHeaderAlignsActionToSummary 账号头右侧「帮我做」必须与摘要行
// 同高对齐；之前的 align-items:center 在摘要文案折行时会整体下沉，看起来像掉到
// 账号卡中间。这里锁住 align-self:start + 固定顶部内边距，而不是靠眼调整列。
func TestGrowthAccountHeaderAlignsActionToSummary(t *testing.T) {
	css := extractCSS(t)
	for _, want := range []string{
		`(?m)^\.growth-head\{[^}]*align-items:start`,
		`(?m)^\.growth-act\{[^}]*align-self:start[^}]*padding-top:14px`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("账号头按钮对齐规则缺失：%s", want)
		}
	}
}

// TestGrowthBlankNicknameFallsBackToUID 昵称只包含零宽字符/空白时，账号摘要
// 不能渲染成空白行；必须回退到 UID 前 8 位，且 title 也使用同一个可见名称。
func TestGrowthBlankNicknameFallsBackToUID(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`function growthAccountName(a)`,
		`.replace(/[\s\u3164\u200B-\u200D\uFEFF]/g, "")`,
		`return n || String((a && a.uid) || "").slice(0, 8);`,
		`var accountName = growthAccountName(a);`,
		`esc(accountName)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("空昵称回退链路缺失：%s", want)
		}
	}
}

// TestGrowthRuntimeAutoScansOnce 首次进入运行态且尚无数据时自动扫描一次；
// 失败不能每切页重试，避免轮询和上游任务接口互相叠加。
func TestGrowthRuntimeAutoScansOnce(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`if (tab === "runtime" && !S.growth && !S.growthBusy && !S.growthAutoTried)`,
		`S.growthAutoTried = true;`,
		`if (S.tab === "runtime" && !S.growth && !S.growthBusy) growthScan();`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("运行态首次自动扫描链路缺失：%s", want)
		}
	}
}

// TestGrowthManualRowsDoNotReserveActionColumn 当账号所有任务都必须手动完成时，
// 表头和行都不能预留空的「操作」列；否则表格右侧会凭空多出一条空白，窄窗下
// 反而挤压任务标题。有任一可执行任务时则必须恢复操作列。
func TestGrowthManualRowsDoNotReserveActionColumn(t *testing.T) {
	css := extractCSS(t)
	body := scriptBody(t)
	for _, want := range []string{
		`(?m)^\.growth-task\.is-manual\{[^}]*grid-template-columns`,
		`var hasOp = tasks.some(function(t){ var a = t.action || "manual"; return a === "run" || a === "claim"; });`,
		`var thead = '<div class="growth-task-head' + (hasOp ? '' : ' no-op') + '">' +`,
		`'<div>任务</div><div>奖励</div><div>状态</div>' +`,
		`(hasOp ? '<div>操作</div>' : '') + '</div>';`,
		`(hasOp ? '<div class="growth-task-op">' + op + '</div>' : '') + '</div>';`,
	} {
		if !strings.Contains(body, want) && !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("手动行空操作列处理缺失：%s", want)
		}
	}
}

// TestGrowthRunOnlyMarksCompleteAfterReload 回归：点击单项后不能先把任务从
// 本地数组删掉，再把“少了一项”当成完成。上报成功只代表事件已提交，完成
// 必须来自重新扫描后的上游状态（任务消失或 status=completed/claimed）。
func TestGrowthRunOnlyMarksCompleteAfterReload(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`var fresh = await api("/admin/growth/tasks");`,
		`completedNow = st === "completed" || st === "claimed";`,
		`if (freshAccts[i].ok && !stillThere) completedNow = true;`,
		`msg = completedNow ? "这一项已完成" : (taskCode ? "已提交这一项，等待上游确认" : "已处理该账号");`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("完成判定必须等待重新扫描：%s", want)
		}
	}
	if strings.Contains(body, `grew = kept.length < list.length`) || strings.Contains(body, `a.tasks = kept`) {
		t.Error("单项执行不能乐观删除任务并据此判定完成")
	}
}

// TestSettingsPageIsCompactAndKeyActionsInline 设置页首屏必须是可操作内容，不是一块
// 「桌面专属设置」大空卡。浏览器直连时只给一行能力边界提示；网关密钥放在第一张卡，
// 并直接复用 gwKeyInline（生成/显示/复制/清空），而不是再跳一个弹窗。
func TestSettingsPageIsCompactAndKeyActionsInline(t *testing.T) {
	body := scriptBody(t)
	if strings.Contains(body, `card("桌面专属设置"`) {
		t.Error("设置页又渲染了整张「桌面专属设置」空卡：浏览器直连时会留下大片空白")
	}
	for _, want := range []string{
		`? "桌面程序 · 悬浮窗与网关由本机托管。"`,
		`: "浏览器直连网关 · 桌面专属功能不可用。"`,
		`html += '<div class="settings-grid">'`,
		`card("网关密钥", "外部客户端访问网关时需要；留空则本机直连免密钥。"`,
		`card("本机配置", "只读信息，便于核对配置文件与账号落点。"`,
		`card("上游凭据入口", "这里只管理本地网关；上游账号看「获取源」。"`,
		`data-action="goto-zcode-app"`,
		`data-action="goto-sources"`,
		`'<div class="row">' + gwKeyInline(S.apiKey) + '</div>'`,
		`["配置文件", cfgPath]`,
		`["账号目录", authDir]`,
		`var cfgPath = o.config_path || "（未记录）";`,
		`var authDir = src.auth_dir || "（未读取）";`,
		`html += '<div class="settings-tip">悬浮窗、网关启停等桌面功能仅在 Free2API 桌面程序中可用。</div>'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("设置页紧凑结构缺失：%s", want)
		}
	}
	if strings.Contains(body, `if (!fwReady){`) && !strings.Contains(body, `正在读取悬浮窗设置…`) {
		t.Error("桌面端悬浮窗状态未加载时必须显示明确的加载态，不能整块消失")
	}
	css := extractCSS(t)
	for _, want := range []string{
		`(?m)^\.settings-grid\{[^}]*display:grid[^}]*grid-template-columns:minmax\(0,1fr\) minmax\(0,1fr\)[^}]*align-items:start`,
		`(?m)^\.settings-grid>\.card\{[^}]*margin-bottom:0[^}]*min-width:0`,
		`(?m)^@media\(max-width:1100px\)\{\.settings-grid\{grid-template-columns:1fr\}\}`,
		`(?m)^\.settings-srcman\{grid-column:1/-1\}`,
		`(?m)^\.settings-srcrow\{[^}]*display:flex[^}]*justify-content:space-between`,
		`(?m)^\.settings-tip\{[^}]*font-size:12px[^}]*line-height`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("设置页紧凑布局样式缺失：%s", want)
		}
	}
}

// TestZCodeAppImportRequiresStaticChat 静态 key 形态完整不等于可支付。zcode-app 与
// zcode-switch 不允许「全部导入」，必须先看候选并完成显式静态 chat 验活。
func TestZCodeAppImportRequiresStaticChat(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`if (k.id !== "zcode-app" && k.id !== "zcode-switch"){`,
		`必须先看候选并做完静态 chat 验活，不能一键把无静态余额的号塞进池子。`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ZCode 静态验活门槛缺失：%s", want)
		}
	}
}

// TestZCodeStaticChatIsExplicitAndNeverAutoPolled 真实 chat 探活会消耗 token，
// 必须由用户点击时显式传 chat=1；探活/额度/后台刷新都不能隐式带上它。
func TestZCodeStaticChatIsExplicitAndNeverAutoPolled(t *testing.T) {
	body := scriptBody(t)

	for _, want := range []string{
		`candProbe: false, candBalance: false, candChat: false`,
		`function candQuery(probe, balance, chat)`,
		`if (chat) q.push("chat=1");`,
		`var wantChat = !!chat;`,
		`var r = await api("/admin/sources/" + S.candKind + "/candidates" + candQuery(wantProbe, wantBalance, wantChat));`,
		`data-action="zcode-chat"`,
		`if (!confirm("这会真实调用一次最小 chat，可能消耗极少量 token。只探一次，不会自动轮询。继续？")) return;`,
		`return reloadCands(false, false, false, true);`,
		`!S.candProbe && !S.candBalance && !S.candChat`,
		`await reloadCands(false, false, true, false);`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("静态 chat 的显式/非轮询边界缺失：%s", want)
		}
	}
	if strings.Contains(body, `var wantChat = !!chat || !!S.candChat`) {
		t.Error("candChat 不能继承：后台刷新会反复触发真实计费 chat")
	}
}

// TestZCodeAppCandidatesUseEnhancedViewAndDynamicTitle zcode-app 与 zcode-switch
// 共用增强候选表；标题必须按实际 kind 显示，不能写死成 zcode-switch。
func TestZCodeAppCandidatesUseEnhancedViewAndDynamicTitle(t *testing.T) {
	body := scriptBody(t)
	for _, want := range []string{
		`S.candKind === "zcode-switch" || S.candKind === "zcode-app"`,
		`return card("候选账号 · " + S.candKind, esc(S.candNote || ""),`,
		`"zcode-app":         { tab: "app",    method: "app",    detail: "ZCode 应用登录态" },`,
		`if (k.id !== "zcode-app" && k.id !== "zcode-switch"){`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("zcode-app 前端映射缺失：%s", want)
		}
	}
}
func TestModelGroupAndFoldTitlesUseCSSDisclosureCaret(t *testing.T) {
	css := extractCSS(t)
	body := scriptBody(t)

	for _, want := range []string{
		`\.mgroup \.mcaret\{[^}]*width:5px[^}]*height:5px[^}]*border-top:1\.5px solid currentColor[^}]*border-right:1\.5px solid currentColor[^}]*transform:rotate\(45deg\)`,
		`\.mgroup\.open \.mcaret\{[^}]*transform:rotate\(135deg\)`,
		`\.card\.fold h2 \.mcaret\{[^}]*transform:rotate\(45deg\)`,
		`\.card\.fold\.open h2 \.mcaret\{[^}]*transform:rotate\(135deg\)`,
	} {
		if !regexp.MustCompile(want).MatchString(css) {
			t.Errorf("折叠箭头应使用 CSS 绘制的细箭头：%s", want)
		}
	}
	for _, forbidden := range []string{`\u25B8`, `\u25BE`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("折叠标题不能在正文里拼 Unicode 三角字符：%s", forbidden)
		}
	}
	for _, want := range []string{
		`<h2><span class="mcaret" aria-hidden="true"></span>' + esc(title)`,
		`<span><span class="mcaret" aria-hidden="true"></span><b>' + esc(groupName)`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("折叠箭头 DOM 结构缺失或选择器被改坏：%s", want)
		}
	}
}

// TestModelGroupsAreJoinedWithoutCommaSeparators mList 的 rows 是数组；
// 如果直接拼进 HTML，JS 会按 Array.prototype.toString() 用逗号连接，
// 在每两个来源分组之间渲染出一个独立逗号。必须显式 .join("")。
func TestModelGroupsAreJoinedWithoutCommaSeparators(t *testing.T) {
	body := scriptBody(t)
	if !strings.Contains(body, `id="mList">' + rows.join("") + '</div>'`) {
		t.Error("模型分组数组必须显式 join(\"\") 后再拼进 #mList，否则分组之间会多出逗号")
	}
	if strings.Contains(body, `id="mList">' + rows + '</div>'`) {
		t.Error("模型分组数组又被直接拼进 #mList：数组会隐式转成逗号分隔字符串")
	}
}

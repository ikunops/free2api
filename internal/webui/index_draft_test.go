package webui

import (
	"regexp"
	"strings"
	"testing"
)

// scriptBody 取出 index.html 里的 <script> 正文（去掉标签本身）。
func scriptBody(t *testing.T) string {
	t.Helper()
	html := string(Page())
	start := strings.Index(html, "<script>")
	if start < 0 {
		t.Fatal("index.html 里找不到 <script> 块")
	}
	body := html[start+len("<script>"):]
	if end := strings.Index(body, "</script>"); end >= 0 {
		body = body[:end]
	}
	return body
}

// extractCSS 把页面里所有 <style> 块拼起来返回，供样式层断言用。
// 为什么要这个辅助：样式断言如果只 grep 整个 HTML，<script> 里的字符串字面量
// （比如 CSS 类名出现在 JS 拼接里）会造成假阳性 —— 命中了但样式其实没生效。
// 只取 <style> 内容才是样式层的事实来源。
func extractCSS(t *testing.T) string {
	t.Helper()
	html := string(Page())
	var b strings.Builder
	rest := html
	for {
		i := strings.Index(rest, "<style>")
		if i < 0 {
			break
		}
		rest = rest[i+len("<style>"):]
		j := strings.Index(rest, "</style>")
		if j < 0 {
			break
		}
		b.WriteString(rest[:j])
		b.WriteString("\n")
		rest = rest[j+len("</style>"):]
	}
	if b.Len() == 0 {
		t.Fatal("index.html 里找不到 <style> 块")
	}
	return b.String()
}
// TestCodexInputsAreDraftBacked Codex 一键接入的输入框必须走草稿，否则后台轮询
// render() 会把用户敲的值顶回 suggested_*。
//
// 症状：Base URL 改成 7871，点「预览改动」左边仍是 7864，看起来像保存不生效。
// 真正原因：codexPreview 只在点击那一刻读一次 DOM，而 30s 一次的状态轮询
// render() 整页 —— 输入框按 suggested_base_url 重填后，用户的值早就没了。
// 与 captureChanDraft 同一个病根，所以用同一个解法（render 前收草稿 + 渲染时读草稿）。
func TestCodexInputsAreDraftBacked(t *testing.T) {
	body := scriptBody(t)

	for _, fn := range []string{"function captureCodexDraft", "function saveMgroupOpen", "function loadMgroupOpen"} {
		if !strings.Contains(body, fn) {
			t.Errorf("缺少 %s —— 草稿/持久化逻辑被删掉会让未保存的输入与展开状态被轮询重画冲掉", fn)
		}
	}

	// render() 必须在重画前调 captureCodexDraft
	re := regexp.MustCompile(`(?s)function render\(opt\)\{.{0,900}?captureCodexDraft\(\)`)
	if !re.MatchString(body) {
		t.Error("render() 里没调 captureCodexDraft()：输入框会在每次轮询重画时被 suggested_* 顶掉")
	}

	// codexBase 的 value 必须优先读草稿，否则草稿收集了也白收集
	re2 := regexp.MustCompile(`id="codexBase" value="'\s*\+\s*esc\((S\.codexDraft[\s\S]{0,120}?suggested_base_url)`)
	if !re2.MatchString(body) {
		t.Error(`#codexBase 的 value 没有优先读 S.codexDraft：草稿收集了但渲染时不用，等于没修`)
	}

	// 写入成功后必须清草稿，否则旧值会一直盖住服务端的新建议值
	re3 := regexp.MustCompile(`(?s)S\.codexPlan = null;.{0,200}?S\.codexDraft = null;`)
	if !re3.MatchString(body) {
		t.Error("codexApply 成功后没清 S.codexDraft：旧草稿会一直盖住服务端的新建议值")
	}
}

// TestModelGroupOpenIsPersisted 分组展开状态必须落 localStorage。
//
// 之前只在内存 S.mgroupOpen 里，用户点开一个来源、勾几个模型，后台 30s 轮询
// render() 一次就按空状态重画 —— 每次点击都生效，但用户看到的是「刚展开又自己合上」。
func TestModelGroupOpenIsPersisted(t *testing.T) {
	body := scriptBody(t)

	// toggle 与批量展开都要落盘
	if n := strings.Count(body, "saveMgroupOpen()"); n < 3 {
		t.Errorf("saveMgroupOpen() 只出现 %d 次（定义 1 + toggle 1 + 批量 1 = 3）：有路径没落盘，展开状态仍会丢", n)
	}
	// boot 时要读回来
	re := regexp.MustCompile(`(?s)boot\(\)\{[\s\S]{0,900}?S\.mgroupOpen = loadMgroupOpen\(\)`)
	if !re.MatchString(body) {
		t.Error("boot() 里没有 S.mgroupOpen = loadMgroupOpen()：刷新页面展开状态就回到默认")
	}
	// 收起筛选态下的强开，否则点分组头看起来「没反应」
	re2 := regexp.MustCompile(`(?s)mgroup-toggle.[\s\S]{0,1000}?classList\.contains\("searching"\)`)
	if !re2.MatchString(body) {
		t.Error("mgroup-toggle 没处理 .searching 强开态：筛选期间点分组头收起不了（display:block!important 盖过 .open）")
	}
}
package webui

import (
	"regexp"
	"strings"
	"testing"
)

// TestSortArrowIsNotInTextFlow 排序箭头必须绝对定位，不能拼进表头文本节点。
//
// 现象：点「请求」或「成功」表头时，箭头像一个普通字符那样参与排版，把这一列
// 撑宽、后面所有列整体左移一截 —— 明明左边模型列有富余空间，整张表还是横向抖。
//
// 原因：旧的 statsTh 拼的是 `esc(label) + " ▲"`。箭头进了文本流，列宽就随它变。
// 修法是绝对定位（th.sorth{position:relative} + .sar{position:absolute}），
// 箭头落在 padding-right 预留出的空位里，列宽恒定。
func TestSortArrowIsNotInTextFlow(t *testing.T) {
	body := scriptBody(t)

	// 1) 渲染时箭头走独立 span，而不是拼进文本
	if regexp.MustCompile(`esc\(label\)\s*\+\s*arrow`).MatchString(body) {
		t.Error("statsTh 还在把箭头拼进文本节点（label + arrow）——箭头会参与排版，点表头时整张表会位移")
	}
	if !strings.Contains(body, `<span class="sar">`) {
		t.Error("statsTh 没有输出独立的 .sar 箭头元素")
	}

	// 2) CSS：th.sorth 必须 relative，.sar 必须 absolute
	css := extractCSS(t)
	if !strings.Contains(css, "th.sorth") || !strings.Contains(css, "position:relative") {
		t.Error("th.sorth 缺 position:relative，绝对定位的箭头会相对更外层定位")
	}
	re := regexp.MustCompile(`(?s)th\.sorth \.sar\{[^}]*position:absolute`)
	if !re.MatchString(css) {
		t.Error(".sar 没有 position:absolute —— 箭头会回到文本流里，重新引入位移")
	}

	// 3) 必须给箭头预留位置，否则会压在文字上
	if !regexp.MustCompile(`th\.sorth\{[^}]*padding-right`).MatchString(css) {
		t.Error("th.sorth 缺 padding-right 预留箭头位置，绝对定位后会压住列标题")
	}

	// 4) 排序表用 fixed 布局：列宽与内容解耦，长模型名也不会把数字列挤歪
	if !strings.Contains(body, `<table class="sortable">`) {
		t.Error("排序表缺 class=sortable，无法套用 table-layout:fixed")
	}
	if !strings.Contains(css, "table.sortable{table-layout:fixed") {
		t.Error("table.sortable 缺 table-layout:fixed —— 列宽仍随内容变化")
	}
}

// TestSortableColumnClassesMatchColumns 语义 class 与列一一对应。
//
// 这条是防回归：曾经用 nth-child 定宽，一旦往表里插列就会静默指错列（表现是
// 某列突然被截断，而代码看起来完全正常）。改成 class 后，class 跟着列语义走。
func TestSortableColumnClassesMatchColumns(t *testing.T) {
	body := scriptBody(t)
	for _, pair := range [][2]string{
		{"Tokens", "wtok"},
		{"缓存命中", "wcache"},
		{"费率", "wrate"},
		{"最近", "wtime"},
	} {
		if !strings.Contains(body, pair[1]) {
			t.Errorf("列 %s 缺语义 class %s：靠 nth-child 定宽会在插列后静默错位", pair[0], pair[1])
		}
	}
	// td 与 th 两侧都要带，否则只有表头定宽、数据行照旧撑开
	css := extractCSS(t)
	for _, c := range []string{"wtok", "wcache", "wrate", "wtime"} {
		re := regexp.MustCompile(`(?s)table\.sortable th\.` + c + `,table\.sortable td\.` + c)
		if !re.MatchString(css) {
			t.Errorf("CSS 只定了一侧宽度：th.%s 与 td.%s 必须一起写", c, c)
		}
	}
}
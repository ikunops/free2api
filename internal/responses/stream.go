package responses

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"free2api/internal/upstream"
)

// StreamChat 读上游 Chat SSE 流（rc），按 OpenAI Responses 事件序列写给 w。
//
// 事件序列（与官方 Responses 流同构）：
//
//	response.created
//	→ [response.output_item.added → response.content_part.added
//	   → response.output_text.delta… → response.output_text.done
//	   → response.content_part.done → response.output_item.done]        正文
//	→ [response.output_item.added → response.function_call_arguments.delta…
//	   → response.function_call_arguments.done → response.output_item.done] 工具调用
//	→ response.completed
//
// 上游 error 帧（{"error":{...}}）→ response.failed（error 原文 + gateway_hint）。
//
// 刻意**不发** data: [DONE]：那是 Chat 的收尾约定，官方 Responses 流不写它，
// Codex 的解析器把每个 data 行当 JSON 解析，多一个 [DONE] 只会报
// "Failed to parse SSE event"。终止信号就是 response.completed / response.failed。
//
// 上游 delta 里若带 reasoning_content（DeepSeek 思维链），本函数**丢弃**：
// Chat 的上游既然只给 chat 形态，就没有可靠的 Responses reasoning item 形态可转；
// 宁可少显示一段思考过程，也不要发一个解析不过的事件把整条流打断。
func StreamChat(w http.ResponseWriter, rc io.Reader, model string, hint func(string) string) error {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)

	e := &encoder{
		w: w, fl: fl, model: model, hint: hint,
		respID: newID("resp"), created: time.Now().Unix(),
		calls: map[int]*streamItem{},
	}
	if err := e.emitCreated(); err != nil {
		return err
	}

	br := bufio.NewReaderSize(rc, 64*1024)
	valid := 0
	for {
		line, err := br.ReadString('\n')
		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "data: [DONE]"):
			// 上游显式收尾：后面的内容（含垃圾帧）一律不再读。
			// 注意不置 valid：只有 [DONE] 而没有数据帧 = 空流（与 chat 侧 StreamHint 同判据）。
			goto drained
		case strings.HasPrefix(trimmed, "data: "):
			n, werr := e.frame(strings.TrimPrefix(trimmed, "data: "))
			valid += n
			if werr != nil {
				return werr
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
drained:
	if e.failed {
		return nil // 已经给客户端发过 response.failed，失败语义完整
	}
	if valid == 0 {
		// 上游 200 但没有任何有效帧（空流）：给一个明确的失败事件，并返回与
		// chat 侧同源的「空流」哨兵——handler 据此把本次请求记成 502 观测。
		_ = e.emitFailed(map[string]any{
			"type":    "upstream_error",
			"code":    "upstream_parse",
			"message": "empty upstream stream",
		})
		return upstream.EmptyStreamError()
	}
	return e.finish()
}

// streamItem 一个已开始但未结束的 output item（正文或工具调用）。
type streamItem struct {
	index  int    // output_index
	kind   string // "message" | "function_call"
	id     string // item id（msg_… / fc_…）
	callID string
	name   string
	buf    strings.Builder
	done   bool
}

// encoder Responses 事件流的编码状态机。
type encoder struct {
	w     http.ResponseWriter
	fl    http.Flusher
	model string
	hint  func(string) string

	seq     int
	respID  string
	created int64

	open   []*streamItem       // 已 added、未 done 的条目（按加入顺序）
	items  []any               // 已 done 的条目 → response.output
	text   *streamItem         // 当前正文条目（nil = 还没有正文）
	calls  map[int]*streamItem // Chat tool_calls index → 条目
	usage  map[string]any      // 上游末帧 usage 原文（Chat 形态）
	failed bool
}

// emit 写一个 SSE 事件：event: <type> / data: <json>，并立即 flush。
func (e *encoder) emit(typ string, body map[string]any) error {
	body["type"] = typ
	body["sequence_number"] = e.seq
	e.seq++
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(e.w, "event: "+typ+"\ndata: "); err != nil {
		return err
	}
	if _, err := e.w.Write(raw); err != nil {
		return err
	}
	if _, err := io.WriteString(e.w, "\n\n"); err != nil {
		return err
	}
	if e.fl != nil {
		e.fl.Flush()
	}
	return nil
}

// responseObj 组装 response 对象（created / in_progress / completed / failed 共用）。
func (e *encoder) responseObj(status string, usage map[string]any) map[string]any {
	out := e.items
	if out == nil {
		out = []any{}
	}
	if usage == nil {
		usage = zeroUsage()
	}
	return map[string]any{
		"id":                  e.respID,
		"object":              "response",
		"created_at":          e.created,
		"status":              status,
		"model":               e.model,
		"output":              out,
		"usage":               usage,
		"error":               nil,
		"incomplete_details":  nil,
		"parallel_tool_calls": true,
		"tool_choice":         "auto",
		"tools":               []any{},
		"metadata":            map[string]any{},
	}
}

// usageResponses 把累积的上游 usage 转成 Responses 口径（缺观测 → 全零）。
func (e *encoder) usageResponses() map[string]any {
	if len(e.usage) == 0 {
		return nil
	}
	return usageFromChat(map[string]any{"usage": e.usage})
}

// emitCreated 首事件：response.created（带完整 response 骨架）。
func (e *encoder) emitCreated() error {
	return e.emit("response.created", map[string]any{
		"response": e.responseObj("in_progress", nil),
	})
}

// emitFailed 失败事件：response.failed（error 对象带上游原文 + 可选 gateway_hint）。
func (e *encoder) emitFailed(errObj map[string]any) error {
	e.failed = true
	if errObj == nil {
		errObj = map[string]any{"message": "upstream error"}
	}
	if _, ok := errObj["type"]; !ok {
		errObj["type"] = "upstream_error"
	}
	return e.emit("response.failed", map[string]any{
		"response": e.responseObj("failed", nil),
		"error":    errObj,
	})
}

// frame 处理一帧上游 Chat SSE payload；返回「有效帧数」（0 或 1，用于空流判定）。
func (e *encoder) frame(payload string) (int, error) {
	var obj map[string]any
	if json.Unmarshal([]byte(payload), &obj) != nil {
		return 0, nil // 非 JSON 帧：Responses 侧没有原样透传的余地，丢弃
	}
	// 上游 error 帧（error-passthrough）：转成 response.failed 并终止。
	if eobj, ok := obj["error"].(map[string]any); ok {
		if hint := e.frameHint(payload); hint != "" {
			eobj["gateway_hint"] = hint
		}
		return 1, e.emitFailed(eobj)
	}
	// usage 可能出现在任意帧（stream_options.include_usage 下是末帧）。
	if u, ok := obj["usage"].(map[string]any); ok && len(u) > 0 {
		e.usage = u
	}
	choices, _ := obj["choices"].([]any)
	if len(choices) == 0 {
		return 0, nil
	}
	c, _ := choices[0].(map[string]any)
	if c == nil {
		return 0, nil
	}
	delta, _ := c["delta"].(map[string]any)
	if delta == nil {
		return 0, nil
	}
	if err := e.delta(delta); err != nil {
		return 0, err
	}
	return 1, nil
}

// frameHint 惰性求值上游 error 帧的 gateway_hint（panic 隔离，绝不打断主路径）。
func (e *encoder) frameHint(payload string) string {
	if e.hint == nil {
		return ""
	}
	defer func() { _ = recover() }()
	return strings.TrimSpace(e.hint(payload))
}

// delta 处理 chat 的一个 delta 对象。
func (e *encoder) delta(d map[string]any) error {
	// 正文增量。
	if txt, ok := d["content"].(string); ok && txt != "" {
		if err := e.ensureText(); err != nil {
			return err
		}
		e.text.buf.WriteString(txt)
		return e.emit("response.output_text.delta", map[string]any{
			"item_id":       e.text.id,
			"output_index":  e.text.index,
			"content_index": 0,
			"delta":         txt,
		})
	}
	// 工具调用增量。
	tcs, _ := d["tool_calls"].([]any)
	for _, raw := range tcs {
		tc, _ := raw.(map[string]any)
		if tc == nil {
			continue
		}
		if err := e.toolDelta(tc); err != nil {
			return err
		}
	}
	return nil
}

// ensureText 确保正文条目已 added（首次写正文时创建 message item + content part）。
func (e *encoder) ensureText() error {
	if e.text != nil {
		return nil
	}
	it := &streamItem{index: len(e.open), kind: "message", id: newID("msg")}
	e.open = append(e.open, it)
	e.text = it
	if err := e.emit("response.output_item.added", map[string]any{
		"output_index": it.index,
		"item": map[string]any{
			"id": it.id, "type": "message", "role": "assistant",
			"status": "in_progress", "content": []any{},
		},
	}); err != nil {
		return err
	}
	return e.emit("response.content_part.added", map[string]any{
		"item_id":       it.id,
		"output_index":  it.index,
		"content_index": 0,
		"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
}

// toolDelta 处理一片 tool_call（Chat 形态：首片带 index/id/name，后续只带 arguments）。
func (e *encoder) toolDelta(tc map[string]any) error {
	idx := 0
	if v, ok := tc["index"].(float64); ok {
		idx = int(v)
	}
	it := e.calls[idx]
	if it == nil {
		it = &streamItem{
			index:  len(e.open),
			kind:   "function_call",
			id:     newID("fc"),
			callID: toolCallID(tc),
			name:   toolCallName(tc),
		}
		e.calls[idx] = it
		e.open = append(e.open, it)
		if err := e.emit("response.output_item.added", map[string]any{
			"output_index": it.index,
			"item": map[string]any{
				"id": it.id, "type": "function_call", "call_id": it.callID,
				"name": it.name, "arguments": "", "status": "in_progress",
			},
		}); err != nil {
			return err
		}
	}
	// 后续分片可能补上真实 id / name（首片缺省时）。
	if v, _ := tc["id"].(string); v != "" {
		it.callID = v
	}
	if v := toolCallName(tc); v != "" {
		it.name = v
	}
	args := toolCallArgs(tc)
	if args == "" {
		return nil
	}
	it.buf.WriteString(args)
	return e.emit("response.function_call_arguments.delta", map[string]any{
		"item_id":      it.id,
		"output_index": it.index,
		"delta":        args,
	})
}

// finishItem 收尾一个条目（发 *.done 事件 + output_item.done，并计入 response.output）。
func (e *encoder) finishItem(it *streamItem) error {
	if it == nil || it.done {
		return nil
	}
	it.done = true
	text := it.buf.String()
	if it.kind == "message" {
		if err := e.emit("response.output_text.done", map[string]any{
			"item_id": it.id, "output_index": it.index,
			"content_index": 0, "text": text,
		}); err != nil {
			return err
		}
		if err := e.emit("response.content_part.done", map[string]any{
			"item_id": it.id, "output_index": it.index, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		}); err != nil {
			return err
		}
		e.items = append(e.items, messageItemOut(it.id, text))
		return e.emitOutputDone(it, messageItemOut(it.id, text))
	}
	if err := e.emit("response.function_call_arguments.done", map[string]any{
		"item_id": it.id, "output_index": it.index,
		"name": it.name, "arguments": text,
	}); err != nil {
		return err
	}
	item := functionCallItem(it.id, it.callID, it.name, text, "completed")
	e.items = append(e.items, item)
	return e.emitOutputDone(it, item)
}

// emitOutputDone 发 response.output_item.done。
func (e *encoder) emitOutputDone(it *streamItem, item map[string]any) error {
	item["status"] = "completed"
	return e.emit("response.output_item.done", map[string]any{
		"output_index": it.index,
		"item":         item,
	})
}

// finish 收尾整条流：关闭所有未完成条目，发 response.completed。
func (e *encoder) finish() error {
	for _, it := range e.open {
		if err := e.finishItem(it); err != nil {
			return err
		}
	}
	return e.emit("response.completed", map[string]any{
		"response": e.responseObj("completed", e.usageResponses()),
	})
}

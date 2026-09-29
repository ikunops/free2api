// sink.go 出口编码器：同一套号池/轮转/重试主干（见 handler.chatEndpoint），
// 按用户选的「输出格式」把上游 Chat 结果编码成对外协议。
//
// 为什么要有这层：Codex CLI 只认 Responses（wire_api = "responses"），
// 而 WorkBuddy 上游只认 Chat Completions。此前这条翻译由外部 CLIProxyAPI（8317）
// 承担——多一个进程、多一个端口、多一跳本机转发。把编码器做成可插拔的接口后，
// 「一个进程、两种协议出口」是同一份主干代码的两种出口形态，不是两套实现。
package server

import (
	"io"
	"net/http"

	"free2api/internal/responses"
	"free2api/internal/upstream"
)

// chatSink 出口编码器。hint 是上游 error 帧的 gateway_hint 判定（懒求值；可 nil）。
type chatSink interface {
	// Stream 把上游 Chat SSE 流编码成对外流式响应。
	Stream(w http.ResponseWriter, rc io.Reader, hint func(string) string) error
	// Complete 把聚合后的 chat.completion 编码成对外响应体。
	Complete(resp map[string]any) (any, error)
}

// chatSinkOpenAI 现状协议：上游 chat.completion / chat SSE 原样透传
// （含既有的帧规范化 + gateway_hint 附加，见 upstream.StreamHint）。
type chatSinkOpenAI struct{}

func (chatSinkOpenAI) Stream(w http.ResponseWriter, rc io.Reader, hint func(string) string) error {
	return upstream.StreamHint(w, rc, hint)
}

func (chatSinkOpenAI) Complete(resp map[string]any) (any, error) { return resp, nil }

// chatSinkResponses Responses 协议：把上游 Chat 翻译成 Responses 对象 / 事件流。
// model 是客户端**原始**请求里的模型名（含 realm/producer/输出前缀），原样回显。
type chatSinkResponses struct{ model string }

func (s chatSinkResponses) Stream(w http.ResponseWriter, rc io.Reader, hint func(string) string) error {
	return responses.StreamChat(w, rc, s.model, hint)
}

func (s chatSinkResponses) Complete(resp map[string]any) (any, error) {
	return responses.FromChat(resp, s.model), nil
}

// growth_chat.go 真实 Web 对话（成长任务用）。
//
// 与 ChatStreamContext 的区分：那条是网关对外提供的 WorkBuddy 聊天上游，服务于任意
// 客户端模型；这里是成长任务专用的 Web 控制台链路，会先创建 conversationId，再走
// /console/chat/completions 读取真实回复。任务事件必须建立在真实回复上，不能只报
// chat_request_send 冒充一次对话。
package upstream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"free2api/internal/auth"
)

const (
	growthWebConversationsPath = "/console/webchat/conversations"
	growthWebChatPath          = "/console/chat/completions"
	growthWebBase              = "https://www.workbuddy.cn"
)

// GrowthChatReply 是一次真实 Web 对话的结果。
type GrowthChatReply struct {
	ConversationID string
	Text           string
}

// CreateGrowthWebConversation 新建任务专用会话。返回上游 conversationId；空值报错，
// 避免后续把空 id 写入事件导致服务端静默丢弃。
func (c *Client) CreateGrowthWebConversation(ctx context.Context, a *auth.Auth, name string) (string, error) {
	base := growthWebBaseFor(c)
	if strings.TrimSpace(name) == "" {
		name = "growth-" + NewGrowthConversationID("conv")
	}
	body, _ := json.Marshal(map[string]any{"name": name})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+growthWebConversationsPath, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	c.growthWebHeaders(req, a, base, "/profile/growth-center")
	data, err := c.doJSON(req)
	if err != nil {
		return "", err
	}
	var resp struct {
		ConversationID  string `json:"conversationId"`
		ConversationID2 string `json:"conversation_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", err
	}
	id := strings.TrimSpace(resp.ConversationID)
	if id == "" {
		id = strings.TrimSpace(resp.ConversationID2)
	}
	if id == "" {
		return "", fmt.Errorf("webchat conversation id missing")
	}
	return id, nil
}

// GrowthWebChat 发起真实 Web 对话并读取 SSE 正文。_meta 只在专家/专家团任务需要时传入。
// 返回的 Text 为空会被视为失败（HTTP 200 空流不应计入任务完成）。
func (c *Client) GrowthWebChat(ctx context.Context, a *auth.Auth, prompt, model string, meta map[string]any) (GrowthChatReply, error) {
	if strings.TrimSpace(prompt) == "" {
		prompt = "你好，请回复 OK"
	}
	if strings.TrimSpace(model) == "" {
		model = "glm-5.2"
	}
	if growthWebExpertMeta(meta) {
		// 专家会话的真实对话优先走网关的 /v1/chat/completions，由网关把
		// 展示/工具模型名解析到账号池真实模型；直连上游仍用于非专家任务。
		if base := c.GrowthGatewayBase(); base != "" {
			return c.growthGatewayWebChat(ctx, base, a, prompt, model, meta)
		}
	}
	convID, err := c.CreateGrowthWebConversation(ctx, a, "growth-"+model)
	if err != nil {
		return GrowthChatReply{}, err
	}
	payload := map[string]any{
		"messages":       []map[string]any{{"role": "user", "content": prompt}},
		"model":          model,
		"stream":         true,
		"conversationId": convID,
	}
	if len(meta) > 0 {
		payload["_meta"] = meta
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return GrowthChatReply{}, err
	}
	base := growthWebBaseFor(c)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+growthWebChatPath, bytes.NewReader(raw))
	if err != nil {
		return GrowthChatReply{}, err
	}
	c.growthWebHeaders(req, a, base, "/")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("x-codebuddy-request", "1")
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		return GrowthChatReply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return GrowthChatReply{}, &Error{Kind: Classify(resp.StatusCode, string(b)), Status: resp.StatusCode, Msg: truncate(string(b), 200)}
	}
	text, err := readGrowthChatSSE(resp.Body)
	if err != nil {
		return GrowthChatReply{}, err
	}
	if strings.TrimSpace(text) == "" {
		return GrowthChatReply{}, fmt.Errorf("webchat returned empty response")
	}
	return GrowthChatReply{ConversationID: convID, Text: text}, nil
}

// GrowthGatewayBase 是可选的本地网关地址（config upstream.gateway_base）。
// 空值时不启用网关回环，保持原有直连行为与测试隔离。
func (c *Client) GrowthGatewayBase() string {
	return strings.TrimRight(strings.TrimSpace(c.GatewayBase), "/")
}
func growthWebExpertMeta(meta map[string]any) bool {
	if len(meta) == 0 {
		return false
	}
	_, ok := meta["codebuddy.ai"]
	return ok
}

// growthGatewayWebChat 用网关的 OpenAI 兼容 chat 端点执行专家会话。
// 网关已持有真实账号与模型路由；这里不再自己猜上游 service id。
func (c *Client) growthGatewayWebChat(ctx context.Context, base string, a *auth.Auth, prompt, model string, meta map[string]any) (GrowthChatReply, error) {
	convID, err := c.CreateGrowthWebConversation(ctx, a, "growth-"+model)
	if err != nil {
		return GrowthChatReply{}, err
	}
	payload := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]any{{"role": "user", "content": prompt}},
	}
	// /v1/chat/completions 不接受官方控制台的 _meta；专家上下文由网关请求的
	// upstream/账户路由负责，任务事件仍以此为真实对话凭据。
	raw, err := json.Marshal(payload)
	if err != nil {
		return GrowthChatReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return GrowthChatReply{}, err
	}
	if c.GatewayAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.GatewayAPIKey)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.chatHTTP().Do(req)
	if err != nil {
		return GrowthChatReply{}, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return GrowthChatReply{}, &Error{Kind: Classify(resp.StatusCode, string(data)), Status: resp.StatusCode, Msg: truncate(string(data), 200)}
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return GrowthChatReply{}, err
	}
	if len(out.Choices) == 0 || strings.TrimSpace(out.Choices[0].Message.Content) == "" {
		return GrowthChatReply{}, fmt.Errorf("gateway webchat returned empty response")
	}
	return GrowthChatReply{ConversationID: convID, Text: out.Choices[0].Message.Content}, nil
}
func readGrowthChatSSE(r io.Reader) (string, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	var out strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "data:") {
			d := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if d == "[DONE]" || d == "[完成]" {
				break
			}
			var frame map[string]any
			if json.Unmarshal([]byte(d), &frame) == nil {
				appendGrowthChoiceText(&out, frame)
			}
		}
		if err == io.EOF {
			break
		}
	}
	return out.String(), nil
}

func appendGrowthChoiceText(out *strings.Builder, frame map[string]any) {
	choices, _ := frame["choices"].([]any)
	for _, ch := range choices {
		c, _ := ch.(map[string]any)
		if c == nil {
			continue
		}
		if d, ok := c["delta"].(map[string]any); ok {
			if s, ok := d["content"].(string); ok {
				out.WriteString(s)
			}
		}
		if msg, ok := c["message"].(map[string]any); ok {
			if s, ok := msg["content"].(string); ok {
				out.WriteString(s)
			}
		}
	}
}

func (c *Client) growthWebHeaders(req *http.Request, a *auth.Auth, base, referer string) {
	c.BillingHeaders(req, a)
	req.Header.Set("Origin", base)
	req.Header.Set("Referer", strings.TrimRight(base, "/")+referer)
	req.Header.Set("x-client-platform", "web")
	req.Header.Set("X-Domain", base)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) WorkBuddy/5.5.4 Chrome/138.0.7204.251 Electron/37.10.3 Safari/537.36")
}

func growthWebBaseFor(c *Client) string {
	if c != nil && c.ChatBaseCN != "" && c.ChatBaseCN != "https://copilot.tencent.com" {
		return strings.TrimRight(c.ChatBaseCN, "/")
	}
	return growthWebBase
}

// GrowthWebChatTimeout 给任务调用方一个统一上限；0 表示沿用调用方 ctx。
func GrowthWebChatTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 90*time.Second)
}

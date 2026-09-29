package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"free2api/internal/auth"
)

// errReadBody 模拟读 body 时连接中断/空闲掐流：返回部分数据 + 读错误。
type errReadBody struct {
	r    io.Reader
	read int // 已读次数（0 = 首次读即报错）
}

func (b *errReadBody) Read(p []byte) (int, error) {
	b.read++
	if b.read == 1 {
		return b.r.Read(p)
	}
	return 0, errors.New("connection reset mid-body")
}

func (b *errReadBody) Close() error { return nil }

// readErrResp 构造「HTTP 500 + body 读一半失败」的响应。
// 半截 body 含 hard-credit 文案——若错误被吞掉，半截文本会进 Classify 误判分类。
func readErrResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &errReadBody{r: strings.NewReader(body)},
	}
}

// globalAuth 构造 global 账号（realm 未导出，按域推断）。
func globalAuth() *auth.Auth {
	return &auth.Auth{AccessToken: "at", UID: "g-p0", Domain: "www.workbuddy.ai"}
}

// TestDoJSONReadErrorNotClassified P0-2：读 body 出错时 doJSON 必须返回传输层错误
// （非 *Error 分类结果，不参与账号惩罚），且 Msg 带下划线 read body 前缀（错误链可辨识）。
// 修复前：raw, _ := io.ReadAll 吞掉读错误，半截 body 进 Classify → 500+半截文案被
// 判成 ErrServer（喂熔断误罚号）。
func TestDoJSONReadErrorNotClassified(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return readErrResp(500, `{"code":11128,"msg":"no credit quota exceeded`), nil
	})
	a := &auth.Auth{AccessToken: "at"}
	// 走一个真实 doJSON 调用链（billingJSON → doJSON）。
	_, err := c.billingJSON(a, http.MethodGet, "/v2/billing/meter/get-user-resource", nil)
	if err == nil {
		t.Fatal("want error on body read failure, got nil")
	}
	var ue *Error
	if errors.As(err, &ue) {
		t.Fatalf("read error must NOT be a classified *Error (kind=%s): 分类错误会喂熔断误罚号", ue.Kind)
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("error should carry \"read body\" prefix, got: %v", err)
	}
}

// TestDoJSONReadErrorOnSuccessStatus P0-2 边界：HTTP 200 但 body 读一半断流。
// 修复前：半截 body 进 json.Unmarshal → "parse failed"（看似无害）；但若半截恰好
// 落在 LimitReader 截断边界（1<<20）外，Unmarshal 收到的是合法前缀也可能静默错解。
// 修复后：统一为传输层错误，不产生误判的 parse/classify 分支。
func TestDoJSONReadErrorOnSuccessStatus(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return readErrResp(200, `{"code":0,"data":{"cre`), nil
	})
	a := &auth.Auth{AccessToken: "at"}
	_, err := c.billingJSON(a, http.MethodGet, "/v2/billing/meter/get-user-resource", nil)
	if err == nil {
		t.Fatal("want error on body read failure at 200, got nil")
	}
	var ue *Error
	if errors.As(err, &ue) {
		t.Fatalf("read error at 200 must NOT be classified *Error (kind=%s)", ue.Kind)
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("error should carry \"read body\" prefix, got: %v", err)
	}
}

// TestChatStreamReadErrorNotClassified P0-2（client.go:820 chat 流式路径）：
// 流式路径 ≥400 分支的 io.ReadAll 读失败时，必须返回传输层错误（terr 非 nil），
// 而不是把半截 raw 交回调用方由 handler 再 Classify → applyErrorPolicy 误罚号。
func TestChatStreamReadErrorNotClassified(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return readErrResp(500, `{"code":0,"msg":"rate limit quota`), nil
	})
	rc, status, respBody, terr := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-read-err"})
	if terr == nil {
		t.Fatalf("want transport error on mid-body read failure, got nil (status=%d body=%q)", status, respBody)
	}
	var ue *Error
	if errors.As(terr, &ue) {
		t.Fatalf("chat stream read error must NOT be classified *Error (kind=%s)", ue.Kind)
	}
	if rc != nil {
		rc.Close()
	}
}

// TestFetchModelsReadErrorNotClassified P0-2（client.go:919 其他 doJSON 变体）：
// FetchModels 的 body 读失败应返回普通错误（非 *Error 分类），错误信息含 read body。
func TestFetchModelsReadErrorNotClassified(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return readErrResp(503, `{"code":1,"msg":"parti`), nil
	})
	_, err := c.FetchModels(&auth.Auth{AccessToken: "at", UID: "u1"})
	if err == nil {
		t.Fatal("want error on models body read failure, got nil")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("FetchModels read error should carry \"read body\", got: %v", err)
	}
}

// TestGlobalModelsOnceReadError P0-2（global_models.go:171）：探测端点 body 读失败
// 应返回普通错误（fmt error，非 panic/非静默半截解析）。
func TestGlobalModelsOnceReadError(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return readErrResp(200, `{"code":0,"data":{"models":[{"id":"m`), nil
	})
	_, _, _, _, err := c.globalModelsOnce(globalAuth(), "/v2/enterprises/personal/models")
	if err == nil {
		t.Fatal("want error on probe body read failure, got nil")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("probe read error should carry \"read body\", got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// P0-1：ChatStreamContext shadowed cancel
// ---------------------------------------------------------------------------

// TestChatStreamContextSinglePathNoPanic P0-1：单路径正常流程（cn 账号）。
// 修复前：外层 var cancel 从未赋值——若循环尾 cancel() 可达即 nil-panic。
// 修复后：编译器保证 return，无不可达残留。
func TestChatStreamContextSinglePathNoPanic(t *testing.T) {
	c := testClient(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	rc, status, _, err := c.ChatStreamContext(context.Background(),
		&auth.Auth{AccessToken: "at"}, []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-p01-a"})
	if err != nil || status != 200 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	rc.Close()
}

// TestChatStreamContextFallbackCancelsEachAttempt 断言错误路径的 reqCtx cancel
// 在返回前被调用（P0-1 原语义的收窄版：#119 后 global 单路径 /v2，无 fallback 链；
// 保留对「404 分支显式 cancel」的守卫）。global 账号出站恒 /v2/chat/completions。
func TestChatStreamContextFallbackCancelsEachAttempt(t *testing.T) {
	cancelled := make(chan struct{}, 4)
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			// 监视 reqCtx：错误分支返回前 cancel 应传播到 r.Context()。
			go func() {
				<-r.Context().Done()
				cancelled <- struct{}{}
			}()
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		})},
		ChatBaseCN:     "https://chat.example",
		ChatBaseGlobal: "https://chat-global.example",
		GlobalEnabled:  true,
	}
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })
	// IdleTimeout>0：monitorBody 才包流持有 cancel（Close → cancel）；=0 是发现 20 的
	// 已文档化取舍（取消传播交 http.Transport），不在本测试射程。
	c.IdleTimeout = time.Second

	_, status, _, err := c.ChatStreamContext(context.Background(),
		globalAuth(), []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-p01-b"})
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrNotFound || status != 404 {
		t.Fatalf("chat 404: want status=404 + *Error{not_found}, got status=%d err=%v", status, err)
	}

	// 错误分支 cancel 必须触发（404 分支显式 cancel，不再有 fallback 二次请求）。
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatalf("reqCtx cancel not observed (错误路径泄漏 cancel)")
	}
}

// TestChatStreamContextCompileGuarantee P0-1 静态面：不可达尾代码已删。
// 通过编译期保证 + 运行期单路径失败（/v2 404，#119 后无 fallback）仍正常返回错误，
// 不落入旧的「循环尾 cancel(); return nil,0,nil,nil」假路径（吞错误返回 nil,nil）。
func TestChatStreamContextCompileGuarantee(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
			return jsonResp(404, `{"code":404,"msg":"not found"}`), nil
		})},
		ChatBaseCN:     "https://chat.example",
		ChatBaseGlobal: "https://chat-global.example",
		GlobalEnabled:  true,
	}
	auth.SetGlobalEnabled(true)
	t.Cleanup(func() { auth.SetGlobalEnabled(true) })

	_, status, _, err := c.ChatStreamContext(context.Background(),
		globalAuth(), []byte(`{"model":"m"}`), "", ChatMeta{ConversationRequestID: "req-p01-c"})
	// 404 错误路径现在返回已分类 *Error（Kind=ErrNotFound）；旧「不可达代码返回
	// (nil,0,nil,nil) 吞错」的形态是 status=0 + err=nil，二者均不再出现。
	var ue *Error
	if !errors.As(err, &ue) || ue.Kind != ErrNotFound || status != 404 {
		t.Fatalf("both paths 404: want status=404 + *Error{not_found}, got status=%d err=%v", status, err)
	}
}

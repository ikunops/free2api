package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// errReadBody 模拟 body 读一半断流（与 upstream 侧同模式，login 是独立包）。
type errReadBody struct {
	r    io.Reader
	read int
}

func (b *errReadBody) Read(p []byte) (int, error) {
	b.read++
	if b.read == 1 {
		return b.r.Read(p)
	}
	return 0, errors.New("connection reset mid-body")
}

func (b *errReadBody) Close() error { return nil }

// TestDoJSONReadError P0-2（cmd/login/main.go:100）：doJSON 吞掉 io.ReadAll 错误。
// 修复前：半截 body 进 json.Unmarshal → "parse failed"（错误语义失真，误指向上游格式
// 而非网络中断）；修复后：返回带 "read body" 的传输层错误。
func TestDoJSONReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		// 先写半截再断连接：模拟连接中断掐流。
		w.Write([]byte(`{"code":0,"data":{"acces`))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// httptest 无法从 handler 内直接断流，改用 panic(http.ErrAbortHandler) 模拟。
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	_, _, err := doJSON(srv.Client(), http.MethodGet, srv.URL+"/v2/plugin/auth/state", nil, nil)
	if err == nil {
		t.Fatal("want error on aborted body, got nil")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("login doJSON read error should carry \"read body\", got: %v", err)
	}
}

// TestDoJSONReadErrorWithFailingReader 直接注入读失败的 body（不依赖网络断流语义）。
func TestDoJSONReadErrorWithFailingReader(t *testing.T) {
	// 构造自定义 RoundTripper 返回半截 body + 读错误。
	client := &http.Client{Transport: failingBodyTransport{}}
	_, _, err := doJSON(client, http.MethodGet, "https://example.invalid/state", nil, nil)
	if err == nil {
		t.Fatal("want error on failing body read, got nil")
	}
	if !strings.Contains(err.Error(), "read body") {
		t.Errorf("want \"read body\" prefix, got: %v", err)
	}
}

// failingBodyTransport 返回 200 + 读一半报错的 body。
type failingBodyTransport struct{}

func (failingBodyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       &errReadBody{r: strings.NewReader(`{"code":0,"msg":"ok partial`)},
	}, nil
}

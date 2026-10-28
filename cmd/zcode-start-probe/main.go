package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"free2api/internal/source"
	"free2api/internal/upstream"
)

const startBase = "https://zcode.z.ai/api/v1/zcode-plan/anthropic/v1/messages"

func main() {
	mode := os.Getenv("ZP_MODE")
	model := os.Getenv("ZP_MODEL")
	cred, err := source.ReadZCodeAppQuotaCred("")
	if err != nil {
		fail("read_credentials", err)
	}
	if strings.TrimSpace(cred.JWT) == "" {
		fail("missing_jwt", fmt.Errorf("empty jwt"))
	}
	if strings.TrimSpace(cred.DeviceMID) == "" {
		fail("missing_device", fmt.Errorf("empty device mid"))
	}
	out := map[string]any{"mode": mode, "checked_at": time.Now().Format(time.RFC3339), "credential_source": cred.Source, "jwt_length": len(cred.JWT), "device_mid_length": len(cred.DeviceMID), "model": model}
	switch mode {
	case "balance":
		b, err := upstream.New().FetchZCodeBalance(context.Background(), cred.JWT, cred.DeviceMID)
		if err != nil {
			out["ok"] = false
			out["error"] = err.Error()
			write(out)
			os.Exit(2)
		}
		out["ok"] = true
		out["plans"] = b.Plans
		out["balances"] = b.Balances
		out["entitled_models"] = b.EntitledModels()
		out["raw"] = b.Raw
	case "chat":
		r, err := chat(cred, model)
		if err != nil {
			out["ok"] = false
			out["error"] = err.Error()
			write(out)
			os.Exit(2)
		}
		out["ok"] = true
		for k, v := range r {
			out[k] = v
		}
	default:
		fail("bad_mode", fmt.Errorf("%s", mode))
	}
	write(out)
}

func chat(cred source.ZCodeQuotaCred, model string) (map[string]any, error) {
	sid := "2f6f0cf2-6d31-4f7a-8d7b-4a68a64c2b7d"
	body := map[string]any{"model": model, "max_tokens": 32, "stream": true, "messages": []any{map[string]any{"role": "user", "content": "Reply with exactly: pong"}}}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, startBase, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.JWT)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("HTTP-Referer", "https://zcode.z.ai")
	req.Header.Set("User-Agent", "ZCode/3.16.3 ai-sdk/anthropic/3.0.81")
	req.Header.Set("X-ZCode-App-Version", "3.16.3")
	req.Header.Set("X-Title", "Z Code@cli")
	req.Header.Set("X-Release-Channel", "production")
	req.Header.Set("X-Client-Language", "zh-CN")
	req.Header.Set("X-Client-Timezone", "Asia/Shanghai")
	req.Header.Set("X-ZCode-Agent", "glm")
	req.Header.Set("X-Platform", "win32-x64")
	req.Header.Set("X-Os-Category", "windows")
	req.Header.Set("X-Os-Version", "10.0.26100")
	req.Header.Set("x-request-id", "f3f6f8e4-18f3-4c41-91df-35c6f2a69f53")
	req.Header.Set("x-zcode-session-type", "main")
	req.Header.Set("x-zcode-trace-id", "b0b9b18b-7f4a-4a7c-9910-8a4e8bb2ca46")
	req.Header.Set("x-zcode-query-source", "main_turn")
	req.Header.Set("x-session-id", sid)
	req.Header.Set("X-Device-Mid", cred.DeviceMID)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := (&http.Client{Timeout: 95 * time.Second}).Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return map[string]any{"http_status": resp.StatusCode, "response_headers": resp.Header, "response_body": string(data)}, nil
}

func write(v any) { b, _ := json.MarshalIndent(v, "", "  "); fmt.Println(string(b)) }
func fail(stage string, err error) {
	write(map[string]any{"ok": false, "stage": stage, "error": err.Error(), "checked_at": time.Now().Format(time.RFC3339)})
	os.Exit(2)
}

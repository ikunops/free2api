package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// —— 网关密钥（api_key）的生成 / 设置 ——
//
// 为什么要有这个端点：之前密钥只能手改 config.json（或者干脆不设，本机直连免密钥）。
// 但用户看到的「网关密钥：未设置」是一句描述，不是一个能点的操作——想开启鉴权
// 的人不知道该去哪改。生成按钮需要一个后端来真正落盘。
//
// 三条语义要跟 withAuth 对齐，否则按钮就成了假操作：
//  1. 落盘的是 config.json 的 api_key，**但当前进程不会热更新**——h.cfg.APIKey 是
//     启动快照。所以回包必须带 restart_required，前端要提示"重启后生效"，
//     否则用户点完立刻试请求，401 会被误读成"密钥没用"。
//  2. 与 Config 校验一致：admin.enabled=true 且 api_key 为空且监听非 loopback 时，
//     网关**拒绝启动**（见 gateway.Load）。所以"生成密钥"在这里是安全侧的默认值。
//  3. 允许清空（关闭鉴权），但要走 body 里的显式字段区分"没传"和"传空串"。

// adminAPIKeyPut 生成/设置/清空网关密钥。
//
// body: {"api_key": "..."}   非空 = 设置；{"api_key": ""} = 关闭鉴权；
//
//	{"generate": true}      = 让服务端生成一把随机密钥（推荐，不用自己想）
func (h *Handler) adminAPIKeyPut(w http.ResponseWriter, r *http.Request) {
	if h.cfg.ConfigPath == "" {
		writeOpenAIErrorHint(w, http.StatusBadRequest, "config_readonly", "本次启动未记录 config.json 路径",
			"嵌入/测试形态下改不了密钥；正常启动的网关可以")
		return
	}
	var body struct {
		APIKey   *string `json:"api_key"`
		Generate bool    `json:"generate"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request", "body must be JSON: "+err.Error())
		return
	}
	key := ""
	switch {
	case body.Generate:
		key = newGatewayAPIKey()
	case body.APIKey != nil:
		key = strings.TrimSpace(*body.APIKey)
	default:
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request",
			"需要 api_key 字段（空串=关闭鉴权）或 generate:true")
		return
	}
	if err := h.writeGatewayAPIKey(key); err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, "config_write_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_key":          key,
		"enabled":          key != "",
		"restart_required": true,
		"note":             "已写入 config.json。当前进程仍在用启动时的旧值，重启网关后生效。",
	})
}

// newGatewayAPIKey 生成一把随机密钥。32 随机字节 → base64url（43 字符，无填充）。
//
// 为什么 base64url 而不是 hex：hex 要 64 字符，base64url 43 字符短一半，
// 且不含 + / = 这类在 URL/JSON/环境变量里需要转义的字符——密钥会被抄进
// Codex 的 config.toml、ZCode 的 provider_config.json、.env，字符集干净很重要。
func newGatewayAPIKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败是不可恢复的环境问题；退化成时间派生会产出可预测的密钥，
		// 那比生成失败更糟，所以这里直接返回空串让上层报"生成失败"。
		return ""
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(b)
}

// writeGatewayAPIKey 只替换 config.json 的 api_key 键，其余键原样回写（与
// writeScheduleEnabled 同款做法：map[string]json.RawMessage 逐键替换，不整体
// 反序列化再序列化——那会顺手抹掉 Config 里没有的字段，比如用户自己加的注释性键）。
func (h *Handler) writeGatewayAPIKey(key string) error {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		return err
	}
	if all == nil {
		all = map[string]json.RawMessage{}
	}
	enc, err := json.Marshal(key)
	if err != nil {
		return err
	}
	all["api_key"] = enc
	buf, err := json.MarshalIndent(all, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	tmp := h.cfg.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, buf, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, h.cfg.ConfigPath)
}

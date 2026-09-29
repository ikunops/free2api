package responses

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"math/rand/v2"
)

// newID 生成 Responses 风格的资源 ID：<prefix>_<32 hex>。
// 上游 Chat 的 id 形态是 "chatcmpl-xxx"，与 Responses 的 "resp_/msg_/fc_/call_"
// 不是一套，直接透传会让严格的客户端困惑，故统一重铸。
// crypto/rand 失败（理论上不可能）时回落 math/rand/v2，恒非空、恒合法。
func newID(prefix string) string {
	b := make([]byte, 16)
	if _, err := cryptorand.Read(b); err == nil {
		return prefix + "_" + hex.EncodeToString(b)
	}
	return fmt.Sprintf("%s_%016x%016x", prefix, uint64(rand.Uint64())|1, rand.Uint64())
}

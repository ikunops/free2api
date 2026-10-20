package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// NewUUIDv7 生成 RFC 9562 UUIDv7 的带连字符文本形态。
//
// WorkBuddy 国际版会话头族里 X-Conversation-ID 要求是这种形态；不能拿普通
// 32-hex 随机会话号顶替。网关没有会话 id 时会生成一个，保证出站永远带齐
// 官方客户端会发的会话头。
func NewUUIDv7() string {
	var b [16]byte
	now := uint64(timeNowUnixMilli())
	// 48-bit big-endian unix milliseconds.
	b[0] = byte(now >> 40)
	b[1] = byte(now >> 32)
	b[2] = byte(now >> 24)
	b[3] = byte(now >> 16)
	b[4] = byte(now >> 8)
	b[5] = byte(now)
	if _, err := rand.Read(b[6:]); err != nil {
		// 熵源故障时仍返回合法形态（时间前缀 + 可预测尾部），不 panic。
		for i := 6; i < 16; i++ {
			b[i] = byte(now >> uint((i%8)*8))
		}
	}
	b[6] = (b[6] & 0x0f) | 0x70 // version 7
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	h := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

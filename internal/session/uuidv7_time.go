package session

import "time"

// timeNowUnixMilli 独立成变量，便于测试 UUIDv7 的时间前缀而不依赖真实时钟。
var timeNowUnixMilli = func() int64 { return time.Now().UnixMilli() }

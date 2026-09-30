// credit_rate.go 上游「积分倍率原文 → 是否免费」的单一判定口径。
//
// 存在的理由：倍率原文（credits）在上游各家形态不一（"x0.00" / "0" / "0.00 credits" /
// 空），而「这个模型跑起来要不要扣积分」是选号分层的**策略输入**（见 pool.pickWith
// 的 freeModel 分支：免费模型不按余额加权）。两处口径若分裂，就会出现「/v1/models
// 标了 -free、选号却仍按余额加权」这类静默不一致。
//
// 与 internal/server 的同名 helper（output.go 的 creditMultiplier/creditIsZero）分工：
// 那边负责**拼输出模型名的费率后缀**（"-free" / "-x0.11"），这边只回答布尔问题。
// 两边都从倍率原文出发，但一个改名字、一个定策略，刻意不互相 import
// （server → upstream 是单向依赖，反向会成环）。
package upstream

import (
	"strconv"
	"strings"
)

// CreditRateIsZero 报告倍率原文是否表示「零积分」（即免费）。认得的形态：
//
//	"x0" / "x0.00" / "x0.000000"   带 x 前缀
//	"0" / "0.00" / "0.000000"      不带 x
//	"0.00 credits" / "x0 credits"  带 credits 尾巴（上游部分端点这么写）
//	"$0" / "$0.00"                 带货币前缀
//
// 空串/无法解析/含非数字字符 → false（**不把不确定当免费**：宁可退回按余额加权，
// 也不能把收费模型误判成免费、把流量全压到一个号上烧积分）。
func CreditRateIsZero(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	s = strings.TrimSpace(strings.TrimSuffix(s, "credits"))
	s = strings.TrimSpace(strings.TrimPrefix(s, "x"))
	s = strings.TrimSpace(strings.TrimPrefix(s, "$"))
	if s == "" {
		return false
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return false
	}
	return n == 0
}

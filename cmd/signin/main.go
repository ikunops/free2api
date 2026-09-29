// signin 一次性批量签到工具：遍历 ./auths/workbuddy-*.json 全部账号，
// 自动 RefreshToken（过期时），逐个调 daily-checkin，顺手查余额。
package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"free2api/internal/auth"
	"free2api/internal/logfmt"
	"free2api/internal/upstream"
)

type row struct {
	file     string
	uid      string
	nick     string
	status   string // OK | ALREADY | FAIL | AUTH_INVALID | LOAD_ERR
	detail   string
	remain   int64
	hasQuota bool
}

func main() {
	dir := "auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	// 文件清单走 auth.LoadAuthFiles（宽侧 workbuddy*.json）：与网关 LoadDir 同口径，
	// 不带连字符的文件不再被跳过（P2-10，审查发现 10）。
	files, err := auth.LoadAuthFiles(dir)
	if err != nil || len(files) == 0 {
		fmt.Fprintf(os.Stderr, "no auth files in %s\n", dir)
		os.Exit(1)
	}
	up := upstream.New()
	// 允许按 realm 路由：global 账号的签到/余额会打到 global base（workbuddy.ai），
	// 由幂等码兜底为「未开启/不适用」，而不是误打到 CN base 产生签到成功的假象。
	// 纯 CN 部署无 global 账号时此开关无影响（账号 realm 全 cn → 全走 CN base）。
	up.GlobalEnabled = true

	var rows []row
	okN, alreadyN, failN := 0, 0, 0
	for _, f := range files {
		r := row{file: filepath.Base(f)}
		raw, err := os.ReadFile(f)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			r.status, r.detail = "LOAD_ERR", err.Error()
			rows = append(rows, r)
			failN++
			continue
		}
		a.FilePath = f
		r.uid, r.nick = a.UID, a.Nickname

		// refresh 过期 token
		if a.NeedsRefresh(2 * 3600) {
			if err := up.RefreshToken(a); err != nil {
				r.status = refreshStatusOf(err)
				r.detail = "refresh: " + short(err.Error())
				rows = append(rows, r)
				failN++
				continue
			}
			// refresh 后写回文件（权限问题已修复）；落盘失败必须暴露，否则重启回旧 token
			a.BackfillRealm() // 老 auth 空 realm → 落盘前补标识（幂等：已有不动）
			if err := a.SaveAtomic(); err != nil {
				log.Printf("signin %s save: %v", a.UID, err)
			}
		}

		err = up.DailyCheckin(a)
		r.status = checkinStatusOf(err)
		if err != nil {
			r.detail = short(err.Error())
		}
		switch r.status {
		case "OK":
			okN++
		case "ALREADY":
			alreadyN++
		default:
			failN++
		}
		// 顺手查余额
		if remain, qerr := up.UserResource(a); qerr == nil {
			r.remain, r.hasQuota = remain, true
		}
		rows = append(rows, r)
	}

	// 报告
	fmt.Printf("uid                                  | nick        | status       | remain | detail\n")
	fmt.Printf("-------------------------------------+-------------+--------------+--------+------------------------------\n")
	for _, r := range rows {
		remain := "-"
		if r.hasQuota {
			remain = fmt.Sprintf("%d", r.remain)
		}
		fmt.Printf("%-36s | %-11s | %-12s | %-6s | %s\n",
			logfmt.Truncate(r.uid, 36), logfmt.Truncate(r.nick, 11), r.status, remain, r.detail)
	}
	fmt.Printf("\ntotal=%d ok=%d already=%d fail=%d\n", len(rows), okN, alreadyN, failN)
}

// idempotentCodes 幂等/不适用**业务码数字**（裸数字，同时覆盖 doJSON 拼出的
// "code=<n> msg=…" 与 HTTP 4xx 原始 JSON body 的 `"code":<n>` 两种拼写）。
// 10001（实测 code=10001 "今天已签到"，login.sh）/ 14001 同义变体。
var idempotentCodes = []string{"10001", "14001"}

// idempotentMarkers 幂等/不适用文案关键词（中文原文 + 英文 lowcase）：
// - "今天已签到"/"今日已签到"/"已签到"/"already"
// - global 无签到体系类：功能未开启 / 未开放 / 已过期 / inactive session
// 对 global 账号是兜底（scheduler 门控已在调度层挡住，这是手动跑 signin 的保护）。
// 校验只对 *upstream.Error（带分类的业务错误）生效；网络层/解析层错误不得当作幂等，
// 否则停机补签遇抖动会误记为 already（与 scheduler.IsAlreadyCheckin 同语义）。
var idempotentMarkers = []string{
	"今天已签到", "今日已签到", "已签到", "already",
	"未开启", "未开放", "已过期", "inactive",
}

// isAlreadyCode 在消息中查找幂等业务码：裸数字为关键，`code=10001` 与 `"code":10001`
// 都含 "10001" 子串；只在前后是分隔符字符时命中，避免把 12001/2010001 这类误判。
func isAlreadyCode(s string) bool {
	for _, code := range idempotentCodes {
		for i := 0; i+len(code) <= len(s); {
			j := strings.Index(s[i:], code)
			if j < 0 {
				break
			}
			start, end := i+j, i+j+len(code)
			before, after := byte(0), byte(0)
			if start > 0 {
				before = s[start-1]
			}
			if end < len(s) {
				after = s[end]
			}
			// 前后必须是非数字/字母/下划线（分隔符），避免 12001 或 1_10001 误命中。
			if !isCodeWordChar(before) && !isCodeWordChar(after) {
				return true
			}
			i = end
		}
	}
	return false
}

// isCodeWordChar code 数字前后的边界字符判定：数字/字母/下划线视为同一 token 成员。
func isCodeWordChar(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_'
}

// bareMarkers 裸错误（非 *upstream.Error，网络/解析层）回退匹配用的**中文文案**子集。
// 英文短词（already/inactive）在传输层错误文本里太常见（如 "address already in use"、
// proxy "session inactive"），不得对裸错误启用——否则停机抖动会被误判成 already。
// 业务 *Error 路径（idempotentMarkers 全量）不受此限：那里的 already/inactive 来自
// 上游真实业务回复。与 upstream.IsAlreadyCheckin 语义一致：非带分类错误不当幂等。
var bareMarkers = []string{"今天已签到", "今日已签到", "已签到", "未开启", "未开放", "已过期"}

// isAlready 报告 err 是否表示"今天已签到 / 功能不适用"（幂等成功，不算失败）。
// 双输入：*upstream.Error 走结构化 Msg 全量匹配；其他错误回退 bareMatch（仅中文文案，
// 排除英文短词，见 bareMarkers——避免传输层文本误命中）。
func isAlready(err error) bool {
	var ue *upstream.Error
	if errors.As(err, &ue) {
		return idempotentMatch(ue.Msg)
	}
	if err == nil {
		return false
	}
	return bareMatch(err.Error())
}

// bareMatch 裸错误回退匹配：只认中文专属文案关键词（业务码只在 *Error 里出现，
// 裸错误无 code 可查）。
func bareMatch(s string) bool {
	for _, m := range bareMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// idempotentMatch 命中幂等信号：业务码或文案任一命中即 true。
func idempotentMatch(msg string) bool {
	low := strings.ToLower(msg)
	if isAlreadyCode(low) {
		return true
	}
	for _, m := range idempotentMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// refreshStatusOf RefreshToken 失败的归一化状态（纯函数，供 main 循环与测试直接断言）：
// session 失效（401 12153 离线）→ AUTH_INVALID（需人工重登，区别于普通失败）；
// 其余一律 FAIL。
func refreshStatusOf(err error) string {
	var ue *upstream.Error
	if errors.As(err, &ue) && ue.Kind == upstream.ErrSessionDead {
		return "AUTH_INVALID"
	}
	return "FAIL"
}

// checkinStatusOf DailyCheckin 结果的归一化状态（纯函数，供 main 循环与测试直接断言）：
// 成功 → OK；幂等码（已签到/不适用）→ ALREADY；其余一律 FAIL。
func checkinStatusOf(err error) string {
	if err == nil {
		return "OK"
	}
	if isAlready(err) {
		return "ALREADY"
	}
	return "FAIL"
}

func short(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 60 {
		return s[:60]
	}
	return s
}

// modelmap —— 模型来源核对工具。
//
// 回答一个反复出现的问题：「网关吐出来的模型清单，到底是账号自己带的，还是网关
// 内置了一张静态表？」
//
// 做法：拿每个账号自己的 access token 直连上游模型端点（与网关上链路完全同一条），
// 打印每个账号的模型数量 + 清单指纹 + 样例，并两两求差。判读标准：
//   - 不同账号指纹不同 / 差集非空  → 清单由账号决定（账号自带能力）
//   - 所有账号指纹相同           → 才可能是静态表
//
// 用法（在项目根目录，需 config.json + auths/）：
//
//	modelmap                          # 读 ./auths（网关号池目录）
//	modelmap -dir ./auths
//	modelmap -ledger %USERPROFILE%\.wb-switch\accounts.json   # 直接读 wb-switch 账本
//	modelmap -alias auto,fast-model,balanced-model,deep-model # 另看这些 id 命中情况
//
// 只读：不签到、不改号池、不落盘（token 过期时提示先让网关刷新一次）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"free2api/internal/auth"
	"free2api/internal/upstream"
)

// wbEntry 是 wb-switch 账本里的单条账号（毫秒 expiresAt 的那个 schema）。
type wbEntry struct {
	Nickname     string `json:"nickname"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    int64  `json:"expiresAt"`
	Domain       string `json:"domain"`
	UID          string `json:"uid"`
}

func main() {
	dir := flag.String("dir", "./auths", "网关号池目录（读其中的 auth 文件）")
	ledger := flag.String("ledger", "", "wb-switch 账本 accounts.json（给了就只读账本）")
	alias := flag.String("alias", "auto,fast-model,balanced-model,deep-model,default-1.1,default-1.2",
		"关注这些模型 id 的命中情况（逗号分隔）")
	flag.Parse()

	accts, src, err := load(*dir, *ledger)
	if err != nil {
		fmt.Println("载入失败:", err)
		os.Exit(1)
	}
	if len(accts) == 0 {
		fmt.Printf("没读到账号（来源 %s）。检查目录或加 -ledger 指定账本。\n", src)
		os.Exit(1)
	}
	fmt.Printf("来源 %s，账号 %d 个\n\n", src, len(accts))

	up := upstream.New()
	up.GlobalEnabled = true // 探针只关心账号自己有什么，不套部署侧路由开关
	watch := map[string]bool{}
	for _, s := range strings.Split(*alias, ",") {
		if s = strings.TrimSpace(s); s != "" {
			watch[s] = true
		}
	}

	sets := make(map[string]map[string]bool, len(accts))
	order := make([]string, 0, len(accts))
	for _, a := range accts {
		tag := a.Nickname
		if tag == "" {
			tag = a.UID
		}
		if len([]rune(tag)) > 16 {
			tag = string([]rune(tag)[:16])
		}
		infos, err := up.FetchModels(a)
		if err != nil {
			hint := ""
			if strings.Contains(err.Error(), "401") {
				hint = "（token 可能已过期：先让网关跑一次刷新，或换 -ledger 里较新的快照）"
			}
			fmt.Printf("%-18s realm=%-7s ERR %v%s\n\n", tag, a.Realm(), err, hint)
			continue
		}
		ids := make([]string, 0, len(infos))
		var hit []string
		set := make(map[string]bool, len(infos))
		for _, mi := range infos {
			if mi.ID == "" {
				continue
			}
			ids = append(ids, mi.ID)
			set[mi.ID] = true
			if watch[mi.ID] {
				hit = append(hit, mi.ID)
			}
		}
		sort.Strings(ids)
		sort.Strings(hit)
		sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
		sets[tag] = set
		order = append(order, tag)
		fmt.Printf("%-18s realm=%-7s 模型=%3d 指纹=%s\n", tag, a.Realm(), len(ids), hex.EncodeToString(sum[:6]))
		fmt.Printf("%-18s   命中关注 id(%d): %s\n", "", len(hit), strings.Join(hit, " "))
		fmt.Printf("%-18s   样例: %s\n\n", "", strings.Join(head(ids, 10), " "))
	}

	// 并集/交集：给「发布清单」一个口径参考
	if len(sets) > 0 {
		union := map[string]bool{}
		var inter map[string]bool
		for _, s := range sets {
			for id := range s {
				union[id] = true
			}
			if inter == nil {
				inter = make(map[string]bool, len(s))
				for id := range s {
					inter[id] = true
				}
				continue
			}
			for id := range inter {
				if !s[id] {
					delete(inter, id)
				}
			}
		}
		fmt.Printf("==== 并集 %d 个 / 交集(所有账号共有) %d 个 ====\n\n", len(union), len(inter))
	}

	if len(order) < 2 {
		return
	}
	fmt.Println("==== 账号间差异（判读模型来源的关键证据）====")
	sort.Strings(order)
	diffSeen := false
	for i := 0; i < len(order); i++ {
		for j := i + 1; j < len(order); j++ {
			x, y := sets[order[i]], sets[order[j]]
			onlyX, onlyY := []string{}, []string{}
			for id := range x {
				if !y[id] {
					onlyX = append(onlyX, id)
				}
			}
			for id := range y {
				if !x[id] {
					onlyY = append(onlyY, id)
				}
			}
			if len(onlyX) == 0 && len(onlyY) == 0 {
				continue
			}
			diffSeen = true
			sort.Strings(onlyX)
			sort.Strings(onlyY)
			fmt.Printf("%s vs %s: 仅左 %d [%s] / 仅右 %d [%s]\n",
				order[i], order[j], len(onlyX), strings.Join(head(onlyX, 8), " "),
				len(onlyY), strings.Join(head(onlyY, 8), " "))
		}
	}
	if !diffSeen {
		fmt.Println("所有账号清单完全一致——这时才需要怀疑是不是静态表。")
	} else {
		fmt.Println("\n结论：清单随账号变化 → 由账号自己携带（上游按账号下发），不是网关内置静态表。")
	}
}

// load 优先读 -ledger 指定的 switch 账本，否则读 -dir 下的 auth 文件（只读，不 backfill 落盘）。
func load(dir, ledger string) ([]*auth.Auth, string, error) {
	if strings.TrimSpace(ledger) != "" {
		list, err := loadLedger(ledger)
		return list, "wb-switch 账本 " + ledger, err
	}
	files, err := auth.LoadAuthFiles(dir)
	if err != nil {
		return nil, dir, err
	}
	var out []*auth.Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := auth.Parse(raw)
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, "auth 目录 " + dir, nil
}

func loadLedger(path string) ([]*auth.Auth, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []wbEntry
	if err := json.Unmarshal(raw, &list); err != nil {
		var wrap struct {
			Accounts []wbEntry `json:"accounts"`
		}
		if err2 := json.Unmarshal(raw, &wrap); err2 != nil {
			return nil, fmt.Errorf("账本既不是数组也不是 {accounts:[]}: %w", err)
		}
		list = wrap.Accounts
	}
	out := make([]*auth.Auth, 0, len(list))
	for _, e := range list {
		out = append(out, &auth.Auth{
			AccessToken:  e.AccessToken,
			RefreshToken: e.RefreshToken,
			ExpiresAt:    e.ExpiresAt / 1000, // 账本毫秒 → auth 秒
			Domain:       e.Domain,
			UID:          e.UID,
			Nickname:     e.Nickname,
		})
	}
	return out, nil
}

func head(s []string, n int) []string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

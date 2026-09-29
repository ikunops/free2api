// watch.go auths 目录热加载：目录内容变化时自动重新对齐账号池，免去加账号后重启网关。
//
// 为什么需要：SyncToDir 只在进程启动时被调用一次（cmd/server/main.go），运行中新增的
// 凭证文件不会进池——表现为控制台显示"已添加"但账号"未加载"，必须手动重启才生效。
//
// 实现选择——轮询而非 fsnotify：
//   - 零新依赖（fsnotify 要引第三方库，本仓库依赖面刻意保持极窄：仅 go-redis）。
//   - 语义更稳：fsnotify 在容器/网络文件系统上有丢事件与 inotify 句柄耗尽的老问题；
//     轮询只看目录内容指纹，漏不掉、也不会因事件风暴抖动。
//   - 代价可接受：目录里只有几十个凭证文件，每次轮询只做一次 ReadDir + 名字比对，
//     默认 5s 周期下 CPU 开销可忽略（实测常驻 0.0%）。
//
// 幂等性依赖 SyncToDir 的既有语义：upsertLocked 对已存在账号只换凭证、保留
// credits/cooling/成本账本，故重复调用不会重置任何运行状态。
package pool

import (
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"free2api/internal/auth"
)

// watchInterval 目录轮询周期。5s 与池状态落盘周期（flushInterval）同量级：
// 加账号是低频人工操作，5s 内生效足够；更短只是徒增 IO。
var watchInterval = 5 * time.Second

// StartAuthDirWatch 启动 auths 目录监听，返回停止函数（幂等，可重复调用）。
//
// 首次调用即建立基线指纹（不触发同步——启动时 main 已 SyncToDir 过一次，
// 此处再同步是多余的重复工作）。之后仅在指纹变化时重新加载并对齐。
//
// dir 为空或不可读时只记一条日志并返回 no-op 停止函数：监听失败不该拖垮网关，
// 加账号仍可用「手动重启」这条既有退路。
func (p *Pool) StartAuthDirWatch(dir string) (stop func()) {
	if dir == "" {
		log.Printf("[watch] auths 目录未配置，跳过热加载监听")
		return func() {}
	}
	base, ok := dirFingerprint(dir)
	if !ok {
		log.Printf("[watch] auths 目录 %s 不可读，跳过热加载监听（加账号后需手动重启）", dir)
		return func() {}
	}
	log.Printf("[watch] auths 目录监听已启用（每 %s 检查一次，新增账号自动加载，无需重启）", watchInterval)

	done := make(chan struct{})
	var stopped bool
	go func() {
		t := time.NewTicker(watchInterval)
		defer t.Stop()
		last := base
		for {
			select {
			case <-done:
				return
			case <-t.C:
				cur, ok := dirFingerprint(dir)
				if !ok || cur == last {
					continue // 目录暂不可读（如正在原子替换）不当作变化，避免误剔除
				}
				last = cur
				p.reloadAuthDir(dir)
			}
		}
	}()

	return func() {
		if stopped {
			return
		}
		stopped = true
		close(done)
	}
}

// reloadAuthDir 重新扫描目录并对齐账号池。
//
// 全量重扫而非增量：目录只有几十个文件，全扫的代价远低于维护增量状态
// （增量需要处理"文件改名""写了一半"等边界）。auth.LoadDir 自身跳过坏文件，
// 故半写入的临时文件（login.sh 用 tempfile + os.replace 原子替换）不会造成误判。
func (p *Pool) reloadAuthDir(dir string) {
	auths, err := auth.LoadDir(dir)
	if err != nil {
		log.Printf("WARN: [watch] 重新加载 %s 失败: %v（保持现有池状态）", dir, err)
		return
	}
	before := len(p.AvailableUIDs())
	p.SyncToDir(auths)
	after := len(p.AvailableUIDs())
	if after != before {
		log.Printf("[watch] auths 目录变化：账号数 %d → %d（已热加载，无需重启）", before, after)
	} else {
		// 数量不变但内容变了（如凭证刷新、文件改名）：仍要落一条，便于对账。
		log.Printf("[watch] auths 目录变化：账号数保持 %d（已热加载凭证更新）", after)
	}
}

// dirFingerprint 生成目录内容指纹：文件名 + 修改时间 + 大小，排序后拼接。
//
// 为什么不用纯文件名：凭证刷新（login.sh 覆盖同 uid 文件）时文件名不变，
// 只比名字会漏掉这类更新。mtime + size 能覆盖"凭证被刷新"与"文件被替换"。
//
// 返回 (指纹, 目录是否可读)。**必须用独立的 ok 而不是"空串=不可读"**：
// 空目录（所有凭证被删）的合法指纹就是空串，若与"不可读"共用哨兵，
// 清空目录会被误判为读失败而跳过同步，导致已删账号滞留在池中。
func dirFingerprint(dir string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	parts := make([]string, 0, len(ents))
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		parts = append(parts, e.Name()+"|"+info.ModTime().Format(time.RFC3339Nano)+"|"+
			strconv.FormatInt(info.Size(), 10))
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n"), true
}

// RescanAuthDir 立即重扫 auths 目录并对齐账号池（不等 5s 轮询）。
// 给「管理端点导入账号后立刻生效」用：导入成功即调用，前端无需等待/刷新。
func (p *Pool) RescanAuthDir(dir string) { p.reloadAuthDir(dir) }

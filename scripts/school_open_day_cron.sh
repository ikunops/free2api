#!/usr/bin/env bash
# school_open_day_cron.sh —— 开学季任务每日定时入口（系统 crontab 调用，cron 邮件不可用时日志即唯一线索）
#
# 两个入口（一个脚本两种模式，crontab 分两条调）：
#   school  12:00  school_open_day_2026.py ALL --run --yes
#                  任务点亮 + claim + 自动抽空抽奖余额。
#                  活动下线（in_period=false）时 task 与抽奖段各自全量跳过、正常退出，不算失败。
#   cat     01:00  task_runner.py ALL --yes --only black_cat
#                  夜猫窗口 23:00–08:00 CST，窗口内最多补 1 次（task_runner 内部判定，非窗口期打印 skip）。
#
# 推荐 crontab（已查本机：`cat /etc/timezone`→Asia/Shanghai，`date`→CST，系统时钟即 CST，crontab 直接用本地时间）：
#
#   0 12 * * *  root /root/free2api/scripts/school_open_day_cron.sh school
#   0  1 * * *  root /root/free2api/scripts/school_open_day_cron.sh cat
#
# 若部署到 UTC 主机，两行整体 -8 小时：12:00 CST = 04:00 UTC，01:00 CST = 17:00 UTC（前一日）。
#
# 健壮性：
#   - flock -n 防重叠：同模式串行共锁，锁被占则跳过本次并记日志
#   - 日志追加到 data/school-cron.log（data/ 已被 .gitignore 覆盖，不进 git），超 2MB 截断保留尾部 2000 行
#   - py 非零退出码记 ERROR 行，脚本继续正常返回（重叠跳过同样正常返回）
set -euo pipefail

# ---- 自定位仓库根（不硬编码 /root 路径） ----
SELF_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SELF_DIR/.." && pwd)"
cd "$REPO_ROOT"
export PYTHONDONTWRITEBYTECODE=1

# ---- Python 解释器选择（优先 PATH 中的 python3） ----
if ! PY="$(command -v python3 2>/dev/null)"; then
    echo "[$(date '+%F %T')] ERROR: python3 不在 PATH，无法运行" >&2
    exit 1
fi

# ---- 模式 -> 实际命令 ----
MODE="${1:-}"
case "$MODE" in
    school) CMD=( "$PY" scripts/school_open_day_2026.py ALL --run --yes ) ;;
    cat)    CMD=( "$PY" scripts/task_runner.py ALL --yes --only black_cat ) ;;
    *)
        echo "用法: $0 {school|cat}" >&2
        echo "  school  12:00 开学任务：任务+claim+自动抽空抽奖" >&2
        echo "  cat     01:00 夜猫窗口(23-08 CST)补 1 次" >&2
        exit 2
        ;;
esac

# ---- 日志与锁 ----
mkdir -p data
LOG="$REPO_ROOT/data/school-cron.log"
LOCK="$REPO_ROOT/data/school-cron.lock"

exec 9>"$LOCK"
if ! flock -n 9; then
    echo "[$(date '+%F %T')] skip[$MODE]: 上一次运行仍持有锁，跳过本次" >>"$LOG"
    exit 0
fi

{
    echo "[$(date '+%F %T')] === cron[$MODE] start: ${CMD[*]} ==="
    if "${CMD[@]}"; then
        echo "[$(date '+%F %T')] === cron[$MODE] end: 完成 ==="
    else
        ec=$?
        echo "[$(date '+%F %T')] ERROR[$MODE]: 退出码 $ec（详见上方输出）"
        echo "[$(date '+%F %T')] === cron[$MODE] end: 失败 ec=$ec ==="
    fi
    # 日志轮转：超 2MB 截断保留尾部 2000 行
    if [[ -f "$LOG" ]] && [[ $(stat -c%s "$LOG") -gt 2000000 ]]; then
        tail -n 2000 "$LOG" >"$LOG.tmp" && mv "$LOG.tmp" "$LOG"
    fi
} >>"$LOG" 2>&1
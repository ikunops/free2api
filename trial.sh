#!/usr/bin/env bash
# trial.sh — global trial 加油包领取辅助工具（薄包装）
#
# 不调 HTTP：cmd/trial 已自带逐账号表格输出（uid/nick/status/detail
# + total=N ok=N already=N na=N fail=N 汇总行），并对 CN 账号明确
# 提示不适用（只有 global 账号会被执行 /billing/ide/trial）。
#
# 用法:
#   ./trial.sh            # cmd/trial 表格
#   ./trial.sh auths_dir  # 指定 auths 目录
#
# 常见状态:
#   OK      领取成功（一次性加油包到账）
#   ALREADY 已领取过（幂等，不算失败）
#   N/A     CN 账号不适用（global 专属端点）
#
# 依赖: go（首次构建 trial_bin）
set -euo pipefail
cd "$(dirname "$0")"

AUTHS_DIR="auths"
if [[ $# -gt 0 ]]; then
    AUTHS_DIR="$1"
fi

# 二进制解析优先序：1) 容器内预置 /app/trial_bin（镜像带产物，无需 go）；2) $CACHE_DIR 缓存；
# 3) 源码更新则重编（需 go 工具链，本地/CI 用）。容器内（/app 有预置产物的环境）零构建直接跑。
TRIAL_BIN="$(cd "$(dirname "$0")" && pwd)/trial_bin"
if [[ ! -x "$TRIAL_BIN" ]]; then
    CACHE_DIR="${TMPDIR:-/tmp}/free2api-bin"
    mkdir -p "$CACHE_DIR"
    TRIAL_BIN="$CACHE_DIR/trial_bin"
    if [[ ! -x "$TRIAL_BIN" ]] || find . -name '*.go' -newer "$TRIAL_BIN" -print -quit | grep -q .; then
        if ! command -v go >/dev/null 2>&1; then
            echo "需要 go 构建 trial_bin（或镜像内置 /app/trial_bin）" >&2
            exit 1
        fi
        echo "构建 trial.." >&2
        go build -o "$TRIAL_BIN" ./cmd/trial
    fi
fi

"$TRIAL_BIN" "$AUTHS_DIR"
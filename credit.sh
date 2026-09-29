#!/usr/bin/env bash
# credit.sh — WorkBuddy 积分日报（默认美化输出）
#
# 用法:
#   ./credit.sh            # 人类可读日报
#   ./credit.sh -json      # 原始 JSON
#
# 二进制解析：缺失或构建输入（*.go 与依赖清单 go.mod/go.sum）比它新时重编。
# 比 trial.sh 的判据多覆盖 go.mod/go.sum——依赖变更后二进制同样不再对应源码。
# 容器内镜像已预置 /app/credit 且不含这些输入，条件恒假，仍是零构建直跑。
set -euo pipefail
cd "$(dirname "$0")"

BIN=./credit
if [[ ! -x "$BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 credit（或镜像内置 /app/credit）" >&2
        exit 1
    fi
    echo "build credit ..." >&2
    go build -o "$BIN" ./cmd/credit
fi

if [[ "${1:-}" == "-json" ]]; then
    exec "$BIN"
fi
exec "$BIN" -pretty

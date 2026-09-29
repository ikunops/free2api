#!/usr/bin/env bash
# test_wrapper_freshness.sh — 包装脚本「源码更新即重编」判据的回归测试（issue #191）
#
# 被测量：credit.sh / signin.sh / checkin.sh / login.sh / dev.sh 在六种情形下
# 是否正确重建二进制：
#   C1 二进制缺失          → 应调用 go build
#   C2 二进制比构建输入新  → 不应调用 go build（零构建直跑，容器内即此路径）
#   C3 Go 源码比二进制新   → 应调用 go build（issue #191 的核心场景）
#   C4 只有 go.mod 变新    → 应调用 go build（依赖清单也是构建输入）
#   C5 需重编但没有 go     → 应显式失败，不得静默沿用旧二进制
#   C6 构建失败            → 同样必须中止，不得沿用旧二进制
#
# 为什么这样测：
# - 用 stub `go` 作观测点：只断言 go 是否被调用及其 -o 目标，不做真实编译 →
#   秒级、无副作用、不要求本机有 Go 工具链，也不碰仓库根目录的真实二进制。
# - 把**真实脚本文件**复制进沙箱执行，而不是复制判据逻辑：脚本改了测试跟着走。
#   脚本自身 cd "$(dirname "$0")"，故复制后在新目录里自成一体。
# - C2/C3/C4 用 touch -t 显式设定 mtime（2030 / 2020 / 2021），不依赖 sleep 或
#   时钟精度，在 1 秒粒度的文件系统上同样确定；也不写死「当前时间」字面量。
#   C4 把全部 *.go 回拨、只让 go.mod 变新，从而**只可能**被「go.mod 纳入判据」满足。
# - checkin.sh 依赖 curl（缺失即 exit 1）且会先探网关，故用 WB2A_URL 指向
#   127.0.0.1:1（立即 connection refused）保持用例无网络;缺 curl 时跳过该用例。
#
# 用法:
#   bash scripts/test_wrapper_freshness.sh
#   WRAPPER_SRC_DIR=<旧版本目录> bash scripts/test_wrapper_freshness.sh   # 复现 RED
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# 被测量的脚本来源目录。指向修复前的树即可复现 RED（C3 必失败）。
WRAPPER_SRC_DIR="${WRAPPER_SRC_DIR:-$REPO_ROOT}"

PASS=0
FAIL=0
say()  { printf '%s\n' "$*"; }
ok()   { say "  ✓ $*"; PASS=$((PASS + 1)); }
fail() { say "  ✗ $*"; FAIL=$((FAIL + 1)); }

# 被测量清单：脚本名|它该产出的二进制|它构建的 Go 包|附加参数
# checkin.sh 与 signin.sh 共用 ./signin_bin，两个都要测：同一二进制必须同一判据。
# dev.sh 不带参数时默认走 status 分支（不涉及构建），必须显式给 start。
WRAPPERS=(
    "credit.sh|credit|./cmd/credit|"
    "signin.sh|signin_bin|./cmd/signin|"
    "checkin.sh|signin_bin|./cmd/signin|"
    "login.sh|login|./cmd/login|"
    "dev.sh|free2api|./cmd/server|start"
)

# ─── 沙箱 ──────────────────────────────────────────────────────────
# 含：假 Go 包目录（供 find 命中 *.go）、stub go、被测量的脚本副本。
# 沙箱内不放 config.json，也不放 auths/ 里的凭证；login.sh 需要可写的 auths/。
setup_sandbox() {
    local dir p s
    dir=$(mktemp -d "${TMPDIR:-/tmp}/wb2a-freshness.XXXXXX")
    mkdir -p "$dir/bin" "$dir/auths"
    for p in credit signin login server; do
        mkdir -p "$dir/cmd/$p"
        printf 'package main\n\nfunc main() {}\n' >"$dir/cmd/$p/main.go"
    done
    # 依赖清单：C4 用它验证 go.mod 也被纳入判据
    printf 'module freshness-probe\n\ngo 1.22\n' >"$dir/go.mod"

    # stub go：把调用追加到 $GO_CALLS，并按其 -o 目标生成可执行占位（模拟编译产物）。
    # GO_STUB_FAIL=1 时模拟「构建失败」：报错退出且**不产出**目标文件（C6 用）。
    cat >"$dir/bin/go" <<'STUB'
#!/usr/bin/env bash
echo "$*" >> "$GO_CALLS"
if [[ "${GO_STUB_FAIL:-0}" == "1" ]]; then
    echo "stub go: forced build failure" >&2
    exit 1
fi
out=""
while [[ $# -gt 0 ]]; do
    if [[ "$1" == "-o" ]]; then out="$2"; shift 2; continue; fi
    shift
done
if [[ -n "$out" ]]; then
    printf '#!/bin/sh\nexit 0\n' > "$out"
    chmod +x "$out"
fi
exit 0
STUB
    chmod +x "$dir/bin/go"

    for s in credit.sh signin.sh checkin.sh login.sh dev.sh; do
        cp "$WRAPPER_SRC_DIR/$s" "$dir/$s"
    done
    printf '%s' "$dir"
}

# run_wrapper 在沙箱里跑真实脚本。退出码不是本测试的关注点：dev.sh 会 nohup 起
# 桩二进制后判定「启动失败」，login.sh 会在 read 处遇 EOF —— 都不影响 go 调用观测。
run_wrapper() {
    local dir="$1" script="$2"
    shift 2
    env PATH="$dir/bin:$PATH" GO_CALLS="$dir/go-calls.log" \
        WB2A_URL="http://127.0.0.1:1" \
        bash "$dir/$script" "$@" </dev/null >/dev/null 2>&1 || true
}

go_called() {
    [[ -s "$1/go-calls.log" ]]
}

# ─── 用例 ──────────────────────────────────────────────────────────
test_wrapper() {
    local name bin pkg extra dir calls
    IFS='|' read -r name bin pkg extra <<<"$1"
    say ""
    say "── $name（产出 ./$bin，构建 $pkg）"

    if [[ "$name" == "checkin.sh" ]] && ! command -v curl >/dev/null 2>&1; then
        say "  - 跳过：checkin.sh 依赖 curl，本机未安装"
        return 0
    fi

    dir=$(setup_sandbox)

    # C1：二进制缺失 → 应编译出它
    rm -f "$dir/go-calls.log"
    run_wrapper "$dir" "$name" $extra
    if [[ -x "$dir/$bin" ]] && go_called "$dir"; then
        calls=$(cat "$dir/go-calls.log")
        if [[ "$calls" == *"$pkg"* ]]; then
            ok "C1 缺失 → 已调用 go build（$calls）"
        else
            fail "C1 调用了 go 但目标包不符：$calls"
        fi
    else
        fail "C1 二进制缺失时未编译（go 调用日志：${dir}/go-calls.log）"
    fi

    # C2：二进制比源码新（mtime 拨到 2030）→ 不得编译
    rm -f "$dir/go-calls.log"
    touch -t 203001010000 "$dir/$bin"
    run_wrapper "$dir" "$name" $extra
    if go_called "$dir"; then
        fail "C2 二进制已最新仍触发编译：$(cat "$dir/go-calls.log")"
    else
        ok "C2 已最新 → 未调用 go（零构建直跑）"
    fi

    # C3：源码比二进制新（二进制 mtime 拨到 2020）→ 必须编译
    rm -f "$dir/go-calls.log"
    touch -t 202001010000 "$dir/$bin"
    run_wrapper "$dir" "$name" $extra
    if go_called "$dir"; then
        ok "C3 源码更新 → 已调用 go build（$(cat "$dir/go-calls.log")）"
    else
        fail "C3 源码比二进制新却未重编（issue #191 的缺陷形态）"
    fi

    # C4：只有 go.mod 变新（所有 *.go 都旧于二进制）→ 依赖清单也是构建输入，
    # 变了同样使二进制不再对应源码。所有 *.go 回拨到 2020、二进制 2021、go.mod 当前，
    # 使该用例只可能被「go.mod 纳入判据」满足，不受 *.go 影响。
    rm -f "$dir/go-calls.log"
    find "$dir/cmd" -name '*.go' -exec touch -t 202001010000 {} +
    touch -t 202101010000 "$dir/$bin"
    touch "$dir/go.mod"
    run_wrapper "$dir" "$name" $extra
    if go_called "$dir"; then
        ok "C4 go.mod 变新 → 已重编"
    else
        fail "C4 只改 go.mod 却未重编（依赖变更后二进制不再对应源码）"
    fi

    # C5：需要重编但没有 go 工具链 → 必须显式失败，不得静默跑旧二进制。
    # 不假设 go 装在哪儿（Debian 系把它放在 /usr/bin，与 find/grep 同目录），而是
    # 把 go 所在目录从 PATH 里剔除；剔除后**自检前提**，构造不出就不测（不假绿）。
    rm -f "$dir/go-calls.log" "$dir/$bin"
    godir=$(dirname "$(command -v go 2>/dev/null || echo /nonexistent)")
    cleanpath=$(printf '%s' "$PATH" | tr ':' '\n' | command grep -vxF "$godir" | paste -sd: -)
    if PATH="$cleanpath" command -v go >/dev/null 2>&1; then
        say "  - 跳过 C5：无法构造不含 go 的 PATH（go 目录: $godir）"
    else
        rc=0
        env PATH="$cleanpath" WB2A_URL="http://127.0.0.1:1" \
            bash "$dir/$name" $extra </dev/null >"$dir/out.txt" 2>&1 || rc=$?
        if [[ $rc -ne 0 ]] && ! go_called "$dir"; then
            ok "C5 无 go 且需构建 → 退出码 $rc，未静默沿用"
        else
            fail "C5 无 go 时的行为不符：退出码 $rc（期望非 0）"
        fi
    fi

    # C6：构建**失败**时也必须中止，不得沿用旧二进制。
    # 这是 dev.sh 上真实出现过的缺陷：它没有 set -e，构建失败会继续走到 nohup，
    # 把陈旧的 free2api 拉起来并报「已启动」。用「被启动就留下标记文件」的桩二进制
    # 作观测点：只要标记出现，就说明旧二进制真的被跑了。
    rm -f "$dir/go-calls.log" "$dir/RAN" "$dir/free2api.pid" "$dir/.server.pid"
    printf '#!/bin/sh\ntouch "%s/RAN"\nexit 0\n' "$dir" >"$dir/$bin"
    chmod +x "$dir/$bin"
    touch -t 202001010000 "$dir/$bin"   # 过期 → 必然尝试构建
    rm -f "$dir/RAN"
    rc=0
    env PATH="$dir/bin:$PATH" GO_CALLS="$dir/go-calls.log" GO_STUB_FAIL=1 \
        WB2A_URL="http://127.0.0.1:1" \
        bash "$dir/$name" $extra </dev/null >"$dir/out.txt" 2>&1 || rc=$?
    if [[ -f "$dir/RAN" ]]; then
        fail "C6 构建失败却仍启动了旧二进制（静默沿用，退出码 $rc）"
    elif [[ $rc -eq 0 ]]; then
        fail "C6 构建失败却以退出码 0 结束（应显式失败）"
    else
        ok "C6 构建失败 → 已中止（退出码 $rc，未启动旧二进制）"
    fi

    rm -rf "$dir"
}

say "包装脚本源码新鲜度判据（来源目录：$WRAPPER_SRC_DIR）"
for w in "${WRAPPERS[@]}"; do
    test_wrapper "$w"
done

say ""
say "通过 $PASS，失败 $FAIL"
[[ "$FAIL" -eq 0 ]]

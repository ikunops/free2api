#!/usr/bin/env bash
# test_login_sh.sh — login.sh chown 后失效修复的回归测试（issue #160）
#
# T1（缺陷 1）：内嵌 Python 落盘段在目录不可写时打印指引而非裸 traceback
#              （mkstemp 移入 try 块，208-210 的诊断可达）。
# T2（缺陷 2）：auths/ 不可写时 login.sh 在 OAuth 流程启动前 fail-fast 退出。
#
# 两个用例都无法用 root 身份复现（root 对任何目录 -w 恒真），用 setpriv 切到
# uid 12345（并以其属主 chown 目录）构造真实权限语义。T2 通过 WB2A_LOGIN_STUB_URL
# 测试钩子伪造授权 URL（login.sh 先写 stub 再跑真实流程，钩子命中即短路，真实
# OAuth 路径零行为变化），从而无需浏览器即可断言"预检在 url 获取之前退出"。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
LOGIN_SH="${LOGIN_SH:-$REPO_ROOT/login.sh}"

PASS=0
FAIL=0
NONROOT_UID=12345

say()  { printf '%s\n' "$*"; }
ok()   { say "  ✓ $*"; PASS=$((PASS + 1)); }
fail() { say "  ✗ $*"; FAIL=$((FAIL + 1)); }

# ─── 公共：构造不可写场景 ──────────────────────────────────────────
# 以 NONROOT_UID 为属主创建 tmpdir，再用 setpriv --reuid 切过去跑命令：
# 目录对"当前用户"可写（属主就是自己）→ 预检能过，写文件时权限正常。
# 若 chmod 555（去 w 位）→ 预检与写入都会失败 → 用来触发两条被测路径。
# root 场景不可用：root 绕过常规写权限检测，必须有非 root uid。
NONROOT="setpriv --reuid $NONROOT_UID --regid $NONROOT_UID --clear-groups"

setup_dir() {
    local dir
    dir=$(mktemp -d /tmp/wb-login-test.XXXXXX)
    mkdir -p "$dir/auths"
    chown -R "$NONROOT_UID:$NONROOT_UID" "$dir"
    printf '%s' "$dir"
}

# T1：喂假凭据 JSON 绕过 OAuth，直测内嵌 Python 落盘段（issue 的复现方式）。
# 落盘段从 login.sh 源码提取后 exec 原样执行（零逻辑副本，脚本改了测试跟着走）。
# 提取器写成临时文件（而非 stdin/heredoc）：以 uid 12345 运行时要能读 login.sh，
# 而仓库内文件不一定对非 root 可读——由调用方先把 login.sh 复制成 644 再传入。
run_write_stage() {
    local login_sh="$1" auth_dir="$2" extractor="$3"
    WB2A_LOGIN_TOKEN=tok \
    WB2A_LOGIN_REFRESH=ref \
    WB2A_LOGIN_EXPIRES_AT=1893456000 \
    WB2A_LOGIN_DOMAIN=www.codebuddy.cn \
    WB2A_LOGIN_USER_ID=12345 \
    WB2A_LOGIN_ENT_ID='' \
    WB2A_LOGIN_NICKNAME=test \
    WB2A_LOGIN_REALM=cn \
    WB2A_LOGIN_AUTH_FILE="$auth_dir/auths/workbuddy-12345.json" \
    WB2A_LOGIN_ACTION=新增 \
    WB2A_LOGIN_TEST_SH="$login_sh" \
    $NONROOT python3 "$extractor" 2>&1
}

# 落盘段提取器：定位第三个 heredoc（"import json, os, sys, tempfile" 起），
# exec 其内容。写到全局可读的临时文件，供 uid 12345 的 python3 读取。
write_extractor() {
    local f
    f=$(mktemp /tmp/wb-login-stage-exec.XXXXXX.py)
    cat >"$f" <<'PYEOF'
import os, sys

login_sh = os.environ["WB2A_LOGIN_TEST_SH"]
src = open(login_sh, encoding="utf-8").read()

# 提取落盘 heredoc 的 Python 体，与 login.sh 同逻辑原样执行。
# 边界从 "import json, os, sys, tempfile" 起，到该 heredoc 的终结符止。
start = src.index("import json, os, sys, tempfile")
end = src.index("PYEOF", start)
exec(compile(src[start:end], "login-auth-stage", "exec"), {"__name__": "__main__"})
PYEOF
    chmod 644 "$f"
    printf '%s' "$f"
}

# ─── T1：写入失败时指引可达（mkstemp 在 try 内）────────────────────
t1() {
    say "T1: 落盘段目录不可写 → 打印指引而非裸 traceback（缺陷 1）"
    local dir rc extractor login_copy
    dir=$(setup_dir)
    chmod 555 "$dir/auths"   # 属主也失去写权限（模拟 chown 给了别的 uid）
    extractor=$(write_extractor)
    login_copy="$dir/login.sh"   # 仓库内文件对 uid 12345 未必可读，复制成 644
    cp "$LOGIN_SH" "$login_copy"
    chmod 644 "$login_copy"

    set +e
    OUT=$(run_write_stage "$login_copy" "$dir" "$extractor" 2>&1)
    rc=$?
    set -e

    if [[ $rc -ne 0 ]]; then
        ok "落盘段以非零退出码失败（rc=$rc）"
    else
        fail "落盘段意外成功"
    fi
    if grep -q "不可写" <<<"$OUT"; then
        ok "输出含「不可写」指引文案"
    else
        fail "缺少「不可写」指引（诊断仍是死代码？）"
    fi
    if grep -q "Traceback" <<<"$OUT"; then
        ok "Traceback 仍在（raise 保留，语义同 issue 修复方向 1）"
    else
        fail "Traceback 消失了（不应吞异常）"
    fi
    if grep -q "chown -R 10001:10001" <<<"$OUT"; then
        ok "指引含 chown 命令"
    else
        fail "指引缺少 chown 命令"
    fi
    rm -rf "$dir" "$extractor"
}

# ─── T2：预检在 OAuth 启动前退出（缺陷 2）───────────────────────────
# 构造不可写 auths/，login 二进制 stub 每次被调（realm/url/poll）都先写 marker：
# marker 出现 = OAuth 流程已启动（预检失效）；marker 未出现且退出码非 0 +
# 打印容器内登录指引 = 预检正确挡在浏览器流程之前。
# 注意 auths/ 必须预先存在：login.sh 的 mkdir -p 对已存在目录幂等（不修权限），
# 若不存在会把它创建成可写，预检就测不到了。
t2() {
    say "T2: auths/ 不可写 → OAuth 启动前 fail-fast（缺陷 2）"
    local dir rc marker
    dir=$(setup_dir)
    marker="$dir/.oauth-started"

    mkdir -p "$dir/skel/auths"
    cp "$LOGIN_SH" "$dir/skel/login.sh"
    cat > "$dir/skel/login" <<'STUB'
#!/usr/bin/env bash
# OAuth 流程 stub：login.sh 会先 $LOGIN_BIN url 拿授权 URL。真实流程此处
# 必然在浏览器打开后才会往下走——这里写 marker 证明"已进入 OAuth 启动段"。
echo "stub-url-called" >> "${WB2A_LOGIN_OAUTH_MARKER:-/dev/null}"
if [[ "${1:-}" == "url" ]]; then echo "https://stub.invalid/auth"; exit 0; fi
if [[ "${1:-}" == "realm" ]]; then echo "cn"; exit 0; fi
exit 0
STUB
    chmod 755 "$dir/skel/login" "$dir/skel/login.sh"
    chown -R "$NONROOT_UID:$NONROOT_UID" "$dir/skel"
    chmod 555 "$dir/skel/auths"   # auths/ 不可写

    set +e
    OUT=$(cd "$dir/skel" && WB2A_LOGIN_OAUTH_MARKER="$marker" \
        $NONROOT bash ./login.sh --realm=cn 2>&1 </dev/null)
    rc=$?
    set -e

    if [[ $rc -ne 0 ]]; then
        ok "预检以非零退出码拦截（rc=$rc）"
    else
        fail "login.sh 意外走完（预检未生效）"
    fi
    if grep -q "无法写入" <<<"$OUT"; then
        ok "输出含「无法写入」预检文案"
    else
        fail "缺少预检指引文案"
    fi
    if grep -q "docker compose exec -it free2api" <<<"$OUT"; then
        ok "指引含容器内登录命令"
    else
        fail "指引缺少容器内登录命令"
    fi
    if [[ ! -e "$marker" ]]; then
        ok "未进入 OAuth 启动段（marker 未写）"
    else
        fail "OAuth 流程已启动（预检晚于浏览器流程）"
    fi
    if compgen -G "$dir/skel/auths/workbuddy-*.json" >/dev/null; then
        fail "意外写入了 auth 文件"
    else
        ok "未落盘任何 auth 文件"
    fi
    rm -rf "$dir"
}

# ─── 对照：目录可写时预检不拦截（防误伤 happy path）─────────────────
t3() {
    say "T3（对照）: auths/ 可写 → 预检不拦截，OAuth 启动段可达"
    local dir rc marker
    dir=$(setup_dir)
    marker="$dir/.oauth-started"
    mkdir -p "$dir/skel"
    cp "$LOGIN_SH" "$dir/skel/login.sh"
    cat > "$dir/skel/login" <<'STUB'
#!/usr/bin/env bash
echo "stub-url-called" >> "${WB2A_LOGIN_OAUTH_MARKER:-/dev/null}"
if [[ "${1:-}" == "url" ]]; then echo "https://stub.invalid/auth"; exit 0; fi
if [[ "${1:-}" == "realm" ]]; then echo "cn"; exit 0; fi
exit 1   # poll 阶段故意失败：只需走到 OAuth 启动，无需真 token
STUB
    chmod 755 "$dir/skel/login"
    mkdir -p "$dir/skel/auths"
    chown -R "$NONROOT_UID:$NONROOT_UID" "$dir/skel"

    set +e
    # login.sh 会 read -rp 等 y：stdin 给 /dev/null（非 tty），read 立即 EOF → ans 空 → 取消退出
    OUT=$(cd "$dir/skel" && WB2A_LOGIN_OAUTH_MARKER="$marker" \
        $NONROOT bash ./login.sh --realm=cn </dev/null 2>&1 | head -20)
    rc=${PIPESTATUS[0]}
    set -e

    if [[ -e "$marker" ]]; then
        ok "OAuth 启动段可达（happy path 未被误伤）"
    else
        fail "可写目录也被预检拦截（误伤）"
    fi
    if grep -q "无法写入" <<<"$OUT"; then
        fail "可写目录误报不可写"
    else
        ok "无「无法写入」误报"
    fi
    rm -rf "$dir"
}

say "login.sh 回归测试（issue #160）：LOGIN_SH=$LOGIN_SH"
t1
t2
t3
say ""
say "通过: $PASS  失败: $FAIL"
[[ $FAIL -eq 0 ]] || exit 1

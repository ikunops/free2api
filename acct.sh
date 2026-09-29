#!/usr/bin/env bash
# acct.sh — 账号运维工具包装（临时停用 / 恢复 / 复活）
#
# 用法:
#   ./acct.sh list                     # 列出账号与双位状态
#   ./acct.sh disable <uid> [原因]     # 临时停用（对话流量摘除，保留在池里）
#   ./acct.sh enable  <uid>            # 解除手动停用
#   ./acct.sh revive  <uid>            # 解除系统自动禁用
#
# 需 config 里 admin.enabled = true（且 api_key 非空）。CLI 参数原样透传 cmd/acct，
# 如 -config / -server 也支持：./acct.sh -server http://127.0.0.1:7863 list
#
# 二进制升级: go build -o acct ./cmd/acct
set -euo pipefail
cd "$(dirname "$0")"
exec go run ./cmd/acct "$@"

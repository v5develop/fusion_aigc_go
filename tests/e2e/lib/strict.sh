#!/usr/bin/env bash
# e2e 脚本的严格模式（source 使用，可重复 source）。
#
# 背景：set -u 遇到未绑定变量时，叠加 EXIT trap（cleanup）后，退出码可能被 cleanup 里最后一条命令的状态盖成 0，
# CI 看到「通过」。这里统一三件事：
#   1. set -Eeuo pipefail，并用 ERR trap 打出出错行号与退出码（-E 让函数 / 子 shell 里也触发）
#   2. EXIT trap 里的 cleanup 必须保留原退出码（写法见下）
#   3. 汇总阶段若失败计数 > 0，显式 exit 1
#
# cleanup 的写法（第一行取退出码，最后一行带着它退出）：
#     cleanup() {
#       local rc=$?
#       ...清理，失败的命令要自己加 || true...
#       exit "$(e2e_rc "$rc")"
#     }
#     trap cleanup EXIT
#
# 为什么要 e2e_rc：实测 macOS 自带的 bash 3.2 在「未绑定变量」致命错误后进入 EXIT trap 时 $? 是 0（ERR trap 也不触发），
# 单靠保留 $? 不够。所以约定：只有走到 e2e_finish 末尾才算正常结束（置 E2E_DONE=1）；EXIT 时 $? 为 0 却没走到那里，
# 说明脚本是中途被打断的，e2e_rc 把退出码改成 1。
#
# 汇总：脚本末尾调用 e2e_finish；用例失败时若不想立刻退出，E2E_FAIL=$((E2E_FAIL+1)) 累计即可。
#
# 变量后紧跟非 ASCII 字符（全角括号 / 冒号等）要写成 ${var}，否则 bash 可能把这些字节当成变量名的一部分，
# 在 set -u 下报「未绑定变量」。检查：perl -CSD -ne 'print "$ARGV:$.: $_" if /\$[A-Za-z_][A-Za-z0-9_]*[^\x00-\x7F]/' 脚本...

[[ -n ${E2E_STRICT_LOADED:-} ]] && return 0
E2E_STRICT_LOADED=1

set -Eeuo pipefail
# 只在顶层 shell 打印：子 shell（$(...) / ( ... ) / 后台）里的失败常是被 if / || 吞掉的探测，打印会误导；
# 子 shell 照样 exit，由外层决定是否致命。
trap 'rc=$?; (( BASH_SUBSHELL == 0 )) && echo "[e2e] 第 $LINENO 行出错，退出码 $rc" >&2; exit $rc' ERR

E2E_FAIL=0

# e2e_finish 汇总收尾：有失败计数就以 1 退出（EXIT trap 里的 cleanup 会保留这个退出码）。
e2e_finish() {
  if (( ${E2E_FAIL:-0} > 0 )); then
    echo "[e2e] 失败 ${E2E_FAIL} 项" >&2
    exit 1
  fi
  E2E_DONE=1
}

# e2e_rc 原退出码 → 最终退出码：$?=0 但没有走到 e2e_finish 末尾（如 set -u 的未绑定变量被吞掉了状态）按 1 处理。
e2e_rc() {
  if [[ $1 == 0 && ${E2E_DONE:-0} != 1 ]]; then echo 1; else echo "$1"; fi
}

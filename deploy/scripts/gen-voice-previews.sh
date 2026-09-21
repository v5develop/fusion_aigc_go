#!/usr/bin/env bash
# 批量生成系统音色试听（会真实调用百炼合成并写入公有桶/本地存储）
# 用法：ADMIN=http://<host>:8889/admin/v1 deploy/scripts/gen-voice-previews.sh [--force]
#   默认只处理还没有试听的音色；--force 则全部重新生成。
set -euo pipefail

ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
ADMIN_ID=${ADMIN_ID:-1}
FORCE=${1:-}

list=$(curl -sS -H "X-Admin-Id: $ADMIN_ID" "$ADMIN/digital-humans/voices")
if [[ $(jq -r .code <<<"$list") != 0 ]]; then
  printf '\033[31m取音色列表失败：%s\033[0m\n' "$list" >&2; exit 1
fi

filter='.data.list[] | select(.status == 1)'
[[ $FORCE != "--force" ]] && filter="$filter | select(.preview_url == \"\" or .preview_url == null)"
ids=$(jq -r "$filter | .id" <<<"$list")

total=$(wc -w <<<"$ids" | tr -d ' ')
[[ $total == 0 ]] && { echo "没有需要生成的音色（加 --force 可全部重来）"; exit 0; }
echo "待生成 ${total} 个"

ok=0; fail=0
for id in $ids; do
  name=$(jq -r ".data.list[] | select(.id == $id) | .name" <<<"$list")
  resp=$(curl -sS -X POST -H "X-Admin-Id: $ADMIN_ID" "$ADMIN/digital-humans/voices/$id/preview")
  if [[ $(jq -r .code <<<"$resp") == 0 ]]; then
    printf '\033[32m  ✓ %-8s %s\033[0m\n' "$name" "$(jq -r .data.preview_url <<<"$resp")"
    ok=$((ok+1))
  else
    printf '\033[31m  ✗ %-8s %s\033[0m\n' "$name" "$(jq -r .msg <<<"$resp")"
    fail=$((fail+1))
  fi
done
echo "完成：成功 ${ok}，失败 ${fail}"

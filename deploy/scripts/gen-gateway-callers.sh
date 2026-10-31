#!/usr/bin/env bash
# 一次生成 ai-gateway 的两个调用方密钥，并同时写好「Go / sidecar 侧」与「gateway 侧」两份配置。
#
# 为什么是两个而不是一个：fusion-go（Go 的 studio / 画布服务）与 prompt-reverse（反推 sidecar）是两个独立身份。
# 分开后可以单独吊销、单独轮换，gateway 的成本与审计直接按调用方归属；sidecar 也不必持有 Go 服务的密钥。
# 两边的 scopes 相同（calls、catalog），分开不增加任何配置负担——本脚本一步完成。
#
# 用法：
#   bash deploy/scripts/gen-gateway-callers.sh --go-env /opt/aigc-local-life/deploy.env --gw-env ./aig-callers.env
#   --go-env  Go 部署的 deploy.env（就地更新 AI_GATEWAY_KEY_ID / AI_GATEWAY_SECRET / REVERSE_AIG_KEY_ID / REVERSE_AIG_SECRET，
#             其余行原样保留；更新前备份为 <文件>.bak.<时间戳>）。文件不存在则新建。
#   --gw-env  输出给 ai-gateway 的环境变量文件（AIG_CALLER_GO_* / AIG_CALLER_REVERSE_*），把这几行合并进 gateway 的 .env 后重启 serve。
#   --rotate  已有密钥时强制重新生成（默认：Go 侧已有非空值就沿用，保证重复执行不会把两边改得不一致）。
#
# 安全：密钥只写入上述两个文件（权限 600），不打印到终端、不写日志。轮换时两边必须同时更新并重启。
set -Eeuo pipefail

GO_ENV="" GW_ENV="" ROTATE=0
while [[ $# -gt 0 ]]; do
  case $1 in
    --go-env) GO_ENV=$2; shift 2 ;;
    --gw-env) GW_ENV=$2; shift 2 ;;
    --rotate) ROTATE=1; shift ;;
    -h|--help) sed -n '2,17p' "$0"; exit 0 ;;
    *) echo "未知参数：$1（--help 查看用法）" >&2; exit 2 ;;
  esac
done
[[ -n $GO_ENV && -n $GW_ENV ]] || { echo "必须同时给出 --go-env 与 --gw-env（--help 查看用法）" >&2; exit 2; }

GO_KEY_ID=fusion-go-v1
REVERSE_KEY_ID=prompt-reverse-v1

gen() { openssl rand -hex 32; }
current() { # current <file> <key>：读已有值（无则空）
  [[ -f $1 ]] || return 0
  sed -n "s/^$2=//p" "$1" | tail -n 1
}

go_secret=$(current "$GO_ENV" AI_GATEWAY_SECRET)
reverse_secret=$(current "$GO_ENV" REVERSE_AIG_SECRET)
if [[ $ROTATE == 1 || -z $go_secret ]]; then go_secret=$(gen); fi
if [[ $ROTATE == 1 || -z $reverse_secret ]]; then reverse_secret=$(gen); fi
[[ $go_secret != "$reverse_secret" ]] || { echo "两个密钥意外相同，请重试" >&2; exit 1; }

umask 077

# Go / sidecar 侧：就地替换或追加四个键，其余内容不动
upsert() { # upsert <file> <key> <value>
  if grep -q "^$2=" "$1"; then
    awk -v k="$2" -v v="$3" 'BEGIN{FS=OFS="="} $1==k{print k"="v; next} {print}' "$1" > "$1.tmp" && mv "$1.tmp" "$1"
  else
    printf '%s=%s\n' "$2" "$3" >> "$1"
  fi
}
if [[ -f $GO_ENV ]]; then cp -p "$GO_ENV" "$GO_ENV.bak.$(date +%Y%m%d%H%M%S).$$"; else : > "$GO_ENV"; fi
upsert "$GO_ENV" AI_GATEWAY_KEY_ID "$GO_KEY_ID"
upsert "$GO_ENV" AI_GATEWAY_SECRET "$go_secret"
upsert "$GO_ENV" REVERSE_AIG_KEY_ID "$REVERSE_KEY_ID"
upsert "$GO_ENV" REVERSE_AIG_SECRET "$reverse_secret"
chmod 600 "$GO_ENV"

# gateway 侧：单独一份，合并进 gateway 的 .env
cat > "$GW_ENV" <<EOF
# 由 fusion_aigc_go deploy/scripts/gen-gateway-callers.sh 生成于 $(date '+%F %T')；合并进 ai-gateway 的 .env 后重启 serve。
AIG_CALLER_GO_KEY_ID=$GO_KEY_ID
AIG_CALLER_GO_SECRET=$go_secret
AIG_CALLER_REVERSE_KEY_ID=$REVERSE_KEY_ID
AIG_CALLER_REVERSE_SECRET=$reverse_secret
EOF
chmod 600 "$GW_ENV"

echo "已写入（密钥未打印）："
echo "  Go / sidecar 侧：$GO_ENV  → AI_GATEWAY_KEY_ID / AI_GATEWAY_SECRET / REVERSE_AIG_KEY_ID / REVERSE_AIG_SECRET"
echo "  gateway 侧：    $GW_ENV  → AIG_CALLER_GO_* / AIG_CALLER_REVERSE_*"
echo "下一步：把 $GW_ENV 合并进 ai-gateway 的 .env 并重启 serve；Go 侧重新执行 install.sh；然后跑 check-studio-readiness.sh 验证。"

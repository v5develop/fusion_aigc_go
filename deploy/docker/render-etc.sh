#!/usr/bin/env bash
# 把 deploy/etc/*.yaml 渲染成容器用的配置：
#   · 127.0.0.1 换成 compose 服务名（etcd / mysql / redis / mock-* / gateway-api …）；
#   · 注入口令、Redis 库号、外部地址、JWT 密钥（来自 .env 或 .env.prod，缺省值见下方）。
#
#   deploy/docker/render-etc.sh          → 开发：deploy/docker/etc（口令默认 123456，mock 可用）
#   AIGC_ENV=prod deploy/docker/render-etc.sh   → 生产：deploy/docker/etc-prod，见下方「生产」
#
# 生产模式额外做的事（缺一项直接报错，不产出半成品）：
#   · 网关 / 管理端认证 Mode dev → token（dev 会信任 X-User-Id / X-Admin-Id 请求头，任何人可冒充）；
#   · Swagger 关闭（默认是开的，接口文档不能在正式环境可见）；
#   · 存储 Storage.Mode local → oss（local 落在本机磁盘，执行面取不到，也不支持直传）；
#   · JWT 密钥必须是随机值（≥32 字节），不能是 local-dev 占位；
#   · 口令、库号、外部地址都必须显式给出，不回落到开发默认值；不允许指向 mock。
# 输出含口令与密钥，已 .gitignore。改了 deploy/etc 或环境文件后重跑即可。
set -euo pipefail
cd "$(dirname "$0")/../.."

ENV_NAME="${AIGC_ENV:-dev}"
case "${ENV_NAME}" in
  dev)  ENV_FILE=deploy/.env;      OUT=deploy/docker/etc ;;
  prod) ENV_FILE=deploy/.env.prod; OUT=deploy/docker/etc-prod ;;
  *) echo "AIGC_ENV 只能是 dev 或 prod" >&2; exit 1 ;;
esac

if [ -f "${ENV_FILE}" ]; then set -a; . "./${ENV_FILE}"; set +a; fi
if [ "${ENV_NAME}" = prod ] && [ ! -f "${ENV_FILE}" ]; then
  echo "生产缺少 ${ENV_FILE}：复制 deploy/docker/prod.env.example 为它并填入真实值" >&2; exit 1
fi

MYSQL_HOST="${MYSQL_HOST:-mysql}"
MYSQL_PASSWORD="${MYSQL_PASSWORD:-123456}"    # 必须与 Java 栈 .env 的 MYSQL_PASSWORD 一致（配置里用的是 root）
REDIS_HOST="${REDIS_HOST:-redis}"
REDIS_PASSWORD="${REDIS_PASSWORD:-123456}"    # 与 Java 栈 .env 的 REDIS_PASSWORD 一致
GATEWAY_REDIS_DB="${GATEWAY_REDIS_DB:-10}"    # 必须与 Java 服务的 REDIS_DB 一致，见 .env.example
AIGC_ENDPOINT="${AIGC_ENDPOINT:-http://mock-aigc:5055}"
UNION_BASE_URL="${UNION_BASE_URL:-http://mock-union:5056}"
LINK_PARSE_URL="${LINK_PARSE_URL:-http://mock-linkparse:5058}"
GATEWAY_FILES_BASE="${GATEWAY_FILES_BASE:-http://gateway-api:8888/files}"
TASK_ID_PREFIX="${TASK_ID_PREFIX:-AiCreationLife}"
JWT_SECRET="${JWT_SECRET:-local-dev-secret-change-me}"
ADMIN_JWT_SECRET="${ADMIN_JWT_SECRET:-local-dev-admin-secret-change-me}"

# 口令与主机进 sed 替换，只允许安全字符，否则直接拒绝（不静默产出坏配置）。
check() { case "$2" in *[!A-Za-z0-9_.:/-]*) echo "$1 含不允许的字符（只允许 [A-Za-z0-9_.:/-]）" >&2; exit 1 ;; esac; }
check MYSQL_HOST "$MYSQL_HOST"; check MYSQL_PASSWORD "$MYSQL_PASSWORD"
check REDIS_HOST "$REDIS_HOST"; check REDIS_PASSWORD "$REDIS_PASSWORD"
check GATEWAY_REDIS_DB "$GATEWAY_REDIS_DB"
check AIGC_ENDPOINT "$AIGC_ENDPOINT"; check UNION_BASE_URL "$UNION_BASE_URL"
check LINK_PARSE_URL "$LINK_PARSE_URL"; check GATEWAY_FILES_BASE "$GATEWAY_FILES_BASE"
check JWT_SECRET "$JWT_SECRET"; check ADMIN_JWT_SECRET "$ADMIN_JWT_SECRET"
case "$TASK_ID_PREFIX" in *[!A-Za-z0-9]*|"") echo "TASK_ID_PREFIX 只能是字母数字（不含下划线）" >&2; exit 1 ;; esac

if [ "${ENV_NAME}" = prod ]; then
  fail() { echo "生产渲染拒绝：$1（判据：$2）" >&2; exit 1; }
  for v in MYSQL_PASSWORD REDIS_PASSWORD GATEWAY_REDIS_DB AIGC_ENDPOINT UNION_BASE_URL LINK_PARSE_URL JWT_SECRET ADMIN_JWT_SECRET; do
    [ -n "${!v:-}" ] || fail "$v 未设置" "生产必须显式给出，不回落到开发默认值"
  done
  for v in MYSQL_PASSWORD REDIS_PASSWORD; do
    case "${!v}" in 123456|REPLACE*) fail "$v 仍是占位/开发口令" "生产口令必须替换" ;; esac
  done
  for v in JWT_SECRET ADMIN_JWT_SECRET; do
    val="${!v}"
    case "$val" in *local-dev*|*change-me*) fail "$v 是开发占位" "生产密钥必须随机生成" ;; esac
    [ "${#val}" -ge 32 ] || fail "$v 长度不足 32" "随机密钥至少 32 字节，用 openssl rand -hex 32 生成"
  done
  for v in AIGC_ENDPOINT UNION_BASE_URL LINK_PARSE_URL; do
    case "${!v}" in *mock*|*127.0.0.1*|*localhost*) fail "$v 指向 mock 或本机" "生产不能连模拟服务" ;; esac
  done
fi

rm -rf "${OUT}"; mkdir -p "${OUT}"
for src in deploy/etc/*.yaml; do
  name=$(basename "$src")
  sed \
    -e "s|127\.0\.0\.1:2379|etcd:2379|g" \
    -e "s|root:123456@tcp(127\.0\.0\.1:3306)|root:${MYSQL_PASSWORD}@tcp(${MYSQL_HOST}:3306)|g" \
    -e "s|RedisAddr: \"127\.0\.0\.1:6379\"|RedisAddr: \"${REDIS_HOST}:6379\"\n    RedisPass: \"${REDIS_PASSWORD}\"|" \
    -e "s|RedisDB: 10|RedisDB: ${GATEWAY_REDIS_DB}|" \
    -e "s|http://127\.0\.0\.1:8888/files|${GATEWAY_FILES_BASE}|g" \
    -e "s|http://127\.0\.0\.1:5055|${AIGC_ENDPOINT}|g" \
    -e "s|http://127\.0\.0\.1:5056|${UNION_BASE_URL}|g" \
    -e "s|http://127\.0\.0\.1:5058|${LINK_PARSE_URL}|g" \
    -e "s|^TaskIDPrefix: .*|TaskIDPrefix: ${TASK_ID_PREFIX}|" \
    -e "s|Secret: local-dev-secret-change-me|Secret: ${JWT_SECRET}|" \
    -e "s|Secret: local-dev-admin-secret-change-me|Secret: ${ADMIN_JWT_SECRET}|" \
    "$src" > "${OUT}/$name"

  if [ "${ENV_NAME}" = prod ]; then
    sed -i.bak \
      -e "s|^Mode: dev|Mode: prod|" \
      -e "s|^  Mode: dev|  Mode: token|" \
      -e "s|^  Mode: local|  Mode: oss|" \
      -e "s|^Name: \(.*\)|Name: \1\nSwagger: false|" \
      "${OUT}/$name" && rm -f "${OUT}/$name.bak"
  fi
done

# 渲染后检查：生产出现任何开发态痕迹就失败（判据写在消息里，便于定位）。
if [ "${ENV_NAME}" = prod ]; then
  # 只看值，不看行尾注释（注释里提到 mock 不算残留）
  bad=$(for f in "${OUT}"/*.yaml; do sed -e 's/^[[:space:]]*#.*$//' -e 's/[[:space:]]#.*$//' "$f" | grep -n -E "^  Mode: dev|local-dev|Mode: local|127\.0\.0\.1|mock-|Swagger: true" | sed "s|^|$f:|"; done || true)
  if [ -n "$bad" ]; then echo "生产配置残留开发态项（判据：见行）：" >&2; echo "$bad" >&2; exit 1; fi
  for f in "${OUT}"/gateway-api.yaml "${OUT}"/admin-api.yaml; do
    grep -q "^Swagger: false" "$f" || { echo "$f 缺 Swagger: false" >&2; exit 1; }
  done
fi

left=$(grep -n "127\.0\.0\.1" "${OUT}"/*.yaml 2>/dev/null | grep -v ":[0-9]*:[[:space:]]*#" || true)
if [ -n "$left" ]; then
  echo "注意：渲染后仍有 127.0.0.1（判据：容器里指向自己，连不上）——请补规则或改 deploy/etc：" >&2
  echo "$left" >&2
fi
echo "✓ 已渲染 $(ls "${OUT}"/*.yaml | wc -l) 份到 ${OUT}（环境：${ENV_NAME}）"

#!/usr/bin/env bash
# aigc-local-life 一键部署 / 升级（Linux x86_64，root 执行）
#
#   bash install.sh            安装或升级（可重复执行）：检查环境 → 建表 → 生成配置 → 注册服务并启动 → 自检
#   bash install.sh status     各服务运行状态
#   bash install.sh restart    重启全部
#   bash install.sh stop       停止全部
#   bash install.sh logs <服务> 看日志，如 logs gateway-api
#   bash install.sh voices     生成系统音色试听（首次部署后执行一次）
#   bash install.sh check-billing   计费配置检查（缺场景/步骤/单价、档位配错会报出来）
#   bash install.sh seed-templates  造联调用的公共素材（公共数字人 / 模板 / 口播背景 / 动作视频）
#   bash install.sh uninstall  停止并注销服务（不删数据库、不删安装目录）
#   bash install.sh prepare    容器部署专用（docker compose 的 prepare 服务调用）：检查数据库 → 迁移 → 生成配置 → 渲染 nginx → 计费自检
#
# 不影响服务器上已有的项目：只新建 aigc-local-life-* 服务，端口不与已占用的冲突（冲突会停下来提示），
# 数据库只新建本项目的 afv_* 新表 / 插配置数据，不改现有表结构。
set -euo pipefail

PKG=$(cd "$(dirname "$0")" && pwd)
RPCS=(copywriting-rpc digitalhuman-rpc note-rpc imagegen-rpc studio-rpc assetlib-rpc canvas-rpc)
APIS=(gateway-api admin-api)

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
step()   { printf '\n\033[36m== %s\033[0m\n' "$*"; }
die()    { red "✗ $*"; exit 1; }

# ---------- 配置来源 ----------
# env_get 文件 键：读 .env 里的一个键。取最后一次出现的值；支持 export、KEY = value、双引号 / 单引号、行尾注释。
# 文件不存在或读不到，输出空串，退出码恒为 0（set -e 下不能因此中断）。
env_get() {
  [[ -n ${1:-} && -f $1 && -r $1 ]] || return 0
  awk -v k="$2" '
    { sub(/\r$/, "") }
    /^[[:space:]]*(export[[:space:]]+)?[A-Za-z_][A-Za-z0-9_]*[[:space:]]*=/ {
      line = $0; sub(/^[[:space:]]*(export[[:space:]]+)?/, "", line)
      key = line; sub(/[[:space:]]*=.*$/, "", key)
      if (key != k) next
      val = line; sub(/^[^=]*=[[:space:]]*/, "", val)
      q = substr(val, 1, 1)
      if (q == "\"" || q == sprintf("%c", 39)) { val = substr(val, 2); sub(q ".*$", "", val) }
      else { sub(/[[:space:]]+#.*$/, "", val); sub(/[[:space:]]+$/, "", val) }
      found = val; has = 1
    }
    END { if (has) printf "%s", found }' "$1" || true
}

# 配置来源清单：每个值记下来自哪里，安装 / prepare 时打印出来，错了一眼能看出
SRC_LINES=()
src_line() { SRC_LINES+=("$(printf '%-30s ← %s' "$1" "$2")"); }

# resolve 变量 Java键 默认值：优先级 deploy.env 显式写的 > Java 的 .env > 默认值（默认值为空表示必须显式给）
resolve() {
  local name=$1 jkey=$2 def=$3 cur="" from=""
  eval "cur=\${$name:-}"
  if [[ -n $cur ]]; then src_line "$name" "deploy.env"; return 0; fi
  if [[ -n $jkey ]]; then
    cur=$(env_get "${JAVA_ENV_FILE:-}" "$jkey")
    if [[ -n $cur ]]; then from="Java .env（${jkey}）"; fi
  fi
  if [[ -z $cur && -n $def ]]; then cur=$def; from="默认值 $def"; fi
  if [[ -z $cur ]]; then from="未设置"; fi
  eval "$name=\$cur"
  src_line "$name" "$from"
}

load_env() {
  local f="" installed=${INSTALL_DIR:-/opt/aigc-local-life}/deploy.env
  if [[ ${AIGC_CONTAINER:-0} == 1 ]]; then
    : # 容器部署：配置全部来自环境变量（compose 的 env_file），不读写 deploy.env
  elif [[ -f $PKG/deploy.env ]]; then
    f=$PKG/deploy.env
    # 两份都存在且内容不一样：十有八九是改错了文件（改了装好的那份，而这里读的是包里这份，
    # 装完还会把它覆盖掉，改动就"凭空消失"）。踩过一次，所以这里拦一下。
    if [[ -f $installed ]] && ! cmp -s "$f" "$installed"; then
      yellow "⚠️  两份 deploy.env 内容不一致，本次用的是：$f"
      yellow "    另一份（会被覆盖）：$installed"
      yellow "    要改配置请改 ${f}；确认无误就回车继续，Ctrl-C 取消。"
      read -r _ < /dev/tty || true
    fi
  elif [[ -f $installed ]]; then
    f=$installed
  else
    cp "$PKG/deploy.env.example" "$PKG/deploy.env"
    yellow "已生成 $PKG/deploy.env，请先填写（至少填数据库密码 DB_PASS），然后重新执行：bash install.sh"
    exit 1
  fi
  # shellcheck disable=SC1090
  if [[ -n $f ]]; then source "$f"; fi
  ENV_FILE=$f
  INSTALL_DIR=${INSTALL_DIR:-/opt/aigc-local-life}
  DB_HOST=${DB_HOST:-127.0.0.1}; DB_PORT=${DB_PORT:-3306}; DB_NAME=${DB_NAME:-fusion_aigc}; DB_USER=${DB_USER:-root}
  # mysql / mysqldump 的额外连接参数（如 --ssl）：MySQL 8 的 caching_sha2_password 账号在不走 TLS 时可能需要
  MYSQL_OPTS=${MYSQL_OPTS:-}; read -r -a MYSQL_OPT_ARR <<< "$MYSQL_OPTS"
  GATEWAY_PORT=${GATEWAY_PORT:-18888}; ADMIN_PORT=${ADMIN_PORT:-18889}
  COPYWRITING_PORT=${COPYWRITING_PORT:-19201}; DIGITALHUMAN_PORT=${DIGITALHUMAN_PORT:-19202}
  NOTE_PORT=${NOTE_PORT:-19203}; IMAGEGEN_PORT=${IMAGEGEN_PORT:-19204}; STUDIO_PORT=${STUDIO_PORT:-19205}; ASSETLIB_PORT=${ASSETLIB_PORT:-19206}; CANVAS_PORT=${CANVAS_PORT:-19207}
  LOG_KEEP_DAYS=${LOG_KEEP_DAYS:-7}
  UNION_BASE_URL=${UNION_BASE_URL:-}; AUTH_MODE=${AUTH_MODE:-token}; SWAGGER_ENABLED=${SWAGGER_ENABLED:-0}; TASK_ID_PREFIX=${TASK_ID_PREFIX:-AiCreationLife}; LINK_PARSE_URL=${LINK_PARSE_URL:-}
  # 与 Java 同源的配置：AIGC 地址、会话 Redis、会话参数都从 Java 的 .env 读（显式写在 deploy.env 的优先）
  JAVA_ENV_FILE=${JAVA_ENV_FILE:-}
  SRC_LINES=()
  resolve AIGC_ENDPOINT AIGC_BASE_URL ""
  BILLING_MODE=${BILLING_MODE:-on}
  # Redis 地址由 Java 的 REDIS_HOST:REDIS_PORT 组成；REDIS_HOST 是容器服务名时宿主机解析不到，记下来交给 validate_release 报错
  REDIS_HOST_BAD=""
  if [[ -n ${SESSION_REDIS_ADDR:-} ]]; then
    src_line SESSION_REDIS_ADDR "deploy.env"
  else
    jh=$(env_get "${JAVA_ENV_FILE:-}" REDIS_HOST); jp=$(env_get "${JAVA_ENV_FILE:-}" REDIS_PORT)
    if [[ -n $jh ]]; then
      SESSION_REDIS_ADDR="$jh:${jp:-6379}"
      src_line SESSION_REDIS_ADDR "Java .env（REDIS_HOST:REDIS_PORT）"
      if [[ $jh != *.* && $jh != localhost ]]; then REDIS_HOST_BAD=$jh; fi
    else
      SESSION_REDIS_ADDR=""; src_line SESSION_REDIS_ADDR "未设置"
    fi
  fi
  resolve SESSION_REDIS_PASS REDIS_PASSWORD ""
  resolve SESSION_REDIS_DB REDIS_DB 0          # 与 Java 的默认一致：REDIS_DB 没配，Java 就在 0 号库
  # 会话参数必须与 Java 一致（上限 / 令牌时长）
  resolve SESSION_MAX_CONCURRENT APP_SECURITY_TOKEN_MAX_CONCURRENT_SESSIONS 0
  resolve TOKEN_ACCESS_HOURS TOKEN_ACCESS_HOURS 2
  resolve TOKEN_REFRESH_DAYS TOKEN_REFRESH_DAYS 7
  # 共享媒体服务（aigc-media：封面/水印/合成/探测/抽帧）：imagegen 复刻时调用。
  # token 的唯一权威是 Java 的 env 文件（JAVA_ENV_FILE 里的 MEDIA_SERVICE_TOKEN）；deploy.env 写了则只用于校验（见 validate_release）。地址/账密见 deploy.env。
  MEDIA_SVC_TOKEN_DEPLOY=${MEDIA_SVC_TOKEN:-}
  resolve MEDIA_SVC_TOKEN MEDIA_SERVICE_TOKEN ""
  MEDIA_SVC_BASE_URL=${MEDIA_SVC_BASE_URL:-http://127.0.0.1:18090}
  # studio（商拍 / 姿态 / 图片生成）：ai-gateway 调用方凭据与反推 sidecar。AI_GATEWAY_URL 留空 = studio 提交返回「暂未开放」
  AI_GATEWAY_URL=${AI_GATEWAY_URL:-}; AI_GATEWAY_KEY_ID=${AI_GATEWAY_KEY_ID:-}; AI_GATEWAY_SECRET=${AI_GATEWAY_SECRET:-}
  # REVERSE_BASE_URL 是 REVERSE_SIDECAR_URL 的别名（两个都写时以 REVERSE_SIDECAR_URL 为准）
  REVERSE_SIDECAR_URL=${REVERSE_SIDECAR_URL:-${REVERSE_BASE_URL:-}}
  STUDIO_INPUT_URL_PREFIXES=${STUDIO_INPUT_URL_PREFIXES:-}
  # 参考图驱动的自动校色求解服务（tools/colorfix-solver）：COLORFIX_SOLVER_URL 留空 = 功能关闭（前端隐藏「自动校色」）
  COLORFIX_SOLVER_URL=${COLORFIX_SOLVER_URL:-}; COLORFIX_SOLVER_TOKEN=${COLORFIX_SOLVER_TOKEN:-}
  # canvas（画布）：Java 主项目内网地址（放大 / 扩图 / 重绘走 creaft v1 兼容工具，透传用户 Bearer）。留空 = 三个 Java 工具返回「暂未开放」
  CANVAS_JAVA_BASE_URL=${CANVAS_JAVA_BASE_URL:-}
  MEDIA_NACOS_ADDR=${MEDIA_NACOS_ADDR:-127.0.0.1:8848}
  MEDIA_NACOS_USERNAME=${MEDIA_NACOS_USERNAME:-}
  MEDIA_NACOS_PASSWORD=${MEDIA_NACOS_PASSWORD:-}
  # 公网域名与 Java 的地址：nginx 把 App 接口和刷新令牌分别转到网关与 Java
  PUBLIC_DOMAIN=${PUBLIC_DOMAIN:-}; JAVA_UPSTREAM=${JAVA_UPSTREAM:-127.0.0.1:18080}
  FUSION_BASE_URL=${FUSION_BASE_URL:-}; FUSION_APP_KEY=${FUSION_APP_KEY:-}
  FUSION_APP_SECRET=${FUSION_APP_SECRET:-}; FUSION_KEY_VERSION=${FUSION_KEY_VERSION:-}
  SERVICES=("${RPCS[@]}" "${APIS[@]}")
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then USE_SYSTEMD=1; else USE_SYSTEMD=0; fi
}

# validate_release 正式版的硬规则。只在 install / prepare 时检查，
# 这样老安装缺新配置项时，status / logs / stop / uninstall 仍然能用。
validate_release() {
  local java_db l java_tok
  # JAVA_ENV_FILE 指向不存在的宿主机路径时，Docker 会把它挂成空目录，后面只会报「缺 AIGC_ENDPOINT」，很难查
  # JAVA_ENV_FILE 必须是可读的普通文件。Docker 在宿主机路径不存在时会建一个空目录，这里要明确报出来
  if [[ -n ${JAVA_ENV_FILE:-} ]]; then
    [[ -f $JAVA_ENV_FILE && -r $JAVA_ENV_FILE ]] || die "JAVA_ENV_FILE=$JAVA_ENV_FILE 不是可读的文件（容器部署时多半是宿主机上这个路径不存在，Docker 会建成空目录）"
  fi
  # 媒体 token 单一真相源（裸机）：Java 的 env 文件是唯一权威。deploy.env 写了 MEDIA_SVC_TOKEN 时，
  # 只能与 Java 的 MEDIA_SERVICE_TOKEN 相同，且 Java 文件里必须有；Java env 不可用（未设 JAVA_ENV_FILE）时才单独采用 deploy.env 的值。
  # 不打印取值，只报不一致 / 缺失。容器形态不读 deploy.env，不在此检查。
  if [[ ${AIGC_CONTAINER:-0} != 1 && -n ${MEDIA_SVC_TOKEN_DEPLOY:-} && -n ${JAVA_ENV_FILE:-} ]]; then
    java_tok=$(env_get "$JAVA_ENV_FILE" MEDIA_SERVICE_TOKEN)
    if [[ -z $java_tok ]]; then
      die "deploy.env 里设了 MEDIA_SVC_TOKEN，但 JAVA_ENV_FILE（${JAVA_ENV_FILE}）里没有 MEDIA_SERVICE_TOKEN。媒体 token 只能有一个来源：请把该值写进 Java 的 env 文件的 MEDIA_SERVICE_TOKEN，并删除 deploy.env 里的 MEDIA_SVC_TOKEN。"
    fi
    if [[ $java_tok != "$MEDIA_SVC_TOKEN_DEPLOY" ]]; then
      die "deploy.env 的 MEDIA_SVC_TOKEN 与 JAVA_ENV_FILE 里的 MEDIA_SERVICE_TOKEN 不一致（取值不回显）。以 Java 的 env 文件为准：删除 deploy.env 里的 MEDIA_SVC_TOKEN，或把两边改成同一个值。"
    fi
  fi
  # 会话库号必须与 Java 一致：Java 的 REDIS_DB 与这里实际用的库号不同，两边的令牌互相找不到
  java_db=$(env_get "${JAVA_ENV_FILE:-}" REDIS_DB)
  if [[ -n $java_db && $java_db != "$SESSION_REDIS_DB" ]]; then die "SESSION_REDIS_DB=$SESSION_REDIS_DB 与 Java 的 REDIS_DB=$java_db 不一致：两边的令牌会互相找不到。删掉 deploy.env 里的 SESSION_REDIS_DB，让它从 Java 读取"; fi
  if [[ -n ${REDIS_HOST_BAD:-} ]]; then die "Java 的 REDIS_HOST=$REDIS_HOST_BAD 看起来是容器服务名，宿主机解析不到。请在 deploy.env 显式设置 SESSION_REDIS_ADDR（如 127.0.0.1:6379）"; fi
  echo "配置来源（显式 > Java .env > 默认值）："
  for l in "${SRC_LINES[@]}"; do echo "    $l"; done
  case $BILLING_MODE in off|shadow|on) ;; *) die "BILLING_MODE 只能是 off / shadow / on，现在是：$BILLING_MODE" ;; esac
  # 正式版不允许免费：只接受 on（off / shadow 都不扣点）
  if [[ $BILLING_MODE != on ]]; then die "正式版只允许 BILLING_MODE=on（当前 ${BILLING_MODE}，线上不允许免费）"; fi
  # 正式版必须与 Java 共用会话：nginx 把登录、点数、素材等 /api/v1/ 接口转给 Java，令牌必须两边都认
  [[ -n $SESSION_REDIS_ADDR ]] || die "会话 Redis 未知：在 Java 的 .env 里要有 REDIS_HOST / REDIS_PORT（设 JAVA_ENV_FILE），或在 deploy.env 显式设 SESSION_REDIS_ADDR。否则 App 令牌在 Java 接口上无效"
  [[ -n $PUBLIC_DOMAIN ]] || die "PUBLIC_DOMAIN 必填：前端访问的域名（nginx 的 server_name）"
  [[ -n ${AIGC_ENDPOINT:-} ]] || die "必须填 AIGC_ENDPOINT（或在 deploy.env 设 JAVA_ENV_FILE，从 Java 的 .env 读取 AIGC_BASE_URL）"
  [[ $TASK_ID_PREFIX =~ ^[A-Za-z0-9]+$ ]] || die "TASK_ID_PREFIX 只能是字母数字（不含下划线），当前：$TASK_ID_PREFIX"
  # 正式版只支持 token：dev 会信任请求头 X-User-Id / X-Admin-Id，任何人可冒充用户或管理员
  if [[ $AUTH_MODE != token ]]; then die "正式版只支持 AUTH_MODE=token（当前 ${AUTH_MODE}）"; fi
}

port_of() {
  case $1 in
    gateway-api) echo "$GATEWAY_PORT" ;; admin-api) echo "$ADMIN_PORT" ;;
    copywriting-rpc) echo "$COPYWRITING_PORT" ;; digitalhuman-rpc) echo "$DIGITALHUMAN_PORT" ;;
    note-rpc) echo "$NOTE_PORT" ;; imagegen-rpc) echo "$IMAGEGEN_PORT" ;;
    studio-rpc) echo "$STUDIO_PORT" ;; assetlib-rpc) echo "$ASSETLIB_PORT" ;; canvas-rpc) echo "$CANVAS_PORT" ;;
  esac
}

# ---------- 数据库 ----------
find_mysql() {
  if [[ -n ${MYSQL_BIN:-} ]]; then :;
  elif command -v mysql >/dev/null 2>&1; then MYSQL_BIN=$(command -v mysql);
  elif [[ -x /www/server/mysql/bin/mysql ]]; then MYSQL_BIN=/www/server/mysql/bin/mysql;
  else die "找不到 mysql 客户端，请在 deploy.env 里填 MYSQL_BIN（宝塔一般是 /www/server/mysql/bin/mysql）"; fi
  [[ -x $MYSQL_BIN ]] || die "MYSQL_BIN=$MYSQL_BIN 不可执行"
}

# q "SQL" → 查询结果（无表头）
q() { MYSQL_PWD="$DB_PASS" "$MYSQL_BIN" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" ${MYSQL_OPT_ARR[@]+"${MYSQL_OPT_ARR[@]}"} --default-character-set=utf8mb4 -N -B "$DB_NAME" -e "$1"; }

run_sql_file() { MYSQL_PWD="$DB_PASS" "$MYSQL_BIN" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" ${MYSQL_OPT_ARR[@]+"${MYSQL_OPT_ARR[@]}"} --default-character-set=utf8mb4 "$DB_NAME" < "$1"; }
db_exec() { MYSQL_PWD="$DB_PASS" "$MYSQL_BIN" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" ${MYSQL_OPT_ARR[@]+"${MYSQL_OPT_ARR[@]}"} --default-character-set=utf8mb4 "$DB_NAME" -e "$1"; }

has_table()  { [[ $(q "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$DB_NAME' AND table_name='$1'") != 0 ]]; }
has_column() { [[ $(q "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='$DB_NAME' AND table_name='$1' AND column_name='$2'") != 0 ]]; }

# 每个脚本先看数据库里是否已经有它建的东西，有就跳过（手工执行过的也能识别），所以可以重复运行 install.sh
# 迁移前的整库备份是可选的：BACKUP_BEFORE_MIGRATE=1 才做（大库耗时长、占磁盘，由运维决定）。
backup_db() {
  local dump out
  if [[ ${BACKUP_BEFORE_MIGRATE:-0} != 1 ]]; then
    yellow "  ! 未开启迁移前备份（BACKUP_BEFORE_MIGRATE=0）。迁移会改生产库，要留回滚手段请设 BACKUP_BEFORE_MIGRATE=1"
    return 0
  fi
  dump="$(dirname "$MYSQL_BIN")/mysqldump"
  [[ -x $dump ]] || dump=$(command -v mysqldump || true)
  [[ -n ${dump:-} && -x $dump ]] || die "BACKUP_BEFORE_MIGRATE=1 但找不到 mysqldump"
  out="${INSTALL_DIR:-/opt/aigc-local-life}/backup/${DB_NAME}-$(date +%Y%m%d-%H%M%S).sql.gz"
  mkdir -p "$(dirname "$out")"
  MYSQL_PWD="$DB_PASS" "$dump" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" ${MYSQL_OPT_ARR[@]+"${MYSQL_OPT_ARR[@]}"} --single-transaction --quick --no-tablespaces "$DB_NAME" | gzip > "$out" \
    || die "迁移前备份失败：$out"
  green "  ✓ 迁移前备份：$out"
}

# 迁移逻辑在 migrate.sh（记录写 go_schema_history，与 Java 的 flyway_schema_history 分开）
. "$PKG/migrate.sh"

# 现有系统的表，我们只读写、不创建。测试/预发的库经常比线上落后，
# 缺表的话装完是能起来的，等前端联调点到那个功能才蹦出 500——很难查。
# 所以装之前先点一遍名，缺什么当场说清楚。
check_external_tables() {
  local missing=() t
  for t in afv_system_config afv_ai_model afv_ai_provider \
           afv_app_user afv_team afv_team_member \
           afv_admin_user afv_admin_role afv_admin_user_role \
           afv_asset afv_asset_item afv_moderation_log afv_creation_task afv_open_app \
           afv_workspace afv_workspace_member afv_workspace_tag \
           afv_public_template_assets afv_public_template_asset_placement \
           afv_public_template_asset_filter afv_public_template_filter afv_public_template_filter_option; do
    has_table "$t" || missing+=("$t")
  done
  if (( ${#missing[@]} )); then
    red "✗ 现有系统缺这些表（本项目只读写、不创建它们）："
    for t in "${missing[@]}"; do red "    - $t"; done
    die "这个库的版本比本项目要求的旧。要么换一个库，要么让现有系统先把迁移跑上来"
  fi
  green "  ✓ 现有系统的 22 张依赖表齐全"

  # 表在、列不对也会在跑到那一步时才炸（2026-10-09 就是这么出的事：Java 的 V1.0.181
  # 把 afv_asset 的 owner_type/owner_id 删了、加了必填的 workspace_id，素材库从此一条写不进去，
  # 装机检查全绿）。所以关键列也点一遍名：写法是 表:列，缺一个就停。
  local col
  for col in afv_asset:workspace_id afv_asset:entry_code afv_asset:aspect_ratio \
             afv_app_user:union_user_id afv_billing_reservation_line:status \
             afv_creation_task:entry_code afv_creation_task:result_urls afv_creation_task:error_msg; do
    has_column "${col%%:*}" "${col##*:}" || missing+=("$col")
  done
  if has_column afv_asset owner_type; then missing+=("afv_asset 还有旧列 owner_type（要求 Java 迁移 ≥ V1.0.181）"); fi
  if (( ${#missing[@]} )); then
    red "✗ 现有系统的表结构和本项目要求的对不上："
    for col in "${missing[@]}"; do red "    - $col"; done
    die "现有系统（Java）的库至少要到 V1.0.181。让他们先把迁移跑上来"
  fi
  green "  ✓ 关键列齐全（afv_asset.workspace_id 等）"

  # 缺了不影响启动、只是对应功能用不了的，提醒一声就行。
  # 注意这里一律用 if，不要写 `[[ 条件 ]] && 动作`——set -e 下条件不成立会直接退出脚本。
  if [[ $(q "SELECT COUNT(*) FROM afv_public_template_asset_placement WHERE biz_type='localLife' AND status=1 AND deleted=0") == 0 ]]; then
    yellow "  ! 模板素材里没有 localLife 的上架数据：公共数字人 / 海报模板 / 长图模板 /"
    yellow "    口播背景 / 预置动作视频都会返回空列表。跑 'bash install.sh seed-templates' 造联调数据"
  fi
}

check_db_config() {
  local missing=() k
  for k in storage_public_bucket storage_public_endpoint storage_public_access_key storage_public_secret_key \
           storage_public_sts_role_arn ai_writing_model_id; do
    [[ $(q "SELECT COUNT(*) FROM afv_system_config WHERE config_key='$k' AND deleted=0 AND COALESCE(config_value,'')<>''") != 0 ]] || missing+=("$k")
  done
  if (( ${#missing[@]} )); then
    die "afv_system_config 缺少：${missing[*]}（这些是现有系统的配置，本项目复用；请确认连的是正确的库）"
  fi

  # 私有桶：只有接真实执行面时才用得上。
  # 执行面的产物落在现有系统的私有桶里，必须转存到公有桶前端才看得到。
  # 缺了这几项，下载产物时不会签名，私有桶直接回 403——
  # 表现是"生成成功了但最后一步失败"，而且只有接真实执行面时才会碰到。
  local priv=() k2
  for k2 in storage_private_bucket storage_private_endpoint storage_private_access_key storage_private_secret_key; do
    [[ $(q "SELECT COUNT(*) FROM afv_system_config WHERE config_key='$k2' AND deleted=0 AND COALESCE(config_value,'')<>''") != 0 ]] || priv+=("$k2")
  done
  if (( ${#priv[@]} )); then
    red "✗ afv_system_config 缺少私有桶配置：${priv[*]}"
    red "  接真实执行面时，产物在它的私有桶里，必须转存才能给前端看。"
    red "  缺了会表现成「任务跑完了但结果保存失败」。"
    die "把这几项配上再装（私有桶配置在后台 系统设置里）"
  fi
  green "  ✓ 私有桶配置齐全（产物转存用）"
  if [[ $(q "SELECT COUNT(*) FROM afv_system_config WHERE config_key='ai_tts_model_id' AND deleted=0 AND COALESCE(config_value,'')<>''") == 0 ]]; then
    local id
    id=$(q "SELECT id FROM afv_ai_model WHERE code='qwen-audio-3.1-tts-flash' AND deleted=0 ORDER BY id LIMIT 1" || true)
    if [[ -n $id ]]; then
      q "INSERT INTO afv_system_config (config_key, config_value, remark) VALUES ('ai_tts_model_id', '$id', 'AI数字人配音使用的TTS模型ID')"
      green "  ✓ 已补配置 ai_tts_model_id=${id}（数字人配音模型）"
    else
      yellow "  ! 缺少配音模型配置：数字人配音、音色试听不可用，其余功能正常。补法见部署文档「配音模型」一节"
    fi
  fi
}

# ---------- 端口 ----------
# 端口被别的程序占用就停下来；被我们自己的旧进程占用（升级）没关系
check_ports() {
  local s p owner bad=0
  for s in "${SERVICES[@]}"; do
    p=$(port_of "$s")
    owner=""
    if command -v ss >/dev/null 2>&1; then
      owner=$(ss -ltnp "sport = :$p" 2>/dev/null | tail -n +2 | head -1 || true)
    elif command -v netstat >/dev/null 2>&1; then
      owner=$(netstat -ltnp 2>/dev/null | awk -v p=":$p" '$4 ~ p"$"' | head -1 || true)
    fi
    [[ -z $owner ]] && continue
    if [[ $owner == *"${s:0:15}"* ]]; then continue; fi # 进程名最多显示 15 个字符
    red "  ✗ 端口 ${p}（${s}）已被占用：$owner"
    bad=1
  done
  [[ $bad == 0 ]] || die "请在 deploy.env 里换一个没被占用的端口后重试"
  green "  ✓ 端口都可用"
}

# ensure_secret 令牌签名密钥：没有就生成一串随机值写回 deploy.env（每个环境一份，升级时沿用）
# App 与后台各一份，互相拿不过去。用 eval 取变量是为了兼容 bash 3.2（${!var} 在它上面不稳）。
ensure_secret() {
  local var=$1 val
  eval "val=\${$var:-}"
  [[ -n $val ]] && return
  # 容器部署没有可写回的 deploy.env：自动生成会让每次重建都换密钥，已登录用户的令牌全部失效
  [[ ${AIGC_CONTAINER:-0} == 1 ]] && die "${var} 必填（容器部署不自动生成；用 openssl rand -hex 32 生成后写进 .env，之后不要再变）"
  val=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  eval "$var=\$val"
  if grep -q "^$var=" "$ENV_FILE"; then
    sed -i.bak "s#^$var=.*#$var=$val#" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
  else
    printf '\n%s=%s\n' "$var" "$val" >> "$ENV_FILE"
  fi
  green "  ✓ 已生成 ${var}，写回 ${ENV_FILE}"
}

# gen_conf <mkconf 路径> <输出目录> [mkconf 的额外参数…]：由开发配置生成正式配置。一键安装与容器部署共用这一份。
gen_conf() {
  local bin=$1 out=$2; shift 2
  local dsn="$DB_USER:$DB_PASS@tcp($DB_HOST:$DB_PORT)/$DB_NAME?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27"
  "$bin" -src "$PKG/etc-dev" -out "$out" -logs "$INSTALL_DIR/logs" \
    -dsn "$dsn" -aigc "$AIGC_ENDPOINT" -keep-days "$LOG_KEEP_DAYS" \
    -auth-mode "$AUTH_MODE" -union "$UNION_BASE_URL" -swagger=$([[ $SWAGGER_ENABLED == 1 ]] && echo true || echo false) -task-id-prefix "$TASK_ID_PREFIX" \
    -link-parse "$LINK_PARSE_URL" \
    -media-token "$MEDIA_SVC_TOKEN" -media-nacos-addr "$MEDIA_NACOS_ADDR" \
    -media-nacos-user "$MEDIA_NACOS_USERNAME" -media-nacos-pass "$MEDIA_NACOS_PASSWORD" \
    -media-base-url "$MEDIA_SVC_BASE_URL" \
    -ai-gateway-url "$AI_GATEWAY_URL" -ai-gateway-key-id "$AI_GATEWAY_KEY_ID" -ai-gateway-secret "$AI_GATEWAY_SECRET" \
    -reverse-url "$REVERSE_SIDECAR_URL" -colorfix-solver-url "$COLORFIX_SOLVER_URL" -colorfix-solver-token "$COLORFIX_SOLVER_TOKEN" -studio-input-url-prefixes "$STUDIO_INPUT_URL_PREFIXES" \
    -free-watermark-enabled "${FREE_WATERMARK_ENABLED:-}" -free-watermark-process "${FREE_WATERMARK_OSS_PROCESS:-}" \
    -canvas-java-base-url "$CANVAS_JAVA_BASE_URL" -canvas-internal-key-id "${CANVAS_INTERNAL_KEY_ID:-}" -canvas-internal-secret "${CANVAS_INTERNAL_SECRET:-}" -plaza-ip-salt "${PLAZA_IP_HASH_SALT:-}" -replicate-enabled "${REPLICATE_ENABLED:-}" \
    -session-redis "$SESSION_REDIS_ADDR" -session-redis-pass "$SESSION_REDIS_PASS" \
    -session-redis-db "$SESSION_REDIS_DB" -session-max "$SESSION_MAX_CONCURRENT" \
    -token-access-hours "$TOKEN_ACCESS_HOURS" -token-refresh-days "$TOKEN_REFRESH_DAYS" \
    -fusion-url "$FUSION_BASE_URL" -fusion-key "$FUSION_APP_KEY" -fusion-secret "$FUSION_APP_SECRET" \
    -fusion-key-version "$FUSION_KEY_VERSION" -billing-mode "$BILLING_MODE" \
    -ports "copywriting=$COPYWRITING_PORT,digitalhuman=$DIGITALHUMAN_PORT,note=$NOTE_PORT,imagegen=$IMAGEGEN_PORT,studio=$STUDIO_PORT,assetlib=$ASSETLIB_PORT,canvas=$CANVAS_PORT,gateway=$GATEWAY_PORT,admin=$ADMIN_PORT" "$@"
  chmod 600 "$out"/*.yaml   # 配置里有数据库口令、JWT 密钥、会话 Redis 口令
}

# render_nginx：渲染 nginx 站点配置到 $INSTALL_DIR/nginx/aigc-local-life.conf（宿主机 nginx 加载它）
render_nginx() {
  mkdir -p "$INSTALL_DIR/nginx"
  sed -e "s|__SERVER_NAME__|$PUBLIC_DOMAIN|g" -e "s|__GATEWAY_PORT__|$GATEWAY_PORT|g" \
    -e "s|__ADMIN_PORT__|$ADMIN_PORT|g" -e "s|__JAVA_UPSTREAM__|$JAVA_UPSTREAM|g" \
    "$PKG/nginx/aigc-local-life.conf.tmpl" > "$INSTALL_DIR/nginx/aigc-local-life.conf"
  check_nginx_site "$INSTALL_DIR/nginx/aigc-local-life.conf"
}

# check_nginx_site 站点文件：用宿主机的 nginx 检查语法（包进最小的 http 块里）。没有 nginx 只提醒，有语法错就中止。
check_nginx_site() {
  local site=$1 d
  if ! command -v nginx >/dev/null 2>&1; then
    yellow "  ! 本机没有 nginx：无法检查站点配置语法，请在宿主机上执行 nginx -t 确认"
    return 0
  fi
  d=$(mktemp -d)
  printf 'events {}\nhttp {\n  include %s;\n}\n' "$site" > "$d/nginx.conf"
  if nginx -t -c "$d/nginx.conf" > "$d/out" 2>&1; then
    green "  ✓ nginx 站点配置语法通过（宿主机 nginx -t）"
    rm -rf "$d"
  else
    red "✗ nginx 站点配置有错："
    cat "$d/out"
    rm -rf "$d"
    die "nginx 站点配置检查失败，修好模板后重跑"
  fi
}

# ---------- 服务 ----------
exec_line() {
  local s=$1
  echo "$INSTALL_DIR/bin/$s -f $INSTALL_DIR/etc/$s.yaml"
}

write_unit() {
  local s=$1
  cat > "/etc/systemd/system/aigc-local-life-$s.service" <<EOF
[Unit]
Description=aigc-local-life $s
After=network.target

[Service]
WorkingDirectory=$INSTALL_DIR
ExecStart=/bin/bash -c "exec $(exec_line "$s") >> $INSTALL_DIR/logs/$s.out 2>&1"
Restart=always
RestartSec=3
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
EOF
}

svc_start() {
  local s=$1
  if [[ $USE_SYSTEMD == 1 ]]; then
    systemctl enable "aigc-local-life-$s" >/dev/null 2>&1 # CentOS 7 的 systemd 不支持 enable --now
    systemctl restart "aigc-local-life-$s"
  else
    nohup bash -c "exec $(exec_line "$s")" >> "$INSTALL_DIR/logs/$s.out" 2>&1 &
    echo $! > "$INSTALL_DIR/run/$s.pid"
  fi
}

svc_stop() {
  local s=$1 pid
  if [[ $USE_SYSTEMD == 1 ]]; then
    systemctl stop "aigc-local-life-$s" >/dev/null 2>&1 || true
  elif [[ -f $INSTALL_DIR/run/$s.pid ]]; then
    pid=$(cat "$INSTALL_DIR/run/$s.pid")
    kill "$pid" 2>/dev/null || true
    for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
    kill -9 "$pid" 2>/dev/null || true
    rm -f "$INSTALL_DIR/run/$s.pid"
  fi
}

svc_active() {
  local s=$1
  if [[ $USE_SYSTEMD == 1 ]]; then
    systemctl is-active --quiet "aigc-local-life-$s"
  else
    [[ -f $INSTALL_DIR/run/$s.pid ]] && kill -0 "$(cat "$INSTALL_DIR/run/$s.pid")" 2>/dev/null
  fi
}

all_stop() { local s; for s in "${APIS[@]}" "${RPCS[@]}"; do svc_stop "$s"; done; }

all_start() {
  local s
  for s in "${SERVICES[@]}"; do svc_start "$s"; done
}

# check_media_svc 校验共享媒体服务（aigc-media）的配置与可达性。
# 本栈（aigc-local-life）与 Java 栈是**两个独立的 compose**，各自构建、各自发布：
#   Java 栈（fusion-aigc 的 docker-compose.prod.yml）负责编排 media-svc 与 r-nacos；
#   本栈的 imagegen 经宿主 127.0.0.1:18090 调它（两边都是 host 网络）。
# 所以 Java 栈的校验管不到这里，而配错的表现全是**静默**的：
#   token 不对 → 每次调用 401，封面这类失败不报错，只是图没了；
#   地址不对 → 连不上，封面同样没有、也不报错。
# 媒体服务不可达只告警不拦（两个栈独立发布，Java 栈可能还没起，拦了会让本栈发布无故失败）；
# token 为空是配置错误，直接停 —— 它意味着封面功能在生产上是坏的。
check_media_svc() {
  local base="${MEDIA_SVC_BASE_URL:-}" code=""
  if [[ -z "${MEDIA_SVC_TOKEN:-}" ]]; then
    red "✗ 媒体服务 token 为空（MEDIA_SVC_TOKEN）"
    echo "  imagegen 复刻视频时要调 media-svc 抽封面，没有可用 token 时每次调用都是 401，"
    echo "  而**不会报错** —— 表现只是封面图没了，很难归因。"
    echo "  取值：deploy.env 的 MEDIA_SVC_TOKEN，或 JAVA_ENV_FILE 里 Java 的 MEDIA_SERVICE_TOKEN（两者必须相同）。"
    die "共享媒体服务的 token 未配置"
  fi
  if [[ -z "$base" ]]; then
    red "✗ 媒体服务地址为空（MEDIA_SVC_BASE_URL）"
    die "共享媒体服务的地址未配置"
  fi
  if ! command -v curl >/dev/null 2>&1; then
    yellow "  ! 本机没有 curl，跳过媒体服务探活"
    return 0
  fi
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${base%/}/healthz" 2>/dev/null || true)"
  if [[ "$code" == "200" ]]; then
    green "  ✓ 媒体服务可达（${base}）"
  else
    yellow "  ! 媒体服务探活返回 ${code:-无响应}（${base%/}/healthz）"
    echo "  媒体服务由 **Java 栈**编排（fusion-aigc 的 docker-compose.prod.yml 里的 media-svc），本栈是独立 compose。"
    echo "  若 Java 栈还没起媒体服务，本栈可以继续起，但 imagegen 的封面/水印会失败（且不报错）。"
  fi
}

# ---------- 自检 ----------
# 健康检查：token 模式下请求没有令牌，业务接口返回 401 是正常的，所以只确认进程在线（能返回 1xx–4xx）。
# 开发模式（dev）仍然校验业务返回 "code":0。
wait_up() { # wait_up 名称 URL
  local name=$1 url=$2 code=""
  for _ in $(seq 1 30); do
    code=$(curl -s -o /dev/null -m 5 -w '%{http_code}' "$url" 2>/dev/null || true)
    if [[ $code =~ ^[1-4][0-9][0-9]$ ]]; then green "  ✓ ${name}（进程在线，HTTP ${code}）"; return 0; fi
    sleep 1
  done
  red "  ✗ ${name}：没有响应（最后 HTTP ${code:-000}）"; return 1
}
wait_check() { # 名称 URL 请求头
  if [[ $AUTH_MODE == token ]]; then wait_up "$1" "$2"; else wait_ok "$@"; fi
}
wait_ok() { # wait_ok 名称 URL 请求头
  local name=$1 url=$2 header=$3 resp=""
  for _ in $(seq 1 30); do
    resp=$(curl -sS -m 5 -H "$header" "$url" 2>/dev/null || true)
    if [[ $resp == *'"code":0'* ]]; then green "  ✓ $name"; return 0; fi
    sleep 1
  done
  red "  ✗ $name 没有正常返回：${resp:-无响应}"
  return 1
}

# check_nginx_routes：后台接口必须真的经 nginx 转到 Go，而不是落到 Java 的 /api/v1/ 规则上。
# 站点配置是手工合并进宝塔的，模板改了不等于线上生效，所以这里从本机经 nginx 探一次。
# 判据：Java 的统一信封带 requestId，Go 的信封没有。命中就说明后台路由没生效。
# 只告警不拦截：第一次安装时站点可能还没合并，不该因此让整个安装失败。
check_nginx_routes() {
  local path="/api/v1/local-life/admin/copywriting?page=1&page_size=1" body="" code=""
  if [[ -z ${PUBLIC_DOMAIN:-} ]]; then yellow "  ! PUBLIC_DOMAIN 未设，跳过 nginx 路由检查"; return 0; fi
  body=$(curl -sk -m 5 --resolve "$PUBLIC_DOMAIN:443:127.0.0.1" -w '\n%{http_code}' "https://$PUBLIC_DOMAIN$path" 2>/dev/null || true)
  code=$(printf '%s' "$body" | tail -n1)
  if [[ -z $code || $code == 000 ]]; then
    body=$(curl -s -m 5 -H "Host: $PUBLIC_DOMAIN" -w '\n%{http_code}' "http://127.0.0.1$path" 2>/dev/null || true)
    code=$(printf '%s' "$body" | tail -n1)
  fi
  body=$(printf '%s' "$body" | sed '$d')
  case "$code" in
    ""|000|5*) yellow "  ! 无法经 nginx 验证后台路由（HTTP ${code:-无响应}），请手动确认 $path"; return 0 ;;
    3*) yellow "  ! 站点把请求跳转了（HTTP ${code}），无法在 HTTP 上验证后台路由，请在 HTTPS 上手动确认"; return 0 ;;
  esac
  if [[ $body == *'"requestId"'* ]]; then
    red "  ✗ 后台路由未生效：$path 被转到了 Java（响应带 requestId）"
    echo "    把 $INSTALL_DIR/nginx/aigc-local-life.conf 里的 location ^~ /api/v1/local-life/admin 合并进线上站点配置，然后 nginx -t && nginx -s reload"
    return 0
  fi
  green "  ✓ 后台路由生效（$path 由 Go 应答，HTTP ${code}）"
}

self_check() {
  local ok=0
  wait_check "App 网关（文案选项）" "http://127.0.0.1:$GATEWAY_PORT/api/v1/local-life/copywriting/options" "X-User-Id: 1" || ok=1
  wait_check "App 网关 → 图文笔记服务" "http://127.0.0.1:$GATEWAY_PORT/api/v1/local-life/notes/options" "X-User-Id: 1" || ok=1
  wait_check "App 网关 → 海报/口播服务" "http://127.0.0.1:$GATEWAY_PORT/api/v1/local-life/creations/options?kind=voiceover" "X-User-Id: 1" || ok=1
  wait_check "App 网关 → 数字人服务" "http://127.0.0.1:$GATEWAY_PORT/api/v1/local-life/voices/categories" "X-User-Id: 1" || ok=1
  wait_check "OSS 直传凭证（STS）" "http://127.0.0.1:$GATEWAY_PORT/api/v1/local-life/upload/credentials?kind=image" "X-User-Id: 1" || ok=1
  wait_check "运营后台" "http://127.0.0.1:$ADMIN_PORT/api/v1/local-life/admin/copywriting/model" "X-Admin-Id: 1" || ok=1
  check_nginx_routes
  if [[ $ok != 0 ]]; then
    red "有检查没通过，看日志：bash install.sh logs <服务名>（日志目录 $INSTALL_DIR/logs）"
    return 1
  fi
}

# ---------- 命令 ----------
cmd_install() {
  validate_release
  step "1/6 检查环境"
  [[ $(id -u) == 0 ]] || die "请用 root 执行（宝塔终端默认就是 root）"
  [[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || die "只支持 Linux x86_64，当前 $(uname -s) $(uname -m)"
  command -v curl >/dev/null 2>&1 || die "缺少 curl"
  [[ -x $PKG/bin/gateway-api ]] || die "安装包不完整：没有 $PKG/bin/gateway-api"
  green "  ✓ $(uname -sm)，$([[ $USE_SYSTEMD == 1 ]] && echo 'systemd 管理进程' || echo '无 systemd，用 nohup 管理进程')"
  echo "  配置文件：${ENV_FILE}（以后改配置就改这一份，改完重跑本脚本）"
  check_ports # 先查端口，冲突就什么都不动
  # 共享媒体服务的 token / 地址校验放在最前面：这条不通只会让封面静默消失，不会报错，
  # 与其装完再猜，不如在动任何文件（含停旧进程）之前就拦下来。
  check_media_svc

  step "2/6 检查数据库"
  find_mysql
  q "SELECT 1" >/dev/null || die "连不上数据库 $DB_USER@$DB_HOST:$DB_PORT/${DB_NAME}，检查 deploy.env 里的 DB_*"
  green "  ✓ 已连接 $DB_HOST:$DB_PORT/$DB_NAME"
  check_external_tables
  check_db_config

  step "3/6 数据库迁移（Go_V*.sql，记录在 go_schema_history，已执行的跳过）"
  migrate_go

  step "4/6 安装程序与配置 → $INSTALL_DIR"
  mkdir -p "$INSTALL_DIR"/{bin,etc,logs,run}
  all_stop # 升级：先停旧进程
  cp -f "$PKG"/bin/* "$INSTALL_DIR/bin/"

  # 计费配置检查：缺配置的入口提交时会被拒，这里提前报出来，不靠人记得。
  if ! cmd_check_billing; then
    if [[ ${BILLING_MODE:-off} == on ]]; then
      die "BILLING_MODE=on 但计费配置有问题（见上），先在后台补齐再装"
    fi
    yellow "  ! 计费配置有问题（见上）。当前 BILLING_MODE=${BILLING_MODE:-off}，不影响安装；开计费前必须全部修好"
  fi
  chmod +x "$INSTALL_DIR"/bin/*
  render_nginx
  [[ $ENV_FILE == "$INSTALL_DIR/deploy.env" ]] || cp -f "$ENV_FILE" "$INSTALL_DIR/deploy.env"
  chmod 600 "$INSTALL_DIR/deploy.env"
  cp -rf "$PKG"/sql "$PKG"/scripts "$PKG"/docs "$PKG"/install.sh "$PKG"/deploy.env.example "$PKG"/VERSION "$INSTALL_DIR/" 2>/dev/null || true
  gen_conf "$INSTALL_DIR/bin/mkconf" "$INSTALL_DIR/etc"

  step "5/6 启动服务"
  local s
  if [[ $USE_SYSTEMD == 1 ]]; then
    for s in "${SERVICES[@]}"; do write_unit "$s"; done
    # 清理早期开发版遗留的 mock 服务（正式版不再包含它）
    systemctl disable aigc-local-life-mock-aigc >/dev/null 2>&1 || true
    rm -f /etc/systemd/system/aigc-local-life-mock-aigc.service
    systemctl daemon-reload
  fi
  all_start
  for s in "${SERVICES[@]}"; do
    sleep 0.3
    svc_active "$s" && green "  ✓ ${s}（端口 $(port_of "$s")）" || red "  ✗ $s 没起来，看 $INSTALL_DIR/logs/$s.out"
  done

  step "6/6 自检"
  self_check || exit 1

  local ip
  ip=$(hostname -I 2>/dev/null | awk '{print $1}')
  echo
  green "部署完成（版本 $(cat "$PKG/VERSION" 2>/dev/null || echo '?')）"
  if [[ $SWAGGER_ENABLED == 1 ]]; then SWAGGER_HINT="http://<服务器公网IP>:$GATEWAY_PORT/swagger（App） ，http://<服务器公网IP>:$ADMIN_PORT/swagger（运营后台）"; else SWAGGER_HINT="已关闭（正式环境默认关；需要时设 SWAGGER_ENABLED=1）"; fi
  cat <<EOF

  App 接口：  https://$PUBLIC_DOMAIN/api/v1/local-life     （经 nginx；网关只监听本机 127.0.0.1:${GATEWAY_PORT}）
  运营后台：  https://$PUBLIC_DOMAIN/api/v1/local-life/admin（与 Java 后台共用登录）
  接口文档：  $SWAGGER_HINT
  执行面：    $AIGC_ENDPOINT

  公网只开 80/443：${GATEWAY_PORT}、$ADMIN_PORT 不对外（不要在安全组里放行）。
  nginx 配置已生成：$INSTALL_DIR/nginx/aigc-local-life.conf，把它加进宝塔站点 $PUBLIC_DOMAIN 的配置并 reload。

  常用：bash $INSTALL_DIR/install.sh status | restart | logs gateway-api
  首次部署：bash $INSTALL_DIR/install.sh voices          生成系统音色试听
            bash $INSTALL_DIR/install.sh seed-templates  造公共数字人 / 模板 / 口播背景 / 动作视频（联调用）
EOF
}

# cmd_prepare 容器部署的一次性准备：服务容器在它成功退出后才启动。规则与一键安装完全一致。
cmd_prepare() {
  [[ ${AIGC_CONTAINER:-0} == 1 ]] || die "prepare 只用于容器部署（由 docker compose 的 prepare 服务调用）"
  validate_release
  echo "版本 $(cat "$PKG/VERSION" 2>/dev/null || echo '?')，执行面 ${AIGC_ENDPOINT}"

  step "1/6 检查数据库"
  find_mysql
  q "SELECT 1" >/dev/null || die "连不上数据库 $DB_USER@$DB_HOST:$DB_PORT/${DB_NAME}，检查 .env 里的 DB_* 与 MYSQL_OPTS"
  green "  ✓ 已连接 $DB_HOST:$DB_PORT/$DB_NAME"
  check_external_tables
  check_db_config

  step "2/6 数据库迁移（Go_V*.sql，记录在 go_schema_history，已执行的跳过）"
  migrate_go

  step "3/6 生成配置 → $INSTALL_DIR/etc"
  mkdir -p "$INSTALL_DIR/etc"
  gen_conf "$PKG/bin/mkconf" "$INSTALL_DIR/etc" -log-console
  # 服务容器以非 root 用户运行：配置交给它，同时只有它能读（含数据库口令、令牌密钥）
  chown -R "${RUN_UID:-10001}:${RUN_UID:-10001}" "$INSTALL_DIR/etc"
  chmod 700 "$INSTALL_DIR/etc"

  step "4/6 校验共享媒体服务（aigc-media）"
  check_media_svc

  step "5/6 渲染 nginx → $INSTALL_DIR/nginx/aigc-local-life.conf"
  render_nginx

  step "6/6 计费配置检查"
  cmd_check_billing || die "计费配置有问题（见上）：补齐后重新部署。正式环境不允许带着计费缺口启动"
  green "准备完成"
}

cmd_status() {
  local s p
  printf '%-18s %-6s %s\n' 服务 端口 状态
  for s in "${SERVICES[@]}"; do
    p=$(port_of "$s")
    if svc_active "$s"; then printf '%-18s %-6s \033[32m%s\033[0m\n' "$s" "$p" 运行中; else printf '%-18s %-6s \033[31m%s\033[0m\n' "$s" "$p" 未运行; fi
  done
}

cmd_logs() {
  local s=${1:-}
  [[ -n $s ]] || die "用法：bash install.sh logs <服务名>，服务名：${SERVICES[*]}"
  local short=${s%-rpc}; short=${short%-api}
  echo "== $INSTALL_DIR/logs/$s.out（启动输出）与 $INSTALL_DIR/logs/$short/（业务日志）"
  tail -n 50 -F "$INSTALL_DIR/logs/$s.out" "$INSTALL_DIR/logs/$short"/*.log 2>/dev/null
}

# cmd_seed_templates 造联调用的公共素材（公共数字人、各种模板、口播背景、动作视频）。
# 这些在现有系统的「模板素材」表里，平时由运营在现有后台维护；这里只是给联调造点占位数据。
cmd_seed_templates() {
  DB_HOST=$DB_HOST DB_PORT=$DB_PORT DB_NAME=$DB_NAME DB_USER=$DB_USER DB_PASS=$DB_PASS \
    MYSQL_BIN=${MYSQL_BIN:-} bash "$INSTALL_DIR/scripts/seed-templates.sh"
}

# cmd_check_billing 计费配置检查：把每个会计费的入口 × 档位真的算一遍价，缺场景/步骤/单价、
# 算出 0 点、档位配错都会报出来。只读。有 ERROR 返回非 0。
# 安装时自动跑一遍；BILLING_MODE=on 时有 ERROR 会中止安装（缺配置不准上线计费）。
cmd_check_billing() {
  local bin="$INSTALL_DIR/bin/billing-check"
  [[ -x $bin ]] || bin="$PKG/bin/billing-check"
  [[ -x $bin ]] || { yellow "  ! 没有 billing-check，跳过计费配置检查"; return 0; }
  local dsn="$DB_USER:$DB_PASS@tcp($DB_HOST:$DB_PORT)/$DB_NAME?charset=utf8mb4&collation=utf8mb4_unicode_ci&parseTime=true&loc=Asia%2FShanghai"
  "$bin" -dsn "$dsn"
}

cmd_voices() {
  command -v jq >/dev/null 2>&1 || die "需要 jq：CentOS 执行 yum install -y jq，Ubuntu/Debian 执行 apt install -y jq"
  ADMIN="http://127.0.0.1:$ADMIN_PORT/api/v1/local-life/admin" bash "$INSTALL_DIR/scripts/gen-voice-previews.sh"
}

cmd_uninstall() {
  all_stop
  if [[ $USE_SYSTEMD == 1 ]]; then
    local s
    for s in "${RPCS[@]}" "${APIS[@]}"; do
      systemctl disable "aigc-local-life-$s" >/dev/null 2>&1 || true
      rm -f "/etc/systemd/system/aigc-local-life-$s.service"
    done
    systemctl daemon-reload
  fi
  green "已停止并注销全部 aigc-local-life 服务。安装目录 $INSTALL_DIR 与数据库都保留，需要的话手动删除安装目录。"
}

load_env
case ${1:-install} in
  install)   cmd_install ;;
  status)    cmd_status ;;
  restart)   all_stop; all_start; sleep 2; cmd_status ;;
  stop)      all_stop; cmd_status ;;
  logs)      cmd_logs "${2:-}" ;;
  voices)    cmd_voices ;;
  check-billing) cmd_check_billing ;;
  seed-templates) cmd_seed_templates ;;
  uninstall) cmd_uninstall ;;
  prepare)   cmd_prepare ;;
  *) sed -n '2,11p' "$0"; exit 1 ;;
esac

#!/usr/bin/env bash
# aigc-life 一键部署 / 升级（Linux x86_64，root 执行）
#
#   bash install.sh            安装或升级（可重复执行）：检查环境 → 建表 → 生成配置 → 注册服务并启动 → 自检
#   bash install.sh status     各服务运行状态
#   bash install.sh restart    重启全部
#   bash install.sh stop       停止全部
#   bash install.sh logs <服务> 看日志，如 logs gateway-api
#   bash install.sh voices     生成系统音色试听（首次部署后执行一次）
#   bash install.sh uninstall  停止并注销服务（不删数据库、不删安装目录）
#
# 不影响服务器上已有的项目：只新建 aigc-life-* 服务，端口不与已占用的冲突（冲突会停下来提示），
# 数据库只新建本项目的 afv_* 新表 / 插配置数据，不改现有表结构。
set -euo pipefail

PKG=$(cd "$(dirname "$0")" && pwd)
RPCS=(copywriting-rpc digitalhuman-rpc note-rpc imagegen-rpc)
APIS=(gateway-api admin-api)

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
step()   { printf '\n\033[36m== %s\033[0m\n' "$*"; }
die()    { red "✗ $*"; exit 1; }

# ---------- 配置 ----------
load_env() {
  local f=""
  if [[ -f $PKG/deploy.env ]]; then
    f=$PKG/deploy.env
  elif [[ -f ${INSTALL_DIR:-/opt/aigc-life}/deploy.env ]]; then
    f=${INSTALL_DIR:-/opt/aigc-life}/deploy.env
  else
    cp "$PKG/deploy.env.example" "$PKG/deploy.env"
    yellow "已生成 $PKG/deploy.env，请先填写（至少填数据库密码 DB_PASS），然后重新执行：bash install.sh"
    exit 1
  fi
  # shellcheck disable=SC1090
  source "$f"
  ENV_FILE=$f
  INSTALL_DIR=${INSTALL_DIR:-/opt/aigc-life}
  DB_HOST=${DB_HOST:-127.0.0.1}; DB_PORT=${DB_PORT:-3306}; DB_NAME=${DB_NAME:-fusion_aigc}; DB_USER=${DB_USER:-root}
  GATEWAY_PORT=${GATEWAY_PORT:-18888}; ADMIN_PORT=${ADMIN_PORT:-18889}
  COPYWRITING_PORT=${COPYWRITING_PORT:-19201}; DIGITALHUMAN_PORT=${DIGITALHUMAN_PORT:-19202}
  NOTE_PORT=${NOTE_PORT:-19203}; IMAGEGEN_PORT=${IMAGEGEN_PORT:-19204}; MOCK_PORT=${MOCK_PORT:-15055}
  MOCK_ENABLED=${MOCK_ENABLED:-1}; LOG_KEEP_DAYS=${LOG_KEEP_DAYS:-7}
  UNION_BASE_URL=${UNION_BASE_URL:-}; AUTH_MODE=${AUTH_MODE:-dev}; JWT_SECRET=${JWT_SECRET:-}
  ADMIN_JWT_SECRET=${ADMIN_JWT_SECRET:-}; LINK_PARSE_URL=${LINK_PARSE_URL:-}
  FUSION_BASE_URL=${FUSION_BASE_URL:-}; FUSION_APP_KEY=${FUSION_APP_KEY:-}
  FUSION_APP_SECRET=${FUSION_APP_SECRET:-}; FUSION_KEY_VERSION=${FUSION_KEY_VERSION:-}
  if [[ $MOCK_ENABLED == 1 ]]; then
    AIGC_ENDPOINT=http://127.0.0.1:$MOCK_PORT
  fi
  [[ -n ${AIGC_ENDPOINT:-} ]] || die "MOCK_ENABLED=0 时必须填 AIGC_ENDPOINT"
  SERVICES=("${RPCS[@]}" "${APIS[@]}")
  [[ $MOCK_ENABLED == 1 ]] && SERVICES=(mock-aigc "${SERVICES[@]}")
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then USE_SYSTEMD=1; else USE_SYSTEMD=0; fi
}

port_of() {
  case $1 in
    gateway-api) echo "$GATEWAY_PORT" ;; admin-api) echo "$ADMIN_PORT" ;;
    copywriting-rpc) echo "$COPYWRITING_PORT" ;; digitalhuman-rpc) echo "$DIGITALHUMAN_PORT" ;;
    note-rpc) echo "$NOTE_PORT" ;; imagegen-rpc) echo "$IMAGEGEN_PORT" ;; mock-aigc) echo "$MOCK_PORT" ;;
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
q() { MYSQL_PWD="$DB_PASS" "$MYSQL_BIN" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" --default-character-set=utf8mb4 -N -B "$DB_NAME" -e "$1"; }

run_sql_file() { MYSQL_PWD="$DB_PASS" "$MYSQL_BIN" -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" --default-character-set=utf8mb4 "$DB_NAME" < "$1"; }

has_table()  { [[ $(q "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='$DB_NAME' AND table_name='$1'") != 0 ]]; }
has_column() { [[ $(q "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema='$DB_NAME' AND table_name='$1' AND column_name='$2'") != 0 ]]; }

# 每个脚本先看数据库里是否已经有它建的东西，有就跳过（手工执行过的也能识别），所以可以重复运行 install.sh
migrate() {
  local f name
  for f in "$PKG"/sql/V*.sql; do
    name=$(basename "$f")
    local done_=0
    case $name in
      V1.0.106__*) has_table afv_copywriting && done_=1 ;;
      V1.0.108__*) has_table afv_digital_human && done_=1 ;;
      V1.0.109__*) has_table afv_image_text_note && done_=1 ;;
      V1.0.110__*) done_=0 ;; # 只插配置数据，脚本本身可重复执行
      V1.0.111__*) has_table afv_image_creation && done_=1 ;;
      V1.0.112__*) has_column afv_image_creation input_image_url && done_=1 ;;
      V1.0.113__*) has_column afv_image_creation input_bg_url && done_=1 ;;
      V1.0.114__*) has_table afv_shop_profile && done_=1 ;;
      V1.0.115__*) has_column afv_image_text_note shop_profile_id && done_=1 ;;
      V1.0.116__*) has_table afv_video_creation && done_=1 ;;
      V1.0.117__*) has_column afv_video_creation source_video_url && done_=1 ;;
      *) die "不认识的迁移脚本 ${name}：请在 install.sh 的 migrate() 里补上它的判断条件" ;;
    esac
    if [[ $done_ == 1 ]]; then
      echo "  - $name 已执行过，跳过"
    else
      run_sql_file "$f" || die "执行 $name 失败"
      green "  ✓ $name"
    fi
  done
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
  val=$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')
  eval "$var=\$val"
  if grep -q "^$var=" "$ENV_FILE"; then
    sed -i.bak "s#^$var=.*#$var=$val#" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
  else
    printf '\n%s=%s\n' "$var" "$val" >> "$ENV_FILE"
  fi
  green "  ✓ 已生成 ${var}，写回 ${ENV_FILE}"
}

# ---------- 服务 ----------
exec_line() {
  local s=$1
  if [[ $s == mock-aigc ]]; then
    echo "$INSTALL_DIR/bin/mock-aigc -addr 127.0.0.1:$MOCK_PORT -sample-image '${MOCK_SAMPLE_IMAGE:-}' -sample-video '${MOCK_SAMPLE_VIDEO:-}'"
  else
    echo "$INSTALL_DIR/bin/$s -f $INSTALL_DIR/etc/$s.yaml"
  fi
}

write_unit() {
  local s=$1
  cat > "/etc/systemd/system/aigc-life-$s.service" <<EOF
[Unit]
Description=aigc-life $s
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
    systemctl enable "aigc-life-$s" >/dev/null 2>&1 # CentOS 7 的 systemd 不支持 enable --now
    systemctl restart "aigc-life-$s"
  else
    nohup bash -c "exec $(exec_line "$s")" >> "$INSTALL_DIR/logs/$s.out" 2>&1 &
    echo $! > "$INSTALL_DIR/run/$s.pid"
  fi
}

svc_stop() {
  local s=$1 pid
  if [[ $USE_SYSTEMD == 1 ]]; then
    systemctl stop "aigc-life-$s" >/dev/null 2>&1 || true
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
    systemctl is-active --quiet "aigc-life-$s"
  else
    [[ -f $INSTALL_DIR/run/$s.pid ]] && kill -0 "$(cat "$INSTALL_DIR/run/$s.pid")" 2>/dev/null
  fi
}

all_stop() { local s; for s in "${APIS[@]}" "${RPCS[@]}" mock-aigc; do svc_stop "$s"; done; }

all_start() {
  local s
  for s in "${SERVICES[@]}"; do svc_start "$s"; done
}

# ---------- 自检 ----------
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

self_check() {
  local ok=0
  wait_ok "App 网关（文案选项）" "http://127.0.0.1:$GATEWAY_PORT/api/v1/copywriting/options" "X-User-Id: 1" || ok=1
  wait_ok "App 网关 → 图文笔记服务" "http://127.0.0.1:$GATEWAY_PORT/api/v1/notes/options" "X-User-Id: 1" || ok=1
  wait_ok "App 网关 → 海报/口播服务" "http://127.0.0.1:$GATEWAY_PORT/api/v1/image-creations/options?kind=voiceover" "X-User-Id: 1" || ok=1
  wait_ok "App 网关 → 数字人服务" "http://127.0.0.1:$GATEWAY_PORT/api/v1/voices/categories" "X-User-Id: 1" || ok=1
  wait_ok "OSS 直传凭证（STS）" "http://127.0.0.1:$GATEWAY_PORT/api/v1/upload/credentials?kind=image" "X-User-Id: 1" || ok=1
  wait_ok "运营后台" "http://127.0.0.1:$ADMIN_PORT/admin/v1/copywriting/model" "X-Admin-Id: 1" || ok=1
  if [[ $ok != 0 ]]; then
    red "有检查没通过，看日志：bash install.sh logs <服务名>（日志目录 $INSTALL_DIR/logs）"
    return 1
  fi
}

# ---------- 命令 ----------
cmd_install() {
  step "1/6 检查环境"
  [[ $(id -u) == 0 ]] || die "请用 root 执行（宝塔终端默认就是 root）"
  [[ $(uname -s) == Linux && $(uname -m) == x86_64 ]] || die "只支持 Linux x86_64，当前 $(uname -s) $(uname -m)"
  command -v curl >/dev/null 2>&1 || die "缺少 curl"
  [[ -x $PKG/bin/gateway-api ]] || die "安装包不完整：没有 $PKG/bin/gateway-api"
  green "  ✓ $(uname -sm)，$([[ $USE_SYSTEMD == 1 ]] && echo 'systemd 管理进程' || echo '无 systemd，用 nohup 管理进程')"
  echo "  配置文件：$ENV_FILE"
  check_ports # 先查端口，冲突就什么都不动

  step "2/6 检查数据库"
  find_mysql
  q "SELECT 1" >/dev/null || die "连不上数据库 $DB_USER@$DB_HOST:$DB_PORT/${DB_NAME}，检查 deploy.env 里的 DB_*"
  green "  ✓ 已连接 $DB_HOST:$DB_PORT/$DB_NAME"
  check_db_config

  step "3/6 建表（只新建本项目的表，已存在的自动跳过）"
  migrate

  step "4/6 安装程序与配置 → $INSTALL_DIR"
  ensure_secret JWT_SECRET
  ensure_secret ADMIN_JWT_SECRET
  mkdir -p "$INSTALL_DIR"/{bin,etc,logs,run}
  all_stop # 升级：先停旧进程
  cp -f "$PKG"/bin/* "$INSTALL_DIR/bin/"
  chmod +x "$INSTALL_DIR"/bin/*
  [[ $ENV_FILE == "$INSTALL_DIR/deploy.env" ]] || cp -f "$ENV_FILE" "$INSTALL_DIR/deploy.env"
  chmod 600 "$INSTALL_DIR/deploy.env"
  cp -rf "$PKG"/sql "$PKG"/scripts "$PKG"/docs "$PKG"/install.sh "$PKG"/deploy.env.example "$PKG"/VERSION "$INSTALL_DIR/" 2>/dev/null || true
  local dsn="$DB_USER:$DB_PASS@tcp($DB_HOST:$DB_PORT)/$DB_NAME?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27"
  "$INSTALL_DIR/bin/mkconf" -src "$PKG/etc-dev" -out "$INSTALL_DIR/etc" -logs "$INSTALL_DIR/logs" \
    -dsn "$dsn" -aigc "$AIGC_ENDPOINT" -keep-days "$LOG_KEEP_DAYS" \
    -jwt-secret "$JWT_SECRET" -admin-jwt-secret "$ADMIN_JWT_SECRET" \
    -auth-mode "$AUTH_MODE" -union "$UNION_BASE_URL" \
    -link-parse "$LINK_PARSE_URL" \
    -fusion-url "$FUSION_BASE_URL" -fusion-key "$FUSION_APP_KEY" -fusion-secret "$FUSION_APP_SECRET" \
    -fusion-key-version "$FUSION_KEY_VERSION" \
    -ports "copywriting=$COPYWRITING_PORT,digitalhuman=$DIGITALHUMAN_PORT,note=$NOTE_PORT,imagegen=$IMAGEGEN_PORT,gateway=$GATEWAY_PORT,admin=$ADMIN_PORT"

  step "5/6 启动服务"
  local s
  if [[ $USE_SYSTEMD == 1 ]]; then
    for s in "${SERVICES[@]}"; do write_unit "$s"; done
    if [[ $MOCK_ENABLED != 1 ]]; then
      systemctl disable aigc-life-mock-aigc >/dev/null 2>&1 || true
      rm -f /etc/systemd/system/aigc-life-mock-aigc.service
    fi
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
  cat <<EOF

  App 接口：  http://<服务器公网IP>:$GATEWAY_PORT/api/v1     （给前端；内网 ${ip:-?}）
  运营后台：  http://<服务器公网IP>:$ADMIN_PORT/admin/v1
  接口文档：  http://<服务器公网IP>:$GATEWAY_PORT/swagger   （App，Swagger，可在线调试）
              http://<服务器公网IP>:$ADMIN_PORT/swagger   （运营后台）
  执行面：    $([[ $MOCK_ENABLED == 1 ]] && echo "mock（联调用，成品是示例图/示例视频）" || echo "$AIGC_ENDPOINT")

  外网访问还要放行端口：宝塔「安全」页 + 云服务器安全组，放行 ${GATEWAY_PORT}、$ADMIN_PORT
  （或者用宝塔建网站做反向代理，见部署文档）

  常用：bash $INSTALL_DIR/install.sh status | restart | logs gateway-api
  首次部署：bash $INSTALL_DIR/install.sh voices   生成系统音色试听
EOF
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

cmd_voices() {
  command -v jq >/dev/null 2>&1 || die "需要 jq：CentOS 执行 yum install -y jq，Ubuntu/Debian 执行 apt install -y jq"
  ADMIN="http://127.0.0.1:$ADMIN_PORT/admin/v1" bash "$INSTALL_DIR/scripts/gen-voice-previews.sh"
}

cmd_uninstall() {
  all_stop
  if [[ $USE_SYSTEMD == 1 ]]; then
    local s
    for s in "${RPCS[@]}" "${APIS[@]}" mock-aigc; do
      systemctl disable "aigc-life-$s" >/dev/null 2>&1 || true
      rm -f "/etc/systemd/system/aigc-life-$s.service"
    done
    systemctl daemon-reload
  fi
  green "已停止并注销全部 aigc-life 服务。安装目录 $INSTALL_DIR 与数据库都保留，需要的话手动删除安装目录。"
}

load_env
case ${1:-install} in
  install)   cmd_install ;;
  status)    cmd_status ;;
  restart)   all_stop; all_start; sleep 2; cmd_status ;;
  stop)      all_stop; cmd_status ;;
  logs)      cmd_logs "${2:-}" ;;
  voices)    cmd_voices ;;
  uninstall) cmd_uninstall ;;
  *) sed -n '2,11p' "$0"; exit 1 ;;
esac

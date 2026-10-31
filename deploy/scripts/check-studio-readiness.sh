#!/usr/bin/env bash
# 真实接入就绪检查：部署前 / 联调前跑一遍，确认 studio / 资产库 / 画布依赖的外部件都已接上。
# 每项一行 [OK] / [WARN] / [FAIL]，最后汇总；有任一 FAIL 退出码为 1（WARN 不影响退出码）。
#
# 用法：
#   bash deploy/scripts/check-studio-readiness.sh [--env-file deploy.env] [--skip <项>[,<项>...]] [--timeout 8]
#   bash deploy/scripts/check-studio-readiness.sh --selftest      # 只自测签名（hmac.md 测试向量）
#   bash deploy/scripts/check-studio-readiness.sh --list          # 列出检查项
#
# 检查项（--skip 用括号里的名字，可重复或逗号分隔）：
#   gateway  ai-gateway：POST /v1/catalog/readiness（HMAC，caller=fusion-go，purpose=catalog）
#            401/403 = secret 或 scopes 没配；200 则逐个检查 AIG_REQUIRED_GROUPS 是否就绪
#   sidecar  反推 sidecar：GET /api/status（service / Key 已配 / 版本），且地址应是本机或内网
#   media    媒体服务：GET /healthz 与带令牌的 GET /v1/mediasvc/capabilities（令牌是否有效）
#   java     Java 上游：/actuator/health；被锁死（401/403/404）则退到只读接口 JAVA_PROBE_PATH
#   mysql    表（studio / asset_ext / canvas）、入口码 offer、admin 权限码是否存在
#   nginx    PUBLIC_BASE_URL 下 studio / asset-library / canvas / prompt-library 的 location 是否已加
#   ports    本机 9205/9206/9207 有监听（本机模式；容器部署请 --skip ports）
#
# 变量（环境变量或 --env-file 里的 KEY=VALUE；凭据只从环境读，绝不打印）：
#   AI_GATEWAY_URL  AI_GATEWAY_KEY_ID  AI_GATEWAY_SECRET     gateway
#   AIG_REQUIRED_GROUPS   默认 "img.gpt-image-2 img.nano-banana-2.1 img.seedream-5-pro vid.seedance-2.5 vlm.deepseek-flash"
#   REVERSE_BASE_URL（缺省取 REVERSE_SIDECAR_URL，再缺省 http://127.0.0.1:8090）
#   REVERSE_MODEL_GROUP（可选）与 sidecar /api/status 的 model_group 比对，不一致告警；sidecar 还检查 llm_configured、gateway_ready，
#   以及 allowed_deployments（REVERSE_ALLOWED_DEPLOYMENTS 排障钉渠道）非空时告警
#   MEDIA_SVC_BASE_URL（默认 http://127.0.0.1:18090）  MEDIA_SVC_TOKEN
#   JAVA_UPSTREAM（host:port，可带 scheme）  JAVA_PROBE_PATH（默认 /api/v1/features/biz-points）  JAVA_TEST_TOKEN（可选）
#   DB_HOST DB_PORT DB_NAME DB_USER DB_PASS  MYSQL_BIN        Go 库（studio/asset/canvas 表、admin 权限）
#   JAVA_DB_NAME（默认同 DB_NAME）                            Java 库（afv_billing_* 入口码 offer）
#   STUDIO_TABLES ASSET_TABLES CANVAS_TABLES ADMIN_PERMS      表名 / 权限码列表（空格分隔）
#   ENTRY_CODES   入口码列表。缺省从 pkg/billing/audit.go 的 Targets 取 studio_* / canvas_*，取不到用 studio 三个
#   PUBLIC_BASE_URL（如 https://aigc.example.com）  NGINX_PROBE_PATHS（空格分隔的 GET 路径）
#   LOCAL_PORTS（默认 "9205 9206 9207"）
#
# 判定都是「只读探测」：不写库、不提交任务、不产生计费。
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=lib/aig-sign.sh
source "$HERE/lib/aig-sign.sh"

ALL_ITEMS="gateway sidecar media java mysql nginx ports"
ENV_FILE=""; SKIP=" "; TIMEOUT=8

while [[ $# -gt 0 ]]; do
  case $1 in
    --env-file) ENV_FILE=${2:?--env-file 需要文件路径}; shift 2 ;;
    --skip)     for s in $(echo "${2:?--skip 需要检查项}" | tr ',' ' '); do SKIP="$SKIP$s "; done; shift 2 ;;
    --timeout)  TIMEOUT=${2:?}; shift 2 ;;
    --selftest) aig_selftest; exit $? ;;
    --list)     echo "$ALL_ITEMS"; exit 0 ;;
    -h|--help)  sed -n '2,35p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *) echo "未知参数: $1（--help 看用法）" >&2; exit 2 ;;
  esac
done
for s in $SKIP; do
  [[ " $ALL_ITEMS " == *" $s "* ]] || { echo "--skip 不认识的检查项: ${s}（可选: ${ALL_ITEMS}）" >&2; exit 2; }
done

if [[ -n $ENV_FILE ]]; then
  [[ -r $ENV_FILE ]] || { echo "读不到 env 文件: $ENV_FILE" >&2; exit 2; }
  set -a; . "$ENV_FILE"; set +a   # 只导入变量，不 echo
fi

command -v curl    >/dev/null || { echo "需要 curl" >&2; exit 2; }
command -v python3 >/dev/null || { echo "需要 python3" >&2; exit 2; }

# ---- 默认值 ----
AI_GATEWAY_URL=${AI_GATEWAY_URL:-}; AI_GATEWAY_KEY_ID=${AI_GATEWAY_KEY_ID:-fusion-go-v1}; AI_GATEWAY_SECRET=${AI_GATEWAY_SECRET:-}
AIG_REQUIRED_GROUPS=${AIG_REQUIRED_GROUPS:-img.gpt-image-2 img.nano-banana-2.1 img.seedream-5-pro vid.seedance-2.5 vlm.deepseek-flash}
REVERSE_BASE_URL=${REVERSE_BASE_URL:-${REVERSE_SIDECAR_URL:-http://127.0.0.1:8090}}
MEDIA_SVC_BASE_URL=${MEDIA_SVC_BASE_URL:-http://127.0.0.1:18090}; MEDIA_SVC_TOKEN=${MEDIA_SVC_TOKEN:-}
JAVA_UPSTREAM=${JAVA_UPSTREAM:-127.0.0.1:18080}
JAVA_PROBE_PATH=${JAVA_PROBE_PATH:-/api/v1/features/biz-points}; JAVA_TEST_TOKEN=${JAVA_TEST_TOKEN:-}
DB_HOST=${DB_HOST:-127.0.0.1}; DB_PORT=${DB_PORT:-3306}; DB_NAME=${DB_NAME:-fusion_aigc}
DB_USER=${DB_USER:-root}; DB_PASS=${DB_PASS:-}; JAVA_DB_NAME=${JAVA_DB_NAME:-$DB_NAME}
STUDIO_TABLES=${STUDIO_TABLES:-afv_studio_batch afv_studio_task afv_studio_prompt_cache}
ASSET_TABLES=${ASSET_TABLES:-afv_asset_ext afv_asset_version afv_asset_relation afv_asset_tag afv_asset_tag_link}
CANVAS_TABLES=${CANVAS_TABLES:-afv_canvas afv_canvas_node_run afv_canvas_template afv_prompt_library_item}
ADMIN_PERMS=${ADMIN_PERMS:-studio:view asset_library:view canvas:view}
PUBLIC_BASE_URL=${PUBLIC_BASE_URL:-}
NGINX_PROBE_PATHS=${NGINX_PROBE_PATHS:-/api/v1/studio/history /api/v1/asset-library/tags /api/v1/canvas/canvases /api/v1/prompt-library/items}
LOCAL_PORTS=${LOCAL_PORTS:-9205 9206 9207}

if [[ -z ${ENTRY_CODES:-} ]]; then
  AUDIT_GO="$HERE/../../pkg/billing/audit.go"
  if [[ -r $AUDIT_GO ]]; then
    ENTRY_CODES=$(sed -nE 's/^[[:space:]]*\{"((studio|canvas)_[a-z0-9_]+)".*/\1/p' "$AUDIT_GO" | tr '\n' ' ')
  fi
  ENTRY_CODES=${ENTRY_CODES:-studio_tryon studio_pose studio_imagegen}
fi

# ---- 输出 ----
if [[ -t 1 ]]; then C_OK=$'\033[32m'; C_WARN=$'\033[33m'; C_FAIL=$'\033[31m'; C_0=$'\033[0m'; else C_OK=; C_WARN=; C_FAIL=; C_0=; fi
N_OK=0; N_WARN=0; N_FAIL=0
ok()   { N_OK=$((N_OK+1));     printf '%s[OK]%s   %s\n'   "$C_OK"   "$C_0" "$*"; }
warn() { N_WARN=$((N_WARN+1)); printf '%s[WARN]%s %s\n'   "$C_WARN" "$C_0" "$*"; }
fail() { N_FAIL=$((N_FAIL+1)); printf '%s[FAIL]%s %s\n'   "$C_FAIL" "$C_0" "$*"; }
skipped() { [[ $SKIP == *" $1 "* ]]; }

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT
BODY_F="$TMP/body"; ERR_F="$TMP/err"; REQ_F="$TMP/req"

# hc <curl 参数...>：结果在 HTTP_CODE，响应体在 ${BODY_F}；连不上 = 000
hc() {
  : >"$BODY_F"; : >"$ERR_F"
  HTTP_CODE=$(curl -sS -m "$TIMEOUT" -o "$BODY_F" -w '%{http_code}' "$@" 2>"$ERR_F") || HTTP_CODE=000
  [[ -n $HTTP_CODE ]] || HTTP_CODE=000
}
curl_err() { head -c 120 "$ERR_F" | tr '\n' ' '; }
# jq 的替身：pyjson <文件> <键路径,...> 逐行输出 "键=值"；缺失输出空值
pyjson() {
  python3 -I -c '
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(3)
for p in sys.argv[2:]:
    v = d
    for k in p.split("."):
        v = v.get(k) if isinstance(v, dict) else None
    print("%s=%s" % (p, "" if v is None else (json.dumps(v) if not isinstance(v, str) else v)))
' "$@"
}
base_of() { local u=$1; [[ $u == *://* ]] || u="http://$u"; echo "${u%/}"; }
host_of() { local u=${1#*://}; u=${u%%/*}; u=${u##*@}; [[ $u == \[* ]] && u=${u%%\]*} && u=${u#[} || u=${u%%:*}; echo "$u"; }
is_internal_host() {
  case $1 in
    localhost|127.*|10.*|192.168.*|169.254.*|::1|172.1[6-9].*|172.2[0-9].*|172.3[01].*) return 0 ;;
    *.*) return 1 ;;      # 带点的非内网域名/IP = 公网
    *)   return 0 ;;      # 不带点 = compose 服务名之类
  esac
}

# ---------- 1. gateway ----------
check_gateway() {
  if [[ -z $AI_GATEWAY_URL ]]; then fail "gateway: AI_GATEWAY_URL 未配置（studio 提交会返回「暂未开放」）"; return; fi
  if [[ -z $AI_GATEWAY_SECRET ]]; then fail "gateway: AI_GATEWAY_SECRET 未配置（无法签名）"; return; fi
  local base path=/v1/catalog/readiness
  base=$(base_of "$AI_GATEWAY_URL")
  python3 -I -c '
import json, sys
print(json.dumps({"items": [{"model_group": g} for g in sys.argv[1:]]}, separators=(",", ":")), end="")
' $AIG_REQUIRED_GROUPS >"$REQ_F"
  unset AIG_TS AIG_NONCE
  AIG_SECRET=$AI_GATEWAY_SECRET AIG_CALLER=fusion-go AIG_PURPOSE=catalog aig_sign POST "$path" "" "$REQ_F" \
    || { fail "gateway: 签名失败"; return; }
  hc -X POST "$base$path" --data-binary "@$REQ_F" \
    -H 'Content-Type: application/json' \
    -H 'X-Aig-Version: v1' -H 'X-Aig-Caller: fusion-go' -H "X-Aig-Key-Id: $AI_GATEWAY_KEY_ID" \
    -H 'X-Aig-Purpose: catalog' -H "X-Aig-Timestamp: $AIG_TS" -H "X-Aig-Nonce: $AIG_NONCE" \
    -H "X-Aig-Signature: $AIG_SIG"
  case $HTTP_CODE in
    000) fail "gateway: 连不上 ${base}（$(curl_err)）"; return ;;
    401|403)
      local chk; chk=$(pyjson "$BODY_F" error.check | cut -d= -f2-)
      fail "gateway: readiness 返回 ${HTTP_CODE}（${chk:-无 check}）——AI_GATEWAY_SECRET / KEY_ID 不对，或该 key 没有 catalog scope（caller=fusion-go 需要 calls+catalog）"; return ;;
    200) ;;
    *) fail "gateway: readiness 返回 $HTTP_CODE"; return ;;
  esac
  ok "gateway: readiness 200，签名与 catalog scope 有效（${base}）"
  local rows; rows=$(python3 -I -c '
import json, sys
try: d = json.load(open(sys.argv[1]))
except Exception: sys.exit(3)
for it in d.get("items", []):
    print("\t".join(str(x) for x in (it.get("model_group"), str(it.get("ready")).lower(),
          it.get("reason") or "-", it.get("enabled_deployments", "?"), it.get("available_deployments", "?"))))
' "$BODY_F") || { fail "gateway: readiness 响应不是 JSON"; return; }
  local g line
  for g in $AIG_REQUIRED_GROUPS; do
    line=$(printf '%s\n' "$rows" | awk -F'\t' -v g="$g" '$1==g {print; exit}')
    if [[ -z $line ]]; then fail "gateway: 模型组 $g 不在响应里"; continue; fi
    IFS=$'\t' read -r _ ready reason en av <<<"$line"
    if [[ $ready == true ]]; then ok "gateway: $g 就绪（可用部署 $av/${en}）"
      if [[ $av =~ ^[0-9]+$ && $en =~ ^[0-9]+$ && $av -lt $en ]]; then
        warn "gateway: $g 有部署被门控（可用 $av/${en}：熔断 / 探测失败 / 账号缺密钥），流量落在备用渠道"
      fi
    else fail "gateway: $g 未就绪（${reason}，可用部署 $av/${en}）"; fi
  done
  return 0
}

# ---------- 2. sidecar ----------
check_sidecar() {
  local base host; base=$(base_of "$REVERSE_BASE_URL"); host=$(host_of "$base")
  hc "$base/api/status"
  if [[ $HTTP_CODE == 000 ]]; then fail "sidecar: 连不上 ${base}（$(curl_err)）"; return; fi
  if [[ $HTTP_CODE != 200 ]]; then fail "sidecar: /api/status 返回 $HTTP_CODE"; return; fi
  local kv svc ver llm mock vision model group gw gready greason pinned
  kv=$(pyjson "$BODY_F" service version llm_configured mock vision model model_group gateway gateway_ready gateway_reason \
         allowed_deployments) \
    || { fail "sidecar: /api/status 不是 JSON"; return; }
  svc=$(sed -n 's/^service=//p' <<<"$kv");  ver=$(sed -n 's/^version=//p' <<<"$kv")
  llm=$(sed -n 's/^llm_configured=//p' <<<"$kv"); mock=$(sed -n 's/^mock=//p' <<<"$kv")
  vision=$(sed -n 's/^vision=//p' <<<"$kv");  model=$(sed -n 's/^model=//p' <<<"$kv")
  group=$(sed -n 's/^model_group=//p' <<<"$kv"); gw=$(sed -n 's/^gateway=//p' <<<"$kv")
  gready=$(sed -n 's/^gateway_ready=//p' <<<"$kv"); greason=$(sed -n 's/^gateway_reason=//p' <<<"$kv")
  pinned=$(sed -n 's/^allowed_deployments=//p' <<<"$kv")
  if [[ $svc == prompt-reverse ]]; then ok "sidecar: service=prompt-reverse，版本 ${ver:-?}，模型组 ${group:-${model:-?}}"
  else fail "sidecar: service=${svc:-空}，不是 prompt-reverse（地址指错了服务？）"; fi
  if [[ -z $group && $svc == prompt-reverse ]]; then
    fail "sidecar: /api/status 没有 model_group——还是直连 DeepSeek 的旧版 sidecar，请升级到经 ai-gateway 的版本"
  fi
  if [[ $llm == true ]]; then ok "sidecar: 已配 ai-gateway（llm_configured=true，网关 ${gw:-?}）"
  else fail "sidecar: llm_configured=${llm:-空}——sidecar 没配 AIG_BASE_URL / AIG_SECRET（compose：REVERSE_AIG_SECRET），读图会 502"; fi
  if [[ $llm == true ]]; then
    if [[ $gready == true ]]; then ok "sidecar: 网关模型组 ${group} 就绪（gateway_ready=true）"
    else fail "sidecar: gateway_ready=${gready:-空}（${greason:-?}）——检查 AIG_SECRET 与 ai-gateway 侧 AIG_CALLER_REVERSE_SECRET 是否一致、gateway 管理端是否已写入 DeepSeek 官方密钥"; fi
  fi
  if [[ -n ${REVERSE_MODEL_GROUP:-} && -n $group && $group != "$REVERSE_MODEL_GROUP" ]]; then
    warn "sidecar: 实际模型组 ${group} 与 env 的 REVERSE_MODEL_GROUP=${REVERSE_MODEL_GROUP} 不一致"
  fi
  if [[ -n $pinned && $pinned != "[]" ]]; then
    warn "sidecar: 只走部署 ${pinned}（REVERSE_ALLOWED_DEPLOYMENTS），排障结束后清空，否则不会切换备用渠道"
  fi
  [[ $vision == false ]] && warn "sidecar: vision=false，当前模型组不支持读图"
  [[ $mock == true ]] && warn "sidecar: mock=true，返回的是假数据，上线前必须关"
  if is_internal_host "$host"; then ok "sidecar: 地址 $host 为本机/内网"
  else warn "sidecar: 地址 $host 看起来是公网，sidecar 无鉴权，只能内网访问——确认安全组/防火墙只放行 Go 服务"; fi
  return 0
}

# ---------- 3. media ----------
check_media() {
  local base; base=$(base_of "$MEDIA_SVC_BASE_URL")
  hc "$base/healthz"
  if [[ $HTTP_CODE == 000 ]]; then fail "media: 连不上 ${base}（$(curl_err)）"; return; fi
  if [[ $HTTP_CODE == 200 ]]; then ok "media: /healthz 200（${base}）"; else fail "media: /healthz 返回 $HTTP_CODE"; return; fi
  if [[ -z $MEDIA_SVC_TOKEN ]]; then fail "media: MEDIA_SVC_TOKEN 未配置，无法验证令牌"; return; fi
  hc -H "X-Media-Token: $MEDIA_SVC_TOKEN" "$base/v1/mediasvc/capabilities"
  case $HTTP_CODE in
    200) ok "media: 令牌有效（capabilities 200）" ;;
    401) fail "media: 令牌无效（capabilities 401）——MEDIA_SVC_TOKEN 与服务端 MEDIA_SERVICE_TOKEN 不一致" ;;
    000) fail "media: capabilities 请求失败（$(curl_err)）" ;;
    *)   fail "media: capabilities 返回 $HTTP_CODE" ;;
  esac
  return 0
}

# ---------- 4. java ----------
check_java() {
  local base; base=$(base_of "$JAVA_UPSTREAM")
  hc "$base/actuator/health"
  case $HTTP_CODE in
    000) fail "java: 连不上 ${base}（$(curl_err)）"; return ;;
    200) ok "java: /actuator/health 200（${base}）"; return ;;
    401|403|404) ;;   # 生产里 actuator 被锁死：改走只读业务接口
    *) fail "java: /actuator/health 返回 ${HTTP_CODE}（${base}）"; return ;;
  esac
  local auth=(); [[ -n $JAVA_TEST_TOKEN ]] && auth=(-H "Authorization: Bearer $JAVA_TEST_TOKEN")
  hc ${auth[@]+"${auth[@]}"} "$base$JAVA_PROBE_PATH"
  case $HTTP_CODE in
    200) ok "java: actuator 已锁死，$JAVA_PROBE_PATH 带令牌 200，可达且令牌有效" ;;
    401|403)
      if [[ -n $JAVA_TEST_TOKEN ]]; then warn "java: 可达，但 JAVA_TEST_TOKEN 被拒（${HTTP_CODE}），令牌可能过期"
      else ok "java: actuator 已锁死，$JAVA_PROBE_PATH 无令牌返回 ${HTTP_CODE}（可达；要验证登录态请设 JAVA_TEST_TOKEN）"; fi ;;
    404) fail "java: actuator 与 $JAVA_PROBE_PATH 都是 404（路径不对，或 JAVA_UPSTREAM 指向了别的服务）——用 JAVA_PROBE_PATH 指定一个只读接口" ;;
    000) fail "java: 连不上 ${base}（$(curl_err)）" ;;
    *)   fail "java: $JAVA_PROBE_PATH 返回 $HTTP_CODE" ;;
  esac
  return 0
}

# ---------- 5. mysql ----------
find_mysql() {
  [[ -n ${MYSQL_BIN:-} && -x ${MYSQL_BIN:-} ]] && { echo "$MYSQL_BIN"; return; }
  command -v mysql 2>/dev/null && return
  local c; for c in /www/server/mysql/bin/mysql /usr/local/mysql/bin/mysql /opt/homebrew/bin/mysql; do [[ -x $c ]] && { echo "$c"; return; }; done
  return 1
}
my() { MYSQL_PWD="$DB_PASS" "$MYSQL" --default-character-set=utf8mb4 -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" -N -B "$@" 2>"$ERR_F"; }
in_list() { local o="" w; for w in $1; do [[ $w =~ ^[A-Za-z0-9_:.-]+$ ]] || { echo "非法标识: $w" >&2; return 1; }; o="$o${o:+,}'$w'"; done; echo "$o"; }

check_mysql() {
  if ! MYSQL=$(find_mysql); then warn "mysql: 没有 mysql 客户端（可 export MYSQL_BIN=...），跳过表 / 入口码 / 权限检查"; return; fi
  if ! my -e 'SELECT 1' >/dev/null; then fail "mysql: 连不上 $DB_USER@$DB_HOST:${DB_PORT}（$(curl_err)）"; return; fi
  ok "mysql: 已连接 $DB_USER@$DB_HOST:$DB_PORT"

  local grp tables inlist have miss t
  for grp in STUDIO:studio ASSET:asset_ext CANVAS:canvas; do
    local var=${grp%%:*}_TABLES label=${grp##*:}
    tables=${!var}
    inlist=$(in_list "$tables") || { fail "mysql: $var 含非法字符"; continue; }
    if ! have=$(my -e "SELECT table_name FROM information_schema.tables WHERE table_schema='$DB_NAME' AND table_name IN ($inlist)"); then
      fail "mysql: 查询 $label 表失败（$(curl_err)）"; continue; fi
    miss=""; for t in $tables; do grep -qx "$t" <<<"$have" || miss="$miss $t"; done
    if [[ -z $miss ]]; then ok "mysql: $label 表齐全（$(wc -w <<<"$tables" | tr -d ' ') 张，库 ${DB_NAME}）"
    else fail "mysql: $label 表缺失:${miss}（库 ${DB_NAME}；对应迁移没跑？）"; fi
  done

  # 入口码 offer：offer 启用 + 至少一个启用档位 + 至少一个 BILLED 步骤（价格是否为 0 交给 billing-check）
  local code row no nv nc bad_offer="" bad_var="" bad_comp="" okn=0
  for code in $ENTRY_CODES; do
    [[ $code =~ ^[A-Za-z0-9_]+$ ]] || { fail "mysql: 入口码非法: $code"; continue; }
    if ! row=$(my -e "SELECT COUNT(DISTINCT o.offer_code), COUNT(DISTINCT v.variant_code), COUNT(c.variant_code)
        FROM \`$JAVA_DB_NAME\`.afv_billing_offer o
        LEFT JOIN \`$JAVA_DB_NAME\`.afv_billing_offer_variant v ON v.offer_code=o.offer_code AND v.deleted=0 AND v.status='ENABLED'
        LEFT JOIN \`$JAVA_DB_NAME\`.afv_billing_charge_component c ON c.variant_code=v.variant_code AND c.deleted=0 AND c.cost_type='BILLED'
        WHERE o.entry_code='$code' AND o.deleted=0 AND o.status='ENABLED'"); then
      fail "mysql: 查询入口码 offer 失败（$(curl_err)）——JAVA_DB_NAME=$JAVA_DB_NAME 对吗"; bad_offer=""; break; fi
    IFS=$'\t' read -r no nv nc <<<"$row"
    if [[ ${no:-0} == 0 ]]; then bad_offer="$bad_offer $code"
    elif [[ ${nv:-0} == 0 ]]; then bad_var="$bad_var $code"
    elif [[ ${nc:-0} == 0 ]]; then bad_comp="$bad_comp $code"
    else okn=$((okn+1)); fi
  done
  [[ -n $bad_offer ]] && fail "mysql: 入口码没有启用的 offer:${bad_offer}（跑 studio-offers-*.sql，或后台「计费场景」手建）"
  [[ -n $bad_var   ]] && fail "mysql: 入口码有 offer 但没有启用档位:$bad_var"
  [[ -n $bad_comp  ]] && fail "mysql: 入口码有档位但没有 BILLED 计费步骤:$bad_comp"
  [[ -z $bad_offer$bad_var$bad_comp && $okn -gt 0 ]] && ok "mysql: 入口码 offer 齐全（$okn 个: ${ENTRY_CODES}）；单价另跑 install.sh check-billing"

  inlist=$(in_list "$ADMIN_PERMS") || { fail "mysql: ADMIN_PERMS 含非法字符"; return; }
  if ! have=$(my "$DB_NAME" -e "SELECT permission_code FROM afv_admin_permission WHERE permission_code IN ($inlist)"); then
    fail "mysql: 查询 admin 权限失败（$(curl_err)）"; return; fi
  miss=""; for t in $ADMIN_PERMS; do grep -qx "$t" <<<"$have" || miss="$miss $t"; done
  if [[ -z $miss ]]; then ok "mysql: admin 权限码齐全（${ADMIN_PERMS}）"; else fail "mysql: admin 权限码缺失:${miss}（Go_V1.0.22/24/26 权限迁移没跑？）"; fi
  return 0
}

# ---------- 6. nginx ----------
check_nginx() {
  if [[ -z $PUBLIC_BASE_URL ]]; then warn "nginx: PUBLIC_BASE_URL 未设置，跳过 location 检查"; return; fi
  local base p; base=$(base_of "$PUBLIC_BASE_URL")
  for p in $NGINX_PROBE_PATHS; do
    hc "$base$p"
    case $HTTP_CODE in
      000) fail "nginx: $p 连不上 ${base}（$(curl_err)）" ;;
      401|403|200|400|405|422) ok "nginx: $p -> ${HTTP_CODE}（已转发到 Go 网关，未登录被拒属正常）" ;;
      404) fail "nginx: $p -> 404（location 没加，被 Java 兜底或直接 404；检查 aigc-local-life.conf 的 ^~ 前缀，且路由在网关里存在）" ;;
      502|503|504) fail "nginx: $p -> ${HTTP_CODE}（location 有了，但后端进程没起 / 端口不对）" ;;
      *) warn "nginx: $p -> ${HTTP_CODE}（非预期，人工确认）" ;;
    esac
  done
  return 0
}

# ---------- 7. ports ----------
check_ports() {
  local p
  for p in $LOCAL_PORTS; do
    if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then ok "ports: 127.0.0.1:$p 有监听"
    else fail "ports: 127.0.0.1:$p 没有监听（对应 rpc 未启动；容器部署请 --skip ports）"; fi
  done
  return 0
}

for item in $ALL_ITEMS; do
  if skipped "$item"; then printf '[SKIP] %s（--skip）\n' "$item"; continue; fi
  "check_$item"
done

echo "----"
printf '汇总: %d OK, %d WARN, %d FAIL\n' "$N_OK" "$N_WARN" "$N_FAIL"
[[ $N_FAIL -eq 0 ]]

#!/usr/bin/env bash
# studio（商拍换装 / 姿态复刻 / 图片生成）端到端测试。
#
# 前提（make run-all 会带起 mock，另需 etcd / MySQL / 媒体服务）：
#   studio-rpc :9205、gateway-api :8888、admin-api :8889、mock-reverse :5057、mock-aigateway :5059、媒体服务 :18090
#   studio-rpc 的 deploy/etc/studio-rpc.yaml 要指向 mock：AiGateway.BaseURL=http://127.0.0.1:5059（Secret=test-secret，
#   与 mock-aigateway 的 MOCK_AIG_SECRET 一致）、Reverse.BaseURL=http://127.0.0.1:5057。
#
# 用例分两组：
#   校验类 —— 提交就被拒的参数问题，只需 gateway-api + studio-rpc，默认就跑；
#   链路类 —— 切图 → 反推 → 生图 → 入库 → 历史 → 后台，依赖 studio 业务逻辑（Submit/Poller）完成。
#            默认标 SKIP；逻辑完成后设 RUN_CHAIN=1 即启用（脚本里没有别的要改的）。
#
# 用法：tests/e2e/e2e-studio.sh            # 只跑校验类，链路类 SKIP
#       RUN_CHAIN=1 tests/e2e/e2e-studio.sh
# 环境变量：GW / ADMIN / USER_ID / MYSQL_CONTAINER / E2E_DB / BILLING_ON(=1 时断言失败任务 billing_status=已退回)
#   BILLING=1     计费用例（要求 studio-rpc 以 Billing.Mode=on 启动、RUN_CHAIN=1）：给测试用户建点数账户（2000 点）与一个
#                 只有 15 点的「余额不足」用户，逐笔核对冻结 / 结算 / 退款 / 回收（tests/e2e/lib/billing.sh），
#                 结果注入靠 mock-aigateway 的 /debug/inject（按提示词里的标记命中）。结束时删除两个用户的账户、流水与预留单。
#                 默认 0：计费用例 SKIP，其余用例不受影响（服务以 Mode=off 启动）。
#   ADMIN_SHARED  按 RBAC 查权限的 admin-api（配了 Auth.Session，与 Java 共用会话 Redis），如 http://127.0.0.1:18889；
#                 设了才跑「只有 local_life:view 的账号访问 studio 后台被拒」用例（dev 模式的 X-Admin-Id 不查权限）
#   REDIS_CONTAINER / REDIS_PASS 会话 Redis 容器（默认找名字含 redis 的）与口令（默认 123456），会话写在 10 号库 fusion:admin:access:<令牌>
ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=lib/strict.sh
source "$ROOT/tests/e2e/lib/strict.sh"   # set -Eeuo pipefail + ERR trap；EXIT 的 cleanup 要保留原退出码
HOST=${HOST:-127.0.0.1}
GW_BASE=${GW_BASE:-http://$HOST:8888}
GW=${GW:-$GW_BASE/api/v1/studio}
ADMIN=${ADMIN:-http://$HOST:8889/api/v1/studio/admin}
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 100000 ))}
RUN_CHAIN=${RUN_CHAIN:-0}
BILLING=${BILLING:-0}
BILLING_ON=${BILLING_ON:-$BILLING}
LOW_USER=$((USER_ID + 1))   # BILLING=1：余额不足用例的用户（15 点）
BILL_INIT=2000; LOW_INIT=15
AIG=${AIG:-http://$HOST:5059}
PASS=0; SKIPPED=0
TMP=$(mktemp -d)
MYSQL_CONTAINER=${MYSQL_CONTAINER:-$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 mysql || true)}
MYSQL=(docker exec -i "$MYSQL_CONTAINER" mysql --default-character-set=utf8mb4 -uroot -p123456 "${E2E_DB:-fusion_aigc}" -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }
# shellcheck source=lib/billing.sh
source "$ROOT/tests/e2e/lib/billing.sh"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }

port_up() { (exec 3<>"/dev/tcp/$HOST/$1") 2>/dev/null; }

need_port() { # need_port 端口 名称 hard|chain
  if port_up "$1"; then return; fi
  if [[ $3 == hard || $RUN_CHAIN == 1 ]]; then
    red "缺少服务：$2（$HOST:$1 连不上）。make run-all 启动全部服务，或见脚本开头说明。"; exit 2
  fi
  yellow "  提示：$2（:$1）没在跑，链路类用例需要它"
}

# ---- 请求助手 ----
# 返回完整信封 JSON；$HDR_KEY 为空串则不带 Idempotency-Key（用来测缺头）
call() { # call METHOD PATH [BODY] [KEY]
  local method=$1 path=$2 body=${3:-} key=${4-}
  local args=(-sS -X "$method" -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json')
  [[ -n $key ]] && args+=(-H "Idempotency-Key: $key")
  [[ -n $body ]] && args+=(-d "$body")
  curl "${args[@]}" "$GW$path"
}
code_of() { jq -r '.code // "none"' <<<"$1"; }
data_of() { jq -c '.data' <<<"$1"; }

api() { # 成功才返回 data，否则退出
  local resp; resp=$(call "$@")
  if [[ $(code_of "$resp") != 0 ]]; then red "FAIL $1 $2 → $resp" >&2; exit 1; fi
  data_of "$resp"
}

admin() { # admin METHOD PATH → data
  local resp; resp=$(curl -sS -X "$1" -H 'X-Admin-Id: 1' "$ADMIN$2")
  if [[ $(code_of "$resp") != 0 ]]; then red "FAIL admin $1 $2 → $resp" >&2; exit 1; fi
  data_of "$resp"
}

check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

# rejected 名称 响应 —— 业务码非 0 即算被拒；顺带打印服务端文案
rejected() {
  local c; c=$(code_of "$2")
  if [[ $c != 0 && $c != none ]]; then green "  ✓ $1（code=${c}：$(jq -r '.msg // .message // ""' <<<"$2")）"; PASS=$((PASS+1))
  else red "  ✗ $1: 本应被拒，实际 $2"; exit 1; fi
}

# 链路类用例：RUN_CHAIN=1 才执行函数体，否则 SKIP
chain() { # chain 名称 函数
  if [[ $RUN_CHAIN == 1 ]]; then echo "-- $1"; "$2"; else yellow "  SKIP $1（studio 业务逻辑完成后 RUN_CHAIN=1 启用）"; SKIPPED=$((SKIPPED+1)); fi
}

png() { # png 文件 —— 生成一张可解码的 256×320 纯色 PNG（颜色由文件名派生，不同图 sha1 不同；媒体服务要能解码才能规整 / 裁图）
  python3 - "$1" <<'PY'
import sys, zlib, struct, hashlib
w, h = 256, 320
c = hashlib.md5(sys.argv[1].encode()).digest()[:3]
raw = b"".join(b"\x00" + bytes(c) * w for _ in range(h))
def chunk(t, d): return struct.pack(">I", len(d)) + t + d + struct.pack(">I", zlib.crc32(t + d) & 0xffffffff)
open(sys.argv[1], "wb").write(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack(">IIBBBBB", w, h, 8, 2, 0, 0, 0)) + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))
PY
}

upload() { # 用本地生活网关的上传接口拿一个自家地址
  png "$TMP/$1.png"
  curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/$1.png" "$GW_BASE/api/v1/local-life/upload" | jq -r .data.url
}

poll_batch() { # poll_batch ID → 终态批次详情（status ≥ 4）
  for _ in $(seq 1 120); do
    local d; d=$(api GET "/batches/$1")
    [[ $(jq -r .batch.status <<<"$d") -ge 4 ]] && { echo "$d"; return; }
    sleep 1
  done
  red "timeout polling batch $1" >&2; exit 1
}

REVERSE_RESTORE=0
cleanup() {
  local rc=$?
  if [[ -n $MYSQL_CONTAINER ]]; then
    local u
    for u in $USER_ID $LOW_USER; do
      sql "DELETE FROM afv_studio_prompt_cache WHERE batch_id IN (SELECT id FROM (SELECT id FROM afv_studio_batch WHERE user_id=$u) x);
           DELETE FROM afv_studio_task WHERE batch_id IN (SELECT id FROM (SELECT id FROM afv_studio_batch WHERE user_id=$u) x);
           DELETE FROM afv_studio_batch WHERE user_id=$u;
           DELETE FROM afv_asset_item WHERE asset_id IN (SELECT id FROM (SELECT id FROM afv_asset WHERE user_id=$u) y);
           DELETE FROM afv_asset WHERE user_id=$u;
           DELETE FROM afv_app_user_feature WHERE user_id=$u;" || true
    done
    if [[ $BILLING == 1 && ${KEEP_BILLING:-0} != 1 ]]; then bill_cleanup "$USER_ID"; bill_cleanup "$LOW_USER"; fi
  fi
  [[ $BILLING == 1 ]] && curl -s -o /dev/null "$AIG/debug/inject?reset=1" || true
  if [[ $REVERSE_RESTORE == 1 ]]; then restart_reverse ""; fi
  rm -rf "$TMP"
  exit "$(e2e_rc "$rc")"
}
trap cleanup EXIT

restart_reverse() { # restart_reverse FAIL列表（空=正常）
  pkill -f 'bin/mock-reverse' 2>/dev/null || true; sleep 0.5
  (cd "$ROOT" && MOCK_REVERSE_FAIL="$1" nohup bin/mock-reverse -addr :5057 > logs/mock-reverse.log 2>&1 &)
  for _ in $(seq 1 20); do port_up 5057 && return; sleep 0.3; done
}

echo "== 前置检查"
need_port 8888 gateway-api hard
need_port 9205 studio-rpc hard
need_port 8889 admin-api hard
need_port 5057 mock-reverse chain
need_port 5059 mock-aigateway chain
need_port 18090 media chain
if [[ -z $MYSQL_CONTAINER ]]; then yellow "  提示：没找到 MySQL 容器，结束时不清理数据（设 MYSQL_CONTAINER）"; fi

echo "== 用户 $USER_ID"
# 链路类要过准入：studio-rpc 的 Billing.CheckEntitlement 默认开，随机用户没有 ecom_image_gen 权益会被拒（「暂未开通该功能」）。
# 先按 deploy/sql/ops/studio-offers-2026-10-30.sql 建好三个 offer，这里给测试用户发 studio 入口的能力码，结束时清掉。
if [[ $RUN_CHAIN == 1 && -n $MYSQL_CONTAINER ]]; then
  sql "INSERT INTO afv_app_user_feature (user_id,feature_code,source,status)
       SELECT DISTINCT $USER_ID,feature_code,'ADMIN','ACTIVE' FROM afv_billing_offer
       WHERE entry_code IN ('studio_tryon','studio_pose','studio_imagegen') AND status='ENABLED' AND deleted=0" || true
  [[ $(sql "SELECT COUNT(*) FROM afv_app_user_feature WHERE user_id=$USER_ID") -ge 1 ]] ||
    yellow "  提示：没发到 studio 权益（三个 offer 没配？先跑 deploy/sql/ops/studio-offers-2026-10-30.sql）"
fi
if [[ $BILLING == 1 ]]; then
  [[ $RUN_CHAIN == 1 && -n $MYSQL_CONTAINER ]] || { red "BILLING=1 需要 RUN_CHAIN=1 与 MySQL 容器"; exit 2; }
  # 测试用户 2000 点；余额不足用户 15 点（一张 1K 图 10 点：第一张冻得上、第二张冻不上）。两人都发 studio 能力码
  bill_seed "$USER_ID" $BILL_INIT; bill_seed "$LOW_USER" $LOW_INIT
  sql "INSERT INTO afv_app_user_feature (user_id,feature_code,source,status)
       SELECT DISTINCT $LOW_USER,feature_code,'ADMIN','ACTIVE' FROM afv_billing_offer
       WHERE entry_code IN ('studio_tryon','studio_pose','studio_imagegen') AND status='ENABLED' AND deleted=0"
  curl -s -o /dev/null "$AIG/debug/inject?reset=1"
  echo "  计费：用户 $USER_ID ${BILL_INIT} 点、余额不足用户 $LOW_USER ${LOW_INIT} 点"
fi

# ============================== 校验类 ==============================
U1=https://example.com/a.png
U2=https://example.com/b.png
garment() { # garment N → N 张服装图 JSON 数组
  jq -nc --arg u "$U1" "[range(0;$1)|{url:\$u,role:\"上衣\",angle:\"front\"}]"
}
urls() { jq -nc --arg u "$U1" "[range(0;$1)|\$u]"; }
tryon_body() { # tryon_body ITEMS_JSON MODELS_JSON GARMENTS_JSON COUNT
  jq -nc --argjson items "$1" --argjson models "$2" --argjson g "$3" --argjson c "$4" \
    '{model_image_urls:$models, garment_images:$g, items:$items, set_mode:"one", count_per_model:$c, model:"Banana", resolution:"1K"}'
}
OK_G=$(garment 1); OK_M=$(urls 1)

echo "== 校验：商拍"
rejected "缺 Idempotency-Key" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$OK_M" "$OK_G" 1)" "")"
rejected "模特图是 blob: URL" "$(call POST /tryon/submit "$(tryon_body '["top"]' '["blob:https://x/1"]' "$OK_G" 1)" "$(uuidgen)")"
rejected "服装图是 blob: URL" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$OK_M" '[{"url":"blob:https://x/1","role":"上衣","angle":"front"}]' 1)" "$(uuidgen)")"
rejected "连衣裙 + 上衣互斥" "$(call POST /tryon/submit "$(tryon_body '["dress","top"]' "$OK_M" "$OK_G" 1)" "$(uuidgen)")"
rejected "连衣裙 + 下装互斥" "$(call POST /tryon/submit "$(tryon_body '["dress","bottom"]' "$OK_M" "$OK_G" 1)" "$(uuidgen)")"
rejected "服装图 10 张" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$OK_M" "$(garment 10)" 1)" "$(uuidgen)")"
rejected "模特 10 位" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$(urls 10)" "$OK_G" 1)" "$(uuidgen)")"
rejected "没有模特" "$(call POST /tryon/submit "$(tryon_body '["top"]' '[]' "$OK_G" 1)" "$(uuidgen)")"
rejected "count_per_model=0" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$OK_M" "$OK_G" 0)" "$(uuidgen)")"
rejected "count_per_model=10" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$OK_M" "$OK_G" 10)" "$(uuidgen)")"
rejected "单品 4 件" "$(call POST /tryon/submit "$(tryon_body '["top","bottom","outer","shoes"]' "$OK_M" "$OK_G" 1)" "$(uuidgen)")"
rejected "单品为空" "$(call POST /tryon/submit "$(tryon_body '[]' "$OK_M" "$OK_G" 1)" "$(uuidgen)")"
rejected "不认识的模型档" "$(call POST /tryon/submit "$(tryon_body '["top"]' "$OK_M" "$OK_G" 1 | jq -c '.model="Midjourney"')" "$(uuidgen)")"

echo "== 校验：姿态"
pose_body() { jq -nc --arg s "$1" --argjson p "$2" '{source_image_url:$s, pose_image_urls:$p}'; }
rejected "缺 Idempotency-Key" "$(call POST /pose/submit "$(pose_body $U1 "$(urls 1)")" "")"
rejected "姿势 10 张" "$(call POST /pose/submit "$(pose_body $U1 "$(urls 10)")" "$(uuidgen)")"
rejected "没有姿势" "$(call POST /pose/submit "$(pose_body $U1 '[]')" "$(uuidgen)")"
rejected "人物图是 blob: URL" "$(call POST /pose/submit "$(pose_body blob:https://x/1 "$(urls 1)")" "$(uuidgen)")"
rejected "姿势图是 blob: URL" "$(call POST /pose/submit "$(pose_body $U1 '["blob:https://x/2"]')" "$(uuidgen)")"

echo "== 校验：图片生成"
ig_body() { jq -nc --arg p "$1" --argjson c "$2" '{prompt:$p, count:$c, model:"GPT", resolution:"2k"}'; }
rejected "缺 Idempotency-Key" "$(call POST /image-generate/submit "$(ig_body 一只猫 1)" "")"
rejected "count=0" "$(call POST /image-generate/submit "$(ig_body 一只猫 0)" "$(uuidgen)")"
rejected "count=10" "$(call POST /image-generate/submit "$(ig_body 一只猫 10)" "$(uuidgen)")"
rejected "提示词为空" "$(call POST /image-generate/submit "$(ig_body '' 1)" "$(uuidgen)")"
rejected "参考图 10 张" "$(call POST /image-generate/submit "$(ig_body 一只猫 1 | jq -c --argjson u "$(urls 10)" '.image_urls=$u')" "$(uuidgen)")"
rejected "提示词超 3000 字" "$(call POST /image-generate/submit "$(ig_body "$(printf 'x%.0s' $(seq 1 3001))" 1)" "$(uuidgen)")"

echo "== 查询：不存在的批次 / 任务"
rejected "批次不存在" "$(call GET /batches/999999999)"
rejected "任务不存在" "$(call GET /tasks/tyo000000000000)"

# ============================== 链路类 ==============================
KEY_T=$(uuidgen)
tryon_flow() {
  local m1 m2 g1 g2 body resp d bid
  m1=$(upload m1); m2=$(upload m2); g1=$(upload g1); g2=$(upload g2)
  body=$(jq -nc --arg m1 "$m1" --arg m2 "$m2" --arg g1 "$g1" --arg g2 "$g2" \
    '{model_image_urls:[$m1,$m2], garment_images:[{url:$g1,role:"上衣",angle:"front"},{url:$g2,role:"上衣",angle:"detail"}],
      items:["top"], set_mode:"one", fabric:"上衣：真丝E4MARK", count_per_model:2, model:"Banana", resolution:"1K"}')
  resp=$(api POST /tryon/submit "$body" "$KEY_T")
  bid=$(jq -r .batch_id <<<"$resp")
  check "2 模特 × 2 张 = 4 任务" "$(jq -r .task_total <<<"$resp")" 4
  check "返回 4 个 task_id" "$(jq '.task_ids|length' <<<"$resp")" 4
  check "task_id 前缀 tyo" "$(jq -r '[.task_ids[]|startswith("tyo")]|all' <<<"$resp")" true
  check "首次提交 replayed=false" "$(jq -r .replayed <<<"$resp")" false
  d=$(poll_batch "$bid")
  check "批次全部成功(4)" "$(jq -r .batch.status <<<"$d")" 4
  check "每个任务有 result_url" "$(jq -r '[.tasks[]|.result_url!=""]|all' <<<"$d")" true
  check "同一模特的任务共用一份 prompt（批次级缓存）" \
    "$(admin GET "/batches/$bid" | jq -r '[.tasks[]|select(.model_index==0)|.prompt_text]|unique|length')" 1
  check "fabric 原样进了 prompt（mock-reverse 回显）" "$(admin GET "/batches/$bid" | jq -r '[.tasks[]|.prompt_text|contains("真丝E4MARK")]|all')" true
  local h; h=$(api GET '/history?kind=tryon&limit=5')
  check "历史第一条就是它" "$(jq -r '.list[0].batch.id' <<<"$h")" "$bid"
  check "payload 可回填(count_per_model)" "$(jq -r '.list[0].batch.payload|fromjson|.count_per_model' <<<"$h")" 2
  check "payload 可回填(items)" "$(jq -c '.list[0].batch.payload|fromjson|.items' <<<"$h")" '["top"]'
  resp=$(api POST /tryon/submit "$body" "$KEY_T")
  check "幂等重放 replayed=true" "$(jq -r .replayed <<<"$resp")" true
  check "幂等重放同一批次" "$(jq -r .batch_id <<<"$resp")" "$bid"
  rejected "同 key 不同 body 冲突" "$(call POST /tryon/submit "$(jq -c '.count_per_model=3' <<<"$body")" "$KEY_T")"
  check "已完成批次可删除" "$(code_of "$(call DELETE "/batches/$bid")")" 0
}
chain "商拍：2 模特 × 2 张 → 轮询 → 历史回填 → 幂等" tryon_flow

pose_flow() {
  local s p1 p2 p3 resp d
  s=$(upload s1); p1=$(upload p1); p2=$(upload p2); p3=$(upload p3)
  resp=$(api POST /pose/submit "$(jq -nc --arg s "$s" --arg a "$p1" --arg b "$p2" --arg c "$p3" \
    '{source_image_url:$s, pose_image_urls:[$a,$b,$c], global_prompt:"自然光", model:"Seedream", resolution:"2k"}')" "$(uuidgen)")
  check "3 姿势 = 3 任务" "$(jq -r .task_total <<<"$resp")" 3
  check "task_id 前缀 pse" "$(jq -r '[.task_ids[]|startswith("pse")]|all' <<<"$resp")" true
  d=$(poll_batch "$(jq -r .batch_id <<<"$resp")")
  check "批次全部成功(4)" "$(jq -r .batch.status <<<"$d")" 4
  check "每个任务有 result_url" "$(jq -r '[.tasks[]|.result_url!=""]|all' <<<"$d")" true
  check "pose_index 为 0..2" "$(jq -c '[.tasks[].pose_index]|sort' <<<"$d")" '[0,1,2]'
}
chain "姿态：3 姿势 → 3 任务" pose_flow

imagegen_flow() {
  local resp d
  resp=$(api POST /image-generate/submit "$(ig_body 一只戴帽子的猫 3)" "$(uuidgen)")
  check "count=3 → 3 任务" "$(jq -r .task_total <<<"$resp")" 3
  check "task_id 前缀 simg" "$(jq -r '[.task_ids[]|startswith("simg")]|all' <<<"$resp")" true
  d=$(poll_batch "$(jq -r .batch_id <<<"$resp")")
  check "批次全部成功(4)" "$(jq -r .batch.status <<<"$d")" 4
  check "单任务查询" "$(api GET "/tasks/$(jq -r '.tasks[0].task_id' <<<"$d")" | jq -r .status)" 2
  check "普通提交 warnings=[]" "$(jq -c .warnings <<<"$resp")" '[]'
}
chain "图片生成：count=3 → 3 任务" imagegen_flow

seedream_flow() {
  local resp bid st before u2 u4 tid
  before=$(bill_snap "$USER_ID")
  resp=$(api POST /image-generate/submit "$(jq -nc '{prompt:"一只猫", count:1, model:"Seedream", resolution:"4k", aspect_ratio:"3:4"}')" "$(uuidgen)")
  check "Seedream 4k 提交带归一告警" "$(jq -r '.warnings|length>0' <<<"$resp")" true
  bid=$(jq -r .batch_id <<<"$resp")
  st=$(api GET "/batches/$bid" | jq -r .batch.status)
  check "提交返回时批次已离开提交中(≠8)" "$([[ $st != 8 ]] && echo yes || echo "no:$st")" yes
  check "payload 快照为归一后的 2k" "$(api GET "/batches/$bid" | jq -r '.batch.payload|fromjson|.resolution')" 2k
  check "Seedream 批次全部成功(4)" "$(poll_batch "$bid" | jq -r .batch.status)" 4
  if [[ $BILLING == 1 ]]; then
    u2=$(bill_unit_price studio_imagegen 2K); u4=$(bill_unit_price studio_imagegen 4K)
    tid=$(jq -r '.task_ids[0]' <<<"$resp")
    check "计费：Seedream 4k 按 2K 档冻结（估价 ${u2}，4K 档是 ${u4}）" "$(jq -r .estimated_points <<<"$resp")" "$u2"
    assert_reservation_status "计费：按 2K 档结算" "$USER_ID" "studio-$tid" CONFIRMED "$u2"
    assert_balance_delta "计费：实扣 2K 档单价" "$USER_ID" "$before" "-$u2" 0
  fi
}
chain "Seedream 档位提交时归一（4k → 2k + warnings）" seedream_flow

cancel_delete_flow() {
  local resp bid c before
  before=$(bill_snap "$USER_ID")
  resp=$(api POST /image-generate/submit "$(ig_body 取消测试 2)" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp")
  rejected "进行中的批次不能删除" "$(call DELETE "/batches/$bid")"
  c=$(api POST "/batches/$bid/cancel")
  check "取消后批次为已取消(7)" "$(jq -r .batch.status <<<"$c")" 7
  check "未开始的任务被取消" "$(jq -r '.canceled_tasks>=1' <<<"$c")" true
  if [[ $BILLING == 1 ]]; then assert_balance_delta "取消后冻结全部退回" "$USER_ID" "$before" 0 0; fi
  check "取消后可删除" "$(code_of "$(call DELETE "/batches/$bid")")" 0
}
chain "取消排队中的批次；进行中不能删除" cancel_delete_flow

admin_flow() {
  local resp bid d
  resp=$(api POST /image-generate/submit "$(ig_body 后台测试 1)" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp"); poll_batch "$bid" >/dev/null
  check "后台列表按 user_id 能查到" "$(admin GET "/batches?user_id=$USER_ID" | jq -r "[.list[].id]|index($bid)!=null")" true
  d=$(admin GET "/batches/$bid")
  check "后台详情有子任务" "$(jq '.tasks|length' <<<"$d")" 1
  check "后台详情带 gateway_call_id" "$(jq -r '.tasks[0].gateway_call_id|startswith("call_")' <<<"$d")" true
  check "sidecar 状态含依赖项" "$(admin GET /sidecar/status | jq '.deps|length>0')" true
  check "成本汇总可读(kind)" "$(admin GET '/cost-summary?group_by=kind' | jq '.rows|type')" '"array"'
  check "成本汇总可读(model_group)" "$(admin GET '/cost-summary?group_by=model_group' | jq '.rows|type')" '"array"'
}
chain "后台：列表 / 详情 / sidecar 状态 / cost-summary" admin_flow

reverse_fail_flow() {
  if [[ ! -x $ROOT/bin/mock-reverse ]]; then red "需要 bin/mock-reverse（make build）"; exit 1; fi
  # 编排先调 analyze-batch，单个模特失败才回落 analyze，所以两条都要打挂
  restart_reverse analyze,analyze-batch; REVERSE_RESTORE=1
  local m g resp bid d before t
  before=$(bill_snap "$USER_ID")
  m=$(upload fm); g=$(upload fg)
  resp=$(api POST /tryon/submit "$(jq -nc --arg m "$m" --arg g "$g" \
    '{model_image_urls:[$m], garment_images:[{url:$g,role:"上衣",angle:"front"}], items:["top"], count_per_model:2}')" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp")
  d=$(poll_batch "$bid")
  check "analyze(-batch) 502 → 重试 3 轮后批次失败(5)" "$(jq -r .batch.status <<<"$d")" 5
  check "批次带失败原因" "$(jq -r '.batch.error_reason!=""' <<<"$d")" true
  d=$(admin GET "/batches/$bid")
  check "任务全部失败" "$(jq -r '[.tasks[]|.status==3]|all' <<<"$d")" true
  if [[ $BILLING_ON == 1 ]]; then
    check "任务全部退款(billing_status=3)" "$(jq -r '[.tasks[]|.billing_status==3]|all' <<<"$d")" true
    for t in $(jq -r '.tasks[].billing_request_id' <<<"$d"); do
      assert_reservation_status "计费：反推失败 → 预留单 REFUNDED" "$USER_ID" "$t" REFUNDED
    done
    assert_balance_delta "计费：反推失败全部退款，余额回到提交前" "$USER_ID" "$before" 0 0
  else
    check "任务没有被结算(billing_status≠2；BILLING_ON=1 时断言已退回)" "$(jq -r '[.tasks[]|.billing_status!=2]|all' <<<"$d")" true
  fi
  restart_reverse ""; REVERSE_RESTORE=0
}
chain "mock-reverse analyze 502 → 批次 failed，任务全部退款" reverse_fail_flow

# ============================== 计费（BILLING=1） ==============================
# 单价从库里取（bill_unit_price：入口码 + 分辨率），不写死。结果注入：aig_inject 标记 结果 [类别] [次数]，
# 标记放进提示词（商拍的 fabric 会被 mock-reverse 回显进 prompt，图片生成直接写在 prompt 里）。
aig_inject() { # aig_inject MATCH OUTCOME [CLASS] [TIMES]
  curl -sS -X POST "$AIG/debug/inject" -d "$(jq -nc --arg m "$1" --arg o "$2" --arg c "${3:-}" --argjson t "${4:-0}" \
    '{match:$m,outcome:$o,class:$c,times:$t}')" >/dev/null
}
billing_case() { # billing_case 名称 函数
  if [[ $BILLING == 1 && $RUN_CHAIN == 1 ]]; then echo "-- 计费：$1"; "$2"
  else yellow "  SKIP 计费：$1（BILLING=1 RUN_CHAIN=1 且 studio-rpc Billing.Mode=on 时启用）"; SKIPPED=$((SKIPPED+1)); fi
}
admin_post() { # admin_post PATH → 信封
  curl -sS -X POST -H 'X-Admin-Id: 1' "$ADMIN$1"
}
task_of() { jq -c --arg t "$2" '.tasks[]|select(.task_id==$t)' <<<"$1"; } # task_of 后台批次详情 task_id

bill_tryon_ok() {
  local before unit m1 m2 g1 resp bid d t
  before=$(bill_snap "$USER_ID"); unit=$(bill_unit_price studio_tryon 1K)
  m1=$(upload bm1); m2=$(upload bm2); g1=$(upload bg1)
  resp=$(api POST /tryon/submit "$(jq -nc --arg m1 "$m1" --arg m2 "$m2" --arg g "$g1" \
    '{model_image_urls:[$m1,$m2], garment_images:[{url:$g,role:"上衣",angle:"front"}], items:["top"], count_per_model:1, model:"Banana", resolution:"1K"}')" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp")
  check "提交返回预估点数 = 2 × 单价(${unit})" "$(jq -r .estimated_points <<<"$resp")" $((2 * unit))
  for t in $(jq -r '.task_ids[]' <<<"$resp"); do
    check "提交时每张冻结一次单价（$t 首条流水）" "$(bill_logs "$USER_ID" "studio-$t" | cut -d' ' -f1)" "FREEZE:-$unit"
  done
  d=$(poll_batch "$bid")
  check "批次全部成功(4)" "$(jq -r .batch.status <<<"$d")" 4
  for t in $(jq -r '.tasks[].task_id' <<<"$d"); do
    assert_reservation_status "成功 → 预留单 CONFIRMED" "$USER_ID" "studio-$t" CONFIRMED "$unit"
    check "流水：冻结 → 确认（${t}）" "$(bill_logs "$USER_ID" "studio-$t")" "FREEZE:-$unit CONSUME:-$unit"
  done
  check "任务计费镜像全部已结算(2)" "$(admin GET "/batches/$bid" | jq -r '[.tasks[]|.billing_status==2]|all')" true
  assert_balance_delta "余额减少 = 实扣 2 × 单价，冻结归零" "$USER_ID" "$before" $((-2 * unit)) 0
}
billing_case "商拍 2 模特 × 1 张全成功：冻结 2×单价 → CONFIRMED" bill_tryon_ok

bill_insufficient() {
  local USER_ID=$LOW_USER # call / upload 按 USER_ID 发请求
  local before m1 m2 g1 resp bid
  before=$(bill_snap "$LOW_USER")
  m1=$(upload lm1); m2=$(upload lm2); g1=$(upload lg1)
  resp=$(call POST /tryon/submit "$(jq -nc --arg m1 "$m1" --arg m2 "$m2" --arg g "$g1" \
    '{model_image_urls:[$m1,$m2], garment_images:[{url:$g,role:"上衣",angle:"front"}], items:["top"], count_per_model:1, model:"Banana", resolution:"1K"}')" "$(uuidgen)")
  check "余额不足 → 3003（$(jq -r .msg <<<"$resp")）" "$(code_of "$resp")" 3003
  assert_balance_delta "余额与冻结都不变" "$LOW_USER" "$before" 0 0
  bid=$(sql "SELECT id FROM afv_studio_batch WHERE user_id=$LOW_USER ORDER BY id DESC LIMIT 1")
  check "批次已置失败(5)" "$(sql "SELECT status FROM afv_studio_batch WHERE id=$bid")" 5
  check "子任务全部失败(3)" "$(sql "SELECT GROUP_CONCAT(DISTINCT status) FROM afv_studio_task WHERE batch_id=$bid")" 3
  check "无残留 HELD 单" "$(bill_held_count "$LOW_USER")" 0
  check "第一张冻上的已退回（REFUNDED 1 张，第二张没建单）" \
    "$(bill_sql "SELECT GROUP_CONCAT(status) FROM afv_billing_reservation WHERE user_id=$LOW_USER")" REFUNDED
}
billing_case "余额不足：3003，余额 / 冻结不变，批次 failed，无残留 HELD" bill_insufficient

bill_partial_retry() {
  local mk="E2EPART$RANDOM" before unit resp bid d ok_t bad_t before2 nreq
  unit=$(bill_unit_price studio_imagegen 2K)
  aig_inject "$mk" FAILED INPUT_REJECTED 1 # 第一张命中、不可重试 → 直接失败
  before=$(bill_snap "$USER_ID")
  resp=$(api POST /image-generate/submit "$(ig_body "部分失败 $mk" 2)" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp")
  check "预估点数 = 2 × 单价(${unit})" "$(jq -r .estimated_points <<<"$resp")" $((2 * unit))
  d=$(poll_batch "$bid")
  check "批次部分成功(6)" "$(jq -r .batch.status <<<"$d")" 6
  ok_t=$(jq -r '[.tasks[]|select(.status==2)][0].task_id' <<<"$d")
  bad_t=$(jq -r '[.tasks[]|select(.status==3)][0].task_id' <<<"$d")
  assert_reservation_status "成功张 CONFIRMED" "$USER_ID" "studio-$ok_t" CONFIRMED "$unit"
  assert_reservation_status "失败张 REFUNDED" "$USER_ID" "studio-$bad_t" REFUNDED "$unit"
  check "失败张流水：冻结 → 退回" "$(bill_logs "$USER_ID" "studio-$bad_t")" "FREEZE:-$unit REFUND:$unit"
  assert_balance_delta "只扣成功张" "$USER_ID" "$before" "-$unit" 0

  echo "-- 计费：后台重做失败张 → 新预留单（-r1）冻结、成功后结算"
  before2=$(bill_snap "$USER_ID")
  check "后台重做受理" "$(code_of "$(admin_post "/tasks/$bad_t/retry")")" 0
  nreq=$(task_of "$(admin GET "/batches/$bid")" "$bad_t" | jq -r .billing_request_id)
  check "重做换新计费号（带 -r1）" "$nreq" "studio-$bad_t-r1"
  check "重做时冻结一次单价" "$(bill_logs "$USER_ID" "$nreq" | cut -d' ' -f1)" "FREEZE:-$unit"
  d=$(poll_batch "$bid")
  check "重做后批次全部成功(4)" "$(jq -r .batch.status <<<"$d")" 4
  assert_reservation_status "重做单 CONFIRMED" "$USER_ID" "$nreq" CONFIRMED "$unit"
  assert_reservation_status "原失败单仍是 REFUNDED（不受重做影响）" "$USER_ID" "studio-$bad_t" REFUNDED
  check "重做张计费镜像已结算(2)" "$(task_of "$(admin GET "/batches/$bid")" "$bad_t" | jq -r .billing_status)" 2
  assert_balance_delta "重做成功扣一次单价" "$USER_ID" "$before2" "-$unit" 0
}
billing_case "部分失败：只扣成功张、失败张退款；后台重做 -r1 冻结并结算" bill_partial_retry

bill_concurrent_retry() {
  local mk="E2ECRT$RANDOM" unit resp bid d bad_t before2 c1 c2 codes nreq n i
  unit=$(bill_unit_price studio_imagegen 2K)
  aig_inject "$mk" FAILED INPUT_REJECTED 1
  resp=$(api POST /image-generate/submit "$(ig_body "并发重做 $mk" 2)" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp")
  d=$(poll_batch "$bid")
  check "批次部分成功(6)" "$(jq -r .batch.status <<<"$d")" 6
  bad_t=$(jq -r '[.tasks[]|select(.status==3)][0].task_id' <<<"$d")
  before2=$(bill_snap "$USER_ID")
  # 同时发两次重做：只能有一次受理（另一次 4002），且不会互相退掉对方在用的预留单
  admin_post "/tasks/$bad_t/retry" >"$TMP/cr1" & c1=$!
  admin_post "/tasks/$bad_t/retry" >"$TMP/cr2" & c2=$!
  wait "$c1" "$c2"
  codes=$(printf '%s\n%s\n' "$(code_of "$(cat "$TMP/cr1")")" "$(code_of "$(cat "$TMP/cr2")")" | sort | tr '\n' ' ')
  check "并发两次重做：一次受理、一次冲突" "$codes" "0 4002 "
  d=$(poll_batch "$bid")
  check "重做后批次全部成功(4)" "$(jq -r .batch.status <<<"$d")" 4
  nreq=$(task_of "$(admin GET "/batches/$bid")" "$bad_t" | jq -r .billing_request_id)
  check "任务用的是重做号（-rN）" "$([[ $nreq == studio-$bad_t-r* ]] && echo yes)" yes
  assert_reservation_status "在用的重做单 CONFIRMED（没被另一次重做误退）" "$USER_ID" "$nreq" CONFIRMED "$unit"
  n=0
  for i in 1 2 3 4 5 6; do [[ $(bill_res_status "$USER_ID" "studio-$bad_t-r$i") == CONFIRMED ]] && n=$((n + 1)); done
  check "重做号里只有一张被确认" "$n" 1
  check "无残留 HELD 单" "$(bill_held_count "$USER_ID")" 0
  assert_balance_delta "并发重做只扣一次单价" "$USER_ID" "$before2" "-$unit" 0
  curl -s -o /dev/null "$AIG/debug/inject?reset=1"
}
billing_case "并发两次重做只扣一次" bill_concurrent_retry

bill_cancel_force() {
  local mk="E2EHOLD$RANDOM" before unit resp bid d c t n running
  unit=$(bill_unit_price studio_imagegen 2K)
  aig_inject "$mk" HOLD "" 0 # 一直生成中：每批最多 3 张在生成（Poll.Concurrency），其余留在排队
  before=$(bill_snap "$USER_ID")
  resp=$(api POST /image-generate/submit "$(ig_body "取消与强制失败 $mk" 5)" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp")
  check "冻结 5 × 单价" "$(jq -r .estimated_points <<<"$resp")" $((5 * unit))
  for _ in $(seq 1 60); do
    d=$(admin GET "/batches/$bid")
    [[ $(jq '[.tasks[]|select(.status==1 and .gateway_call_id!="")]|length' <<<"$d") -ge 1 ]] && break
    sleep 0.5
  done
  running=$(jq -r '[.tasks[]|select(.status==1)|.task_id]|join(" ")' <<<"$d")
  check "已有子任务进入生成中" "$([[ -n $running ]] && echo yes)" yes
  c=$(api POST "/batches/$bid/cancel")
  check "取消了排队中的子任务" "$(jq -r '.canceled_tasks>=1' <<<"$c")" true
  d=$(admin GET "/batches/$bid")
  for t in $(jq -r '.tasks[]|select(.status==4)|.task_id' <<<"$d"); do
    assert_reservation_status "取消的排队任务 → REFUNDED" "$USER_ID" "studio-$t" REFUNDED "$unit"
    check "取消任务计费镜像已退回(3)" "$(task_of "$d" "$t" | jq -r .billing_status)" 3
  done
  n=$(jq '[.tasks[]|select(.status==4)]|length' <<<"$d")
  assert_balance_delta "取消退回 ${n} 张（其余仍冻结在生成中）" "$USER_ID" "$before" $(( -(5 - n) * unit )) $(( (5 - n) * unit ))

  echo "-- 计费：后台强制失败生成中的任务 → 退款且 admin_locked"
  running=$(jq -r '[.tasks[]|select(.status==1)|.task_id]|join(" ")' <<<"$d")
  t=${running%% *}
  check "强制失败受理" "$(code_of "$(admin_post "/tasks/$t/force-fail")")" 0
  d=$(admin GET "/batches/$bid")
  check "强制失败：任务失败(3)、admin_locked、计费镜像已退回(3)" \
    "$(task_of "$d" "$t" | jq -r '"\(.status),\(.admin_locked),\(.billing_status)"')" "3,true,3"
  assert_reservation_status "强制失败 → 预留单 REFUNDED" "$USER_ID" "studio-$t" REFUNDED "$unit"
  check "admin_locked 的任务不能再重做" "$(code_of "$(admin_post "/tasks/$t/retry")")" 4002
  for t in ${running#* }; do [[ $t == "${running%% *}" ]] || admin_post "/tasks/$t/force-fail" >/dev/null; done
  d=$(poll_batch "$bid")
  check "批次收敛到终态" "$(jq -r '.batch.status>=4' <<<"$d")" true
  check "无残留 HELD 单" "$(bill_held_count "$USER_ID")" 0
  assert_balance_delta "冻结的 5 张全部退回" "$USER_ID" "$before" 0 0
  curl -s -o /dev/null "$AIG/debug/inject?reset=1"
}
billing_case "取消排队中的批次全部退回；后台强制失败生成中的任务退款" bill_cancel_force

# wait_task_billing 任务号 期望值 秒 —— 等子任务的 billing_status 变成期望值，返回最后读到的值（守护每分钟对账一次）
wait_task_billing() {
  local i v=""
  for ((i = 0; i < $3; i++)); do
    v=$(sql "SELECT billing_status FROM afv_studio_task WHERE task_id='$1' LIMIT 1")
    [[ $v == "$2" ]] && break
    sleep 1
  done
  echo "$v"
}

bill_unknown() {
  local mk="E2EUNK$RANDOM" before unit resp bid d t st
  unit=$(bill_unit_price studio_imagegen 2K)
  aig_inject "$mk" UNKNOWN "" 1
  before=$(bill_snap "$USER_ID")
  resp=$(api POST /image-generate/submit "$(ig_body "结果未知 $mk" 1)" "$(uuidgen)")
  bid=$(jq -r .batch_id <<<"$resp"); t=$(jq -r '.task_ids[0]' <<<"$resp")
  d=$(poll_batch "$bid")
  check "UNKNOWN → 批次失败(5)、任务「结果未知，待对账」" "$(jq -r '"\(.batch.status),\(.tasks[0].error_reason)"' <<<"$d")" "5,结果未知，待对账"
  assert_reservation_status "UNKNOWN 不主动退款，预留单保持 HELD" "$USER_ID" "studio-$t" HELD "$unit"
  assert_balance_delta "点数仍冻结" "$USER_ID" "$before" "-$unit" "$unit"
  bill_expire "$USER_ID" "studio-$t"
  st=$(bill_wait_status "$USER_ID" "studio-$t" EXPIRED 90)
  check "到期后回收器释放（HELD → EXPIRED）" "$st" EXPIRED
  st=$(wait_task_billing "$t" 3 90)
  check "守护回写镜像：billing_status 冻结(1) → 已退回(3)" "$st" 3
  check "流水：冻结 → 过期解冻" "$(bill_logs "$USER_ID" "studio-$t")" "FREEZE:-$unit UNFREEZE:$unit"
  assert_balance_delta "余额回到原值" "$USER_ID" "$before" 0 0
}
billing_case "UNKNOWN：任务失败待对账、预留单 HELD；到期由 Reclaimer 释放" bill_unknown

bill_reconcile() {
  check "对账 StudioTasksBillingMismatch（pkg/auditdb）：测试用户 0 行" "$(bill_studio_mismatch "$USER_ID")" 0
  check "对账 StudioTasksBillingMismatch：余额不足用户 0 行" "$(bill_studio_mismatch "$LOW_USER")" 0
  assert_ledger_consistent "账务自洽：测试用户" "$USER_ID" $BILL_INIT
  assert_ledger_consistent "账务自洽：余额不足用户" "$LOW_USER" $LOW_INIT
  echo "  测试用户：$(bill_snap "$USER_ID" | awk -v i=$BILL_INIT '{printf "初始 %d → 可用 %d、冻结 %d（共扣 %d）", i, $1, $2, i-$1-$2}')"
}
billing_case "对账：任务终态与预留单一致、账户与流水自洽" bill_reconcile

# ============================== 权限 ==============================
# studio 后台按前缀取权限码（GET=studio:view，POST=studio:update，见 admin 中间件 prefixPerms）。
ADMIN_SHARED=${ADMIN_SHARED:-}
REDIS_CONTAINER=${REDIS_CONTAINER:-$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 redis || true)}
perm_flow() {
  local aid rid tok resp
  sql "INSERT INTO afv_admin_user (username,password_hash,status,deleted) VALUES ('e2e_studio_$USER_ID','x',1,0);
       INSERT INTO afv_admin_role (role_code,role_name,status,deleted) VALUES ('e2e_ll_view_$USER_ID','e2e',1,0)"
  aid=$(sql "SELECT id FROM afv_admin_user WHERE username='e2e_studio_$USER_ID'")
  rid=$(sql "SELECT id FROM afv_admin_role WHERE role_code='e2e_ll_view_$USER_ID'")
  sql "INSERT INTO afv_admin_user_role (admin_id,role_id) VALUES ($aid,$rid);
       INSERT INTO afv_admin_role_permission (role_id,permission_id) SELECT $rid,id FROM afv_admin_permission WHERE permission_code='local_life:view'"
  tok=$(openssl rand -hex 16) # Java 会话令牌：32 位小写十六进制（session.Looks）
  docker exec "$REDIS_CONTAINER" redis-cli --no-auth-warning -a "${REDIS_PASS:-123456}" -n 10 SET "fusion:admin:access:$tok" "{\"adminId\":$aid,\"username\":\"e2e\"}" EX 600 >/dev/null
  perm_cleanup() {
    docker exec "$REDIS_CONTAINER" redis-cli --no-auth-warning -a "${REDIS_PASS:-123456}" -n 10 DEL "fusion:admin:access:$tok" >/dev/null || true
    sql "DELETE FROM afv_admin_role_permission WHERE role_id=$rid; DELETE FROM afv_admin_user_role WHERE admin_id=$aid;
         DELETE FROM afv_admin_role WHERE id=$rid; DELETE FROM afv_admin_user WHERE id=$aid" || true
  }
  resp=$(curl -sS -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/local-life/admin/auth/me")
  check "local_life:view 账号能访问本地生活后台" "$(code_of "$resp")" 0
  resp=$(curl -sS -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/studio/admin/batches")
  check "只有 local_life:view → studio 后台 GET 被拒(2005 ACCESS_DENIED)" "$(code_of "$resp")" 2005
  check "拒绝文案点名 studio:view" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:view"
  resp=$(curl -sS -X POST -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/studio/admin/tasks/1/retry")
  check "只有 local_life:view → studio 重做被拒(要 studio:update)" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:update"
  sql "INSERT INTO afv_admin_role_permission (role_id,permission_id) SELECT $rid,id FROM afv_admin_permission WHERE permission_code='studio:view'"
  resp=$(curl -sS -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/studio/admin/batches?user_id=$USER_ID")
  check "补上 studio:view 后可查看" "$(code_of "$resp")" 0
  resp=$(curl -sS -X POST -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/studio/admin/tasks/1/force-fail")
  check "只有 studio:view 不能强制失败" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:update"
  # 有 local_life:update、没有 studio:update：不规范路径与本地生活任务入口都不能绕进 studio
  sql "INSERT INTO afv_admin_role_permission (role_id,permission_id) SELECT $rid,id FROM afv_admin_permission WHERE permission_code='local_life:update'"
  resp=$(curl -sS --path-as-is -X POST -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1//studio/admin/tasks/1/force-fail")
  check "路径 // 绕不过 studio:update" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:update"
  resp=$(curl -sS --path-as-is -X POST -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/local-life/../studio/admin/tasks/1/retry")
  check "路径 .. 绕不过 studio:update" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:update"
  resp=$(curl -sS -X POST -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/local-life/admin/tasks/tyo:1/retry")
  check "本地生活入口重做 studio 任务要 studio:update" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:update"
  resp=$(curl -sS -X POST -H "Authorization: Bearer $tok" "$ADMIN_SHARED/api/v1/local-life/admin/tasks/simg:1/force-fail")
  check "本地生活入口强制失败 studio 任务要 studio:update" "$(jq -r '.msg' <<<"$resp")" "没有权限：studio:update"
  perm_cleanup
}
echo "-- 权限：studio 后台按 studio:view / studio:update 校验"
if [[ -n $ADMIN_SHARED && -n $MYSQL_CONTAINER && -n $REDIS_CONTAINER ]]; then perm_flow
else yellow "  SKIP 权限用例（需 ADMIN_SHARED=按 RBAC 查权限的 admin-api 地址，及 MySQL / Redis 容器）"; SKIPPED=$((SKIPPED+1)); fi

echo
green "通过 $PASS 项，跳过 $SKIPPED 组用例"
e2e_finish

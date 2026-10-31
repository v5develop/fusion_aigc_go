#!/usr/bin/env bash
# studio 真实链路回归：对着**已部署的环境**（真 ai-gateway / 真反推 sidecar + DeepSeek / 真媒体服务 + OSS），
# 三种各提交一次最小规模：商拍 1 模特 × 1 张、姿态 1 姿势、图片生成 count=1，轮询到终态后断言：
#   - 批次全部成功，result_url 可 HEAD 200（素材库公有桶地址）；
#   - 后台详情 step_log 走过对应阶段：商拍 cropping → prompting → generating；姿态 / 图片生成没有切图，只有 prompting → generating；
#   - step_log 里 generate 步有成本（cost_cny > 0，来自 gateway call.cost.amount_cny）。
#
# ⚠️ 会**真实产生费用**：3 次生图（gateway 供应商成本）+ 商拍 crop-plan / crop-check / analyze、姿态 source / prompt 的 DeepSeek 调用；
#    BILLING_MODE=on 时还会真实扣测试账号的点数（每张按 studio 三个 offer 的价目）。慢：每条 1–5 分钟。
#
# 用法：
#   GW=https://test.example.com TOKEN=<用户令牌> ADMIN_TOKEN=<后台令牌> \
#   SAMPLE_GARMENT_URLS="https://…/top-front.jpg https://…/top-back.jpg" SAMPLE_MODEL_URL=https://…/model.jpg \
#   SAMPLE_POSE_URLS="https://…/source.jpg https://…/pose.jpg" bash tests/e2e/e2e-real-studio.sh [tryon] [pose] [imagegen]
#
# 环境变量：
#   GW            网关根地址（不带 /api/v1），默认 http://127.0.0.1:18888
#   ADMIN         后台根地址，默认 http://127.0.0.1:18889
#   TOKEN         用户令牌（Authorization: Bearer）；不设则用 USER_ID 走 X-User-Id（仅 dev 模式网关）
#   USER_ID       同上，默认 990909
#   ADMIN_TOKEN   后台令牌（要有 studio:view）；不设则 X-Admin-Id: 1（仅 dev 模式 admin-api）
#   SAMPLE_GARMENT_URLS  2 张服装图（上衣，第 1 张正面、第 2 张背面），空格分隔
#   SAMPLE_MODEL_URL     1 张模特图
#   SAMPLE_POSE_URLS     2 张：第 1 张人物原图、第 2 张姿势参考图
#   MODEL         模型档（通用 / GPT / Banana / Seedream），默认 通用
#   TIMEOUT       每条轮询上限（秒），默认 900
# 样例图会先经本地生活上传接口转成「我们的」地址（线上走预签名直传），不依赖 InputURLPrefixes。
set -uo pipefail

GW=${GW:-http://127.0.0.1:18888}
ADMIN=${ADMIN:-http://127.0.0.1:18889}
USER_ID=${USER_ID:-990909}
TOKEN=${TOKEN:-}
ADMIN_TOKEN=${ADMIN_TOKEN:-}
MODEL=${MODEL:-通用}
TIMEOUT=${TIMEOUT:-900}
read -r -a GARMENTS <<<"${SAMPLE_GARMENT_URLS:-}"
read -r -a POSES <<<"${SAMPLE_POSE_URLS:-}"
SAMPLE_MODEL_URL=${SAMPLE_MODEL_URL:-}
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
OK=0; FAIL=0; SKIP=0

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
gray()  { printf '\033[90m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[36m== %s\033[0m\n' "$*"; }
pass() { green "  ✓ $1"; [[ -n ${2:-} ]] && gray "      $2"; OK=$((OK+1)); }
skip() { printf '\033[33m  - %s\033[0m\n' "$1"; SKIP=$((SKIP+1)); }
bad()  { red   "  ✗ $1"; [[ -n ${2:-} ]] && gray "      $2"; FAIL=$((FAIL+1)); }

user_auth() { if [[ -n $TOKEN ]]; then echo "Authorization: Bearer $TOKEN"; else echo "X-User-Id: $USER_ID"; fi; }
admin_auth() { if [[ -n $ADMIN_TOKEN ]]; then echo "Authorization: Bearer $ADMIN_TOKEN"; else echo "X-Admin-Id: 1"; fi; }

api() { # api METHOD PATH [BODY] → data（PATH 相对 /api/v1；POST 自动带 Idempotency-Key）
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -X "$method" -H "$(user_auth)" -H 'Content-Type: application/json')
  [[ $method == POST ]] && args+=(-H "Idempotency-Key: $(uuidgen)")
  [[ -n $body ]] && args+=(-d "$body")
  local resp; resp=$(curl "${args[@]}" "$GW/api/v1$path" 2>&1)
  if [[ $(jq -r .code 2>/dev/null <<<"$resp") != 0 ]]; then
    red "    请求失败 $method $path → $(head -c 300 <<<"$resp")" >&2
    return 1
  fi
  jq -c '.data' <<<"$resp"
}

admin() { # admin PATH → data
  local resp; resp=$(curl -sS -H "$(admin_auth)" "$ADMIN/api/v1/studio/admin$1" 2>&1)
  if [[ $(jq -r .code 2>/dev/null <<<"$resp") != 0 ]]; then
    red "    后台请求失败 $1 → $(head -c 300 <<<"$resp")" >&2
    return 1
  fi
  jq -c '.data' <<<"$resp"
}

# upload 公网样例图 → 我们的地址：优先预签名直传（线上），本地存储模式退回代传
upload() {
  local src=$1 file=$TMP/$RANDOM.img
  curl -sSfL -o "$file" "$src" || return 1
  local ct; ct=$(file -b --mime-type "$file" 2>/dev/null || echo image/jpeg)
  local p; p=$(api POST /local-life/upload/presign "{\"kind\":\"image\",\"content_type\":\"$ct\"}" 2>/dev/null)
  if [[ -n $p ]]; then
    curl -sS -X PUT -H "Content-Type: $ct" --data-binary "@$file" "$(jq -r .put_url <<<"$p")" >/dev/null || return 1
    jq -r .url <<<"$p"
    return
  fi
  curl -sS -H "$(user_auth)" -F "file=@$file" "$GW/api/v1/local-life/upload" | jq -r .data.url
}

# poll_batch ID → 终态批次详情（status ≥ 4）
poll_batch() {
  local t0=$SECONDS d
  while (( SECONDS - t0 < TIMEOUT )); do
    d=$(api GET "/studio/batches/$1") || return 1
    if [[ $(jq -r .batch.status <<<"$d") -ge 4 ]]; then printf '%s\n' "$d"; return 0; fi
    sleep 5
  done
  red "    超时（${TIMEOUT}s）" >&2; return 1
}

# verify 名称 批次号 期望阶段… —— 终态、result_url HEAD 200、step_log 阶段与成本
verify() {
  local name=$1 bid=$2; shift 2
  local t0=$SECONDS d st
  d=$(poll_batch "$bid") || { bad "$name 轮询失败"; return; }
  st=$(jq -r .batch.status <<<"$d")
  if [[ $st != 4 ]]; then
    bad "$name 批次未全部成功（status=$st）" "$(jq -c '{err:.batch.error_reason, tasks:[.tasks[]|{status,error_reason}]}' <<<"$d")"
    return
  fi
  pass "$name 全部成功" "耗时 $(( SECONDS - t0 ))s"
  local u code
  while read -r u; do
    code=$(curl -sS -o /dev/null -w '%{http_code}' -I "$u")
    if [[ $code == 200 ]]; then pass "result_url HEAD 200" "$u"; else bad "result_url HEAD $code" "$u"; fi
  done < <(jq -r '.tasks[].result_url' <<<"$d")
  local a steps want cost
  a=$(admin "/batches/$bid") || { bad "$name 后台详情读取失败"; return; }
  steps=$(jq -r '.step_log|fromjson|[.[].step]|join(",")' <<<"$a")
  for want in "$@"; do
    if [[ ",$steps," == *",$want,"* ]]; then pass "step_log 含 $want"; else bad "step_log 缺 $want" "$steps"; fi
  done
  cost=$(jq -r '[.step_log|fromjson|.[]|select(.step=="generate")|(.cost_cny // 0)]|add // 0' <<<"$a")
  if awk "BEGIN{exit !($cost > 0)}"; then pass "生图成本 > 0" "cost=$cost"; else bad "step_log 里生图成本为 0" "$steps"; fi
}

run_tryon() {
  step "商拍换装：1 模特 × 1 张"
  if (( ${#GARMENTS[@]} < 2 )) || [[ -z $SAMPLE_MODEL_URL ]]; then skip "需要 SAMPLE_GARMENT_URLS（2 张）与 SAMPLE_MODEL_URL"; return; fi
  local g1 g2 m body r
  g1=$(upload "${GARMENTS[0]}") && g2=$(upload "${GARMENTS[1]}") && m=$(upload "$SAMPLE_MODEL_URL") || { bad "样例图上传失败"; return; }
  body=$(jq -nc --arg m "$m" --arg g1 "$g1" --arg g2 "$g2" --arg model "$MODEL" \
    '{model_image_urls:[$m], garment_images:[{url:$g1,role:"上衣",angle:"front"},{url:$g2,role:"上衣",angle:"back"}],
      items:["top"], set_mode:"one", count_per_model:1, model:$model, resolution:"2k"}')
  r=$(api POST /studio/tryon/submit "$body") || { bad "提交失败"; return; }
  verify "商拍" "$(jq -r .batch_id <<<"$r")" cropping prompting generating
}

run_pose() {
  step "姿态复刻：1 姿势"
  if (( ${#POSES[@]} < 2 )); then skip "需要 SAMPLE_POSE_URLS（人物原图 + 姿势图）"; return; fi
  local s p r
  s=$(upload "${POSES[0]}") && p=$(upload "${POSES[1]}") || { bad "样例图上传失败"; return; }
  r=$(api POST /studio/pose/submit "$(jq -nc --arg s "$s" --arg p "$p" --arg model "$MODEL" \
    '{source_image_url:$s, pose_image_urls:[$p], model:$model, resolution:"2k"}')") || { bad "提交失败"; return; }
  verify "姿态" "$(jq -r .batch_id <<<"$r")" prompting generating # 姿态没有切图阶段
}

run_imagegen() {
  step "图片生成：count=1"
  local r
  r=$(api POST /studio/image-generate/submit "$(jq -nc --arg model "$MODEL" \
    '{prompt:"白色背景上的一只陶瓷马克杯，柔和自然光，商品摄影", count:1, model:$model, resolution:"1k", aspect_ratio:"1:1"}')") ||
    { bad "提交失败"; return; }
  verify "图片生成" "$(jq -r .batch_id <<<"$r")" prompting generating # 图片生成没有切图阶段
}

step "依赖状态"
if d=$(admin /sidecar/status); then
  jq -r '.deps[]|"  \(if .ok then "✓" else "✗" end) \(.name) \(.base_url) \(.latency_ms)ms \(.detail)"' <<<"$d"
  [[ $(jq '[.deps[]|select(.ok|not)]|length' <<<"$d") == 0 ]] || red "  有依赖不可用，下面的用例大概率失败"
fi

mods=("$@"); (( ${#mods[@]} )) || mods=(tryon pose imagegen)
for m in "${mods[@]}"; do
  case $m in tryon|pose|imagegen) "run_$m" ;; *) red "未知模块 $m"; exit 2 ;; esac
done

echo
printf '通过 %d，失败 %d，跳过 %d\n' "$OK" "$FAIL" "$SKIP"
(( FAIL == 0 ))

#!/usr/bin/env bash
# 真实链路回归：对着**已部署的环境**，每个模块真实跑一条，确认返回结构与耗时。
#
#   bash e2e-real.sh                 # 只跑便宜的（文案、店铺档案、笔记、海报、长图）
#   bash e2e-real.sh video           # 只跑出视频的（口播、动作迁移、探店、短剧、复刻、数字人）
#   bash e2e-real.sh all             # 全部
#   bash e2e-real.sh note poster     # 指定模块
#
# 可用模块：copywriting shopProfile note poster longImage voiceover actionTransfer
#           exploreShop shortDrama videoRemake digitalHuman
#
# 环境变量：GW=http://127.0.0.1:18888/api/v1  USER_ID=990909  TIMEOUT=2700（秒）
#
# ⚠️ 出视频的模块会**真实产生费用**，而且慢（云端 2–10 分钟，本地 GPU 可能十几分钟）。
set -uo pipefail

GW=${GW:-http://127.0.0.1:18888/api/v1}
USER_ID=${USER_ID:-990909}
TIMEOUT=${TIMEOUT:-2700}
SAMPLE_IMAGE=${SAMPLE_IMAGE:-https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/images/20260915/9db717232a3645a79f006d92260dc7d4.png}
SAMPLE_VIDEO=${SAMPLE_VIDEO:-https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/videos/20260915/e8b22c6d21654a41aec8bca97065921c.mp4}
TMP=$(mktemp -d)
OK=0; FAIL=0; SKIP=0

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
gray()  { printf '\033[90m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[36m== %s\033[0m\n' "$*"; }

api() { # api METHOD PATH [BODY] → data（失败返回空并打印）
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -X "$method" -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -H "X-Request-Id: $(uuidgen)")
  [[ -n $body ]] && args+=(-d "$body")
  local resp; resp=$(curl "${args[@]}" "$GW$path" 2>&1)
  if [[ $(jq -r .code 2>/dev/null <<<"$resp") != 0 ]]; then
    red "    请求失败 $method $path → $(head -c 300 <<<"$resp")" >&2
    return 1
  fi
  jq -c '.data' <<<"$resp"
}

pass() { green "  ✓ $1"; [[ -n ${2:-} ]] && gray "      $2"; OK=$((OK+1)); }
skip() { printf '\033[33m  - %s\033[0m\n' "$1"; SKIP=$((SKIP+1)); }
bad()  { red   "  ✗ $1"; [[ -n ${2:-} ]] && gray "      $2"; FAIL=$((FAIL+1)); }

# upload 把一个公网文件传成「我们的」地址：优先预签名（线上），本地存储模式退回代传
upload() { # upload URL kind → 我们的地址
  local src=$1 kind=$2 ct file
  case $kind in image) ct=image/png; file=$TMP/s.png ;; video) ct=video/mp4; file=$TMP/s.mp4 ;; esac
  [[ -s $file ]] || curl -sS -o "$file" "$src" || return 1
  local p; p=$(api POST /upload/presign "{\"kind\":\"$kind\",\"content_type\":\"$ct\"}" 2>/dev/null)
  if [[ -n $p ]]; then # 线上：预签名直传
    curl -sS -X PUT -H "Content-Type: $ct" --data-binary "@$file" "$(jq -r .put_url <<<"$p")" >/dev/null || return 1
    jq -r .url <<<"$p"
    return
  fi
  curl -sS -H "X-User-Id: $USER_ID" -F "file=@$file" "$GW/upload" | jq -r .data.url
}

# poll 轮询到终态；poll 路径 完成状态 失败状态 [取地址的 jq 表达式]
poll() {
  local path=$1 doneStatus=$2 failStatus=$3 t0=$SECONDS d s
  while (( SECONDS - t0 < TIMEOUT )); do
    d=$(api GET "$path") || return 1
    s=$(jq -r .status <<<"$d")
    if [[ $s == "$doneStatus" ]]; then printf '%s\n' "$d"; return 0; fi
    if [[ $s == "$failStatus" ]]; then printf '%s\n' "$d"; return 2; fi
    sleep 5
  done
  red "    超时（${TIMEOUT}s）" >&2; return 1
}

took() { echo "耗时 $(( SECONDS - $1 ))s"; }

run_copywriting() {
  step "AI文案（直连百炼）"
  local t0=$SECONDS d
  d=$(api POST /copywriting/generate '{"source":"AI_WRITE","content_type":"explore","style":"recommend","topic":"西湖边新开的临湖咖啡店","target_words":120}') || { bad "提交失败"; return; }
  d=$(poll "/copywriting/$(jq -r .id <<<"$d")" 1 2)
  case $? in
    0) pass "生成成功（$(took $t0)）" "$(jq -r '.title' <<<"$d")" ;;
    2) bad "生成失败" "$(jq -r .fail_reason <<<"$d")" ;;
    *) bad "轮询异常" ;;
  esac
}

run_shopProfile() {
  step "店铺档案（不调模型）"
  local d
  d=$(api POST /shop-profiles '{"name":"回归测试店","open_date":"2025-10-03","self_title":"店长小林","selling_points":"临湖景观位、手冲单品"}') || { bad "创建失败"; return; }
  SHOP_ID=$(jq -r .id <<<"$d")
  pass "创建成功，id=$SHOP_ID"
}

run_note() {
  step "AI图文笔记（74 构思 → 75 出图，1 张）"
  local t0=$SECONDS d id
  d=$(api POST /notes/plan "{\"note_type\":\"explore\",\"topic\":\"西湖边的手冲咖啡店\",\"material_urls\":[\"$IMG\"],\"image_count\":1}") || { bad "提交失败"; return; }
  id=$(jq -r .id <<<"$d")
  d=$(poll "/notes/$id" 1 2)
  [[ $? == 0 ]] || { bad "构思失败" "$(jq -r .fail_reason <<<"$d")"; return; }
  pass "构思完成（$(took $t0)）" "标题：$(jq -r .title <<<"$d")"
  t0=$SECONDS
  api POST "/notes/$id/generate" '{}' >/dev/null || { bad "确认生成失败"; return; }
  d=$(poll "/notes/$id" 4 5)
  case $? in
    0) pass "出图完成（$(took $t0)）" "$(jq -r '.image_urls[0]' <<<"$d")" ;;
    2) bad "出图失败" "$(jq -r .fail_reason <<<"$d")" ;;
    *) bad "轮询异常" ;;
  esac
}

run_image_kind() { # run_image_kind kind 说明 额外参数
  step "$2"
  local t0=$SECONDS d id
  d=$(api POST /image-creations "$3") || { bad "提交失败"; return; }
  id=$(jq -r .id <<<"$d")
  d=$(poll "/image-creations/$id" 1 2)
  case $? in
    0) pass "生成完成（$(took $t0)）" "$(jq -r '.image_urls[0]' <<<"$d")" ;;
    2) bad "生成失败" "$(jq -r .fail_reason <<<"$d")" ;;
    *) bad "轮询异常" ;;
  esac
}

run_poster() {
  local t; t=$(api GET '/image-creations/templates?kind=poster' | jq -r '.list[0].id // empty')
  [[ -n $t ]] || { step "AI海报（78）"; skip "跳过：运营还没配海报模板"; return; }
  run_image_kind poster "AI海报（78）" "{\"kind\":\"poster\",\"content\":\"周末手冲咖啡 买一送一\",\"template_id\":$t,\"materials\":[],\"image_count\":1}"
}

run_longImage() {
  local t; t=$(api GET '/image-creations/templates?kind=longImage' | jq -r '.list[0].id // empty')
  [[ -n $t ]] || { step "AI长图（76）"; skip "跳过：运营还没配长图模板"; return; }
  run_image_kind longImage "AI长图（76）" "{\"kind\":\"longImage\",\"content\":\"手冲咖啡入门：选豆、水温、手法\",\"template_id\":$t,\"image_count\":1}"
}

run_voiceover() {
  run_image_kind voiceover "数字人口播（92，出视频）" "{\"kind\":\"voiceover\",\"content\":\"欢迎光临，今天推荐我们的手冲单品。\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"$IMG\",\"duration\":15,\"size\":\"9:16\"}"
}

run_actionTransfer() {
  local v; v=$(api GET /action-videos | jq -r '.presets[0].id // empty')
  local body="{\"kind\":\"actionTransfer\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"$IMG\",\"model\":\"doubao-seedance-2-0-260128\""
  if [[ -n $v ]]; then body="$body,\"action_video_id\":$v}"; else
    local mine; mine=$(api POST /action-videos "{\"url\":\"$VID\"}" | jq -r .id)
    body="$body,\"my_action_video_id\":$mine}"
  fi
  run_image_kind actionTransfer "动作迁移（9 → 86，出视频）" "$body"
}

run_video_two_step() { # run_video_two_step 说明 提交体
  step "$1"
  local t0=$SECONDS d id
  d=$(api POST /video-creations "$2") || { bad "提交失败"; return; }
  id=$(jq -r .id <<<"$d")
  d=$(poll "/video-creations/$id" 1 4)
  [[ $? == 0 ]] || { bad "第一步失败" "$(jq -r .fail_reason <<<"$d")"; return; }
  pass "第一步完成（$(took $t0)）" "$(jq -r '.script | tostring | .[0:120]' <<<"$d")"
  t0=$SECONDS
  api POST "/video-creations/$id/generate" '{}' >/dev/null || { bad "确认生成失败"; return; }
  d=$(poll "/video-creations/$id" 3 4)
  case $? in
    0) pass "出片完成（$(took $t0)）" "$(jq -r .video_url <<<"$d")" ;;
    2) bad "出片失败" "$(jq -r .fail_reason <<<"$d")" ;;
    *) bad "轮询异常" ;;
  esac
}

run_exploreShop() {
  run_video_two_step "探店视频（72 → 73/81，出视频）" \
    "{\"kind\":\"exploreShop\",\"user_text\":\"西湖边新开的临湖咖啡店，主打手冲\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"$IMG\",\"duration\":15,\"size\":\"9:16\"}"
}

run_shortDrama() {
  run_video_two_step "带货短剧（87 → 88/89，出视频）" \
    "{\"kind\":\"shortDrama\",\"user_text\":\"上班族早餐反转短剧\",\"selling_points\":\"便携榨汁杯、30秒出汁、USB充电\",\"prop_url\":\"$IMG\",\"duration\":15,\"size\":\"9:16\"}"
}

run_videoRemake() {
  run_video_two_step "爆款视频复刻（70 解析 → 71 复刻，出视频）" \
    "{\"kind\":\"videoRemake\",\"source\":\"upload\",\"video_url\":\"$VID\"}"
}

run_digitalHuman() {
  step "AI数字人（配音 TTS + 85 出视频）"
  local t0=$SECONDS d id voice
  voice=$(api GET /voices | jq -r '.list[0].id // empty')
  [[ -n $voice ]] || { skip "跳过：没有可用音色（先跑 install.sh voices）"; return; }
  d=$(api POST /digital-humans "{\"portrait_source\":\"UPLOAD\",\"image_url\":\"$IMG\",\"voice_id\":$voice,\"script\":\"欢迎光临，今天推荐我们的手冲单品。\"}") || { bad "提交失败"; return; }
  id=$(jq -r .id <<<"$d")
  d=$(poll "/digital-humans/$id" 2 3)
  case $? in
    0) pass "出片完成（$(took $t0)）" "$(jq -r .video_url <<<"$d")" ;;
    2) bad "失败" "$(jq -r .fail_reason <<<"$d")" ;;
    *) bad "轮询异常" ;;
  esac
}

CHEAP=(copywriting shopProfile note poster longImage)
VIDEO=(voiceover actionTransfer exploreShop shortDrama videoRemake digitalHuman)
case ${1:-cheap} in
  cheap) MODULES=("${CHEAP[@]}") ;;
  video) MODULES=("${VIDEO[@]}") ;;
  all)   MODULES=("${CHEAP[@]}" "${VIDEO[@]}") ;;
  *)     MODULES=("$@") ;;
esac

echo "网关：$GW    用户：$USER_ID    模块：${MODULES[*]}"
for m in "${MODULES[@]}"; do
  case " ${VIDEO[*]} " in *" $m "*) echo "提示：${MODULES[*]} 里含出视频的模块，会产生真实费用，且可能要等十几分钟"; break ;; esac
done

step "准备素材（下载示例图 / 视频并上传成我们的地址）"
IMG=$(upload "$SAMPLE_IMAGE" image) || { red "素材图上传失败，后面用到图的模块会失败"; IMG=""; }
gray "  图片：${IMG:-（失败）}"
VID=""
case " ${MODULES[*]} " in
  *videoRemake*|*actionTransfer*) VID=$(upload "$SAMPLE_VIDEO" video); gray "  视频：${VID:-（失败）}" ;;
esac

for m in "${MODULES[@]}"; do
  if declare -F "run_$m" >/dev/null; then "run_$m"; else red "未知模块 $m"; fi
done

echo
if (( FAIL == 0 )); then green "== 全部通过：$OK 项，跳过 $SKIP 项"; else red "== 通过 $OK 项，失败 $FAIL 项，跳过 $SKIP 项"; fi
gray "清理这次的测试数据：DELETE FROM afv_asset WHERE user_id=$USER_ID; 各业务表同理（user_id=$USER_ID）"
rm -rf "$TMP"
(( FAIL == 0 ))

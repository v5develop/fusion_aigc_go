#!/usr/bin/env bash
# 探店视频 / 带货短剧端到端测试（依赖 make run-all 已启动）
# 两步流程：出剧本（72 / 87）→ 确认 → 出成片（73→81 / 88→89），执行面走本地 mock-aigc。
# AI帮写真实调用模型（SKIP_MODEL=1 可跳过）。
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/api/v1}
ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
MOCK=${MOCK:-http://127.0.0.1:5055}
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 100000 ))}
PASS=0
TMP=$(mktemp -d)
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

api() { # api METHOD PATH [BODY] [REQUEST_ID] → data
  local method=$1 path=$2 body=${3:-} rid=${4:-}
  local args=(-sS -X "$method" -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json')
  [[ -n $rid ]] && args+=(-H "X-Request-Id: $rid")
  [[ -n $body ]] && args+=(-d "$body")
  local resp; resp=$(curl "${args[@]}" "$GW$path")
  if [[ $(jq -r .code <<<"$resp") != 0 ]]; then red "FAIL $method $path → $resp" >&2; exit 1; fi
  jq -c '.data' <<<"$resp"
}

api_code() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -X "$method" -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -H "X-Request-Id: $(uuidgen)")
  [[ -n $body ]] && args+=(-d "$body")
  curl "${args[@]}" "$GW$path" | jq -r .code
}

check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

poll() { # poll ID 目标状态 → data（到达目标状态或失败就返回）
  for _ in $(seq 1 60); do
    local d; d=$(api GET "/video-creations/$1")
    local s; s=$(jq -r .status <<<"$d")
    [[ $s == "$2" || $s == 4 ]] && { echo "$d"; return; }
    sleep 1
  done
  red "timeout polling $1" >&2; exit 1
}

upload() { # 本地存储模式下用网关代传，线上是前端直传
  printf '\x89PNG\r\n\x1a\n' > "$TMP/$1.png"; head -c 600 /dev/urandom >> "$TMP/$1.png"
  curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/$1.png" "$GW/upload" | jq -r .data.url
}

task_body() { curl -sS "$MOCK/mock/task/$1"; }

cleanup() {
  sql "DELETE FROM afv_asset WHERE user_id=$USER_ID;
       DELETE FROM afv_video_creation WHERE user_id=$USER_ID;
       DELETE FROM afv_shop_profile WHERE user_id=$USER_ID;"
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== 用户 $USER_ID"
M1=$(upload m1); M2=$(upload m2); M3=$(upload m3)

echo "== 表单配置"
EO=$(api GET '/video-creations/options?kind=exploreShop')
check "模型 2 个，默认 seedance2.5" "$(jq -r '([.models[].label]|join(","))+"|"+.default_model' <<<"$EO")" "seedance 2.5,XQ Video 1.0 Lite|seedance2.5"
check "时长 15/30，默认 15" "$(jq -r '(.durations|map(tostring)|join(","))+"|"+(.default_duration|tostring)' <<<"$EO")" "15,30|15"
check "比例 9:16/16:9，默认 9:16" "$(jq -r '([.sizes[].code]|join(","))+"|"+.default_size' <<<"$EO")" "9:16,16:9|9:16"
check "探店不要求商品图" "$(jq -r .prop_required <<<"$EO")" false
DO=$(api GET '/video-creations/options?kind=shortDrama')
check "短剧商品图必填" "$(jq -r .prop_required <<<"$DO")" true
check "非法 kind" "$(curl -sS -H "X-User-Id: $USER_ID" "$GW/video-creations/options?kind=bad" | jq -r .code)" 1001

echo "== 校验"
VB=$(jq -nc --arg m "$M1" '{kind:"exploreShop",portrait_source:"UPLOAD",image_url:$m}')
vb() { jq -c ". + $1" <<<"$VB"; }
B=$(vb '{}')
check "没有文案被拒" "$(api_code POST /video-creations "$B")" 1001
B=$(jq -c --arg t "$(printf '字%.0s' $(seq 1 501))" '. + {user_text:$t}' <<<"$VB")
check "文案超 500 字被拒" "$(api_code POST /video-creations "$B")" 1001
B=$(vb '{"user_text":"探店文案","duration":20}')
check "时长非 15/30 被拒" "$(api_code POST /video-creations "$B")" 1001
B=$(vb '{"user_text":"探店文案","model":"bad"}')
check "模型无效被拒" "$(api_code POST /video-creations "$B")" 1001
B=$(vb '{"user_text":"探店文案","env1_url":"https://evil.example.com/a.png"}')
check "外链素材被拒" "$(api_code POST /video-creations "$B")" 1001
B='{"kind":"shortDrama","user_text":"上班族早餐反转短剧"}'
check "短剧不传商品图被拒" "$(api_code POST /video-creations "$B")" 1001

echo "== 探店视频：构思（mock 72）"
SPID=$(api POST /shop-profiles '{"name":"西湖边咖啡店","self_title":"店长小林"}' | jq -r .id)
RID=$(uuidgen)
B=$(jq -nc --arg m "$M1" --arg e1 "$M2" --arg e2 "$M3" --argjson sp "$SPID" \
  '{kind:"exploreShop",user_text:"西湖边新开的临湖咖啡店，主打手冲",portrait_source:"UPLOAD",image_url:$m,env1_url:$e1,env2_url:$e2,shop_profile_id:$sp,duration:15,size:"9:16"}')
E1=$(api POST /video-creations "$B" "$RID")
EID=$(jq -r .id <<<"$E1")
check "构思中" "$(jq -r .status <<<"$E1")" 0
check "幂等：同一 request_id 同一条" "$(api POST /video-creations "$B" "$RID" | jq -r .id)" "$EID"
PT=$(sql "SELECT plan_task_id FROM afv_video_creation WHERE id=$EID")
B72=$(task_body "$PT")
check "72 入参：文案/时长/达人图/门店图/店内图" "$(jq -c '[.task_type,(.user_text|length>0),.duration,(.person_url|length>0),(.ENV_01_url|length>0),(.ENV_02_url|length>0)]' <<<"$B72")" '[72,true,15,true,true,true]'
check "72 收到拼好的店铺档案" "$(jq -r '.shop_profile|contains("店长小林")' <<<"$B72")" true
E1=$(poll "$EID" 1)
check "剧本已生成，待确认" "$(jq -r .status <<<"$E1")" 1
check "剧本有 3 幕，带镜头和台词" "$(jq -r '.script|fromjson|[(.scenes|length),(.scenes[0]|has("camera")),(.scenes[0]|has("dialogue"))]|map(tostring)|join(",")' <<<"$E1")" "3,true,true"

echo "== 探店视频：重新生成剧本"
E1=$(api POST "/video-creations/$EID/replan" '')
check "回到构思中，剧本已清空" "$(jq -r '[.status,(.script|length)]|map(tostring)|join(",")' <<<"$E1")" 0,0
PT2=$(sql "SELECT plan_task_id FROM afv_video_creation WHERE id=$EID")
check "换了新的任务号" "$([[ $PT2 != "$PT" ]] && echo yes)" yes
E1=$(poll "$EID" 1)
check "重新构思完成" "$(jq -r .status <<<"$E1")" 1

echo "== 探店视频：确认剧本 → 出片（mock 73→81）"
api POST "/video-creations/$EID/generate" '' > /dev/null
E1=$(poll "$EID" 3)
check "已完成，有成片" "$(jq -r '[.status,(.video_url|length>0)]|map(tostring)|join(",")' <<<"$E1")" 3,true
GT=$(sql "SELECT gen_task_id FROM afv_video_creation WHERE id=$EID")
B73=$(task_body "$GT")
check "73 入参：剧本原样回传 + 同一批图" "$(jq -c '[(.script.scenes|length),(.person_url|length>0),(.ENV_01_url|length>0),.duration,.aspectRatio,.resolution]' <<<"$B73")" '[3,true,true,15,"9:16","720p"]'
check "缺省 model 路由到云端 81" "$(curl -sS "$MOCK/aigc/getRes/$GT" | jq -r .task_type)" 81
check "成品进素材库，封面用人物图" "$(sql "SELECT CONCAT_WS('|',type,entry_code,cover_url='$M1') FROM afv_asset WHERE user_id=$USER_ID AND entry_code='exploreShop'")" "video|exploreShop|1"

echo "== 带货短剧：两步（mock 87 → 88）"
B=$(jq -nc --arg p "$M1" --arg e "$M2" --arg c "$M3" \
  '{kind:"shortDrama",user_text:"上班族早餐反转短剧",selling_points:"便携榨汁杯、30秒出汁、USB充电",prop_url:$p,env1_url:$e,portrait_source:"UPLOAD",image_url:$c,duration:30,size:"16:9",model:"lite"}')
D1=$(api POST /video-creations "$B" "$(uuidgen)")
DID=$(jq -r .id <<<"$D1")
D1=$(poll "$DID" 1)
check "短剧剧本已生成" "$(jq -r .status <<<"$D1")" 1
B87=$(task_body "$(sql "SELECT plan_task_id FROM afv_video_creation WHERE id=$DID")")
check "87 入参：商品图/卖点/主题/时长/比例" "$(jq -c '[.task_type,(.PROP_01_url|length>0),(.selling_points|length>0),(.theme|length>0),.duration,.aspect_ratio]' <<<"$B87")" '[87,true,true,true,30,"16:9"]'
api POST "/video-creations/$DID/generate" '' > /dev/null
D1=$(poll "$DID" 3)
check "短剧已完成" "$(jq -r '[.status,(.video_url|length>0)]|map(tostring)|join(",")' <<<"$D1")" 3,true
GT2=$(sql "SELECT gen_task_id FROM afv_video_creation WHERE id=$DID")
check "lite 留在本地 GPU 的 88" "$(curl -sS "$MOCK/aigc/getRes/$GT2" | jq -r .task_type)" 88
check "88 入参：剧本 + 商品图" "$(task_body "$GT2" | jq -c '[(.script.scenes|length),(.PROP_01_url|length>0),.duration]')" '[3,true,30]'

echo "== 失败与列表"
B=$(vb '{"user_text":"这条构思会失败 mock-fail-72"}')
FID=$(api POST /video-creations "$B" "$(uuidgen)" | jq -r .id)
F=$(poll "$FID" 1)
check "构思失败有原因" "$(jq -r '[.status,.fail_reason]|map(tostring)|join(",")' <<<"$F")" "4,构思失败，请重试"
check "失败后可以重新构思" "$(api POST "/video-creations/$FID/replan" '' | jq -r .status)" 0
check "全部 3 条" "$(api GET /video-creations | jq .total)" 3
check "按 kind 筛：探店 2 条" "$(api GET '/video-creations?kind=exploreShop' | jq .total)" 2
check "按 kind 筛：短剧 1 条" "$(api GET '/video-creations?kind=shortDrama' | jq .total)" 1
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/video-creations/$EID" | jq -r .code)" 1003
api DELETE "/video-creations/$EID" > /dev/null
check "删除后不可见" "$(api_code GET "/video-creations/$EID")" 1003
check "删除时素材库同步删除" "$(sql "SELECT count(*) FROM afv_asset WHERE user_id=$USER_ID AND entry_code='exploreShop' AND deleted=0")" 0

echo "== 运营后台"
check "后台按用户查" "$(curl -sS -H 'X-Admin-Id: 1' "$ADMIN/video-creations?user_id=$USER_ID" | jq '.data.total')" 2
check "后台按 kind 查" "$(curl -sS -H 'X-Admin-Id: 1' "$ADMIN/video-creations?user_id=$USER_ID&kind=shortDrama" | jq '.data.total')" 1

if [[ ${SKIP_MODEL:-0} != 1 ]]; then
  echo "== AI帮写（真实调模型）"
  T=$(api POST /copywriting/ai-write '{"scene":"explore_shop","context":"西湖边的咖啡店，主打手冲"}')
  check "探店文案" "$(jq '.text|length > 0' <<<"$T")" true
  T=$(api POST /copywriting/ai-write '{"scene":"short_drama","context":"便携榨汁杯"}')
  check "短剧主题" "$(jq '.text|length > 0' <<<"$T")" true
else
  echo "== AI帮写（SKIP_MODEL=1，已跳过）"
fi

green "== 全部通过：$PASS 项"

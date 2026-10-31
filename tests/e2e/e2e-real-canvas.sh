#!/usr/bin/env bash
# 画布真实链路回归：对着**已部署的环境**（nginx → gateway-api → canvas-rpc → 真实 ai-gateway / 媒体服务 / Java 兼容工具）跑一遍。
#
# ⚠️ 会**真实产生费用**：image（1 张）走 Go 计费，upscale 由 Java 按用户扣点，i2p 走 Go 计费；crop 不计费。
#    请用测试账号，BILLING_MODE=on 时确保账号有足够点数。
#
#   GW=https://aigc.example.com TOKEN=<32 位会话令牌> SAMPLE_IMAGE_URL=<我方 OSS 图片地址> bash tests/e2e/e2e-real-canvas.sh
#
# 环境变量：
#   GW               网关根地址（经 nginx：/api/v1/canvas、/api/v1/prompt-library 转 Go 网关），默认 http://127.0.0.1:18888
#   TOKEN            用户 Bearer（与 Java 共用的 32 位 hex 会话令牌；画布把它原样透传给 Java 兼容工具）
#   JAVA_TOKEN       可选：Java 侧令牌（默认同 TOKEN）；只用于直连 Java 核对任务（JAVA_URL 配了才查）
#   JAVA_URL         可选：Java 地址，配了会用 GET /api/v1/tasks/{taskUid} 交叉核对放大任务（taskUid 取后台 run 列表的 backend_ref）
#   USER_ID          仅 Auth.Mode=dev 且不给 TOKEN 时用 X-User-Id（这时 Java 工具会返回 7003「需要登录令牌」）
#   SAMPLE_IMAGE_URL 参考图（必须是我方 OSS / 素材库地址，且在 ai-gateway input_source_hosts 白名单内或经媒体服务可拉取）
#   TIMEOUT          单个 run 的轮询上限（秒），默认 600
#   KEEP=1           不删除本次创建的画布与提示词
set -uo pipefail

GW=${GW:-http://127.0.0.1:18888}
TOKEN=${TOKEN:-}
JAVA_TOKEN=${JAVA_TOKEN:-$TOKEN}
JAVA_URL=${JAVA_URL:-}
USER_ID=${USER_ID:-}
SAMPLE_IMAGE_URL=${SAMPLE_IMAGE_URL:-https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/images/20260915/9db717232a3645a79f006d92260dc7d4.png}
TIMEOUT=${TIMEOUT:-600}
KEEP=${KEEP:-0}
OK=0; FAIL=0
CID=""; PID=""

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
gray()  { printf '\033[90m%s\033[0m\n' "$*"; }
step()  { printf '\n\033[36m== %s\033[0m\n' "$*"; }
pass()  { green "  ✓ $1"; [[ -n ${2:-} ]] && gray "      $2"; OK=$((OK+1)); }
bad()   { red   "  ✗ $1"; [[ -n ${2:-} ]] && gray "      $2"; FAIL=$((FAIL+1)); }

auth() { if [[ -n $TOKEN ]]; then echo "Authorization: Bearer $TOKEN"; else echo "X-User-Id: ${USER_ID:?TOKEN 与 USER_ID 至少给一个}"; fi; }
call() { # call METHOD PATH [BODY] [IDEM] → 完整信封
  local args=(-sS -m 120 -X "$1" -H "$(auth)" -H 'Content-Type: application/json')
  [[ -n ${4:-} ]] && args+=(-H "Idempotency-Key: $4")
  [[ -n ${3:-} ]] && args+=(--data-binary "$3")
  curl "${args[@]}" "$GW$2"
}
api() { local r; r=$(call "$@"); if [[ $(jq -r .code 2>/dev/null <<<"$r") != 0 ]]; then red "    请求失败 $1 $2 → $(head -c 400 <<<"$r")" >&2; return 1; fi; jq -c .data <<<"$r"; }

cleanup() {
  [[ $KEEP == 1 ]] && return
  [[ -n $CID ]] && call DELETE "/api/v1/canvas/canvases/$CID" >/dev/null
  [[ -n $PID ]] && call DELETE "/api/v1/prompt-library/prompts/$PID" >/dev/null
}
trap cleanup EXIT

# run_node 节点 ID body → 终态 run（失败打印）
run_node() {
  local node=$1 body=$2 key r rid d st t0=$SECONDS
  key=$(uuidgen | tr 'A-Z' 'a-z')
  r=$(api POST "/api/v1/canvas/canvases/$CID/nodes/$node/run" "$(jq -c --arg k "$key" '.run_id=$k' <<<"$body")" "$key") || return 1
  rid=$(jq -r .request_id <<<"$r")
  while (( SECONDS - t0 < TIMEOUT )); do
    d=$(api GET "/api/v1/canvas/runs/$rid") || return 1
    st=$(jq -r .status <<<"$d")
    case $st in succeeded|failed|cancelled) echo "$d"; gray "      run $rid $st，耗时 $((SECONDS - t0))s" >&2; return 0;; esac
    sleep 2
  done
  red "    run $rid 超时（${TIMEOUT}s），最后状态 $st" >&2; return 1
}
head200() { [[ $(curl -sS -o /dev/null -m 30 -I -w '%{http_code}' "$1") == 200 ]]; }
cand_ok() { # cand_ok 名称 run
  local d=$2 u
  if [[ $(jq -r .status <<<"$d") != succeeded ]]; then bad "$1" "$(jq -c '{status,error,error_code}' <<<"$d")"; return 1; fi
  u=$(jq -r '.candidates[0].url // empty' <<<"$d")
  if [[ -n $u ]] && head200 "$u"; then pass "$1" "$u（$(jq -r '"\(.candidates[0].width)x\(.candidates[0].height)"' <<<"$d")，credits=$(jq -r .credits <<<"$d")）"
  else bad "$1：候选地址不可下载" "$u"; fi
}

step "画布：新建 / 保存 / 读取"
C=$(api POST /api/v1/canvas/canvases '{"title":"真实链路回归"}') && CID=$(jq -r .id <<<"$C")
[[ -n $CID ]] && pass "新建画布" "$CID" || { bad "新建画布"; exit 1; }
SNAP=$(jq -nc --arg img "$SAMPLE_IMAGE_URL" '{nodes:[
  {id:"t1",type:"text",position:{x:0,y:0},data:{kind:"text",title:"文字",text:"白底电商主图，商品居中，柔和棚拍光，高细节"}},
  {id:"i1",type:"image",position:{x:0,y:220},data:{kind:"input",title:"图片",image:$img}},
  {id:"m1",type:"model",position:{x:420,y:80},data:{kind:"model",title:"生图",model:"Image",modelId:"image-v1",ratio:"1:1",size:"1K",count:1}},
  {id:"c1",type:"tool",position:{x:420,y:420},data:{kind:"tool",tool:"crop",title:"裁剪",sourceNodeId:"i1"}},
  {id:"u1",type:"tool",position:{x:420,y:620},data:{kind:"tool",tool:"upscale",title:"放大",sourceNodeId:"i1"}},
  {id:"p1",type:"tool",position:{x:420,y:820},data:{kind:"tool",tool:"i2p",title:"反推",sourceNodeId:"i1"}}],
 edges:[{id:"e1",source:"t1",sourceHandle:"text",target:"m1",targetHandle:"prompt"},
        {id:"e2",source:"i1",sourceHandle:"image",target:"m1",targetHandle:"image",data:{order:1}}]}')
S=$(api PUT "/api/v1/canvas/canvases/$CID" "$(jq -nc --argjson s "$SNAP" --arg r "$(jq -r .revision <<<"$C")" '{snapshot:$s,if_match_revision:$r}')") \
  && pass "保存快照" "revision=$(jq -r .revision <<<"$S") node_count=$(jq -r .node_count <<<"$S")" || bad "保存快照"
CAT=$(api GET /api/v1/canvas/catalog) && pass "目录" "$(jq -c '{image:[.image_models[]|{model,max_image_refs,ready}],tools:[.tools[]|{tool,ready}]}' <<<"$CAT")" || bad "目录"

REF=$(jq -nc --arg img "$SAMPLE_IMAGE_URL" '[{handle:"image",index:1,node_id:"i1",image:$img}]')

step "image：真实 ai-gateway（GPT Image 2，1 张，Go 计费）"
D=$(run_node m1 "$(jq -nc --argjson r "$REF" '{kind:"image",model:"Image",model_id:"image-v1",ratio:"1:1",size:"1K",quality:"medium",count:1,
   references:([{handle:"prompt",node_id:"t1",text:"白底电商主图，商品居中，柔和棚拍光，高细节"}] + $r)}')") && cand_ok "image → succeeded，候选可下载" "$D"
G=$(api GET "/api/v1/canvas/canvases/$CID") && [[ $(jq -r '.snapshot.nodes[]|select(.id=="m1")|.data.status' <<<"$G") == done ]] \
  && pass "GetCanvas 回填 m1 运行态" || bad "GetCanvas 回填"

step "tool_crop：真实媒体服务（不计费）"
D=$(run_node c1 "$(jq -nc --argjson r "$REF" '{kind:"tool",tool:"crop",references:$r,params:{shape:"rect",x:0.2,y:0.2,w:0.6,h:0.6}}')") && cand_ok "crop → succeeded" "$D"

step "tool_upscale：真实 Java 兼容接口 /api/v1/tools/ai-image-upscaler（透传 Bearer，Java 计费）"
if [[ -z $TOKEN ]]; then bad "upscale 需要 TOKEN（Bearer 透传给 Java）"; else
  D=$(run_node u1 "$(jq -nc --argjson r "$REF" '{kind:"tool",tool:"upscale",references:$r,params:{scale:2}}')") && cand_ok "upscale → succeeded" "$D"
  if [[ -n $JAVA_URL ]]; then
    gray "      Java 任务核对请在后台 /api/v1/canvas/admin/runs 查 backend_ref（taskUid），再 GET $JAVA_URL/api/v1/tasks/{taskUid}"
  fi
fi

step "tool_i2p：真实 ai-gateway chat.completion（vlm.qwen-3.8-flash）"
D=$(run_node p1 "$(jq -nc --argjson r "$REF" '{kind:"tool",tool:"i2p",references:$r,params:{language:"zh"}}')")
if [[ $(jq -r .status <<<"$D") == succeeded && -n $(jq -r '.text // empty' <<<"$D") ]]; then pass "i2p → text 非空" "$(jq -r '.text' <<<"$D" | head -c 160)…"
else bad "i2p" "$(jq -c '{status,error,error_code}' <<<"$D")"; fi

step "提示词库 CRUD"
L=$(api GET '/api/v1/prompt-library/prompts?page_size=50') && pass "列表" "官方 $(jq '[.items[]|select(.owner=="official")]|length' <<<"$L") 条" || bad "列表"
P=$(api POST /api/v1/prompt-library/prompts '{"title":"回归 · 临时","prompt":"白底棚拍","models":["通用"],"tags":["回归"]}') && PID=$(jq -r .id <<<"$P") && pass "新增" "id=$PID" || bad "新增"
[[ -n $PID ]] && { api PUT "/api/v1/prompt-library/prompts/$PID" '{"title":"回归 · 改名","prompt":"白底棚拍 2"}' >/dev/null && pass "修改" || bad "修改"; }
[[ -n $PID ]] && { api DELETE "/api/v1/prompt-library/prompts/$PID" >/dev/null && { pass "删除"; PID=""; } || bad "删除"; }

printf '\n通过 %d，失败 %d\n' "$OK" "$FAIL"
(( FAIL == 0 ))

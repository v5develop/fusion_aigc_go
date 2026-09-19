#!/usr/bin/env bash
# AI文案端到端测试（依赖 make run-all 已启动，会真实调用千问模型）
# 用法：deploy/scripts/e2e.sh
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/api/v1}
ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 100000 ))}
PASS=0

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

api() { # api METHOD PATH [BODY] [REQUEST_ID] → data
  local method=$1 path=$2 body=${3:-} rid=${4:-}
  local args=(-sS -X "$method" -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json')
  [[ -n $rid ]] && args+=(-H "X-Request-Id: $rid")
  [[ -n $body ]] && args+=(-d "$body")
  local resp; resp=$(curl "${args[@]}" "$GW$path")
  if [[ $(jq -r .code <<<"$resp") != 0 ]]; then red "FAIL $method $path → $resp"; exit 1; fi
  jq -c '.data' <<<"$resp"
}

api_code() { # → 业务 code
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -X "$method" -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -H "X-Request-Id: $(uuidgen)")
  [[ -n $body ]] && args+=(-d "$body")
  curl "${args[@]}" "$GW$path" | jq -r .code
}

admin() {
  local method=$1 path=$2 body=${3:-}
  local args=(-sS -X "$method" -H 'X-Admin-Id: 1' -H 'Content-Type: application/json')
  [[ -n $body ]] && args+=(-d "$body")
  local resp; resp=$(curl "${args[@]}" "$ADMIN$path")
  if [[ $(jq -r .code <<<"$resp") != 0 ]]; then red "FAIL admin $method $path → $resp"; exit 1; fi
  jq -c '.data' <<<"$resp"
}

check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

poll() { # poll ID → 终态 data
  for _ in $(seq 1 30); do
    local d; d=$(api GET "/copywriting/$1")
    [[ $(jq -r .status <<<"$d") != 0 ]] && { echo "$d"; return; }
    sleep 2
  done
  red "timeout polling $1"; exit 1
}

echo "== 用户 $USER_ID"

echo "== 选项与模型"
O=$(api GET /copywriting/options)
check "选项接口不返回提示词/敏感词" "$(jq '[.content_types[],.styles[],.industries[],.word_counts[]]|length' <<<"$O")" 18
check "内容类型 5 个" "$(jq '.content_types|length' <<<"$O")" 5
check "文案风格 5 个" "$(jq '.styles|length' <<<"$O")" 5
check "行业 5 个" "$(jq '.industries|length' <<<"$O")" 5
check "字数 3 个" "$(jq '.word_counts|length' <<<"$O")" 3
check "当前模型" "$(jq -r '.model.code' <<<"$O")" qwen3-vl-plus
check "未登录返回 1002" "$(curl -sS "$GW/copywriting/options" | jq -r .code)" 1002

echo "== AI帮写（同步调模型）"
T=$(api POST /copywriting/ai-write '{"scene":"topic","content_type":"explore","context":"新开的临湖咖啡店"}' "$(uuidgen)")
check "帮写返回文本" "$([[ -n $(jq -r .text <<<"$T") ]] && echo yes)" yes
check "不支持的场景" "$(api_code POST /copywriting/ai-write '{"scene":"bad","context":"x"}')" 1001

echo "== 幂等"
RID=$(uuidgen)
BODY='{"source":"AI_WRITE","content_type":"general","style":"recommend","industry":"general","topic":"幂等测试","target_words":100}'
A=$(api POST /copywriting/generate "$BODY" "$RID")
B=$(api POST /copywriting/generate "$BODY" "$RID")
check "同一 request_id 只落一条" "$(jq -r .id <<<"$B")" "$(jq -r .id <<<"$A")"
check "缺少 request_id 被拒" "$(curl -sS -X POST -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -d "$BODY" "$GW/copywriting/generate" | jq -r .code)" 1001
poll "$(jq -r .id <<<"$A")" > /dev/null

echo "== 内容审核（关键词）"
WID=$(jq -r .id <<<"$(admin POST /copywriting/options '{"option_type":"BLOCKWORD","code":"敏感词测试","label":"测试用敏感词","status":1}')")
check "输入命中敏感词被拒" "$(api_code POST /copywriting/generate '{"source":"AI_WRITE","content_type":"general","topic":"这里有敏感词测试内容"}')" 1001
check "AI帮写也走审核" "$(api_code POST /copywriting/ai-write '{"scene":"topic","context":"敏感词测试"}')" 1001
admin DELETE "/copywriting/options/$WID" > /dev/null
check "删掉词后恢复正常" "$(api_code POST /copywriting/ai-write '{"scene":"topic","context":"正常的咖啡店文案"}')" 0

echo "== 提示词模板"
check "提示词有 7 条（含数字人配音、笔记主题）" "$(admin GET '/copywriting/options?option_type=PROMPT' | jq '.list|length')" 7
PID=$(admin GET '/copywriting/options?option_type=PROMPT' | jq -r '.list[]|select(.code=="generate_suffix")|.id')
check "模板内容非空" "$([[ -n $(admin GET '/copywriting/options?option_type=PROMPT' | jq -r '.list[0].value') ]] && echo yes)" yes

echo "== AI帮写生成"
BODY='{"source":"AI_WRITE","content_type":"explore","style":"recommend","industry":"general","topic":"西湖边新开的临湖咖啡店，主打手冲和甜品","target_words":200}'
C=$(api POST /copywriting/generate "$BODY" "$(uuidgen)")
ID=$(jq -r .id <<<"$C")
check "初始状态生成中" "$(jq -r .status <<<"$C")" 0
C=$(poll "$ID")
check "生成成功" "$(jq -r .status <<<"$C")" 1
check "有标题" "$([[ -n $(jq -r .title <<<"$C") ]] && echo yes)" yes
check "字数已统计" "$([[ $(jq -r .word_count <<<"$C") -gt 0 ]] && echo yes)" yes
check "类型标签" "$(jq -r .content_type_label <<<"$C")" 探店
check "非法选项被拒" "$(api_code POST /copywriting/generate '{"source":"AI_WRITE","content_type":"nope","topic":"x"}')" 1001
check "仿写缺范文被拒" "$(api_code POST /copywriting/generate '{"source":"IMITATE","topic":"x"}')" 1001

echo "== 爆款仿写"
REF=$(jq -r .body <<<"$C")
BODY=$(jq -n --arg r "$REF" '{source:"IMITATE",content_type:"promotion",style:"recommend",reference_text:$r,topic:"城东新开的川味小馆，主打麻辣香锅",target_words:200}')
C2=$(api POST /copywriting/generate "$BODY" "$(uuidgen)")
ID2=$(jq -r .id <<<"$C2")
C2=$(poll "$ID2")
check "仿写生成成功" "$(jq -r .status <<<"$C2")" 1

echo "== 重新生成"
C3=$(api POST "/copywriting/$ID/regenerate" '' "$(uuidgen)")
ID3=$(jq -r .id <<<"$C3")
check "重新生成是新记录" "$([[ $ID3 != "$ID" ]] && echo yes)" yes
check "沿用原参数" "$(jq -r .content_type <<<"$C3")" explore
poll "$ID3" > /dev/null

echo "== 自主撰写与编辑"
M=$(api POST /copywriting '{"title":"手写标题","body":"这是我自己写的文案正文"}')
MID=$(jq -r .id <<<"$M")
check "自主撰写直接完成" "$(jq -r .status <<<"$M")" 1
check "来源 MANUAL" "$(jq -r .source <<<"$M")" MANUAL
check "自主撰写不能重新生成" "$(api_code POST "/copywriting/$MID/regenerate" '')" 1004
U=$(api PUT "/copywriting/$MID" '{"title":"改过的标题","body":"改过的正文内容"}')
check "编辑生效" "$(jq -r .title <<<"$U")" 改过的标题
check "正文为空被拒" "$(api_code PUT "/copywriting/$MID" '{"title":"x","body":"  "}')" 1001

echo "== 列表与搜索"
check "列表 5 条" "$(api GET '/copywriting' | jq '.total')" 5
check "按类型筛选" "$(api GET '/copywriting?content_type=explore' | jq '.total')" 2
check "关键词搜索" "$(api GET '/copywriting?keyword=改过的' | jq '.total')" 1
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/copywriting/$MID" | jq -r .code)" 1003

# 生成历史本版不交付，这里只做接口连通性与鉴权校验
echo "== 生成历史（TODO 本版不交付）"
D=$(api GET "/copywriting/$ID/derivatives?kind=all")
check "暂无衍生内容" "$(jq '.total' <<<"$D")" 0
check "不存在的文案" "$(curl -sS -H "X-User-Id: $USER_ID" "$GW/copywriting/99999999/derivatives" | jq -r .code)" 1003

echo "== 删除"
api DELETE "/copywriting/$MID" > /dev/null
check "删除后不可见" "$(api_code GET "/copywriting/$MID")" 1003
check "删除后列表 4 条" "$(api GET '/copywriting' | jq '.total')" 4

echo "== 运营后台"
check "后台未登录" "$(curl -sS "$ADMIN/copywriting/options" | jq -r .code)" 1002
check "后台模型只读" "$(admin GET /copywriting/model | jq -r .code)" qwen3-vl-plus
check "后台选项全量 25 条" "$(admin GET /copywriting/options | jq '.list|length')" 25
check "按类型筛选" "$(admin GET '/copywriting/options?option_type=STYLE' | jq '.list|length')" 5
NO=$(admin POST /copywriting/options '{"option_type":"INDUSTRY","code":"pet","label":"宠物","value":"宠物门店场景","sort_order":9,"status":0}')
NID=$(jq -r .id <<<"$NO")
check "停用选项不在 App 端" "$(api GET /copywriting/options | jq '[.industries[]|select(.code=="pet")]|length')" 0
admin PUT "/copywriting/options/$NID" '{"option_type":"INDUSTRY","code":"pet","label":"宠物","value":"宠物门店场景","sort_order":9,"status":1}' > /dev/null
check "启用后出现在 App 端" "$(api GET /copywriting/options | jq '[.industries[]|select(.code=="pet")]|length')" 1
check "编码重复被拒" "$(curl -sS -X POST -H 'X-Admin-Id: 1' -H 'Content-Type: application/json' -d '{"option_type":"INDUSTRY","code":"pet","label":"x","status":1}' "$ADMIN/copywriting/options" | jq -r .code)" 1005
admin DELETE "/copywriting/options/$NID" > /dev/null
check "删除后消失" "$(admin GET '/copywriting/options?option_type=INDUSTRY' | jq '[.list[]|select(.code=="pet")]|length')" 0
NO2=$(admin POST /copywriting/options '{"option_type":"INDUSTRY","code":"pet","label":"宠物","status":1}')
check "删除后同 code 可重建" "$([[ $(jq -r .id <<<"$NO2") != "$NID" ]] && echo yes)" yes
admin DELETE "/copywriting/options/$(jq -r .id <<<"$NO2")" > /dev/null
check "后台能查用户文案" "$(admin GET "/copywriting?user_id=$USER_ID" | jq '.total')" 4

green "== 全部通过：$PASS 项"

#!/usr/bin/env bash
# AI海报 / AI长图端到端测试（依赖 make run-all 已启动）
# 执行面（78 海报 / 76 长图）走本地 mock-aigc。模板会临时插进现有模板素材表，结束时清理。
# 用法：deploy/scripts/e2e-imagegen.sh
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/api/v1}
ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
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

admin() {
  local resp; resp=$(curl -sS -H 'X-Admin-Id: 1' "$ADMIN$1")
  if [[ $(jq -r .code <<<"$resp") != 0 ]]; then red "FAIL admin $1 → $resp" >&2; exit 1; fi
  jq -c '.data' <<<"$resp"
}

check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

poll() { # poll ID → 终态 data
  for _ in $(seq 1 60); do
    local d; d=$(api GET "/image-creations/$1")
    [[ $(jq -r .status <<<"$d") != 0 ]] && { echo "$d"; return; }
    sleep 1
  done
  red "timeout polling $1" >&2; exit 1
}

upload() {
  printf '\x89PNG\r\n\x1a\n' > "$TMP/$1.png"; head -c 800 /dev/urandom >> "$TMP/$1.png"
  curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/$1.png" "$GW/upload" | jq -r .data.url
}

# 模板：临时插进现有模板素材表（localLife/poster|longImage/template）
template() { # template 标题 模块 [海报类型code] → 素材 id
  local id
  id=$(sql "INSERT INTO afv_public_template_assets (biz_type,type,title,url) VALUES ('localLife','image','$1','https://mock-oss.example.com/tpl/$1-$USER_ID.png');
            SET @a=LAST_INSERT_ID();
            INSERT INTO afv_public_template_asset_placement (biz_type,module_code,slot_code,asset_id,sort_order,status) VALUES ('localLife','$2','template',@a,1,1);
            SELECT @a;")
  if [[ -n ${3:-} ]]; then
    sql "INSERT INTO afv_public_template_asset_filter (asset_id, filter_id, option_id)
         SELECT $id, f.id, o.id FROM afv_public_template_filter f
         JOIN afv_public_template_filter_option o ON o.filter_id=f.id AND o.option_code='$3'
         WHERE f.biz_type='localLife' AND f.filter_code='posterType'"
  fi
  echo "$id"
}

cleanup() {
  sql "DELETE af FROM afv_public_template_asset_filter af JOIN afv_public_template_assets a ON a.id=af.asset_id WHERE a.url LIKE '%-$USER_ID.png';
       DELETE p FROM afv_public_template_asset_placement p JOIN afv_public_template_assets a ON a.id=p.asset_id WHERE a.url LIKE '%-$USER_ID.png';
       DELETE FROM afv_public_template_assets WHERE url LIKE '%-$USER_ID.png';
       DELETE FROM afv_asset_item WHERE asset_id IN (SELECT id FROM (SELECT id FROM afv_asset WHERE user_id=$USER_ID) x);
       DELETE FROM afv_asset WHERE user_id=$USER_ID;
       DELETE FROM afv_image_creation WHERE user_id=$USER_ID;"
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== 用户 $USER_ID"

echo "== 表单配置"
PO=$(api GET '/image-creations/options?kind=poster')
check "海报类型 6 个" "$(jq -r '[.poster_types[].label]|join(",")' <<<"$PO")" 社媒,营销,节日,喜庆,邀请函,招聘
check "海报风格 15 个，首个为 AI自动匹配" "$(jq -r '[(.poster_styles|length|tostring), .poster_styles[0].label]|join(",")' <<<"$PO")" 15,AI自动匹配
check "比例 6 个，默认 9:16" "$(jq -r '[(.sizes|length|tostring), .default_size]|join(",")' <<<"$PO")" 6,9:16
check "海报模板必选" "$(jq -r .template_required <<<"$PO")" true
check "素材上限 3、张数上限 4" "$(jq -r '[.max_materials,.max_images]|map(tostring)|join(",")' <<<"$PO")" 3,4
LO=$(api GET '/image-creations/options?kind=longImage')
check "长图比例固定 1:4" "$(jq -r '[(.sizes|length|tostring), .default_size]|join(",")' <<<"$LO")" 1,1:4
check "长图模板可不选" "$(jq -r .template_required <<<"$LO")" false
check "长图无海报类型/风格" "$(jq -r '[(.poster_types|length),(.poster_styles|length)]|map(tostring)|join(",")' <<<"$LO")" 0,0
check "非法 kind" "$(curl -sS -H "X-User-Id: $USER_ID" "$GW/image-creations/options?kind=bad" | jq -r .code)" 1001

echo "== 模板（复用现有模板素材表）"
T_FEST=$(template 节日模板 poster festival)
T_MKT=$(template 营销模板 poster marketing)
T_LONG=$(template 长图模板 longImage)
check "海报模板全部 2 个" "$(api GET '/image-creations/templates?kind=poster' | jq "[.list[]|select(.id==$T_FEST or .id==$T_MKT)]|length")" 2
check "按类型筛只剩节日的" "$(api GET '/image-creations/templates?kind=poster&poster_type=festival' | jq -r "[.list[]|select(.id==$T_FEST or .id==$T_MKT)]|length")" 1
check "长图模板不在海报列表里" "$(api GET '/image-creations/templates?kind=poster' | jq "[.list[]|select(.id==$T_LONG)]|length")" 0

echo "== 海报：校验"
M1=$(upload m1); M2=$(upload m2); M3=$(upload m3); M4=$(upload m4)
check "内容为空被拒" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"  \",\"template_id\":$T_FEST}")" 1001
check "没选模板被拒" "$(api_code POST /image-creations '{"kind":"poster","content":"夏日促销"}')" 1001
check "模板不存在被拒" "$(api_code POST /image-creations '{"kind":"poster","content":"x","template_id":99999999}')" 1001
check "长图模板不能用于海报" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_LONG}")" 1001
check "素材超 3 张被拒" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"material_urls\":[\"$M1\",\"$M2\",\"$M3\",\"$M4\"]}")" 1001
check "外链素材被拒" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"material_urls\":[\"https://evil.example.com/a.png\"]}")" 1001
check "风格无效被拒" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"poster_style\":\"bad\"}")" 1001
check "比例无效被拒" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"size\":\"5:7\"}")" 1001
check "张数超上限被拒" "$(api_code POST /image-creations "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"image_count\":5}")" 1001
check "缺少 request_id 被拒" "$(curl -sS -X POST -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -d "{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST}" "$GW/image-creations" | jq -r .code)" 1001

echo "== 海报：生成（mock 78）"
RID=$(uuidgen)
BODY="{\"kind\":\"poster\",\"content\":\"夏日购物狂欢，全场满200立减50\",\"template_id\":$T_FEST,\"poster_type\":\"festival\",\"poster_style\":\"flat\",\"size\":\"3:4\",\"image_count\":2,\"material_urls\":[\"$M1\",\"$M2\"]}"
P=$(api POST /image-creations "$BODY" "$RID")
PID=$(jq -r .id <<<"$P")
check "生成中" "$(jq -r .status <<<"$P")" 0
check "幂等：同一 request_id 同一条" "$(api POST /image-creations "$BODY" "$RID" | jq -r .id)" "$PID"
check "一张图一个任务" "$(sql "SELECT JSON_LENGTH(tasks) FROM afv_image_creation WHERE id=$PID")" 2
check "模板地址已快照" "$(jq -r .template_url <<<"$P")" "https://mock-oss.example.com/tpl/节日模板-$USER_ID.png"
P=$(poll "$PID")
check "已完成" "$(jq -r .status <<<"$P")" 1
check "出 2 张图" "$(jq '.image_urls|length' <<<"$P")" 2
T1=$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$PID")
B78=$(curl -sS "http://127.0.0.1:5055/mock/task/$T1")
check "78 入参：模板/素材/比例/模型/类型与风格传中文" "$(jq -c '[.task_type,(.materials|length),.size,.model,.poster_type,.poster_style]' <<<"$B78")" '[78,2,"3:4","max","节日","扁平插画"]'
check "78 收到的模板就是所选模板" "$(jq -r .template <<<"$B78")" "https://mock-oss.example.com/tpl/节日模板-$USER_ID.png"

echo "== 长图：生成（mock 76，不选模板）"
L=$(api POST /image-creations '{"kind":"longImage","content":"三段式长图内容"}' "$(uuidgen)")
LID=$(jq -r .id <<<"$L")
L=$(poll "$LID")
check "已完成，默认出 1 张" "$(jq -r '[.status,(.image_urls|length)]|map(tostring)|join(",")' <<<"$L")" 1,1
check "长图比例固定 1:4" "$(jq -r .size <<<"$L")" 1:4
T2=$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$LID")
B76=$(curl -sS "http://127.0.0.1:5055/mock/task/$T2")
check "76 入参：类型/比例/不传 model" "$(jq -c '[.task_type,.size,has("model")]' <<<"$B76")" '[76,"1:4",false]'
L2=$(api POST /image-creations "{\"kind\":\"longImage\",\"content\":\"带模板的长图\",\"template_id\":$T_LONG,\"material_urls\":[\"$M1\"]}" "$(uuidgen)")
L2=$(poll "$(jq -r .id <<<"$L2")")
check "长图可选模板" "$(jq -r .status <<<"$L2")" 1

echo "== 失败与部分成功"
F=$(api POST /image-creations "{\"kind\":\"poster\",\"content\":\"这张会失败 mock-fail-78\",\"template_id\":$T_MKT}" "$(uuidgen)")
F=$(poll "$(jq -r .id <<<"$F")")
check "全部失败有原因" "$(jq -r '[.status,.fail_reason]|map(tostring)|join(",")' <<<"$F")" "2,生成失败，请重试"
PT=$(api POST /image-creations "{\"kind\":\"poster\",\"content\":\"一半成功 mock-partial\",\"template_id\":$T_MKT,\"image_count\":2}" "$(uuidgen)")
PT=$(poll "$(jq -r .id <<<"$PT")")
check "部分成功：算完成，只返回成功的那张" "$(jq -r '[.status,(.image_urls|length)]|map(tostring)|join(",")' <<<"$PT")" 1,1

echo "== 素材库"
GROUP=$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$PID")
check "海报成品每张图一行" "$(sql "SELECT count(*) FROM afv_asset WHERE source_task_id='$GROUP' AND deleted=0")" 2
check "type/entry_code/比例" "$(sql "SELECT CONCAT_WS('|',type,entry_code,aspect_ratio) FROM afv_asset WHERE source_task_id='$GROUP' AND deleted=0 LIMIT 1")" "poster|poster|3:4"
check "长图成品也进素材库" "$(sql "SELECT count(*) FROM afv_asset WHERE user_id=$USER_ID AND entry_code='longImage' AND deleted=0")" 2
check "素材库分类已挂上入口" "$(sql "SELECT GROUP_CONCAT(c.category_name ORDER BY c.category_code) FROM afv_asset_category_entry e JOIN afv_asset_category c ON c.id=e.category_id WHERE e.entry_code IN ('poster','longImage')")" 长图,海报

echo "== 列表与删除"
check "全部 5 条" "$(api GET /image-creations | jq .total)" 5
check "按 kind 筛：海报 3 条" "$(api GET '/image-creations?kind=poster' | jq .total)" 3
check "按状态筛：已完成 4 条" "$(api GET '/image-creations?status=1' | jq .total)" 4
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/image-creations/$PID" | jq -r .code)" 1003
api DELETE "/image-creations/$PID" > /dev/null
check "删除后不可见" "$(api_code GET "/image-creations/$PID")" 1003
check "删除时素材库同步删除" "$(sql "SELECT count(*) FROM afv_asset WHERE source_task_id='$GROUP' AND deleted=0")" 0
check "删除后 4 条" "$(api GET /image-creations | jq .total)" 4

echo "== 运营后台"
check "后台按用户查" "$(admin "/image-creations?user_id=$USER_ID" | jq .total)" 4
check "后台按 kind 查" "$(admin "/image-creations?user_id=$USER_ID&kind=longImage" | jq .total)" 2

green "== 全部通过：$PASS 项"

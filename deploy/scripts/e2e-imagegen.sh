#!/usr/bin/env bash
# AI海报 / AI长图 / 动作迁移 / 数字人口播端到端测试（依赖 make run-all 已启动）
# 执行面（78 / 76 / 9 / 92）走本地 mock-aigc。模板会临时插进现有模板素材表，结束时清理。
# AI帮写真实调用模型（SKIP_MODEL=1 可跳过）。
# 用法：deploy/scripts/e2e-imagegen.sh
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/lf-api/v1}
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

# 模板：临时插进现有模板素材表（localLife/poster|longImage/template、actionTransfer/actionVideo）
template() { # template 标题 模块 [海报类型code] [素材类型 image|video] [槽位] → 素材 id
  local id
  id=$(sql "INSERT INTO afv_public_template_assets (biz_type,type,title,url) VALUES ('localLife','${4:-image}','$1','https://mock-oss.example.com/tpl/$1-$USER_ID.png');
            SET @a=LAST_INSERT_ID();
            INSERT INTO afv_public_template_asset_placement (biz_type,module_code,slot_code,asset_id,sort_order,status) VALUES ('localLife','$2','${5:-template}',@a,1,1);
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
       DELETE FROM afv_workspace_tag WHERE workspace_id IN (SELECT id FROM (SELECT id FROM afv_workspace WHERE owner_user_id=$USER_ID) x);
       DELETE FROM afv_workspace_member WHERE user_id=$USER_ID;
       DELETE FROM afv_workspace WHERE owner_user_id=$USER_ID;
       DELETE FROM afv_image_creation WHERE user_id=$USER_ID;
       DELETE FROM afv_action_video WHERE user_id=$USER_ID;"
  rm -rf "$TMP"
}
trap cleanup EXIT

echo "== 用户 $USER_ID"

echo "== 表单配置"
PO=$(api GET '/image-creations/options?kind=poster')
check "海报类型 6 个" "$(jq -r '[.poster_types[].label]|join(",")' <<<"$PO")" 社媒,营销,节日,喜庆,邀请函,招聘
check "海报风格 15 个，首个为 AI自动匹配" "$(jq -r '[(.poster_styles|length|tostring), .poster_styles[0].label]|join(",")' <<<"$PO")" 15,AI自动匹配
check "比例 6 个，默认 2:3（产品定）" "$(jq -r '[(.sizes|length|tostring), .default_size]|join(",")' <<<"$PO")" 6,2:3
check "海报模板必选" "$(jq -r .template_required <<<"$PO")" true
check "素材上限 3、张数上限 4" "$(jq -r '[.max_materials,.max_images]|map(tostring)|join(",")' <<<"$PO")" 3,4
LO=$(api GET '/image-creations/options?kind=longImage')
check "长图比例固定 1:3（产品定）" "$(jq -r '[(.sizes|length|tostring), .default_size]|join(",")' <<<"$LO")" 1,1:3
check "长图模板必选（最新对接文档 76 的 template_url 必填）" "$(jq -r .template_required <<<"$LO")" true
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
B="{\"kind\":\"poster\",\"content\":\"  \",\"template_id\":$T_FEST}"
check "内容为空被拒" "$(api_code POST /image-creations "$B")" 1001
check "没选模板被拒" "$(api_code POST /image-creations '{"kind":"poster","content":"夏日促销"}')" 1001
check "模板不存在被拒" "$(api_code POST /image-creations '{"kind":"poster","content":"x","template_id":99999999}')" 1001
B="{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_LONG}"
check "长图模板不能用于海报" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"material_urls\":[\"$M1\",\"$M2\",\"$M3\",\"$M4\"]}"
check "素材超 3 张被拒" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"material_urls\":[\"https://evil.example.com/a.png\"]}"
check "外链素材被拒" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"poster_style\":\"bad\"}"
check "风格无效被拒" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"size\":\"5:7\"}"
check "比例无效被拒" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"poster\",\"content\":\"x\",\"template_id\":$T_FEST,\"image_count\":5}"
check "张数超上限被拒" "$(api_code POST /image-creations "$B")" 1001
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

echo "== 长图：生成（mock 76）"
check "长图不选模板被拒" "$(api_code POST /image-creations '{"kind":"longImage","content":"三段式长图内容"}')" 1001
BL=$(jq -nc --argjson t "$T_LONG" '{kind:"longImage",content:"三段式长图内容",template_id:$t}')
L=$(api POST /image-creations "$BL" "$(uuidgen)")
LID=$(jq -r .id <<<"$L")
L=$(poll "$LID")
check "已完成，默认出 1 张" "$(jq -r '[.status,(.image_urls|length)]|map(tostring)|join(",")' <<<"$L")" 1,1
check "长图比例固定 1:3（产品定）" "$(jq -r .size <<<"$L")" 1:3
T2=$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$LID")
B76=$(curl -sS "http://127.0.0.1:5055/mock/task/$T2")
check "76 入参：类型/比例/模板/不传 model" "$(jq -c '[.task_type,.size,.template_url,has("model")]' <<<"$B76")" "[76,\"1:3\",\"https://mock-oss.example.com/tpl/长图模板-$USER_ID.png\",false]"
BL2=$(jq -nc --argjson t "$T_LONG" --arg m "$M1" '{kind:"longImage",content:"带素材的长图",template_id:$t,material_urls:[$m]}')
L2=$(api POST /image-creations "$BL2" "$(uuidgen)")
L2=$(poll "$(jq -r .id <<<"$L2")")
check "长图模板 + 素材" "$(jq -r .status <<<"$L2")" 1

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
check "type/entry_code/比例" "$(sql "SELECT CONCAT_WS('|',type,entry_code,aspect_ratio) FROM afv_asset WHERE source_task_id='$GROUP' AND deleted=0 LIMIT 1")" "image|poster|3:4"
check "海报资产挂在本人的个人空间" "$(sql "SELECT COUNT(DISTINCT a.workspace_id) FROM afv_asset a JOIN afv_workspace w ON w.id=a.workspace_id AND w.owner_user_id=$USER_ID AND w.kind='PERSONAL' WHERE a.user_id=$USER_ID AND a.deleted=0")" 1
check "长图成品也进素材库" "$(sql "SELECT count(*) FROM afv_asset WHERE user_id=$USER_ID AND entry_code='longImage' AND deleted=0")" 2
check "素材库分类已挂上入口" "$(sql "SELECT GROUP_CONCAT(c.category_name ORDER BY c.category_code) FROM afv_asset_category_entry e JOIN afv_asset_category c ON c.id=e.category_id WHERE e.entry_code IN ('poster','longImage')")" 长图,海报

echo "== 动作迁移：选项与动作视频"
AO=$(api GET '/image-creations/options?kind=actionTransfer')
check "模型 2 个" "$(jq -r '[.models[].label]|join(",")' <<<"$AO")" "seedance 2.0,xiaoqi-video-1.0-lite"
check "动作迁移一次一条" "$(jq -r .max_images <<<"$AO")" 1
T_ACT=$(template 跳舞 actionTransfer "" video actionVideo)
V1=$(upload v1)   # 当作用户直传上来的动作视频
AV=$(api POST /action-videos "{\"url\":\"$V1\"}")
check "导入后自动命名" "$(jq -r .name <<<"$AV")" 我的动作1
AVID=$(jq -r .id <<<"$AV")
check "外链视频被拒" "$(api_code POST /action-videos '{"url":"https://evil.example.com/a.mp4"}')" 1001
L=$(api GET /action-videos)
check "预置动作可见" "$(jq "[.presets[]|select(.id==$T_ACT)]|length" <<<"$L")" 1
check "我的动作可见且标记 mine" "$(jq -r "[.mine[]|select(.id==$AVID)]|map(.mine)|join(\",\")" <<<"$L")" true

echo "== 动作迁移：生成（mock 9 / 86）"
PUB=$(template 公共形象 digitalHuman "" image publicModel)   # 公共数字人（复用模板素材表）
B="{\"kind\":\"actionTransfer\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"$M1\",\"model\":\"lite\"}"
check "没选动作视频被拒" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"actionTransfer\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"$M1\",\"action_video_id\":$T_ACT,\"model\":\"bad\"}"
check "模型无效被拒" "$(api_code POST /image-creations "$B")" 1001
B="{\"kind\":\"actionTransfer\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"https://evil.example.com/a.png\",\"action_video_id\":$T_ACT,\"model\":\"lite\"}"
check "人物图外链被拒" "$(api_code POST /image-creations "$B")" 1001
A1=$(api POST /image-creations "{\"kind\":\"actionTransfer\",\"portrait_source\":\"UPLOAD\",\"image_url\":\"$M1\",\"action_video_id\":$T_ACT,\"model\":\"lite\"}" "$(uuidgen)")
A1ID=$(jq -r .id <<<"$A1")
check "人物图与动作视频已解析" "$(jq -r '[(.input_image_url|length>0),(.input_video_url|length>0)]|map(tostring)|join(",")' <<<"$A1")" true,true
A1=$(poll "$A1ID")
check "生成完成，出 1 个视频" "$(jq -r '[.status,(.image_urls|length)]|map(tostring)|join(",")' <<<"$A1")" 1,1
AT1=$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$A1ID")
B9=$(curl -sS "http://127.0.0.1:5055/mock/task/$AT1")
check "9 入参：视频/人物图/模型" "$(jq -c '[.task_type,(.input_video|length>0),(.reference_image|length>0),.model]' <<<"$B9")" '[9,true,true,"lite"]'
A2=$(api POST /image-creations "{\"kind\":\"actionTransfer\",\"portrait_source\":\"PUBLIC\",\"public_id\":$PUB,\"my_action_video_id\":$AVID,\"model\":\"doubao-seedance-2-0-260128\"}" "$(uuidgen)")
A2ID=$(jq -r .id <<<"$A2")
check "公共数字人作为人物图可用" "$(jq -r '.input_image_url|endswith("公共形象-'"$USER_ID"'.png")' <<<"$A2")" true
A2=$(poll "$A2ID")
check "seedance 2.0 路由到 86" "$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$A2ID" | xargs -I{} curl -sS "http://127.0.0.1:5055/aigc/getRes/{}" | jq -r .task_type)" 86
check "动作迁移成品进素材库" "$(sql "SELECT CONCAT_WS('|',type,entry_code) FROM afv_asset WHERE user_id=$USER_ID AND entry_code='actionTransfer' AND deleted=0 LIMIT 1")" "video|actionTransfer"
api DELETE "/action-videos/$AVID" > /dev/null
check "删除我的动作后不可见" "$(api GET /action-videos | jq "[.mine[]|select(.id==$AVID)]|length")" 0

echo "== 列表与删除"
check "全部 7 条" "$(api GET /image-creations | jq .total)" 7
check "按 kind 筛：海报 3 条" "$(api GET '/image-creations?kind=poster' | jq .total)" 3
check "按状态筛：已完成 6 条" "$(api GET '/image-creations?status=1' | jq .total)" 6
check "按 kind 筛：动作迁移 2 条" "$(api GET '/image-creations?kind=actionTransfer' | jq .total)" 2
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/image-creations/$PID" | jq -r .code)" 1003
api DELETE "/image-creations/$PID" > /dev/null
check "删除后不可见" "$(api_code GET "/image-creations/$PID")" 1003
check "删除时素材库同步删除" "$(sql "SELECT count(*) FROM afv_asset WHERE source_task_id='$GROUP' AND deleted=0")" 0
check "删除后 6 条" "$(api GET /image-creations | jq .total)" 6

echo "== 运营后台"
check "后台按用户查" "$(admin "/image-creations?user_id=$USER_ID" | jq .total)" 6
check "后台按 kind 查" "$(admin "/image-creations?user_id=$USER_ID&kind=longImage" | jq .total)" 2

echo "== 数字人口播：表单配置与预设背景"
VO=$(api GET '/image-creations/options?kind=voiceover')
check "模型 2 个，默认 seedance2.5" "$(jq -r '([.models[].label]|join(","))+"|"+.default_model' <<<"$VO")" "seedance 2.5,XQ Video 1.0 Lite|seedance2.5"
check "时长 15/30，默认 15" "$(jq -r '(.durations|map(tostring)|join(","))+"|"+(.default_duration|tostring)' <<<"$VO")" "15,30|15"
check "比例 9:16/16:9，默认 9:16" "$(jq -r '([.sizes[].code]|join(","))+"|"+.default_size' <<<"$VO")" "9:16,16:9|9:16"
check "文案不限字数（0）、一次一条" "$(jq -r '[.max_content_len,.max_images]|map(tostring)|join(",")' <<<"$VO")" 0,1
T_BG=$(template 卧室 voiceover "" image background)
check "预设背景可见" "$(api GET '/image-creations/templates?kind=voiceover' | jq "[.list[]|select(.id==$T_BG)]|length")" 1

echo "== 数字人口播：校验"
# 请求体一律先用 jq 拼好再传：macOS 自带 bash 3.2 会把 "$(… "{$VAR,…}" …)" 里的花括号做展开
VB=$(jq -nc --arg m "$M1" '{kind:"voiceover",portrait_source:"UPLOAD",image_url:$m}')
vb() { jq -c ". + $1" <<<"$VB"; }   # vb '{"content":"…"}' → 在上传人物图的基础上加字段
vo_code() { local b; b=$(vb "$1"); api_code POST /image-creations "$b"; }
check "没有文案被拒" "$(vo_code '{}')" 1001
check "时长非 15/30 被拒" "$(vo_code '{"content":"欢迎光临","duration":20}')" 1001
check "模型无效被拒" "$(vo_code '{"content":"欢迎光临","model":"bad"}')" 1001
check "比例无效被拒" "$(vo_code '{"content":"欢迎光临","size":"1:1"}')" 1001
check "没选人物图被拒" "$(api_code POST /image-creations '{"kind":"voiceover","content":"欢迎光临"}')" 1001
check "预设背景没给 id 被拒" "$(vo_code '{"content":"欢迎光临","background_source":"PRESET"}')" 1001
check "预设背景不存在被拒" "$(vo_code '{"content":"欢迎光临","background_source":"PRESET","background_id":999999999}')" 1001
check "背景外链被拒" "$(vo_code '{"content":"欢迎光临","background_source":"UPLOAD","background_url":"https://evil.example.com/a.png"}')" 1001
check "背景来源无效被拒" "$(vo_code '{"content":"欢迎光临","background_source":"BAD"}')" 1001
BOK=$(jq -c --arg c "$(printf '字%.0s' $(seq 1 2000))" '. + {content:$c, background_source:"AI"}' <<<"$VB")
check "文案不限字数（2000 字）、显式 AI智能匹配可提交" "$(api_code POST /image-creations "$BOK")" 0

echo "== 数字人口播：生成（mock 92 / 91）"
task_id() { sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(tasks,'\$[0].task_id')) FROM afv_image_creation WHERE id=$1"; }
task_body() { curl -sS "http://127.0.0.1:5055/mock/task/$(task_id "$1")"; }
RID=$(uuidgen)
B1=$(vb '{"content":"欢迎光临我们的小店，今天推荐一款洗面奶。"}')
V1=$(api POST /image-creations "$B1" "$RID")
V1ID=$(jq -r .id <<<"$V1")
check "默认值：15 秒 / 9:16 / seedance2.5 / AI智能匹配" "$(jq -r '[.duration,.size,.model,(.input_bg_url=="")]|map(tostring)|join(",")' <<<"$V1")" "15,9:16,seedance2.5,true"
V1B=$(api POST /image-creations "$B1" "$RID")
check "同一 X-Request-Id 不重复生成" "$(jq -r .id <<<"$V1B")" "$V1ID"
V1=$(poll "$V1ID")
check "生成完成，出 1 个视频" "$(jq -r '[.status,(.image_urls|length)]|map(tostring)|join(",")' <<<"$V1")" 1,1
B92=$(task_body "$V1ID")
check "92 入参：文案/人物图/时长/比例/分辨率/模型" "$(jq -c '[.task_type,.text,(.CHAR_01_url|length>0),.duration,.aspectRatio,.resolution,.model]' <<<"$B92")" '[92,"欢迎光临我们的小店，今天推荐一款洗面奶。",true,15,"9:16","720p","seedance2.5"]'
check "AI智能匹配不传 ENV_01_url" "$(jq -r 'has("ENV_01_url")' <<<"$B92")" false
B2=$(jq -nc --argjson p "$PUB" --argjson bg "$T_BG" '{kind:"voiceover",portrait_source:"PUBLIC",public_id:$p,content:"周末来店里坐坐",background_source:"PRESET",background_id:$bg,duration:30,size:"16:9",model:"lite"}')
V2=$(api POST /image-creations "$B2" "$(uuidgen)")
V2ID=$(jq -r .id <<<"$V2")
check "公共数字人作为人物图" "$(jq -r '.input_image_url|endswith("公共形象-'"$USER_ID"'.png")' <<<"$V2")" true
check "预设背景快照了地址" "$(jq -r '.input_bg_url|endswith("卧室-'"$USER_ID"'.png")' <<<"$V2")" true
poll "$V2ID" > /dev/null
B91=$(task_body "$V2ID")
check "预设背景作为 ENV_01_url，30 秒 16:9，model=lite" "$(jq -c '[(.ENV_01_url|endswith("卧室-'"$USER_ID"'.png")),.duration,.aspectRatio,.model]' <<<"$B91")" '[true,30,"16:9","lite"]'
check "lite 路由到 91" "$(curl -sS "http://127.0.0.1:5055/aigc/getRes/$(task_id "$V2ID")" | jq -r .task_type)" 91
BG=$(upload bg1)
B3=$(jq -c --arg bg "$BG" '. + {content:"新店开业", background_source:"UPLOAD", background_url:$bg}' <<<"$VB")
V3ID=$(api POST /image-creations "$B3" "$(uuidgen)" | jq -r .id)
poll "$V3ID" > /dev/null
check "导入背景作为 ENV_01_url" "$(task_body "$V3ID" | jq -r .ENV_01_url)" "$BG"
B4=$(vb '{"content":"mock-fail-92 模拟失败"}')
V4ID=$(api POST /image-creations "$B4" "$(uuidgen)" | jq -r .id)
check "执行面失败记失败" "$(poll "$V4ID" | jq -r '[.status,.fail_reason]|map(tostring)|join(",")')" "2,生成失败，请重试"
check "口播成品进素材库，封面为人物图" "$(sql "SELECT CONCAT_WS('|',type,entry_code,name,cover_url='$M1') FROM afv_asset WHERE user_id=$USER_ID AND entry_code='voiceover' AND source_task_id='$(task_id "$V1ID")'")" "video|voiceover|数字人口播|1"
B5=$(vb '{"content":"生成中就删除"}')
V5ID=$(api POST /image-creations "$B5" "$(uuidgen)" | jq -r .id)
api DELETE "/image-creations/$V5ID" > /dev/null
sleep 20   # mock 第 3 次轮询才出结果，轮询间隔 5 秒
check "生成中删除：不再轮询、成品不进素材库" "$(sql "SELECT CONCAT(c.status,'|',(SELECT COUNT(*) FROM afv_asset a WHERE a.user_id=$USER_ID AND a.source_task_id=JSON_UNQUOTE(JSON_EXTRACT(c.tasks,'\$[0].task_id')))) FROM afv_image_creation c WHERE c.id=$V5ID")" "0|0"
check "按 kind 筛：数字人口播 5 条（含 2000 字那条）" "$(api GET '/image-creations?kind=voiceover' | jq .total)" 5

if [[ ${SKIP_MODEL:-0} != 1 ]]; then
  echo "== AI帮写口播文案（真实调模型）"
  T=$(api POST /copywriting/ai-write '{"scene":"voiceover","context":"西湖边的咖啡店，主打手冲","max_len":60}')
  check "返回口播文案" "$(jq '.text|length > 0' <<<"$T")" true
else
  echo "== AI帮写口播文案（SKIP_MODEL=1，已跳过）"
fi

green "== 全部通过：$PASS 项"

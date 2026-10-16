#!/usr/bin/env bash
# AI图文笔记端到端测试（依赖 make run-all 已启动）
# 执行面（74 构思 / 75 出图）走本地 mock-aigc；AI帮写真实调用模型（SKIP_MODEL=1 可跳过）。
# 用法：tests/e2e/e2e-note.sh
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/api/v1/local-life}
ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 100000 ))}
PASS=0
TMP=$(mktemp -d)
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 "${E2E_DB:-fusion_aigc}" -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }
trap 'sql "DELETE FROM afv_digital_human WHERE user_id IN ($USER_ID, $((USER_ID+1)))"; sql "DELETE FROM afv_asset_item WHERE asset_id IN (SELECT id FROM (SELECT id FROM afv_asset WHERE user_id=$USER_ID) x); DELETE FROM afv_asset WHERE user_id=$USER_ID; DELETE FROM afv_workspace_tag WHERE workspace_id IN (SELECT id FROM (SELECT id FROM afv_workspace WHERE owner_user_id=$USER_ID) x); DELETE FROM afv_workspace_member WHERE user_id=$USER_ID; DELETE FROM afv_workspace WHERE owner_user_id=$USER_ID"; rm -rf "$TMP"' EXIT

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

api_code() { # → 业务 code（自动带新的 X-Request-Id）
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

poll() { # poll ID 进行中状态… → 终态 data
  local id=$1; shift
  for _ in $(seq 1 60); do
    local d; d=$(api GET "/notes/$id")
    [[ " $* " != *" $(jq -r .status <<<"$d") "* ]] && { echo "$d"; return; }
    sleep 1
  done
  red "timeout polling note $id" >&2; exit 1
}

upload() { # → 上传后的地址
  printf '\x89PNG\r\n\x1a\n' > "$TMP/$1.png"; head -c 1000 /dev/urandom >> "$TMP/$1.png"
  curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/$1.png" "$GW/upload" | jq -r .data.url
}

echo "== 用户 $USER_ID"

echo "== 表单配置"
O=$(api GET /notes/options)
check "笔记类型 4 个" "$(jq -r '[.types[].label]|join(",")' <<<"$O")" 探店,种草,情感,知识
check "探店上传区文案" "$(jq -r '.types[0].material_label' <<<"$O")" 上传门店素材
check "素材最多 3 张" "$(jq .max_materials <<<"$O")" 3
check "套图默认 4、编辑上限 6" "$(jq -r '[.default_images,.max_edit_images]|map(tostring)|join(",")' <<<"$O")" 4,6

echo "== 准备：素材与数字人"
M1=$(upload m1); M2=$(upload m2); M3=$(upload m3); M4=$(upload m4)
# 数字人直接写库（数字人流程见 e2e-digitalhuman.sh，这里只需要一个已完成 / 一个未完成）
DH=$(sql "INSERT INTO afv_digital_human (user_id,name,portrait_source,image_url,voice_id,script,status) VALUES ($USER_ID,'测试数字人','PUBLIC','https://mock-oss.example.com/dh/person-$USER_ID.png',1,'x',2); SELECT LAST_INSERT_ID();")
DH_BUSY=$(sql "INSERT INTO afv_digital_human (user_id,name,portrait_source,image_url,voice_id,script,status) VALUES ($USER_ID,'生成中','PUBLIC','https://mock-oss.example.com/dh/busy.png',1,'x',1); SELECT LAST_INSERT_ID();")
DH_OTHER=$(sql "INSERT INTO afv_digital_human (user_id,name,portrait_source,image_url,voice_id,script,status) VALUES ($((USER_ID+1)),'别人的','PUBLIC','https://mock-oss.example.com/dh/other.png',1,'x',2); SELECT LAST_INSERT_ID();")

echo "== 上传"
check "本地存储模式下直传凭证不可用（线上走直传）" "$(curl -sS -H "X-User-Id: $USER_ID" "$GW/upload/credentials?kind=image" | jq -r .code)" 1006
check "非图片被拒" "$(printf 'not an image' > "$TMP/x.txt"; curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/x.txt" "$GW/upload" | jq -r .code)" 1001

echo "== 开始构思：校验"
check "主题为空被拒" "$(api_code POST /notes/plan '{"note_type":"explore","topic":"  "}')" 1001
check "无效类型被拒" "$(api_code POST /notes/plan '{"note_type":"bad","topic":"x"}')" 1001
B="{\"note_type\":\"explore\",\"topic\":\"x\",\"material_urls\":[\"$M1\",\"$M2\",\"$M3\",\"$M4\"]}"
check "素材超 3 张被拒" "$(api_code POST /notes/plan "$B")" 1001
check "外链素材被拒" "$(api_code POST /notes/plan '{"note_type":"explore","topic":"x","material_urls":["https://evil.example.com/a.png"]}')" 1001
B="{\"note_type\":\"explore\",\"topic\":\"x\",\"digital_human_id\":$DH_BUSY}"
check "未完成的数字人被拒" "$(api_code POST /notes/plan "$B")" 1001
B="{\"note_type\":\"explore\",\"topic\":\"x\",\"digital_human_id\":$DH_OTHER}"
check "别人的数字人被拒" "$(api_code POST /notes/plan "$B")" 1001
check "缺少 request_id 被拒" "$(curl -sS -X POST -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -d '{"note_type":"explore","topic":"x"}' "$GW/notes/plan" | jq -r .code)" 1001

echo "== 构思（mock 74）→ 规划页"
RID=$(uuidgen)
SPID=$(api POST /shop-profiles '{"name":"临湖咖啡店","self_title":"店长小林","selling_points":"手冲单品、临湖景观位"}' | jq -r .id)
BODY="{\"note_type\":\"explore\",\"topic\":\"西湖边新开的临湖咖啡店\",\"material_urls\":[\"$M1\",\"$M2\",\"$M3\"],\"digital_human_id\":$DH,\"image_count\":5,\"shop_profile_id\":$SPID}"
N=$(api POST /notes/plan "$BODY" "$RID")
NID=$(jq -r .id <<<"$N")
check "构思中" "$(jq -r .status <<<"$N")" 0
check "幂等：同一 request_id 同一条" "$(api POST /notes/plan "$BODY" "$RID" | jq -r .id)" "$NID"
N=$(poll "$NID" 0)
check "待确认（规划页）" "$(jq -r .status <<<"$N")" 1
check "标题取 note.title" "$(jq -r .title <<<"$N")" "探店｜mock 标题"
check "正文取 note.content" "$(jq -r '.content|startswith("mock 正文：西湖边")' <<<"$N")" true
check "话题统一带 # 并去重" "$(jq -r '.tags|join(" ")' <<<"$N")" "#本地探店 #周末去哪儿"
check "套图数量不设上限：前端传 5 就是 5" "$(jq -r .image_count <<<"$N")" 5
check "74 按 5 张出方案" "$(sql "SELECT JSON_LENGTH(plan_script, '$.images') FROM afv_image_text_note WHERE id=$NID")" 5
check "类型名" "$(jq -r .note_type_label <<<"$N")" 探店
check "关联的店铺档案存下来了" "$(sql "SELECT shop_profile_id FROM afv_image_text_note WHERE id=$NID")" "$SPID"
T74=$(curl -sS "http://127.0.0.1:5055/mock/task/$(sql "SELECT plan_task_id FROM afv_image_text_note WHERE id=$NID")")
check "74 收到拼好的店铺档案" "$(jq -r '.shop_profile|contains("店长小林") and contains("临湖咖啡店")' <<<"$T74")" true
check "74 收到数字人形象 + 3 张素材" "$(sql "SELECT JSON_LENGTH(plan_script, '$.images_url') FROM afv_image_text_note WHERE id=$NID")" 4
check "74 的 person_url 是数字人形象图" "$(sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(plan_script, '$.images_url[0]')) FROM afv_image_text_note WHERE id=$NID")" "https://mock-oss.example.com/dh/person-$USER_ID.png"

echo "== 重新构思"
R=$(api POST "/notes/$NID/replan" '{"image_count":4}')
check "回到构思中" "$(jq -r .status <<<"$R")" 0
N=$(poll "$NID" 0)
check "再次待确认" "$(jq -r .status <<<"$N")" 1
check "重新构思可换套图数量" "$(sql "SELECT CONCAT(image_count, ',', JSON_LENGTH(plan_script, '$.images')) FROM afv_image_text_note WHERE id=$NID")" 4,4

echo "== 确认生成（mock 75）"
check "超过构思张数被拒（需重新构思）" "$(api_code POST "/notes/$NID/generate" '{"title":"t","content":"c","image_count":5}')" 1001
check "标题为空被拒" "$(api_code POST "/notes/$NID/generate" '{"title":" ","content":"c"}')" 1001
G=$(api POST "/notes/$NID/generate" '{"title":"西湖边宝藏咖啡店｜临湖景观位","content":"改过的正文","tags":["临湖咖啡","#西湖"],"image_count":2}')
GEN_TASK=$(sql "SELECT gen_task_id FROM afv_image_text_note WHERE id=$NID")
T75=$(curl -sS "http://127.0.0.1:5055/mock/task/$GEN_TASK")
check "75 参考图字段是 image_urls（按最新对接文档）" "$(jq -c '[has("image_urls"), has("images_url"), (.image_urls|length)]' <<<"$T75")" '[true,false,4]'
check "75 的 images 带 index、取前 2 条" "$(jq -c '[.images[].index]' <<<"$T75")" '[1,2]'
check "75 固定 model=max，size 不传（用默认）" "$(jq -c '[.model, has("size")]' <<<"$T75")" '["max",false]'
check "生成中" "$(jq -r .status <<<"$G")" 3
check "规划页的修改已保存" "$(jq -r '[.title,.content,(.tags|join(" ")),.image_count]|join("|")' <<<"$G")" "西湖边宝藏咖啡店｜临湖景观位|改过的正文|#临湖咖啡 #西湖|2"
check "重复点确认不重复提交" "$(api POST "/notes/$NID/generate" '{"title":"x","content":"y"}' | jq -r .title)" "西湖边宝藏咖啡店｜临湖景观位"
N=$(poll "$NID" 3)
check "已完成" "$(jq -r .status <<<"$N")" 4
check "按套图数量出 2 张" "$(jq '.image_urls|length' <<<"$N")" 2
check "已完成不能重新构思" "$(api_code POST "/notes/$NID/replan")" 1005

echo "== 编辑"
IMG1=$(jq -r '.image_urls[0]' <<<"$N")
NEW=$(upload new1)
U=$(api PUT "/notes/$NID" "{\"title\":\"编辑后的标题\",\"content\":\"编辑后的正文\",\"tags\":[\"#西湖\"],\"image_urls\":[\"$IMG1\",\"$NEW\"]}")
check "删一张原图、加一张新图" "$(jq -r '.image_urls|join(",")' <<<"$U")" "$IMG1,$NEW"
check "文字已改" "$(jq -r .title <<<"$U")" 编辑后的标题
# 产品 2026-09-24 把编辑上限定到 6 张（生成得更多时以生成张数为准）
B="{\"title\":\"t\",\"content\":\"c\",\"image_urls\":[\"$IMG1\",\"$NEW\",\"$M1\",\"$M2\"]}"
check "4 张在 6 张上限内" "$(api_code PUT "/notes/$NID" "$B")" 0
# 编辑会重写素材库，改回 2 张，别影响后面的素材库断言
api PUT "/notes/$NID" "{\"title\":\"编辑后的标题\",\"content\":\"编辑后的正文\",\"image_urls\":[\"$IMG1\",\"$NEW\"]}" > /dev/null
# 注意后端会去重，所以得用 7 个不同的地址
M5=$(upload m5); M6=$(upload m6); M7=$(upload m7)
B=$(jq -nc --args '{title:"t",content:"c",image_urls:$ARGS.positional}' "$IMG1" "$NEW" "$M1" "$M2" "$M5" "$M6" "$M7")
check "超过 6 张被拒" "$(api_code PUT "/notes/$NID" "$B")" 1001
B="{\"title\":\"t\",\"content\":\"c\",\"image_urls\":[\"https://evil.example.com/x.png\"]}"
check "外链图片被拒" "$(api_code PUT "/notes/$NID" "$B")" 1001
check "至少保留一张" "$(api_code PUT "/notes/$NID" '{"title":"t","content":"c","image_urls":[]}')" 1001

echo "== 生成张数多于上限时，编辑可保留全部"
B5=$(api POST /notes/plan "{\"note_type\":\"seeding\",\"topic\":\"五张图的种草笔记\",\"image_count\":5}" "$(uuidgen)")
B5ID=$(jq -r .id <<<"$B5")
poll "$B5ID" 0 > /dev/null
api POST "/notes/$B5ID/generate" > /dev/null
B5=$(poll "$B5ID" 3)
check "出 5 张" "$(jq '.image_urls|length' <<<"$B5")" 5
check "5 张全部保留可保存" "$(api PUT "/notes/$B5ID" "$(jq -c '{title, content, image_urls}' <<<"$B5")" | jq '.image_urls|length')" 5
# 生成 5 张、上限 6 张，所以加到 6 张还行，第 7 张才拒
check "加到 6 张还行" "$(api_code PUT "/notes/$B5ID" "$(jq -c --arg u "$M1" '{title, content, image_urls: (.image_urls + [$u])}' <<<"$B5")")" 0
check "不能再加第 7 张" "$(api_code PUT "/notes/$B5ID" "$(jq -c --arg a "$M1" --arg b "$M2" '{title, content, image_urls: (.image_urls + [$a, $b])}' <<<"$B5")")" 1001
api DELETE "/notes/$B5ID" > /dev/null

echo "== 失败与重试"
F=$(api POST /notes/plan '{"note_type":"seeding","topic":"这条构思会失败 mock-fail-74"}' "$(uuidgen)")
F=$(poll "$(jq -r .id <<<"$F")" 0)
check "构思失败有原因" "$(jq -r '[.status,.fail_reason]|join(",")' <<<"$F")" "2,构思失败，请重试"
check "构思失败不能生成" "$(api_code POST "/notes/$(jq -r .id <<<"$F")/generate")" 1005
G2=$(api POST /notes/plan '{"note_type":"knowledge","topic":"这条出图会失败 mock-fail-75"}' "$(uuidgen)")
G2ID=$(jq -r .id <<<"$G2")
poll "$G2ID" 0 > /dev/null
check "编辑前不能改" "$(api_code PUT "/notes/$G2ID" '{"title":"t","content":"c","image_urls":["x"]}')" 1005
api POST "/notes/$G2ID/generate" > /dev/null
G2=$(poll "$G2ID" 3)
check "出图失败有原因" "$(jq -r '[.status,.fail_reason]|join(",")' <<<"$G2")" "5,生成失败，请重试"
check "失败后可重新生成（沿用构思结果）" "$(api POST "/notes/$G2ID/generate" | jq -r .status)" 3
poll "$G2ID" 3 > /dev/null

echo "== 素材库（afv_asset）"
check "笔记成品每张图一行" "$(sql "SELECT count(*) FROM afv_asset WHERE source_task_id='$GEN_TASK' AND deleted=0")" 2
A=$(sql "SELECT CONCAT_WS('|', type, entry_code, name, source_type, (SELECT kind FROM afv_workspace w WHERE w.id=workspace_id AND w.owner_user_id=$USER_ID), JSON_UNQUOTE(JSON_EXTRACT(properties,'$.noteId'))) FROM afv_asset WHERE source_task_id='$GEN_TASK' AND deleted=0 LIMIT 1")
check "编辑后素材库图片同步" "$(sql "SELECT GROUP_CONCAT(result_url ORDER BY id) FROM afv_asset WHERE source_task_id='$GEN_TASK' AND deleted=0")" "$IMG1,$NEW"
check "type/entry_code/名称/来源/个人空间/笔记ID（编辑后已同步）" "$A" "image|imageTextNote|编辑后的标题|2|PERSONAL|$NID"
check "素材库分类已挂上入口" "$(sql "SELECT c.category_name FROM afv_asset_category_entry e JOIN afv_asset_category c ON c.id=e.category_id WHERE e.entry_code='imageTextNote'")" 图文笔记

echo "== 列表与删除"
check "列表 3 条" "$(api GET /notes | jq .total)" 3
check "按状态：已完成 1 条" "$(api GET '/notes?status=4' | jq .total)" 1
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/notes/$NID" | jq -r .code)" 1003
api DELETE "/notes/$NID" > /dev/null
check "删除笔记时素材库同步删除" "$(sql "SELECT count(*) FROM afv_asset WHERE source_task_id='$GEN_TASK' AND deleted=0")" 0
api DELETE "/notes/$G2ID" > /dev/null
check "删除后不可见" "$(api_code GET "/notes/$G2ID")" 1003
check "删除后 1 条" "$(api GET /notes | jq .total)" 1

# 模型不可用时（如账号欠费）可用 SKIP_MODEL=1 跳过这一段
if [[ ${SKIP_MODEL:-0} != 1 ]]; then
  echo "== AI帮写笔记主题（真实调模型）"
  T=$(api POST /copywriting/ai-write '{"scene":"note_topic","note_type":"探店","context":"西湖边的咖啡店"}')
  check "返回主题思路" "$(jq '.text|length > 0' <<<"$T")" true
else
  echo "== AI帮写笔记主题（SKIP_MODEL=1，已跳过）"
fi

echo "== 运营后台"
check "后台按用户查笔记" "$(admin "/notes?user_id=$USER_ID" | jq .total)" 1

green "== 全部通过：$PASS 项"

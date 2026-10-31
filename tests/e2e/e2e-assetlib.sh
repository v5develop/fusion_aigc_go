#!/usr/bin/env bash
# assetlib（资产库增强，/api/v1/asset-library）端到端测试：本地联调库，自己插测试资产、跑完清理。
#
# 前提：assetlib-rpc :9206、gateway-api :8888、admin-api :8889（make run-all），MySQL 已执行 Go_V1.0.23 / V1.0.24，
#       网关与后台 Auth.Mode=dev（认 X-User-Id / X-Admin-Id）。
# 做法：用 SQL 往 Java 的 afv_asset 插 3 行（当前用户 2 行：AI 生成图片 + 上传视频；另一个用户 1 行做越权），
#       列照 pkg/asset 的写法（workspace_id 用 0，不建工作空间）；trap 里删掉这些资产与它们的旁表数据。
#
# 用法：tests/e2e/e2e-assetlib.sh
# 环境变量：OWN_URL_BASE / GW_BASE / ADMIN_BASE / USER_ID / MYSQL_CONTAINER / E2E_DB
set -euo pipefail

HOST=${HOST:-127.0.0.1}
# 版本图片须是本存储签发的地址（assetlib Storage 默认 local：LocalBaseUrl/Prefix）；oss / 自定义时用 OWN_URL_BASE 覆盖
OWN=${OWN_URL_BASE:-http://127.0.0.1:8888/files/aigc-local-life}/e2e
GW_BASE=${GW_BASE:-http://$HOST:8888}
ADMIN_BASE=${ADMIN_BASE:-http://$HOST:8889}
GW=$GW_BASE/api/v1/asset-library
ADMIN=$ADMIN_BASE/api/v1/asset-library/admin
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 900000000 ))}
OTHER_ID=$((USER_ID + 1))
PASS=0
MYSQL_CONTAINER=${MYSQL_CONTAINER:-$(docker ps --format '{{.Names}}' 2>/dev/null | grep -m1 mysql || true)}
MYSQL=(docker exec -i "$MYSQL_CONTAINER" mysql --default-character-set=utf8mb4 -uroot -p123456 "${E2E_DB:-fusion_aigc}" -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

for p in "${GW_BASE##*:}" "${ADMIN_BASE##*:}"; do
  (exec 3<>"/dev/tcp/$HOST/$p") 2>/dev/null || { red "缺少服务：$HOST:$p 连不上（make run-all）"; exit 2; }
done
[[ -n $MYSQL_CONTAINER ]] || { red "找不到 MySQL 容器（MYSQL_CONTAINER）"; exit 2; }

call() { # call METHOD PATH [BODY] [UID] → 完整信封
  local args=(-sS -X "$1" -H "X-User-Id: ${4:-$USER_ID}" -H 'Content-Type: application/json')
  [[ -n ${3:-} ]] && args+=(-d "$3")
  curl "${args[@]}" "$GW$2"
}
code_of() { jq -r '.code // "none"' <<<"$1"; }
api() { # 成功才返回 data，否则退出
  local resp; resp=$(call "$@")
  if [[ $(code_of "$resp") != 0 ]]; then red "FAIL $1 $2 → $resp" >&2; exit 1; fi
  jq -c '.data' <<<"$resp"
}
admin() {
  local resp; resp=$(curl -sS -H 'X-Admin-Id: 1' "$ADMIN$1")
  if [[ $(code_of "$resp") != 0 ]]; then red "FAIL admin $1 → $resp" >&2; exit 1; fi
  jq -c '.data' <<<"$resp"
}
check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}
code_is() { # code_is 名称 响应 期望码
  local c; c=$(code_of "$2")
  if [[ $c == "$3" ]]; then green "  ✓ $1（code=${c}：$(jq -r '.msg' <<<"$2")）"; PASS=$((PASS+1))
  else red "  ✗ $1: want code=$3, got $2"; exit 1; fi
}

# ---- 测试数据（列照 pkg/asset：source_type 2=AI 生成 1=上传；type image / video）----
TAG="e2e-assetlib-$USER_ID"
cleanup() {
  local ids; ids=$(sql "SELECT GROUP_CONCAT(id) FROM afv_asset WHERE user_id IN ($USER_ID, $OTHER_ID) AND source_task_id = '$TAG'")
  if [[ -n $ids && $ids != NULL ]]; then
    sql "DELETE FROM afv_asset_tag_link WHERE asset_id IN ($ids);
         DELETE FROM afv_asset_relation WHERE asset_id IN ($ids) OR related_asset_id IN ($ids);
         DELETE FROM afv_asset_version WHERE asset_id IN ($ids);
         DELETE FROM afv_asset_ext WHERE asset_id IN ($ids);
         DELETE FROM afv_asset WHERE id IN ($ids);"
  fi
  sql "DELETE FROM afv_asset_tag_link WHERE tag_id IN (SELECT id FROM afv_asset_tag WHERE user_id IN ($USER_ID, $OTHER_ID));
       DELETE FROM afv_asset_tag WHERE user_id IN ($USER_ID, $OTHER_ID);"
}
trap cleanup EXIT
trap 'exit 130' INT TERM HUP PIPE   # 被信号打断也走 EXIT 清理

ins() { # ins 用户 type 名称 结果地址 source_type → 新 id
  sql "INSERT INTO afv_asset (user_id, workspace_id, source_task_id, type, entry_code, name, cover_url, result_url, aspect_ratio, ai_prompt, source_type, status)
       VALUES ($1, 0, '$TAG', '$2', '', '$3', 'https://e2e.example.com/$3.jpg', '$4', '1:1', 'e2e prompt $3', $5, 1); SELECT LAST_INSERT_ID();"
}
A=$(ins "$USER_ID" image 生成图A https://e2e.example.com/a.png 2)
B=$(ins "$USER_ID" video 上传视频B https://e2e.example.com/b.mp4 1)
X=$(ins "$OTHER_ID" image 别人的图X https://e2e.example.com/x.png 2)
echo "user=$USER_ID A=$A B=$B X(别人)=$X"

echo "-- 列表与分类"
d=$(api GET "/assets")
check "列表 total=2（只看到自己的）" "$(jq .total <<<"$d")" 2
check "新的在前、字段齐全" "$(jq -r '[.items[0].id, .items[0].media, .items[0].source, .items[0].video_url != ""] | @tsv' <<<"$d")" "$B	video	upload	true"
check "source=generated → A" "$(api GET "/assets?source=generated" | jq -c '[.items[].id]')" "[$A]"
check "source=input（official+upload）→ B" "$(api GET "/assets?source=input" | jq -c '[.items[].id]')" "[$B]"
check "media=image → A" "$(api GET "/assets?media=image" | jq -c '[.items[].id]')" "[$A]"
check "q=上传 → B" "$(api GET "/assets?q=%E4%B8%8A%E4%BC%A0" | jq -c '[.items[].id]')" "[$B]"
d=$(api GET "/assets/categories")
check "分类：all=2 uncategorized=2" "$(jq -c '[.items[] | select(.key=="all" or .key=="uncategorized") | .count]' <<<"$d")" "[2,2]"
code_is "非法 kind 筛选被拒" "$(call GET "/assets?kind=shoes")" 1003

echo "-- 修改分类 / 收藏 / 标题"
d=$(api PATCH "/assets/$A" '{"kind":"model","favorite":true,"metadata":{"style":"日系","industry":"服装"},"license":"internal"}')
check "PATCH 返回新状态" "$(jq -r '[.kind, .favorite, .metadata.style, .license] | @tsv' <<<"$d")" "model	true	日系	internal"
d=$(api GET "/assets/categories")
check "分类：model=1 uncategorized=1" "$(jq -c '[.items[] | select(.key=="model" or .key=="uncategorized") | .count]' <<<"$d")" "[1,1]"
check "分类计数与 List(kind=model) 一致" "$(api GET "/assets?kind=model" | jq .total)" "$(jq '.items[] | select(.key=="model") | .count' <<<"$d")"
check "kind=uncategorized → B" "$(api GET "/assets?kind=uncategorized" | jq -c '[.items[].id]')" "[$B]"
check "favorite_only → A" "$(api GET "/assets?favorite_only=true" | jq -c '[.items[].id]')" "[$A]"
check "分类计数随 source：generated 下 model=1" "$(api GET "/assets/categories?source=generated" | jq '.items[] | select(.key=="model") | .count')" 1
api PATCH "/assets/$A" '{"title":"改过的标题"}' >/dev/null
check "标题写回 Java afv_asset.name" "$(sql "SELECT name FROM afv_asset WHERE id=$A")" "改过的标题"
api PATCH "/assets/$A" '{"kind":""}' >/dev/null
check "kind 置空 = 改回未分类（favorite 不受影响）" "$(api GET "/assets/$A" | jq -r '[.asset.kind, .asset.favorite] | @tsv')" "	true"
api PATCH "/assets/$A" '{"kind":"model"}' >/dev/null
code_is "非法 kind 修改被拒" "$(call PATCH "/assets/$A" '{"kind":"shoes"}')" 1003
code_is "标题超 100 字被拒" "$(call PATCH "/assets/$A" "{\"title\":\"$(printf '字%.0s' $(seq 1 101))\"}")" 1003

echo "-- 标签"
t1=$(api POST "/tags" '{"name":"夏季"}' | jq .id)
code_is "同名标签 → 冲突" "$(call POST "/tags" '{"name":"夏季"}')" 4002
code_is "标签名超 20 字被拒" "$(call POST "/tags" "{\"name\":\"$(printf '字%.0s' $(seq 1 21))\"}")" 1003
d=$(api PATCH "/assets/$A" '{"tags":["夏季","新标签"]}')
check "PATCH tags 整组替换（字典没有的自动建）" "$(jq -c .tags <<<"$d")" '["夏季","新标签"]'
check "tag 筛选 → A" "$(api GET "/assets?tag=%E5%A4%8F%E5%AD%A3" | jq -c '[.items[].id]')" "[$A]"
body="{\"asset_ids\":[$A,$B],\"tag_ids\":[$t1]}"   # 含逗号的 JSON 先放变量：bash 3.2 在 "$(…)" 里会对它做花括号展开
check "批量打标签：B 加 夏季" "$(api POST "/assets/batch-tag" "$body" | jq .added)" 1
check "标签字典含资产数" "$(api GET "/tags" | jq -c '[.items[] | {name, asset_count}]')" '[{"name":"夏季","asset_count":2},{"name":"新标签","asset_count":1}]'
api PUT "/tags/$t1" '{"name":"盛夏"}' >/dev/null
check "改名后资产上的标签跟着变" "$(api GET "/assets/$B" | jq -c .asset.tags)" '["盛夏"]'
api DELETE "/tags/$t1" >/dev/null
check "删标签后解除关联" "$(api GET "/assets/$A" | jq -c .asset.tags)" '["新标签"]'
code_is "每资产标签 ≤10" "$(call PATCH "/assets/$A" '{"tags":["1","2","3","4","5","6","7","8","9","10","11"]}')" 1003

echo "-- 版本"
v1=$(api POST "/assets/$A/versions" "{\"image_url\":\"$OWN/a-v1.png\",\"prompt\":\"v1\",\"model\":\"GPT\",\"params\":\"{\\\"seed\\\":1}\"}")
v2=$(api POST "/assets/$A/versions" "{\"image_url\":\"$OWN/a-v2.png\",\"prompt\":\"v2\",\"model\":\"Banana\"}")
check "版本号 MAX+1" "$(jq .version_no <<<"$v1"),$(jq .version_no <<<"$v2")" "1,2"
d=$(api GET "/assets/$A")
check "详情：版本 2 个、新的在前、当前版本=v2、展示图=v2" \
  "$(jq -r '[.versions.total, .versions.items[0].version_no, .asset.current_version_id == '"$(jq .id <<<"$v2")"', .asset.image] | @tsv' <<<"$d")" \
  "2	2	true	$OWN/a-v2.png"
api PATCH "/assets/$A" "{\"current_version_id\":$(jq .id <<<"$v1")}" >/dev/null
check "切回 v1" "$(api GET "/assets/$A" | jq -r .asset.image)" "$OWN/a-v1.png"
api PATCH "/assets/$A" '{"current_version_id":0}' >/dev/null
check "current_version_id=0 回到原图" "$(api GET "/assets/$A" | jq -r .asset.image)" "https://e2e.example.com/a.png"
code_is "别的资产的版本不能设为当前" "$(call PATCH "/assets/$B" "{\"current_version_id\":$(jq .id <<<"$v1")}")" 1003
code_is "版本图片必须是本服务上传的地址（外链被拒）" "$(call POST "/assets/$A/versions" '{"image_url":"https://e2e.example.com/ext.png"}')" 1003
code_is "版本图片必须 http(s)" "$(call POST "/assets/$A/versions" '{"image_url":"blob:abc"}')" 1003

echo "-- 关系"
api POST "/assets/$A/relations" "{\"related_asset_id\":$B,\"relation_type\":\"reference\"}" >/dev/null
check "reference 双向：A 的相关=B" "$(api GET "/assets/$A" | jq -c '[.related.total, .asset.related_ids]')" "[1,[$B]]"
check "reference 双向：B 的相关=A" "$(api GET "/assets/$B" | jq -c '[.related.items[].id]')" "[$A]"
api POST "/assets/$B/relations" "{\"related_asset_id\":$A,\"relation_type\":\"generated_from\"}" >/dev/null
check "B generated_from A → A 的生成结果=B" "$(api GET "/assets/$A" | jq -c '[.generated_results.total, [.generated_results.items[].id]]')" "[1,[$B]]"
api DELETE "/assets/$B/relations?related_asset_id=$A&relation_type=reference" >/dev/null
check "从另一端删 reference 也生效" "$(api GET "/assets/$A" | jq .related.total)" 0
code_is "删不存在的关系 → 4001" "$(call DELETE "/assets/$B/relations?related_asset_id=$A&relation_type=reference")" 4001

echo "-- 越权一律按不存在"
code_is "详情：别人的资产" "$(call GET "/assets/$X")" 4001
code_is "修改：别人的资产" "$(call PATCH "/assets/$X" '{"favorite":true}')" 4001
code_is "追加版本：别人的资产" "$(call POST "/assets/$X/versions" '{"image_url":"https://e2e.example.com/z.png"}')" 4001
body="{\"related_asset_id\":$X,\"relation_type\":\"reference\"}"
code_is "关系：关联到别人的资产" "$(call POST "/assets/$A/relations" "$body")" 4001
body="{\"asset_ids\":[$A,$X],\"tag_ids\":[1]}"
code_is "批量打标签：含别人的资产" "$(call POST "/assets/batch-tag" "$body")" 4001
code_is "别人看我的资产" "$(call GET "/assets/$A" '' "$OTHER_ID")" 4001
check "别人的旁表没被写" "$(sql "SELECT COUNT(*) FROM afv_asset_ext WHERE asset_id=$X")" 0
code_is "不存在的 id" "$(call GET "/assets/999999999999")" 4001

echo "-- 后台"
check "后台标签字典（按用户）" "$(admin "/tags?user_id=$USER_ID" | jq -c '[.total, [.list[].name]]')" '[1,["新标签"]]'
check "后台分类统计（按用户）" "$(admin "/kind-stats?user_id=$USER_ID" | jq -c '[.rows[] | [.kind, .source, .count]] | sort')" '[["model","generated",1],["uncategorized","upload",1]]'

green "全部通过：$PASS 项"

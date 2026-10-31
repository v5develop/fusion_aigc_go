#!/usr/bin/env bash
# assetlib（资产库增强）对**已部署环境**的验收：用该用户在 Java 素材库里已有的真实资产跑一遍，不插测试行。
#
#   GW=https://<域名> TOKEN=<用户 Bearer 令牌> USER_ID=<该用户 id> tests/e2e/e2e-real-assetlib.sh
#
# 环境变量：
#   GW              网关根地址（经 nginx，脚本自己拼 /api/v1/asset-library）
#   TOKEN           用户令牌（与 Java 同一套，Authorization: Bearer）；联调环境可不给、改用 USER_ID 走 X-User-Id
#   USER_ID         仅用于日志与 X-User-Id 兜底
#   ASSET_ID        指定拿哪条资产做修改类用例；默认取列表第一条
#   OTHER_ASSET_ID  一条别人的资产 id（验证越权按不存在）；默认用一个不存在的超大 id
#   MYSQL_CMD       可选，如 "mysql -h127.0.0.1 -uroot -pxxx fusion_aigc"：给了就在收尾删掉本脚本追加的版本行
#
# 只改本脚本改过的东西，并在 trap 里还原：
#   - kind / favorite / current_version_id：跑前记下原值，收尾 PATCH 回去（原来没有旁表行的，会留下一行与「未分类、未收藏、原图」等价的旁表行）
#   - 标签：新建一个 e2e 专用标签，挂上再删（删标签会解除关联），不碰资产原有标签
#   - 版本：接口没有删版本，追加的版本行会留下（图片用资产自己的展示图，当前版本收尾时还原）；给了 MYSQL_CMD 则一并删掉
#   - 标题（写 Java afv_asset.name）不测
set -euo pipefail

GW=${GW:?需要 GW（网关根地址）}
API=$GW/api/v1/asset-library
TOKEN=${TOKEN:-}
USER_ID=${USER_ID:-}
OTHER_ASSET_ID=${OTHER_ASSET_ID:-999999999999}
MYSQL_CMD=${MYSQL_CMD:-}
PASS=0
[[ -n $TOKEN || -n $USER_ID ]] || { echo "需要 TOKEN（或联调环境的 USER_ID）"; exit 2; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[33m%s\033[0m\n' "$*"; }

call() { # call METHOD PATH [BODY]
  local args=(-sS -m 20 -X "$1" -H 'Content-Type: application/json')
  if [[ -n $TOKEN ]]; then args+=(-H "Authorization: Bearer $TOKEN"); else args+=(-H "X-User-Id: $USER_ID"); fi
  [[ -n ${3:-} ]] && args+=(-d "$3")
  curl "${args[@]}" "$API$2"
}
code_of() { jq -r '.code // "none"' <<<"$1" 2>/dev/null || echo "not-json"; }
api() {
  local resp; resp=$(call "$@")
  if [[ $(code_of "$resp") != 0 ]]; then red "FAIL $1 $2 → $resp" >&2; exit 1; fi
  jq -c '.data' <<<"$resp"
}
check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}
code_is() {
  local c; c=$(code_of "$2")
  if [[ $c == "$3" ]]; then green "  ✓ $1（code=${c}）"; PASS=$((PASS+1)); else red "  ✗ $1: want code=$3, got $2"; exit 1; fi
}

# ---- 0. 连通与路由：必须是 Go 网关的信封（nginx 没加 location 时会落到 Java，返回 Java 的 404 信封）----
first=$(call GET "/assets?page=1&page_size=20")
if [[ $(code_of "$first") != 0 ]] || ! jq -e '.data | has("items") and has("total")' <<<"$first" >/dev/null; then
  red "列表失败或不是 assetlib 的响应形状（检查 nginx 的 ^~ /api/v1/asset-library location 与 assetlib-rpc）：$first"; exit 1
fi
TOTAL=$(jq '.data.total' <<<"$first")
echo "用户 ${USER_ID:-（令牌）} 共 $TOTAL 条资产"
(( TOTAL > 0 )) || { yellow "该用户没有资产，换一个有素材的账号"; exit 2; }

echo "-- 1. 列表"
check "列表每页条数 ≤ page_size" "$(jq '.data.items | length <= 20' <<<"$first")" true
check "每条都有 id / image / media / source / created_at" \
  "$(jq '[.data.items[] | (.id > 0) and (.media == "image" or .media == "video") and (.source | IN("upload","generated","official")) and (.created_at > 0)] | all' <<<"$first")" true
check "按 created_at 倒序" "$(jq '[.data.items[].created_at] | . == (sort | reverse)' <<<"$first")" true

echo "-- 2. 分类计数与列表过滤一致"
for src in "" generated input; do
  cats=$(api GET "/assets/categories?source=$src")
  for key in all model scene makeup hair accessory uncategorized; do
    want=$(jq ".items[] | select(.key==\"$key\") | .count" <<<"$cats")
    q="source=$src&page_size=1"; [[ $key != all ]] && q="$q&kind=$key"
    check "source=${src:-全部} ${key}：分类 ${want} = 列表 total" "$(api GET "/assets?$q" | jq .total)" "$want"
  done
done
for m in image video; do
  cats=$(api GET "/assets/categories?media=$m")
  check "media=${m}：分类 all = 列表 total" "$(api GET "/assets?media=$m&page_size=1" | jq .total)" "$(jq '.items[] | select(.key=="all") | .count' <<<"$cats")"
done

# ---- 选一条资产，记原值 ----
ASSET_ID=${ASSET_ID:-$(jq '.data.items[0].id' <<<"$first")}
orig=$(api GET "/assets/$ASSET_ID")
O_KIND=$(jq -r '.asset.kind' <<<"$orig")
O_FAV=$(jq -r '.asset.favorite' <<<"$orig")
O_CUR=$(jq -r '.asset.current_version_id' <<<"$orig")
O_IMAGE=$(jq -r '.asset.image' <<<"$orig")
echo "用资产 $ASSET_ID 做修改类用例（原 kind=${O_KIND:-空} favorite=$O_FAV current_version_id=${O_CUR}）"
TAG_ID=""; VER_ID=""
restore() {
  set +e
  echo "-- 还原"
  local body="{\"kind\":\"$O_KIND\",\"favorite\":$O_FAV,\"current_version_id\":$O_CUR}"
  local r; r=$(call PATCH "/assets/$ASSET_ID" "$body")
  [[ $(code_of "$r") == 0 ]] && echo "  已还原 kind / favorite / current_version_id" || red "  还原失败，请手工处理：PATCH /assets/$ASSET_ID $body → $r"
  if [[ -n $TAG_ID ]]; then call DELETE "/tags/$TAG_ID" >/dev/null && echo "  已删除 e2e 标签 $TAG_ID"; fi
  if [[ -n $VER_ID ]]; then
    if [[ -n $MYSQL_CMD ]]; then
      $MYSQL_CMD -e "DELETE FROM afv_asset_version WHERE id = $VER_ID AND asset_id = $ASSET_ID" && echo "  已删除追加的版本 $VER_ID"
    else
      yellow "  追加的版本 $VER_ID 留在 afv_asset_version（接口不提供删除；要删请给 MYSQL_CMD 或手工 DELETE ... WHERE id=${VER_ID}）"
    fi
  fi
}
trap restore EXIT
trap 'exit 130' INT TERM HUP PIPE

echo "-- 3. 改分类 / 收藏"
NEW_KIND=scene; [[ $O_KIND == scene ]] && NEW_KIND=hair
NEW_FAV=true; [[ $O_FAV == true ]] && NEW_FAV=false
body="{\"kind\":\"$NEW_KIND\",\"favorite\":$NEW_FAV}"
d=$(api PATCH "/assets/$ASSET_ID" "$body")
check "PATCH 返回新 kind / favorite" "$(jq -r '[.kind, .favorite] | @tsv' <<<"$d")" "$NEW_KIND	$NEW_FAV"
check "按新 kind 能筛到它" "$(api GET "/assets?kind=$NEW_KIND&page_size=100" | jq "[.items[].id] | index($ASSET_ID) != null")" true
cats=$(api GET "/assets/categories")
check "改后分类计数仍与列表一致（${NEW_KIND}）" "$(api GET "/assets?kind=$NEW_KIND&page_size=1" | jq .total)" "$(jq ".items[] | select(.key==\"$NEW_KIND\") | .count" <<<"$cats")"
code_is "非法 kind 被拒" "$(call PATCH "/assets/$ASSET_ID" '{"kind":"shoes"}')" 1003

echo "-- 4. 详情"
d=$(api GET "/assets/$ASSET_ID")
check "详情含 versions / related / generated_results 三组分页" "$(jq '[.versions, .related, .generated_results] | all(has("items") and has("total"))' <<<"$d")" true
check "详情的资产与列表同一条" "$(jq -r '[.asset.id, .asset.kind] | @tsv' <<<"$d")" "$ASSET_ID	$NEW_KIND"

echo "-- 5. 标签增删（e2e 专用标签，不动原有标签）"
TAG_NAME="e2e$(date +%H%M%S)"
TAG_ID=$(api POST "/tags" "{\"name\":\"$TAG_NAME\"}" | jq .id)
check "新建标签出现在字典" "$(api GET "/tags" | jq "[.items[].id] | index($TAG_ID) != null")" true
body="{\"asset_ids\":[$ASSET_ID],\"tag_ids\":[$TAG_ID]}"
r=$(call POST "/assets/batch-tag" "$body")
if [[ $(code_of "$r") == 1003 ]]; then
  yellow "  SKIP 挂标签：$(jq -r .msg <<<"$r")（资产标签已满）"
else
  check "挂上标签" "$(jq .data.added <<<"$r")" 1
  check "按标签筛到它" "$(api GET "/assets?tag=$TAG_NAME" | jq -c '[.items[].id]')" "[$ASSET_ID]"
fi
api DELETE "/tags/$TAG_ID" >/dev/null
check "删标签后资产上不再有它" "$(api GET "/assets/$ASSET_ID" | jq --arg n "$TAG_NAME" '.asset.tags | index($n) == null')" true
TAG_ID=""

echo "-- 6. 追加版本"
before=$(jq '.versions.total' <<<"$d")
body="{\"image_url\":\"$O_IMAGE\",\"prompt\":\"e2e-real-assetlib\",\"model\":\"e2e\"}"
v=$(api POST "/assets/$ASSET_ID/versions" "$body")
VER_ID=$(jq .id <<<"$v")
check "版本号 = 原版本数 + 1" "$(jq .version_no <<<"$v")" "$((before + 1))"
check "追加后成为当前版本" "$(api GET "/assets/$ASSET_ID" | jq .asset.current_version_id)" "$VER_ID"

echo "-- 7. 越权 / 不存在"
code_is "别人的（或不存在的）资产详情 → 4001" "$(call GET "/assets/$OTHER_ASSET_ID")" 4001
code_is "修改别人的资产 → 4001" "$(call PATCH "/assets/$OTHER_ASSET_ID" '{"favorite":true}')" 4001
code_is "给别人的资产追加版本 → 4001" "$(call POST "/assets/$OTHER_ASSET_ID/versions" "{\"image_url\":\"$O_IMAGE\"}")" 4001

green "全部通过：$PASS 项（收尾还原见下）"

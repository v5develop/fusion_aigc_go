#!/usr/bin/env bash
# 造联调用的公共素材：公共数字人、海报模板、长图模板、口播背景、预置动作视频。
#
# 这些不是我们的表——它们在现有系统的「模板素材」里（afv_public_template_assets +
# afv_public_template_asset_placement），运营平时在现有后台的模板页面维护。
# 我们只读、按投放位（biz_type + module_code + slot_code）取上架中的。
#
# ⚠️ 造出来的图片/视频是**占位素材**（用的是公有桶里已有的示例文件），只为让前端有东西可点。
#    上线前要运营在现有后台把 url 换成真实素材。
#
# 用法（在服务器上，install.sh 装过之后）：
#   bash install.sh seed-templates          # 推荐：自动读 deploy.env 里的数据库配置
#   bash seed-templates.sh                  # 或者自己 export DB_HOST/DB_PORT/DB_NAME/DB_USER/DB_PASS
#
# 可重复执行：按标题判断，已存在的跳过，不会造重复。
# 想换占位素材：export SEED_IMAGE_URL=... SEED_VIDEO_URL=...
set -euo pipefail

BIZ=${SEED_BIZ_TYPE:-localLife}
IMG=${SEED_IMAGE_URL:-https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/images/20260915/9db717232a3645a79f006d92260dc7d4.png}
VID=${SEED_VIDEO_URL:-https://xx-shop-img.oss-cn-hangzhou.aliyuncs.com/videos/20260915/e8b22c6d21654a41aec8bca97065921c.mp4}

DB_HOST=${DB_HOST:-127.0.0.1}; DB_PORT=${DB_PORT:-3306}
DB_NAME=${DB_NAME:-fusion_aigc}; DB_USER=${DB_USER:-root}; DB_PASS=${DB_PASS:-}

red()   { printf '\033[31m%s\033[0m\n' "$*" >&2; }   # 写 stderr：否则在 $( ) 里会被吞掉
green() { printf '\033[32m%s\033[0m\n' "$*"; }
gray()  { printf '\033[90m%s\033[0m\n' "$*"; }

# mysql 客户端：优先用 MYSQL_BIN，其次 PATH，最后宝塔的常见位置
find_mysql() {
  [[ -n ${MYSQL_BIN:-} && -x ${MYSQL_BIN} ]] && { echo "$MYSQL_BIN"; return; }
  command -v mysql >/dev/null 2>&1 && { command -v mysql; return; }
  for p in /www/server/mysql/bin/mysql /usr/local/mysql/bin/mysql; do
    [[ -x $p ]] && { echo "$p"; return; }
  done
  red "✗ 找不到 mysql 客户端，请 export MYSQL_BIN=/path/to/mysql"; exit 1
}
MYSQL=$(find_mysql)
# --default-character-set=utf8mb4 必须显式带上：客户端默认字符集不对时中文会写成乱码
q() { "$MYSQL" --default-character-set=utf8mb4 -h"$DB_HOST" -P"$DB_PORT" -u"$DB_USER" \
        ${DB_PASS:+-p"$DB_PASS"} "$DB_NAME" -N -e "$1" 2>/dev/null; }

q "SELECT 1" >/dev/null || { red "✗ 连不上数据库 $DB_USER@$DB_HOST:$DB_PORT/$DB_NAME"; exit 1; }

# seed 造一条素材 + 一个投放位。已存在（同 biz_type + 同标题）就跳过。
#   seed <module> <slot> <type> <排序> <标题> <url> <extra-json>
# assetID 按 biz_type + 标题回查。不用 LAST_INSERT_ID()——每次调 q 都是一条新连接，取不到。
assetID() { q "SELECT id FROM afv_public_template_assets
               WHERE biz_type='$BIZ' AND title='$1' AND deleted=0 ORDER BY id LIMIT 1"; }

# uniqueURL 让每条素材的 url 不一样：表上有唯一键 (biz_type, url_hash, deleted)，
# 同一个 biz_type 下同一个 URL 只能有一条。占位素材都指向同一个文件，所以挂个查询参数区分
# （OSS 公有读会忽略未知参数，图照样能打开——验证过）。
uniqueURL() {
  local url=$1 tag=$2
  case $url in
    *\?*) echo "$url&seed=$tag" ;;
    *)    echo "$url?seed=$tag" ;;
  esac
}

seed() {
  local module=$1 slot=$2 type=$3 sort=$4 title=$5 url extra=${7:-NULL}
  url=$(uniqueURL "$6" "$module-$sort")
  local id
  id=$(assetID "$title")
  if [[ -z $id ]]; then
    q "INSERT INTO afv_public_template_assets (biz_type,type,title,url,extra)
       VALUES ('$BIZ','$type','$title','$url',$extra)"
    id=$(assetID "$title")
    [[ -n $id ]] || { red "✗ 素材写入失败：$title"; return 1; }
    gray "  + 素材 $id  $title"
  else
    gray "  · 素材 $id  $title（已存在）"
  fi
  # 投放位：同一素材在同一个位置只放一次
  if [[ -z $(q "SELECT id FROM afv_public_template_asset_placement
                WHERE biz_type='$BIZ' AND module_code='$module' AND slot_code='$slot'
                  AND asset_id=$id AND deleted=0 LIMIT 1") ]]; then
    q "INSERT INTO afv_public_template_asset_placement
         (biz_type,module_code,slot_code,asset_id,sort_order,status)
       VALUES ('$BIZ','$module','$slot',$id,$sort,1)"
  fi
}

echo "== 公共数字人（digitalHuman / publicModel）"
# extra.role 是职业标签，extra.avatarUrl 是头像（可空，空则前端用形象图）
seed digitalHuman publicModel image 10 '公共数字人-店长小林'   "$IMG" "JSON_OBJECT('role','店长','avatarUrl','$IMG')"
seed digitalHuman publicModel image 20 '公共数字人-探店达人阿杰' "$IMG" "JSON_OBJECT('role','探店达人','avatarUrl','$IMG')"
seed digitalHuman publicModel image 30 '公共数字人-白领小雅'   "$IMG" "JSON_OBJECT('role','白领','avatarUrl','$IMG')"
seed digitalHuman publicModel image 40 '公共数字人-学生小北'   "$IMG" "JSON_OBJECT('role','学生','avatarUrl','$IMG')"

echo "== 海报模板（poster / template）"
seed poster template image 10 '海报模板-促销大字报' "$IMG"
seed poster template image 20 '海报模板-新店开业' "$IMG"
seed poster template image 30 '海报模板-节日祝福' "$IMG"
seed poster template image 40 '海报模板-菜品推荐' "$IMG"

echo "== 长图模板（longImage / template）"
seed longImage template image 10 '长图模板-探店攻略' "$IMG"
seed longImage template image 20 '长图模板-产品介绍' "$IMG"
seed longImage template image 30 '长图模板-活动预告' "$IMG"

echo "== 口播背景（voiceover / background）"
seed voiceover background image 10 '口播背景-纯色浅灰' "$IMG"
seed voiceover background image 20 '口播背景-咖啡店内景' "$IMG"
seed voiceover background image 30 '口播背景-城市街景' "$IMG"

echo "== 预置动作视频（actionTransfer / actionVideo）"
seed actionTransfer actionVideo video 10 '动作-挥手打招呼' "$VID"
seed actionTransfer actionVideo video 20 '动作-指向商品' "$VID"

echo
green "== 造好了。各投放位当前上架数量："
q "SELECT CONCAT('  ', p.module_code, ' / ', p.slot_code, '  ', COUNT(*), ' 条')
   FROM afv_public_template_asset_placement p
   WHERE p.biz_type='$BIZ' AND p.status=1 AND p.deleted=0
   GROUP BY p.module_code, p.slot_code ORDER BY p.module_code"
echo
gray "占位素材而已，上线前让运营在现有后台的「模板素材」页面把 url 换成真实图片/视频。"
gray "前端马上能看到数据的接口："
gray "  GET /api/v1/digital-humans/public                      公共数字人"
gray "  GET /api/v1/image-creations/templates?kind=poster      海报模板"
gray "  GET /api/v1/image-creations/templates?kind=longImage   长图模板"
gray "  GET /api/v1/image-creations/templates?kind=voiceover   口播预设背景"
gray "  GET /api/v1/action-videos                              预置动作视频"

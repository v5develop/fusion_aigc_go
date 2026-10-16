#!/usr/bin/env bash
# AI数字人端到端测试（依赖 make run-all 已启动）
# 执行面（75 出图 / 85 视频）走本地 mock-aigc；配音走真实百炼 TTS（会产生少量调用费用）。
# AI帮写那段真实调 LLM，SKIP_MODEL=1 可跳过。
# 用法：deploy/scripts/e2e-digitalhuman.sh
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/lf-api/v1}
ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 100000 ))}
PASS=0
TMP=$(mktemp -d)

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

api_code() { # → 业务 code（自动带新的 X-Request-Id）
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

poll() { # poll PATH → 终态 data（status 不在 $2 列表里即为终态）
  local path=$1 running=$2
  for _ in $(seq 1 60); do
    local d; d=$(api GET "$path")
    [[ " $running " != *" $(jq -r .status <<<"$d") "* ]] && { echo "$d"; return; }
    sleep 2
  done
  red "timeout polling $path"; exit 1
}

echo "== 用户 $USER_ID"

echo "== 使用约定"
check "初始未同意" "$(api GET /agreements/aigc | jq -r .accepted)" false
check "未同意不能定制形象" "$(api_code POST /digital-humans/portraits '{"gender":"female","age":"youth","skin_tone":"natural"}')" 1007
check "同意" "$(api POST /agreements/aigc/accept | jq -r .accepted)" true
check "重复同意不报错" "$(api POST /agreements/aigc/accept | jq -r .accepted)" true
check "已同意" "$(api GET /agreements/aigc | jq -r .accepted)" true

echo "== 选项与音色"
O=$(api GET /digital-humans/options)
check "性别 2 个" "$(jq '.genders|length' <<<"$O")" 2
check "年龄 3 个" "$(jq '.ages|length' <<<"$O")" 3
check "肤色 4 个带色值" "$(jq '[.skin_tones[]|select(.value|startswith("#"))]|length' <<<"$O")" 4
check "职业 10 个" "$(jq '.roles|length' <<<"$O")" 10
check "配音上限 50 字" "$(jq '.max_script_len' <<<"$O")" 50
check "出片模型 2 档，默认云端" "$(jq -r '"\(.models|length)/\(.models[0].code)"' <<<"$O")" 2/seedance2.5
C=$(api GET /voices/categories)
check "音色分类 7 个（方言/外语停用）" "$(jq '.list|length' <<<"$C")" 7
check "启用音色 22 个（6 个账号不可用的已停用）" "$(api GET /voices | jq '.list|length')" 22
check "男声 3 个" "$(api GET '/voices?category=male' | jq '.list|length')" 3
check "App 端不暴露音色参数" "$(api GET /voices | jq '[.list[]|has("voice")]|any')" false
VOICE_ID=$(api GET '/voices?category=selling' | jq -r '.list[0].id')

echo "== 公共数字人（复用现有模板素材表，本地测试直接插数据，结束时清理）"
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 "${E2E_DB:-fusion_aigc}" -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }
public_model() { # public_model 名字 职业 图片 上架(0/1) 排序 [业务域] → 素材 id
  sql "INSERT INTO afv_public_template_assets (biz_type,type,title,url,extra) VALUES ('${6:-localLife}','image','$1','$3',JSON_OBJECT('role','$2'));
       SET @a=LAST_INSERT_ID();
       INSERT INTO afv_public_template_asset_placement (biz_type,module_code,slot_code,asset_id,sort_order,status) VALUES ('${6:-localLife}','digitalHuman','publicModel',@a,$5,$4);
       SELECT @a;"
}
RUN=$USER_ID
P1ID=$(public_model 文慧 店长 "https://mock-oss.example.com/public/wenhui-$RUN.png" 1 1)
P2ID=$(public_model 失败测试 测试 "https://mock-oss.example.com/public/mock-fail-$RUN.png" 1 2)
P3ID=$(public_model 下架的 测试 "https://mock-oss.example.com/public/off-$RUN.png" 0 3)
P4ID=$(public_model 电商模特 模特 "https://mock-oss.example.com/public/ecom-$RUN.png" 1 4 ecommerce)
cleanup_public() {
  sql "DELETE FROM afv_public_template_asset_placement WHERE asset_id IN ($P1ID,$P2ID,$P3ID,$P4ID);
       DELETE FROM afv_public_template_assets WHERE id IN ($P1ID,$P2ID,$P3ID,$P4ID);
       DELETE FROM afv_asset_item WHERE asset_id IN (SELECT id FROM (SELECT id FROM afv_asset WHERE user_id=$USER_ID) x);
       DELETE FROM afv_asset WHERE user_id=$USER_ID;
       DELETE FROM afv_workspace_tag WHERE workspace_id IN (SELECT id FROM (SELECT id FROM afv_workspace WHERE owner_user_id=$USER_ID) x);
       DELETE FROM afv_workspace_member WHERE user_id=$USER_ID;
       DELETE FROM afv_workspace WHERE owner_user_id=$USER_ID;"
}
trap 'cleanup_public; rm -rf "$TMP"' EXIT
L=$(api GET /digital-humans/public)
check "只看到本投放位上架的" "$(jq "[.list[].id]|map(select(. == $P1ID or . == $P2ID or . == $P3ID or . == $P4ID))|sort|map(tostring)|join(\",\")" <<<"$L" | tr -d '"')" "$P1ID,$P2ID"
check "名字取素材标题" "$(jq -r ".list[]|select(.id==$P1ID)|.name" <<<"$L")" 文慧
check "职业取 extra.role" "$(jq -r ".list[]|select(.id==$P1ID)|.role_label" <<<"$L")" 店长
check "头像缺省用形象图" "$(jq -r ".list[]|select(.id==$P1ID)|.avatar_url" <<<"$L")" "https://mock-oss.example.com/public/wenhui-$RUN.png"

echo "== 上传照片"
printf '\x89PNG\r\n\x1a\n' > "$TMP/a.png"; head -c 2000 /dev/urandom >> "$TMP/a.png"
UP=$(curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/a.png" "$GW/upload")
check "上传成功" "$(jq -r .code <<<"$UP")" 0
IMG=$(jq -r .data.url <<<"$UP")
check "按内容识别为 png" "$(jq -r .data.content_type <<<"$UP")" image/png
check "文件可访问" "$(curl -s -o /dev/null -w '%{http_code}' "$IMG")" 200
echo "not an image" > "$TMP/a.txt"
check "非图片被拒" "$(curl -sS -H "X-User-Id: $USER_ID" -F "file=@$TMP/a.txt" "$GW/upload" | jq -r .code)" 1001
check "不能列目录" "$(curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8888/files/aigc-life/")" 404

echo "== AI 定制形象（mock 执行面 75）"
RID=$(uuidgen)
BODY='{"gender":"female","age":"youth","skin_tone":"natural","role":"store_manager","description":"长发，微笑"}'
PT=$(api POST /digital-humans/portraits "$BODY" "$RID")
PTID=$(jq -r .id <<<"$PT")
check "生成中" "$(jq -r .status <<<"$PT")" 0
check "幂等：同一 request_id 同一条" "$(api POST /digital-humans/portraits "$BODY" "$RID" | jq -r .id)" "$PTID"
check "缺少 request_id 被拒" "$(curl -sS -X POST -H "X-User-Id: $USER_ID" -H 'Content-Type: application/json' -d "$BODY" "$GW/digital-humans/portraits" | jq -r .code)" 1001
check "无效年龄被拒" "$(api_code POST /digital-humans/portraits '{"gender":"female","age":"bad","skin_tone":"natural"}')" 1001
PT=$(poll "/digital-humans/portraits/$PTID" 0)
check "出图完成" "$(jq -r .status <<<"$PT")" 1
check "4 张图" "$(jq '.image_urls|length' <<<"$PT")" 4
RG=$(api POST "/digital-humans/portraits/$PTID/regenerate" "" "$(uuidgen)")
check "重新生成是新记录" "$([[ $(jq -r .id <<<"$RG") != "$PTID" ]] && echo yes)" yes
check "沿用原参数" "$(jq -r .role <<<"$RG")" store_manager
FP=$(api POST /digital-humans/portraits '{"gender":"male","age":"elder","skin_tone":"tan","description":"mock-fail"}' "$(uuidgen)")
FP=$(poll "/digital-humans/portraits/$(jq -r .id <<<"$FP")" 0)
check "出图失败有原因" "$(jq -r '.status, .fail_reason' <<<"$FP" | paste -sd, -)" "2,生成失败，请重试"
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/digital-humans/portraits/$PTID" | jq -r .code)" 1003

echo "== 开始生成：真实百炼配音 + mock 执行面 85"
B="{\"portrait_source\":\"PUBLIC\",\"public_id\":$P1ID,\"voice_id\":$VOICE_ID,\"script\":\"  \"}"
check "配音为空被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"PUBLIC\",\"public_id\":$P1ID,\"voice_id\":$VOICE_ID,\"script\":\"$(printf '字%.0s' {1..51})\"}"
check "配音超 50 字被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"UPLOAD\",\"image_url\":\"https://evil.example.com/a.png\",\"voice_id\":$VOICE_ID,\"script\":\"你好\"}"
check "外链照片被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"PUBLIC\",\"public_id\":$P4ID,\"voice_id\":$VOICE_ID,\"script\":\"你好\"}"
check "其他业务域的素材被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"PUBLIC\",\"public_id\":$P3ID,\"voice_id\":$VOICE_ID,\"script\":\"你好\"}"
check "下架的公共数字人被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"PUBLIC\",\"public_id\":$P1ID,\"voice_id\":99999999,\"script\":\"你好\"}"
check "无效音色被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"AI\",\"portrait_id\":$PTID,\"image_index\":4,\"voice_id\":$VOICE_ID,\"script\":\"你好\"}"
check "AI 形象下标越界被拒" "$(api_code POST /digital-humans "$B")" 1001
B="{\"portrait_source\":\"PUBLIC\",\"public_id\":$P1ID,\"voice_id\":$VOICE_ID,\"script\":\"你好\",\"model\":\"sd3\"}"
check "不认识的模型被拒（不默默按云端扣钱）" "$(api_code POST /digital-humans "$B")" 1001

RID=$(uuidgen)
D1=$(api POST /digital-humans "{\"portrait_source\":\"PUBLIC\",\"public_id\":$P1ID,\"voice_id\":$VOICE_ID,\"script\":\"欢迎来到西湖边的临湖咖啡，今天的手冲和甜品都很推荐。\"}" "$RID")
D1ID=$(jq -r .id <<<"$D1")
check "配音中" "$(jq -r .status <<<"$D1")" 0
check "默认名取公共数字人名" "$(jq -r .name <<<"$D1")" 文慧
check "幂等" "$(api POST /digital-humans '{"portrait_source":"PUBLIC","public_id":1,"voice_id":1,"script":"x"}' "$RID" | jq -r .id)" "$D1ID"
D2=$(api POST /digital-humans "{\"name\":\"店长小慧\",\"portrait_source\":\"AI\",\"portrait_id\":$PTID,\"image_index\":2,\"voice_id\":$VOICE_ID,\"script\":\"今天给大家推荐我们的招牌好物。\"}" "$(uuidgen)")
D2ID=$(jq -r .id <<<"$D2")
D3=$(api POST /digital-humans "{\"portrait_source\":\"UPLOAD\",\"image_url\":\"$IMG\",\"voice_id\":$VOICE_ID,\"script\":\"上传照片生成的数字人。\"}" "$(uuidgen)")
D3ID=$(jq -r .id <<<"$D3")
D4ID=$(api POST /digital-humans "{\"portrait_source\":\"PUBLIC\",\"public_id\":$P2ID,\"voice_id\":$VOICE_ID,\"script\":\"这条视频会失败。\"}" "$(uuidgen)" | jq -r .id)
# lite 路由到 45：mock 执行面会卡「两个路径必须是 OSS key、不能是 URL」，所以这条同时验了
# 我们有没有把公网地址换成 key——换错了这里会失败，而不是等到服务器上才发现。
D5ID=$(api POST /digital-humans "{\"portrait_source\":\"UPLOAD\",\"image_url\":\"$IMG\",\"voice_id\":$VOICE_ID,\"script\":\"这条走本地模型。\",\"model\":\"lite\"}" "$(uuidgen)" | jq -r .id)

D1=$(poll "/digital-humans/$D1ID" "0 1")
check "公共数字人 → 生成完成" "$(jq -r .status <<<"$D1")" 2
check "有视频" "$([[ $(jq -r .video_url <<<"$D1") == *.mp4 ]] && echo yes)" yes
check "时长按配音算（4–30 秒）" "$(jq '.duration >= 4 and .duration <= 30' <<<"$D1")" true
check "音色名" "$(jq -r '.voice_name|length > 0' <<<"$D1")" true
D2=$(poll "/digital-humans/$D2ID" "0 1")
check "AI 形象 → 生成完成" "$(jq -r .status <<<"$D2")" 2
check "用了选中的第 3 张" "$(jq -r .image_url <<<"$D2")" "$(jq -r '.image_urls[2]' <<<"$PT")"
check "自定义名称" "$(jq -r .name <<<"$D2")" 店长小慧
D5=$(poll "/digital-humans/$D5ID" "0 1")
check "lite → 生成完成" "$(jq -r .status <<<"$D5")" 2
check "lite 记在库里" "$(jq -r .model <<<"$D5")" lite
check "云端那条仍是默认档" "$(jq -r .model <<<"$D1")" seedance2.5
D3=$(poll "/digital-humans/$D3ID" "0 1")
check "上传照片 → 生成完成" "$(jq -r .status <<<"$D3")" 2
D4=$(poll "/digital-humans/$D4ID" "0 1")
check "视频失败有原因" "$(jq -r '.status, .fail_reason' <<<"$D4" | paste -sd, -)" "3,视频生成失败，请重试"

echo "== 素材库（afv_asset）"
PT_TASK=$(sql "SELECT task_id FROM afv_digital_human_portrait WHERE id=$PTID")
check "定制形象：一行主资产" "$(sql "SELECT CONCAT_WS('|', type, entry_code, aspect_ratio) FROM afv_asset WHERE source_task_id='$PT_TASK' AND deleted=0")" "customizedModel|digitalHumanPortrait|9:16"
check "定制形象：4 条子资产" "$(sql "SELECT count(*) FROM afv_asset_item i JOIN afv_asset a ON a.id=i.asset_id WHERE a.source_task_id='$PT_TASK' AND i.deleted=0")" 4
V_TASK=$(sql "SELECT video_task_id FROM afv_digital_human WHERE id=$D1ID")
check "口播视频：一行资产，封面是形象图" "$(sql "SELECT CONCAT_WS('|', type, entry_code, name, cover_url) FROM afv_asset WHERE source_task_id='$V_TASK' AND deleted=0")" "video|digitalHuman|文慧|https://mock-oss.example.com/public/wenhui-$RUN.png"

echo "== 我的数字人"
check "列表 5 条" "$(api GET /digital-humans | jq .total)" 5
check "按状态筛选：完成 4 条" "$(api GET '/digital-humans?status=2' | jq .total)" 4
D3_TASK=$(sql "SELECT video_task_id FROM afv_digital_human WHERE id=$D3ID")
api DELETE "/digital-humans/$D3ID" > /dev/null
check "删除数字人时素材库同步删除" "$(sql "SELECT count(*) FROM afv_asset WHERE source_task_id='$D3_TASK' AND deleted=0")" 0
check "删除后不可见" "$(api_code GET "/digital-humans/$D3ID")" 1003
check "删除后 4 条" "$(api GET /digital-humans | jq .total)" 4
check "别人看不到" "$(curl -sS -H 'X-User-Id: 999999' "$GW/digital-humans/$D1ID" | jq -r .code)" 1003

if [[ ${SKIP_MODEL:-0} != 1 ]]; then
  echo "== AI 帮写配音内容（真实调模型）"
  T=$(api POST /copywriting/ai-write '{"scene":"dh_script","context":"西湖边的咖啡店，主打手冲"}')
  check "返回配音文案且不超过 50 字" "$(jq '.text|length > 0 and length <= 50' <<<"$T")" true
else
  echo "== AI 帮写配音内容（SKIP_MODEL=1，已跳过）"
fi

echo "== 运营后台"
check "后台未登录" "$(curl -sS "$ADMIN/digital-humans/voices" | jq -r .code)" 1002
check "配音模型" "$(admin GET /digital-humans/tts-model | jq -r .code)" qwen-audio-3.1-tts-flash
check "后台看得到音色参数" "$(admin GET /digital-humans/voices | jq -r '.list[0].voice')" yuxiaoyun_v3.1
check "后台看得到停用分类" "$(admin GET '/digital-humans/options?option_type=VOICE_CATEGORY' | jq '.list|length')" 9
NV=$(admin POST /digital-humans/voices '{"name":"测试音色","voice":"anxiaolan_v3.1_e2e","gender":"female","style":"测试","categories":["female"],"status":0}')
check "音色参数重复被拒" "$(curl -sS -X POST -H 'X-Admin-Id: 1' -H 'Content-Type: application/json' -d '{"name":"x","voice":"yuxiaoyun_v3.1","gender":"female","status":1}' "$ADMIN/digital-humans/voices" | jq -r .code)" 1005
check "停用音色 App 不可见" "$(api GET /voices | jq '.list|length')" 22
admin DELETE "/digital-humans/voices/$(jq -r .id <<<"$NV")" > /dev/null
PV=$(admin POST "/digital-humans/voices/$VOICE_ID/preview")
check "生成试听（真实合成）" "$([[ $(jq -r .preview_url <<<"$PV") == *.wav ]] && echo yes)" yes
check "试听文件可访问" "$(curl -s -o /dev/null -w '%{http_code}' "$(jq -r .preview_url <<<"$PV")")" 200
check "App 能看到试听地址" "$(api GET /voices | jq -r ".list[]|select(.id==$VOICE_ID)|.preview_url")" "$(jq -r .preview_url <<<"$PV")"
check "后台按用户查数字人" "$(admin GET "/digital-humans?user_id=$USER_ID" | jq .total)" 4
sql "UPDATE afv_public_template_asset_placement SET status=0 WHERE asset_id=$P1ID"
check "现有后台下架后 App 不可见" "$(api GET /digital-humans/public | jq "[.list[]|select(.id==$P1ID)]|length")" 0

green "== 全部通过：$PASS 项"

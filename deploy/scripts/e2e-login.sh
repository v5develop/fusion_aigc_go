#!/usr/bin/env bash
# 登录端到端测试（依赖 make run-all 已启动）
# 联盟用本地 mock-union（:5056）模拟。真实链路：
#   前端：联盟 token → 临时 token（GET /api/v1/user/auth/token）
#   我们：临时 token → 用户信息 → 建档 → 签发我们的令牌
# 临时 token 一次性，所以每次登录都要重新换一个。
#
# ⚠️ 只用假的联盟用户 ID（9990000000000000xx）：真实 ID 会命中库里已有的真人，
#    登录会把昵称/头像同步成测试数据。
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/api/v1}
MOCKUNION=${MOCKUNION:-http://127.0.0.1:5056}
UNION_ID=${UNION_ID:-999000000000000042}
PASS=0
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
# temp 像前端那样去联盟换一个临时 token（5 分钟、一次性）
temp() { curl -sS -H "Authorization: Bearer $1" "$MOCKUNION/api/v1/user/auth/token" | jq -r .data.tempToken; }
# login 用临时 token 换我们的令牌
login() { curl -sS -X POST -H 'Content-Type: application/json' -d "{\"temp_token\":\"$(temp "$1")\"}" "$GW/auth/login"; }

check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

cleanup() {
  sql "DELETE tm FROM afv_team_member tm JOIN afv_app_user u ON u.id = tm.user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE t FROM afv_team t JOIN afv_app_user u ON u.id = t.owner_user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE FROM afv_app_user WHERE union_user_id LIKE '999%'"
}
trap cleanup EXIT
cleanup   # 上一次跑剩下的

echo "== 登录：新用户自动建档"
R=$(login "u$UNION_ID")
check "登录成功" "$(jq -r .code <<<"$R")" 0
TOKEN=$(jq -r .data.token <<<"$R")
UID1=$(jq -r .data.user.id <<<"$R")
check "返回令牌与过期时间" "$(jq -r '[(.data.token|length>20),(.data.expires_at > (now*1000))]|map(tostring)|join(",")' <<<"$R")" true,true
check "手机号是脱敏的" "$(jq -r .data.user.phone <<<"$R")" "130****7881"
check "按联盟约定建档" "$(sql "SELECT CONCAT_WS('|',username,login_source,data_source,union_bound,status) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" "union_$UNION_ID|UNION|2|1|1"
check "密码是 BCrypt（本地登不进来）" "$(sql "SELECT LEFT(password,4) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" '$2a$'
# uk_app_external_user(source_app_key, external_user_id) 要成对写，少写一个唯一键就形同虚设
check "唯一键两列都写了" "$(sql "SELECT CONCAT_WS('|',IFNULL(source_app_key,'空'),IFNULL(external_user_id,'空')) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" \
  "$(sql "SELECT app_key FROM afv_open_app WHERE is_builtin=1 AND status=1 ORDER BY id LIMIT 1")|$UNION_ID"

echo "== 建号同时建团队（Java 那边所有用户都有团队）"
check "有个人团队且本人是拥有者" "$(sql "SELECT CONCAT_WS('|',t.name,t.owner_user_id,t.status,tm.role,tm.status) FROM afv_team_member tm JOIN afv_team t ON t.id=tm.team_id WHERE tm.user_id=$UID1")" \
  "$(sql "SELECT nickname FROM afv_app_user WHERE id=$UID1")|$UID1|1|1|1"

echo "== 登录：老用户不重复建档"
R2=$(login "u$UNION_ID")
check "还是同一个用户" "$(jq -r .data.user.id <<<"$R2")" "$UID1"
check "库里只有一条" "$(sql "SELECT COUNT(*) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" 1
check "团队没重复建" "$(sql "SELECT COUNT(*) FROM afv_team_member WHERE user_id=$UID1")" 1

echo "== 联盟没给昵称头像（很常见）"
NO_PROFILE=999000000000000043
R3=$(login "n$NO_PROFILE")
UID2=$(jq -r .data.user.id <<<"$R3")
check "昵称兜底成「用户{id}」" "$(sql "SELECT nickname FROM afv_app_user WHERE id=$UID2")" "用户$UID2"
check "团队名跟着兜底的昵称" "$(sql "SELECT t.name FROM afv_team t WHERE t.owner_user_id=$UID2")" "用户$UID2"
login "u$NO_PROFILE" >/dev/null   # 用户在联盟补了资料
check "补了资料会同步过来" "$(sql "SELECT nickname FROM afv_app_user WHERE id=$UID2")" "mock 用户0043"
login "n$NO_PROFILE" >/dev/null   # 又拿到一份空资料
check "空值不覆盖，昵称没被抹掉" "$(sql "SELECT nickname FROM afv_app_user WHERE id=$UID2")" "mock 用户0043"
check "头像也没被抹掉" "$(sql "SELECT IFNULL(avatar,'空') FROM afv_app_user WHERE id=$UID2")" "https://mock-oss.example.com/avatar.png"

echo "== 联盟侧冻结的账号"
FROZEN=999000000000000044
check "冻结的登不进来" "$(login "f$FROZEN" | jq -r .code)" 1004
check "本地也标成禁用" "$(sql "SELECT status FROM afv_app_user WHERE union_user_id='$FROZEN'")" 0

echo "== 临时 token 的性质"
T=$(temp "u$UNION_ID")
check "换一次能用" "$(curl -sS -X POST -H 'Content-Type: application/json' -d "{\"temp_token\":\"$T\"}" "$GW/auth/login" | jq -r .code)" 0
check "用过就失效（不能当长期凭证）" "$(curl -sS -X POST -H 'Content-Type: application/json' -d "{\"temp_token\":\"$T\"}" "$GW/auth/login" | jq -r .code)" 1002
check "临时 token 不能直接调业务接口" "$(curl -sS -H "Authorization: Bearer $(temp "u$UNION_ID")" "$GW/copywriting/options" | jq -r .code)" 1002
check "放 Authorization 头登录也行" "$(curl -sS -X POST -H "Authorization: Bearer $(temp "u$UNION_ID")" "$GW/auth/login" | jq -r .code)" 0

echo "== 用我们的令牌访问接口"
check "当前用户" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/auth/me" | jq -r .data.id)" "$UID1"
check "业务接口放行" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/copywriting/options" | jq -r .code)" 0
check "数据按令牌里的用户隔离" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/shop-profiles" | jq -r .data.total)" 0

echo "== 拒绝的情况"
check "不带凭证" "$(curl -sS "$GW/auth/me" | jq -r .code)" 1002
check "乱写的令牌" "$(curl -sS -H 'Authorization: Bearer not-a-token' "$GW/auth/me" | jq -r .code)" 1002
check "别的密钥签的令牌" "$(curl -sS -H "Authorization: Bearer $(jq -r .data.token <<<"$R" | sed 's/.$/X/')" "$GW/auth/me" | jq -r .code)" 1002
check "临时 token 是乱写的" "$(curl -sS -X POST -H 'Content-Type: application/json' -d '{"temp_token":"not-a-temp-token"}' "$GW/auth/login" | jq -r .code)" 1002
check "登录不带凭证" "$(curl -sS -X POST -H 'Content-Type: application/json' -d '{}' "$GW/auth/login" | jq -r .code)" 1002

echo "== 联调模式：没带令牌时仍认 X-User-Id"
check "X-User-Id 可用（Auth.Mode=dev）" "$(curl -sS -H "X-User-Id: $UID1" "$GW/copywriting/options" | jq -r .code)" 0

green "== 全部通过：$PASS 项"

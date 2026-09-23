#!/usr/bin/env bash
# 登录端到端测试（依赖 make run-all 已启动）
# 联盟用本地 mock-union（:5056）模拟：联盟 token → 临时 token → 用户信息 → 我们的令牌。
#
# ⚠️ 只用假的联盟用户 ID（9990000000000000xx）：真实 ID 会命中库里已有的真人，
#    登录会把昵称/头像同步成测试数据。
set -euo pipefail

GW=${GW:-http://127.0.0.1:8888/api/v1}
UNION_ID=${UNION_ID:-999000000000000042}
PASS=0
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

cleanup() { sql "DELETE FROM afv_app_user WHERE union_user_id LIKE '999%'"; }
trap cleanup EXIT
cleanup   # 上一次跑剩下的

echo "== 登录：新用户自动建档"
R=$(curl -sS -X POST -H "Authorization: Bearer u$UNION_ID" "$GW/auth/login")
check "登录成功" "$(jq -r .code <<<"$R")" 0
TOKEN=$(jq -r .data.token <<<"$R")
UID1=$(jq -r .data.user.id <<<"$R")
check "返回令牌与过期时间" "$(jq -r '[(.data.token|length>20),(.data.expires_at > (now*1000))]|map(tostring)|join(",")' <<<"$R")" true,true
check "手机号是脱敏的" "$(jq -r .data.user.phone <<<"$R")" "130****7881"
check "按联盟约定建档" "$(sql "SELECT CONCAT_WS('|',username,login_source,data_source,union_bound,status) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" "union_$UNION_ID|UNION|2|1|1"
check "密码是 BCrypt（本地登不进来）" "$(sql "SELECT LEFT(password,4) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" '$2a$'

echo "== 登录：老用户不重复建档"
R2=$(curl -sS -X POST -H "Authorization: Bearer u$UNION_ID" "$GW/auth/login")
check "还是同一个用户" "$(jq -r .data.user.id <<<"$R2")" "$UID1"
check "库里只有一条" "$(sql "SELECT COUNT(*) FROM afv_app_user WHERE union_user_id='$UNION_ID'")" 1

echo "== 用我们的令牌访问接口"
check "当前用户" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/auth/me" | jq -r .data.id)" "$UID1"
check "业务接口放行" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/copywriting/options" | jq -r .code)" 0
check "数据按令牌里的用户隔离" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/shop-profiles" | jq -r .data.total)" 0

echo "== 拒绝的情况"
check "不带凭证" "$(curl -sS "$GW/auth/me" | jq -r .code)" 1002
check "乱写的令牌" "$(curl -sS -H 'Authorization: Bearer not-a-token' "$GW/auth/me" | jq -r .code)" 1002
check "别的密钥签的令牌" "$(curl -sS -H "Authorization: Bearer $(jq -r .data.token <<<"$R" | sed 's/.$/X/')" "$GW/auth/me" | jq -r .code)" 1002
check "联盟 token 无效" "$(curl -sS -X POST -H 'Authorization: Bearer bad' "$GW/auth/login" | jq -r .code)" 1002
check "登录不带凭证" "$(curl -sS -X POST "$GW/auth/login" | jq -r .code)" 1002

echo "== 联调模式：没带令牌时仍认 X-User-Id"
check "X-User-Id 可用（Auth.Mode=dev）" "$(curl -sS -H "X-User-Id: $UID1" "$GW/copywriting/options" | jq -r .code)" 0

green "== 全部通过：$PASS 项"

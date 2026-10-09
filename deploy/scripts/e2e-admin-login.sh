#!/usr/bin/env bash
# 运营后台登录端到端测试（依赖 make run-all 已启动）。
#
# 账号用现有的 afv_admin_user 表。测试会自己插两个临时账号（e2e_admin / e2e_off），
# 跑完删掉；**不碰真实账号**，也不会去动它们的 login_fail_count。
set -euo pipefail

ADMIN=${ADMIN:-http://127.0.0.1:8889/admin/v1}
GW=${GW:-http://127.0.0.1:8888/api/v1}
MOCKUNION=${MOCKUNION:-http://127.0.0.1:5056}
USER=e2e_admin
PWD_OK='e2e-Pass-1234'
# 上面这个密码的 BCrypt（固定值，省得测试时还要现算）
HASH='$2a$10$2ye0nSWQOQ7M7TZzoYFRTe5TFnWY3K/LWcNkp/w3/rhBfwLT87X9u'
PASS=0
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

cleanup() {
  sql "DELETE r FROM afv_admin_user_role r JOIN afv_admin_user u ON u.id = r.admin_id WHERE u.username LIKE 'e2e\\_%'"
  sql "DELETE FROM afv_admin_user WHERE username LIKE 'e2e\\_%'"
}
trap cleanup EXIT
cleanup

# 一个启用、一个停用
sql "INSERT INTO afv_admin_user (username,password_hash,real_name,status) VALUES ('$USER','$HASH','端到端测试',1)"
sql "INSERT INTO afv_admin_user (username,password_hash,real_name,status) VALUES ('e2e_off','$HASH','已停用',0)"
UID1=$(sql "SELECT id FROM afv_admin_user WHERE username='$USER'")
# 给个运营角色，验证登录返回角色
sql "INSERT INTO afv_admin_user_role (admin_id,role_id) SELECT $UID1, id FROM afv_admin_role WHERE role_code='operator'"

login() { curl -sS -X POST -H 'Content-Type: application/json' -d "{\"username\":\"$1\",\"password\":\"$2\"}" "$ADMIN/auth/login"; }

echo "== 用现有后台账号登录"
R=$(login "$USER" "$PWD_OK")
check "登录成功" "$(jq -r .code <<<"$R")" 0
TOKEN=$(jq -r .data.token <<<"$R")
check "返回令牌与过期时间" "$(jq -r '[(.data.token|length>20),(.data.expires_at > (now*1000))]|map(tostring)|join(",")' <<<"$R")" true,true
check "带上角色" "$(jq -r '.data.admin.roles|join(",")' <<<"$R")" operator
check "带上姓名" "$(jq -r .data.admin.real_name <<<"$R")" 端到端测试
check "记下了最后登录时间" "$(sql "SELECT last_login_time IS NOT NULL FROM afv_admin_user WHERE id=$UID1")" 1

echo "== 用令牌调后台接口"
check "当前管理员" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$ADMIN/auth/me" | jq -r .data.id)" "$UID1"
check "业务接口放行" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$ADMIN/copywriting/options" | jq -r .code)" 0

echo "== 拒绝的情况"
check "不带凭证" "$(curl -sS "$ADMIN/auth/me" | jq -r .code)" 1002
check "乱写的令牌" "$(curl -sS -H 'Authorization: Bearer not-a-token' "$ADMIN/auth/me" | jq -r .code)" 1002
check "密码错" "$(login "$USER" wrong-password | jq -r .code)" 1002
check "账号不存在（提示与密码错一致，不暴露账号）" "$(login no_such_admin whatever | jq -r .msg)" "$(login "$USER" wrong-password | jq -r .msg)"
check "停用的账号" "$(login e2e_off "$PWD_OK" | jq -r .code)" 1004
check "用户名为空" "$(login '' "$PWD_OK" | jq -r .code)" 1001

echo "== App 的令牌不能调后台（两套密钥分开 + kind 校验）"
APP_TOKEN=$(curl -sS -X POST -H 'Content-Type: application/json' \
  -d "{\"temp_token\":\"$(curl -sS -H 'Authorization: Bearer u999000000000000081' "$MOCKUNION/api/v1/user/auth/token" | jq -r .data.tempToken)\"}" \
  "$GW/auth/login" | jq -r .data.token)
check "App 令牌被后台拒绝" "$(curl -sS -H "Authorization: Bearer $APP_TOKEN" "$ADMIN/auth/me" | jq -r .code)" 1002
check "后台令牌也调不了 App 接口" "$(curl -sS -H "Authorization: Bearer $TOKEN" "$GW/auth/me" | jq -r .code)" 1002
sql "DELETE tm FROM afv_team_member tm JOIN afv_app_user u ON u.id=tm.user_id WHERE u.union_user_id='999000000000000081'"
sql "DELETE t FROM afv_team t JOIN afv_app_user u ON u.id=t.owner_user_id WHERE u.union_user_id='999000000000000081'"
sql "DELETE wt FROM afv_workspace_tag wt JOIN afv_workspace w ON w.id=wt.workspace_id JOIN afv_app_user u ON u.id=w.owner_user_id WHERE u.union_user_id='999000000000000081'"
sql "DELETE wm FROM afv_workspace_member wm JOIN afv_app_user u ON u.id=wm.user_id WHERE u.union_user_id='999000000000000081'"
sql "DELETE w FROM afv_workspace w JOIN afv_app_user u ON u.id=w.owner_user_id WHERE u.union_user_id='999000000000000081'"
sql "DELETE FROM afv_app_user WHERE union_user_id='999000000000000081'"

echo "== 连错 5 次锁 15 分钟"
for _ in 1 2 3 4; do login "$USER" wrong-password >/dev/null; done
check "第 5 次起锁定" "$(login "$USER" wrong-password | jq -r .code)" 1004
check "锁定期间密码对也不放行" "$(login "$USER" "$PWD_OK" | jq -r .code)" 1004
check "库里记了锁定到期时间" "$(sql "SELECT locked_until > NOW() FROM afv_admin_user WHERE id=$UID1")" 1
sql "UPDATE afv_admin_user SET locked_until = NULL, login_fail_count = 0 WHERE id=$UID1"
check "解锁后能登录" "$(login "$USER" "$PWD_OK" | jq -r .code)" 0

echo "== 联调模式：没带令牌时仍认 X-Admin-Id"
check "X-Admin-Id 可用（Auth.Mode=dev）" "$(curl -sS -H "X-Admin-Id: $UID1" "$ADMIN/copywriting/options" | jq -r .code)" 0

green "== 全部通过：$PASS 项"

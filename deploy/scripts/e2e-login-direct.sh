#!/usr/bin/env bash
# 登录：自己验临时 token 那条路（服务器访问不到联盟域名时用的降级方案）。
#
# 不调联盟的 HTTP 接口，而是照它的做法：
#   读它 Redis 的 temp:token:<token>（值是 userId）→ 查它的 uec_app_users → 用过就删。
#
# 本地正好有它的库（unionec_app，和 fusion_aigc 同一个 MySQL 实例）和 Redis，所以能真跑。
# ⚠️ 只用假用户（id 999…）：真实 ID 会命中库里的真人，登录会把资料同步过去。
#
# 自己起临时网关和数据，跑完收摊。
set -euo pipefail

cd "$(dirname "$0")/../.."

PORT=${PORT:-8891}
GW="http://127.0.0.1:$PORT/api/v1"
REDIS=${REDIS:-redis-7}          # docker 容器名
UNION_UID=999000000000000091     # 联盟库里的假用户
AES_KEY=1234567890ABCDEF         # 联盟的手机号加密密钥（他们配置里的默认值）
PHONE_CT='zopm9HfAwJUgPKoF0UezTw=='  # 上面这个密钥加密的 13012347881
PASS=0
# 默认库选 fusion_aigc：多表 DELETE 即使写了全限定名也要求有默认库
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 -N fusion_aigc)
sql()   { "${MYSQL[@]}" -e "$1" 2>/dev/null; }
rediscli() { docker exec -i "$REDIS" redis-cli "$@"; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

CONF=$(mktemp -t gateway-direct).yaml
cleanup() {
  set +m
  [[ -n ${GW_PID:-} ]] && { kill "$GW_PID"; wait "$GW_PID"; } 2>/dev/null || true
  rm -f "$CONF"
  rediscli del "temp:token:tok-e2e-1" "temp:token:tok-e2e-2" >/dev/null 2>&1 || true
  sql "DELETE FROM unionec_app.uec_app_users WHERE id = $UNION_UID"
  sql "DELETE tm FROM fusion_aigc.afv_team_member tm JOIN fusion_aigc.afv_app_user u ON u.id = tm.user_id WHERE u.union_user_id LIKE '999%';
       DELETE t FROM fusion_aigc.afv_team t JOIN fusion_aigc.afv_app_user u ON u.id = t.owner_user_id WHERE u.union_user_id LIKE '999%';
       DELETE FROM fusion_aigc.afv_app_user WHERE union_user_id LIKE '999%'"
}
trap cleanup EXIT
cleanup

go build -o bin/gateway-api ./app/gateway/api

# 网关：照本地那份改端口，并配上「自己验」那条路（UserDSN 留空 = 复用本服务的 DataSource）
sed -e "s|^Port: .*|Port: $PORT|" deploy/etc/gateway-api.yaml > "$CONF"
cat >> "$CONF" <<EOF
UnionDirect: placeholder
EOF
python3 - "$CONF" <<'PY'
import sys, re, pathlib
p = pathlib.Path(sys.argv[1]); s = p.read_text()
s = s.replace("UnionDirect: placeholder\n", "")
# 把 Direct 挂到 Union 下面（BaseURL 故意留着，用来验证「配了 Direct 就优先走它」）
s = re.sub(r'(?m)^(Union:.*\n(?:  .*\n)*)', r'''\1  Direct:
    RedisAddr: "127.0.0.1:6379"
    PhoneAesKey: "1234567890ABCDEF"
''', s, count=1)
p.write_text(s)
PY

# 联盟库里造一个假用户（字段照 uec_app_users）
# phone_hash / register_channel 是非空无默认值的，随便填（我们不读它们）
sql "INSERT INTO unionec_app.uec_app_users
       (id,nickname,avatar,phone,phone_hash,register_channel,gender,province,city,real_name_status,status,roles,created_at)
     VALUES ($UNION_UID,'直连测试用户','https://cdn.example.com/a.png','$PHONE_CT',UNHEX(SHA2('e2e',256)),'e2e',
             1,'浙江省','杭州市','approved','active','1',NOW())"

mkdir -p logs
bin/gateway-api -f "$CONF" > logs/gateway-direct.log 2>&1 &
GW_PID=$!
for _ in $(seq 1 40); do
  curl -sf "$GW/copywriting/options" -H 'X-User-Id: 1' >/dev/null 2>&1 && break
  sleep 0.5
done

login() { curl -sS -X POST -H 'Content-Type: application/json' -d "{\"temp_token\":\"$1\"}" "$GW/auth/login"; }

echo "== 自己验临时 token（不调联盟接口）"
rediscli setex "temp:token:tok-e2e-1" 300 "$UNION_UID" >/dev/null
R=$(login tok-e2e-1)
check "登录成功" "$(jq -r .code <<<"$R")" 0
UID1=$(jq -r .data.user.id <<<"$R")
check "资料取自联盟的用户表" "$(jq -r '[.data.user.nickname,.data.user.avatar]|join("|")' <<<"$R")" \
  "直连测试用户|https://cdn.example.com/a.png"
# 手机号在它库里是 AES 加密的，配了密钥才解得出来，然后按它的规则脱敏
check "手机号解密并脱敏" "$(jq -r .data.user.phone <<<"$R")" "130****7881"
check "按联盟约定建档" "$(sql "SELECT CONCAT_WS('|',username,login_source,data_source) FROM fusion_aigc.afv_app_user WHERE union_user_id='$UNION_UID'")" \
  "union_$UNION_UID|UNION|2"
check "建档同时建了团队" "$(sql "SELECT COUNT(*) FROM fusion_aigc.afv_team_member WHERE user_id=$UID1")" 1
check "资料里的省市实名也同步了" "$(sql "SELECT CONCAT_WS('|',province,city,real_name_status) FROM fusion_aigc.afv_app_user WHERE id=$UID1")" \
  "浙江省|杭州市|approved"

echo "== 一次性：和联盟行为一致，用过就删"
check "Redis 里的 key 已删" "$(rediscli exists "temp:token:tok-e2e-1" | tr -d '\r')" 0
check "同一个 token 再用就失效" "$(login tok-e2e-1 | jq -r .code)" 1002

echo "== 拒绝的情况"
check "不存在的 token" "$(login no-such-token | jq -r .code)" 1002
rediscli setex "temp:token:tok-e2e-2" 300 "999999999999999999" >/dev/null
check "token 指向联盟里没有的用户" "$(login tok-e2e-2 | jq -r .code)" 1002
check "token 为空" "$(curl -sS -X POST -H 'Content-Type: application/json' -d '{}' "$GW/auth/login" | jq -r .code)" 1002

echo "== 老用户再登录只同步资料"
rediscli setex "temp:token:tok-e2e-1" 300 "$UNION_UID" >/dev/null
sql "UPDATE unionec_app.uec_app_users SET nickname='改过的昵称' WHERE id=$UNION_UID"
R2=$(login tok-e2e-1)
check "还是同一个用户" "$(jq -r .data.user.id <<<"$R2")" "$UID1"
check "昵称跟着联盟变了" "$(jq -r .data.user.nickname <<<"$R2")" 改过的昵称
check "库里只有一条" "$(sql "SELECT COUNT(*) FROM fusion_aigc.afv_app_user WHERE union_user_id='$UNION_UID'")" 1

green "== 全部通过：$PASS 项"

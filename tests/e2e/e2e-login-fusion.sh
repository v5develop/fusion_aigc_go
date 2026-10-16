#!/usr/bin/env bash
# 登录：建档交给现有 Java 服务那条路（Fusion 配了密钥时）。
#
# 真的 Java 服务本地没有，用 bin/mock-fusion 顶替：它按对方文档**独立验签**，
# 并且真的写库（afv_app_user + afv_team + afv_team_member）。
# 所以这个脚本能证明两件事：我们的 HMAC 签名拼对了、建档确实走了对方而不是我们自己写表。
#
# 自己起进程、自己收摊，不依赖 make run-all 里的网关（那个没配 Fusion）。
set -euo pipefail

cd "$(dirname "$0")/../.."

PORT=${PORT:-8890}
FUSION_PORT=${FUSION_PORT:-5057}
GW="http://127.0.0.1:$PORT/api/v1/local-life"
MOCKUNION=${MOCKUNION:-http://127.0.0.1:5056}
FUSION="http://127.0.0.1:$FUSION_PORT"
APP_KEY=12345678910
APP_SECRET=mock-secret
DSN='root:123456@tcp(127.0.0.1:3306)/fusion_aigc?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27'
UNION_ID=${UNION_ID:-999000000000000071}
PASS=0
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
check() {
  if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; exit 1; fi
}

CONF=$(mktemp -t gateway-fusion).yaml
cleanup() {
  set +m   # 别把「Terminated」打到屏幕上
  for pid in "${GW_PID:-}" "${MF_PID:-}"; do
    [[ -n "$pid" ]] && { kill "$pid"; wait "$pid"; } 2>/dev/null || true
  done
  rm -f "$CONF"
  sql "DELETE tm FROM afv_team_member tm JOIN afv_app_user u ON u.id = tm.user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE t FROM afv_team t JOIN afv_app_user u ON u.id = t.owner_user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE wt FROM afv_workspace_tag wt JOIN afv_workspace w ON w.id=wt.workspace_id JOIN afv_app_user u ON u.id=w.owner_user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE wm FROM afv_workspace_member wm JOIN afv_app_user u ON u.id=wm.user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE w FROM afv_workspace w JOIN afv_app_user u ON u.id=w.owner_user_id WHERE u.union_user_id LIKE '999%'"
  sql "DELETE FROM afv_app_user WHERE union_user_id LIKE '999%'"
}
trap cleanup EXIT
cleanup

go build -o bin/gateway-api ./app/gateway/api
go build -o bin/mock-fusion ./tools/mocks/mock-fusion

# 网关配置：照本地那份，改个端口 + 配上 Fusion
sed -e "s|^Port: .*|Port: $PORT|" deploy/etc/gateway-api.yaml > "$CONF"
cat >> "$CONF" <<EOF
Fusion:
  BaseURL: $FUSION
  AppKey: "$APP_KEY"
  AppSecret: $APP_SECRET
  TimeoutSeconds: 10
EOF

mkdir -p logs
bin/mock-fusion -addr ":$FUSION_PORT" -dsn "$DSN" -app-key "$APP_KEY" -app-secret "$APP_SECRET" \
  > logs/mock-fusion.log 2>&1 &
MF_PID=$!
bin/gateway-api -f "$CONF" > logs/gateway-fusion.log 2>&1 &
GW_PID=$!
for _ in $(seq 1 40); do
  curl -sf "$GW/copywriting/options" -H 'X-User-Id: 1' >/dev/null 2>&1 && break
  sleep 0.5
done

temp()  { curl -sS -H "Authorization: Bearer $1" "$MOCKUNION/api/v1/user/auth/token" | jq -r .data.tempToken; }
login() { curl -sS -X POST -H 'Content-Type: application/json' -d "{\"temp_token\":\"$(temp "$1")\"}" "$GW/auth/login"; }

echo "== 新用户：建档交给 Java（mock-fusion）"
curl -sS -o /dev/null "$FUSION/debug/calls?reset=1"
R=$(login "u$UNION_ID")
check "登录成功" "$(jq -r .code <<<"$R")" 0
UID1=$(jq -r .data.user.id <<<"$R")
# 签名错的话 mock 会回 401，登录就失败了；能走到这里说明待签串拼对了
check "确实调了对方的 user-register（签名通过）" \
  "$(curl -sS "$FUSION/debug/calls" | jq -r '."/api/v1/union/user-register"')" 1
check "用户是对方建的（source_app_key 是它写的）" \
  "$(sql "SELECT CONCAT_WS('|',username,source_app_key,external_user_id) FROM afv_app_user WHERE id=$UID1")" \
  "union_$UNION_ID|$APP_KEY|$UNION_ID"
check "团队也是对方建的" "$(sql "SELECT COUNT(*) FROM afv_team_member WHERE user_id=$UID1 AND role=1")" 1
check "能用返回的令牌调接口" "$(curl -sS -H "Authorization: Bearer $(jq -r .data.token <<<"$R")" "$GW/auth/me" | jq -r .data.id)" "$UID1"

echo "== 老用户：不再麻烦对方"
curl -sS -o /dev/null "$FUSION/debug/calls?reset=1"
check "还是同一个人" "$(jq -r .data.user.id <<<"$(login "u$UNION_ID")")" "$UID1"
check "没有再调 user-register" "$(curl -sS "$FUSION/debug/calls" | jq -r '."/api/v1/union/user-register" // 0')" 0

echo "== 签名不对时对方会拒"
check "乱签的请求被拒" "$(curl -sS -o /dev/null -w '%{http_code}' -X POST "$FUSION/api/v1/union/user-register" \
  -H "X-Union-App-Key: $APP_KEY" -H "X-Union-Timestamp: $(date +%s)" \
  -H "X-Union-Nonce: 00000000000000000000000000000000" -H "X-Union-Signature: deadbeef" \
  -H 'Content-Type: application/json;charset=utf-8' -d '{"unionUserId":"999000000000000072","roles":[]}')" 401
check "没把人建进去" "$(sql "SELECT COUNT(*) FROM afv_app_user WHERE union_user_id='999000000000000072'")" 0

green "== 全部通过：$PASS 项"

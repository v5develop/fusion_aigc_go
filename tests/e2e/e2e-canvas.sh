#!/usr/bin/env bash
# 画布（canvas）+ 提示词库 本地冒烟（mock 链路，不产生费用）。
#
# 覆盖：建画布 → 保存 → if_match_revision 冲突 / force → 连线规则与 dataURL 校验 → RunNode(image) 经 mock ai-gateway 到 succeeded
#       → GetRun 有候选 → GetCanvas 节点 data 回填了候选 → 幂等重放 / 同 key 不同 body → 反推（chat.completion）→ 视频（video.generate）
#       → 裁剪（mock 媒体服务）→ 高清放大经 mock Java 兼容接口（断言 Bearer 原样透传）
#       → 回填：最近 run 失败后仍显示上一次成功的候选、选中第 2 张（select-candidate）刷新后仍是第 2 张、无 if_match 且非 force 的保存被拒
#       → 模板列表不带快照 / 详情带、元素替换模板参考视频接 video 口 → 模板建画布（id 重映射）/ 另存为
#       → 提示词 CRUD（官方 4002）→ 后台 run 列表、有权限 / 无权限 / 未登录 → 任务中心与素材库落表 → 删除画布。
#
# 用法：
#   START=1 tests/e2e/e2e-canvas.sh     # 自己起全套（推荐）：mock-aigateway :5059、mock-media :18090、mock-java :5061（python），
#                                       # canvas-rpc / gateway-api / admin-api 用 mkconf 从 deploy/etc 生成的配置副本（不改仓库 deploy/etc），
#                                       # 库与 Redis 用本机 compose：MySQL 127.0.0.1:43306、Redis 127.0.0.1:46379（口令 123456）。
#   tests/e2e/e2e-canvas.sh             # 服务已在跑：GW / ADMIN / MYSQL_CONTAINER / REDIS_CONTAINER 指过去
# 前提：已执行 deploy/sql/Go_V1.0.25__canvas.sql、Go_V1.0.26__admin_canvas_permission.sql 与 deploy/sql/seed/canvas-official-*.sql。
# 结束时（trap）停掉自己起的进程，删除本次用户的画布 / run / 提示词 / 模板 / 素材 / 任务中心行与临时 Redis 会话。
#
# BILLING=1：canvas-rpc 以 Billing.Mode=on 启动（START=1 时改配置副本；自带服务时须自己配好），给测试用户建 3000 点账户，
#   逐笔核对：image count=2 全成功按 2 张结算、一张失败按 1 张结算（PARTIALLY_REFUNDED）、i2p / 视频计费、裁剪与 Java 工具
#   没有 Go 预留单、取消排队 run 退款、UNKNOWN 待对账到期由 Reclaimer 释放；最后跑对账与账务自洽检查，结束删掉账户与流水。
#   结果注入靠 mock-aigateway 的 /debug/inject（按提示词里的标记命中）。
#
# 严格模式（tests/e2e/lib/strict.sh）：set -Eeuo pipefail、ERR trap 打印行号与退出码、cleanup 保留原退出码、
# 末尾 e2e_finish 汇总（有失败 exit 1）。任一断言失败 / 未定义变量 / 命令出错，退出码都非 0。
set -Eeuo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
# shellcheck source=lib/strict.sh
source "$ROOT/tests/e2e/lib/strict.sh"
START=${START:-0}
DB_PORT=${DB_PORT:-43306}
REDIS_PORT=${REDIS_PORT:-46379}
REDIS_PASS=${REDIS_PASS:-123456}
MYSQL_CONTAINER=${MYSQL_CONTAINER:-compose-mysql-1}
REDIS_CONTAINER=${REDIS_CONTAINER:-compose-redis-1}
GW_PORT=${GW_PORT:-28888}; ADMIN_PORT=${ADMIN_PORT:-28889}; CANVAS_PORT=${CANVAS_PORT:-29207}
AIG_PORT=${AIG_PORT:-5059}; MEDIA_PORT=${MEDIA_PORT:-18090}; JAVA_PORT=${JAVA_PORT:-5061}
GW=${GW:-http://127.0.0.1:$GW_PORT}
ADMIN=${ADMIN:-http://127.0.0.1:$ADMIN_PORT}
USER_ID=${USER_ID:-$(( (RANDOM << 15 | RANDOM) + 700000000 ))}
BILLING=${BILLING:-0}
BILL_INIT=3000
TMP=$(mktemp -d)
PIDS=()
PASS=0

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
up()     { nc -z -w1 127.0.0.1 "$1" >/dev/null 2>&1; }
sql()    { docker exec -i "$MYSQL_CONTAINER" mysql --default-character-set=utf8mb4 -uroot -p123456 fusion_aigc -N -e "$1" 2>/dev/null; }
rcli()   { docker exec -i "$REDIS_CONTAINER" redis-cli -a "$REDIS_PASS" --no-auth-warning -n 10 "$@" >/dev/null; }
hex32()  { uuidgen | tr -d '-' | tr 'A-Z' 'a-z'; }
# shellcheck source=lib/billing.sh
source "$ROOT/tests/e2e/lib/billing.sh"

USER_TOKEN=$(hex32); ADMIN_TOKEN=$(hex32); NOPERM_TOKEN=$(hex32)

cleanup() {
  local rc=$?
  trap - ERR
  set +e # 清理尽力而为：单步失败不中断、不改写退出码
  rc=$(e2e_rc "$rc")
  for p in "${PIDS[@]:-}"; do [[ -n $p ]] && kill "$p" 2>/dev/null || true; done
  sql "DELETE FROM afv_asset WHERE user_id=$USER_ID;
       DELETE FROM afv_creation_task WHERE user_id=$USER_ID AND origin_ref LIKE 'cv:%';
       DELETE FROM afv_canvas_node_run WHERE user_id=$USER_ID;
       DELETE FROM afv_canvas WHERE user_id=$USER_ID;
       DELETE FROM afv_canvas_template WHERE user_id=$USER_ID;
       DELETE FROM afv_prompt_library_item WHERE user_id=$USER_ID;
       DELETE FROM afv_app_user_feature WHERE user_id=$USER_ID AND source='E2E';" || true
  if [[ $BILLING == 1 && ${KEEP_BILLING:-0} != 1 ]]; then bill_cleanup "$USER_ID"; fi
  [[ $BILLING == 1 ]] && curl -s -o /dev/null "http://127.0.0.1:$AIG_PORT/debug/inject?reset=1" || true
  rcli DEL "fusion:token:$USER_TOKEN" "fusion:admin:access:$ADMIN_TOKEN" "fusion:admin:access:$NOPERM_TOKEN" || true
  if [[ $rc != 0 && $START == 1 ]]; then
    yellow "  日志：${TMP}/*.log（保留）"; tail -n 20 "$TMP"/canvas-rpc.log 2>/dev/null || true
  else
    rm -rf "$TMP"
  fi
  [[ $rc != 0 ]] && red "== 退出码 ${rc}"
  exit "$rc"
}
trap cleanup EXIT

wait_port() { for _ in $(seq 1 60); do up "$1" && return; sleep 0.5; done; red "端口 $1 没起来（$2）"; tail -n 30 "$TMP/$2.log" 2>/dev/null; exit 2; }

# ---------------- 起服务（START=1） ----------------
start_all() {
  echo "== 构建与启动（日志在 ${TMP}）"
  (cd "$ROOT" && go build -o "$TMP/bin/canvas-rpc" ./app/canvas/rpc && go build -o "$TMP/bin/gateway-api" ./app/gateway/api \
     && go build -o "$TMP/bin/admin-api" ./app/admin/api && go build -o "$TMP/bin/mock-aigateway" ./tools/mocks/mock-aigateway \
     && go build -o "$TMP/bin/mock-media" ./tools/mocks/mock-media && go build -o "$TMP/bin/mkconf" ./tools/ops/mkconf)
  # mock Java 兼容接口：只认 Authorization = Bearer ${USER_TOKEN}；任务第一次查 RUNNING、之后 SUCCEED（结果图用 mock-aigateway 的图）
  # 取消接口 POST /api/v1/creation/{taskId}/cancel 照 Java cancelByUser：不存在 4001、已结束 3001、否则置取消 code=0。
  # 控制口（Java 取消用例用）：/debug/mode?m=normal|hold（新任务一直 RUNNING）|finish_on_cancel（一直 RUNNING，取消时恰好成功 → 3001）、
  # /debug/reject?on=1（所有请求 401）、/debug/cancels（收到的取消与提交记录）、
  # /debug/fail?m=after_create（下一次提交建了任务却回 502）|before_create（下一次提交没到 Java，回 502）。
  # 幂等：同 Idempotency-Key 同 body（原始字节）返回原 taskId，不同 body 回 1003（Java 实际按语义摘要比，这里更严）。
  cat > "$TMP/mock_java.py" <<'PY'
import json, sys, threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
port, token, img = int(sys.argv[1]), sys.argv[2], sys.argv[3]
state = {"n": 0, "polls": {}, "seen": [], "mode": "normal", "tmode": {}, "final": {}, "cancels": [], "reject": False, "rejected": 0, "idem": {}, "fail": "", "submits": []}
lock = threading.Lock()
class H(BaseHTTPRequestHandler):
    def log_message(self, *a): pass
    def send(self, code, obj):
        b = json.dumps(obj).encode()
        self.send_response(code); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b)
    def authed(self):
        ok = self.headers.get("Authorization", "") == "Bearer " + token and not state["reject"]
        if not ok:
            with lock: state["rejected"] += 1
            self.send(401, {"code": 2001, "reason": "UNAUTHORIZED", "msg": "未登录"})
        return ok
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0)) or 0)
        if not self.authed(): return
        if self.path.startswith("/api/v1/tools/"):
            with lock:
                key = self.headers.get("Idempotency-Key"); f = state["fail"]; state["fail"] = ""
                old = state["idem"].get(key) if key and f != "before_create" else None
                if f == "before_create":
                    tid = None
                elif old is not None:
                    if old["body"] != body:
                        return self.send(200, {"code": 1003, "msg": "同一幂等键已用于内容不同的请求，请更换 Idempotency-Key"})
                    tid = old["tid"]
                else:
                    state["n"] += 1; tid = "JT%04d" % state["n"]; state["tmode"][tid] = state["mode"]
                    if key: state["idem"][key] = {"body": body, "tid": tid}
                    state["seen"].append({"path": self.path, "auth": self.headers.get("Authorization"), "idem": key, "body": json.loads(body or b"{}")})
                state["submits"].append({"idem": key, "tid": tid, "replay": old is not None})
            if f:
                b = b"<html>bad gateway</html>"
                self.send_response(502); self.send_header("Content-Length", str(len(b))); self.end_headers(); self.wfile.write(b); return
            return self.send(200, {"code": 0, "msg": "success", "data": {"taskId": tid, "taskIds": [tid]}})
        if self.path.startswith("/api/v1/creation/") and self.path.endswith("/cancel"):
            tid = self.path.split("/")[4]
            with lock:
                state["cancels"].append({"tid": tid, "auth": self.headers.get("Authorization")})
                m = state["tmode"].get(tid)
                if m is None: return self.send(200, {"code": 4001, "msg": "创作任务不存在: " + tid})
                if tid in state["final"] or (m == "normal" and state["polls"].get(tid, 0) >= 2):
                    return self.send(200, {"code": 3001, "msg": "任务已完成或失败，无法取消"})
                if m == "finish_on_cancel":
                    state["final"][tid] = "SUCCEED"
                    return self.send(200, {"code": 3001, "msg": "任务已完成或失败，无法取消"})
                state["final"][tid] = "CANCELLED"
            return self.send(200, {"code": 0, "msg": "success", "data": True})
        self.send(404, {"code": 4001, "msg": "not found"})
    def do_GET(self):
        if self.path.startswith("/debug/"):
            from urllib.parse import urlparse, parse_qs
            u = urlparse(self.path); q = {k: v[0] for k, v in parse_qs(u.query).items()}
            with lock:
                if u.path == "/debug/mode": state["mode"] = q.get("m", "normal")
                if u.path == "/debug/reject": state["reject"] = q.get("on") == "1"
                if u.path == "/debug/fail": state["fail"] = q.get("m", "")
                return self.send(200, {"cancels": state["cancels"], "rejected": state["rejected"], "mode": state["mode"], "submits": state["submits"]})
        if self.path.startswith("/debug"): return self.send(200, state["seen"])
        if not self.authed(): return
        if self.path.startswith("/api/v1/tasks/"):
            tid = self.path.split("/")[4].split("?")[0]
            with lock:
                c = state["polls"].get(tid, 0) + 1; state["polls"][tid] = c
                m, fin = state["tmode"].get(tid, "normal"), state["final"].get(tid)
            st = fin or ("RUNNING" if c < 2 or m != "normal" else "SUCCEED")
            return self.send(200, {"code": 0, "data": {"taskUid": tid, "status": st, "result": {"url": img if st == "SUCCEED" else None}}})
        if self.path.startswith("/api/v1/features/"):
            return self.send(200, {"code": 0, "data": [{"model": "creaft-image-2", "points": 12}]})
        self.send(404, {"code": 4001, "msg": "not found"})
ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
PY
  if ! up $MEDIA_PORT; then MOCK_MEDIA_TOKEN=change-me "$TMP/bin/mock-media" -addr 127.0.0.1:$MEDIA_PORT > "$TMP/mock-media.log" 2>&1 & PIDS+=($!); fi
  if ! up $AIG_PORT; then MOCK_AIG_SECRET=test-secret MOCK_AIG_MEDIA_URL=http://127.0.0.1:$MEDIA_PORT MOCK_AIG_MEDIA_TOKEN=change-me \
       "$TMP/bin/mock-aigateway" -addr :$AIG_PORT > "$TMP/mock-aigateway.log" 2>&1 & PIDS+=($!); fi
  python3 -I "$TMP/mock_java.py" $JAVA_PORT "$USER_TOKEN" "http://127.0.0.1:$AIG_PORT/img/7.png" > "$TMP/mock-java.log" 2>&1 & PIDS+=($!)
  wait_port $MEDIA_PORT mock-media; wait_port $AIG_PORT mock-aigateway; wait_port $JAVA_PORT mock-java

  # 配置副本：mkconf 从 deploy/etc 生成（rpc 直连 Endpoints、不用 etcd），再把存储改回 local、所有 rpc 客户端 NonBlock
  "$TMP/bin/mkconf" -src "$ROOT/deploy/etc" -out "$TMP/etc" -logs "$TMP/logs" -log-console \
    -dsn "root:123456@tcp(127.0.0.1:$DB_PORT)/fusion_aigc?charset=utf8mb4&parseTime=true&loc=Asia%2FShanghai&time_zone=%27%2B08%3A00%27" \
    -aigc http://127.0.0.1:5055 -auth-mode dev -session-redis 127.0.0.1:$REDIS_PORT -session-redis-pass "$REDIS_PASS" \
    -ai-gateway-url http://127.0.0.1:$AIG_PORT -ai-gateway-secret test-secret -media-token change-me -media-base-url http://127.0.0.1:$MEDIA_PORT \
    -canvas-java-base-url http://127.0.0.1:$JAVA_PORT -swagger=false \
    -ports "copywriting=29201,digitalhuman=29202,note=29203,imagegen=29204,studio=29205,assetlib=29206,canvas=$CANVAS_PORT,gateway=$GW_PORT,admin=$ADMIN_PORT" >/dev/null
  python3 -I - "$TMP/etc" <<'PY'
import re, sys, pathlib
for p in pathlib.Path(sys.argv[1]).glob("*.yaml"):
    s = p.read_text()
    s = re.sub(r"(?m)^(Storage:\n(?:  .*\n)*?)  Mode: oss", r"\1  Mode: local", s)
    s = re.sub(r"(?m)^  NacosServerAddr: .*\n", "", s)          # 媒体服务只走静态地址（mock-media）
    out = []
    for line in s.splitlines():
        out.append(line)
        if re.match(r"^\w+Rpc:", line): out.append("  NonBlock: true")
    p.write_text("\n".join(out) + "\n")
PY
  if [[ $BILLING == 1 ]]; then # 只改副本：canvas-rpc 真扣（Reclaimer 随 Mode=on 启动）
    python3 -I - "$TMP/etc/canvas-rpc.yaml" <<'PY'
import re, sys, pathlib
p = pathlib.Path(sys.argv[1]); s = p.read_text()
s, n = re.subn(r'(?m)^(Billing:\n(?:  .*\n)*?)  Mode: .*\n', r'\1  Mode: "on"\n', s)
assert n == 1, "canvas-rpc.yaml 里没找到 Billing.Mode"
p.write_text(s)
PY
  fi
  "$TMP/bin/canvas-rpc"  -f "$TMP/etc/canvas-rpc.yaml"  > "$TMP/canvas-rpc.log" 2>&1 & PIDS+=($!)
  "$TMP/bin/gateway-api" -f "$TMP/etc/gateway-api.yaml" > "$TMP/gateway-api.log" 2>&1 & PIDS+=($!)
  "$TMP/bin/admin-api"   -f "$TMP/etc/admin-api.yaml"   > "$TMP/admin-api.log" 2>&1 & PIDS+=($!)
  wait_port $CANVAS_PORT canvas-rpc; wait_port $GW_PORT gateway-api; wait_port $ADMIN_PORT admin-api
}
[[ $START == 1 ]] && start_all

# 准入：画布的 image / video / 反推按入口码查能力（billing.CheckAccess），给本次用户发两个能力码（结束时删除）。
# 入口码的计费方案见 deploy/sql/ops/canvas-offers-2026-10-31.sql（缺了会 2005「暂未开通该功能」）。
sql "INSERT INTO afv_app_user_feature (user_id, feature_code, source, status) VALUES ($USER_ID,'ecom_image_gen','E2E','ACTIVE'),($USER_ID,'ecom_video_gen','E2E','ACTIVE')"
[[ $(sql "SELECT COUNT(*) FROM afv_billing_offer WHERE entry_code IN ('canvas_image','canvas_video','canvas_tool_i2p') AND deleted=0") == 3 ]] \
  || { red "缺计费方案：先执行 deploy/sql/ops/canvas-offers-2026-10-31.sql"; exit 2; }
if [[ $BILLING == 1 ]]; then
  bill_seed "$USER_ID" $BILL_INIT
  curl -s -o /dev/null "http://127.0.0.1:$AIG_PORT/debug/inject?reset=1"
  U_IMG1K=$(bill_unit_price canvas_image 1K); U_I2P=$(bill_unit_price canvas_tool_i2p "")
  echo "  计费：用户 $USER_ID ${BILL_INIT} 点；canvas_image 1K 单价 ${U_IMG1K}、i2p 单价 ${U_I2P}"
fi
# run_req 请求号 → run 的计费请求号（无则空）
run_req() { sql "SELECT COALESCE(billing_request_id,'') FROM afv_canvas_node_run WHERE id=$1"; }

# 等 gateway → canvas-rpc 链路真正可用（rpc 客户端是 NonBlock，端口起来不等于连上）
for _ in $(seq 1 60); do
  [[ $(curl -s -H "X-User-Id: $USER_ID" "$GW/api/v1/prompt-library/prompts?page_size=1" | jq -r .code 2>/dev/null) == 0 ]] && break; sleep 0.5
done

# 会话：用户 Bearer（共用会话格式，网关与 Java 都认）、有 canvas 权限的管理员（super_admin）、不存在的管理员（无权限）
SUPER_ADMIN=$(sql "SELECT u.id FROM afv_admin_user u JOIN afv_admin_user_role ur ON ur.admin_id=u.id JOIN afv_admin_role r ON r.id=ur.role_id
                   WHERE r.role_code='super_admin' AND u.deleted=0 AND u.status=1 LIMIT 1")
rcli SET "fusion:token:$USER_TOKEN" "{\"userId\":$USER_ID,\"username\":\"e2e\",\"currentTeamId\":0,\"issuedAt\":$(date +%s)}" EX 3600
rcli SET "fusion:admin:access:$ADMIN_TOKEN" "{\"adminId\":${SUPER_ADMIN:-1},\"username\":\"e2e\",\"roleCode\":\"super_admin\",\"issuedAt\":$(date +%s)}" EX 3600
rcli SET "fusion:admin:access:$NOPERM_TOKEN" "{\"adminId\":987654321,\"username\":\"noperm\",\"roleCode\":\"none\",\"issuedAt\":$(date +%s)}" EX 3600

# ---------------- 请求助手 ----------------
call() { # call METHOD PATH [BODY] [IDEM]
  local args=(-sS -X "$1" -H "Authorization: Bearer $USER_TOKEN" -H 'Content-Type: application/json')
  [[ -n ${4:-} ]] && args+=(-H "Idempotency-Key: $4")
  [[ -n ${3:-} ]] && args+=(--data-binary "$3")
  curl "${args[@]}" "$GW$2"
}
code_of() { jq -r '.code' <<<"$1"; }
ok() { local r; r=$(call "$@"); if [[ $(code_of "$r") != 0 ]]; then red "FAIL $1 $2 → $r" >&2; exit 1; fi; jq -c '.data' <<<"$r"; }
# 断言失败立即以 1 退出（后续步骤依赖前面的结果）；E2E_FAIL 计数供汇总，cleanup 保留退出码
check() { if [[ "$2" == "$3" ]]; then green "  ✓ $1"; PASS=$((PASS+1)); else red "  ✗ $1: got [$2] want [$3]"; E2E_FAIL=$((E2E_FAIL+1)); exit 1; fi; }
want_code() { local c; c=$(code_of "$3"); if [[ $c == "$2" ]]; then green "  ✓ $1（code=${c}：$(jq -r '.msg' <<<"$3")）"; PASS=$((PASS+1)); else red "  ✗ $1: want code=$2 got $3"; E2E_FAIL=$((E2E_FAIL+1)); exit 1; fi; }
poll_run() { # poll_run REQUEST_ID → 终态 run
  local d
  for _ in $(seq 1 60); do
    d=$(ok GET "/api/v1/canvas/runs/$1")
    case $(jq -r .status <<<"$d") in succeeded|failed|cancelled) echo "$d"; return;; esac
    sleep 1
  done
  red "run $1 超时：$d" >&2; exit 1
}

IMG=http://127.0.0.1:$AIG_PORT/img/3.png
echo "== 用户 $USER_ID"

echo "== 画布 CRUD 与乐观锁"
C=$(ok POST /api/v1/canvas/canvases '{"title":"e2e 画布"}')
CID=$(jq -r .id <<<"$C"); REV0=$(jq -r .revision <<<"$C")
check "新建画布 id 前缀 cv_、空快照" "$(jq -r '(.id|startswith("cv_")) and (.snapshot.nodes|length==0)' <<<"$C")" true
check "列表里有它" "$(ok GET '/api/v1/canvas/canvases?page=1&page_size=50' | jq --arg id "$CID" '[.items[]|select(.id==$id)]|length')" 1

SNAP=$(jq -nc --arg img "$IMG" '{nodes:[
  {id:"t1",type:"text",position:{x:0,y:0},data:{kind:"text",title:"文字",text:"白底棚拍，商品居中"}},
  {id:"i1",type:"image",position:{x:0,y:200},data:{kind:"input",title:"图片",image:$img,assetId:"88001"}},
  {id:"m1",type:"model",position:{x:400,y:100},data:{kind:"model",title:"生图",model:"Banana",modelId:"banana-v1",ratio:"1:1",size:"1K",count:1,status:"done",candidates:["https://old/x.png"]}},
  {id:"v1",type:"model",position:{x:800,y:100},data:{kind:"video",title:"视频",model:"Seedance 2.0",modelId:"seedance-2-0"}},
  {id:"p1",type:"tool",position:{x:400,y:400},data:{kind:"tool",tool:"i2p",title:"反推",sourceNodeId:"i1"}},
  {id:"c1",type:"tool",position:{x:400,y:600},data:{kind:"tool",tool:"crop",title:"裁剪",sourceNodeId:"i1",crop:{x:0.25,y:0.25,w:0.5,h:0.5,shape:"rect"}}},
  {id:"u1",type:"tool",position:{x:400,y:800},data:{kind:"tool",tool:"upscale",title:"放大",sourceNodeId:"i1"}}],
 edges:[
  {id:"e1",source:"t1",sourceHandle:"text",target:"m1",targetHandle:"prompt"},
  {id:"e2",source:"i1",sourceHandle:"image",target:"m1",targetHandle:"image",data:{order:1}},
  {id:"e3",source:"m1",sourceHandle:"image-out",target:"v1",targetHandle:"image1"}],
 viewport:{x:0,y:0,zoom:1}}')
save() { jq -nc --argjson s "$1" --arg rev "${2:-}" --argjson force "${3:-false}" '{snapshot:$s, if_match_revision:$rev, force:$force}'; }

S1=$(ok PUT "/api/v1/canvas/canvases/$CID" "$(save "$SNAP" "$REV0")")
REV1=$(jq -r .revision <<<"$S1")
check "保存成功：node_count=7，revision 换新（<毫秒>-<4hex>）" "$(jq -r '.node_count' <<<"$S1"),$( [[ $REV1 =~ ^[0-9]{13}-[0-9a-f]{4}$ && $REV1 != "$REV0" ]] && echo ok)" "7,ok"
R=$(call PUT "/api/v1/canvas/canvases/$CID" "$(save "$SNAP" "$REV0")")
want_code "旧 revision 保存 → 冲突" 4002 "$R"
check "冲突回包 data.conflict=revision 且带服务端 revision" "$(jq -r '.data.conflict+","+.data.revision' <<<"$R")" "revision,$REV1"
S2=$(ok PUT "/api/v1/canvas/canvases/$CID" "$(save "$SNAP" "$REV0" true)")
REV2=$(jq -r .revision <<<"$S2")
check "force=true 覆盖保存" "$( [[ $REV2 != "$REV1" ]] && echo ok)" ok

BAD=$(jq -c '.nodes[1].data.image="data:image/png;base64,iVBORw0KGgo="' <<<"$SNAP")
want_code "节点里有 dataURL → 1003" 1003 "$(call PUT "/api/v1/canvas/canvases/$CID" "$(save "$BAD" "" true)")"
BAD=$(jq -c '.nodes += [{id:"t2",type:"text",data:{text:"b"}}] | .edges += [{id:"e9",source:"t2",sourceHandle:"text",target:"m1",targetHandle:"prompt"}]' <<<"$SNAP")
want_code "提示词口接 2 条 → 1003" 1003 "$(call PUT "/api/v1/canvas/canvases/$CID" "$(save "$BAD" "" true)")"
BAD=$(jq -c '.edges += [{id:"e8",source:"v1",sourceHandle:"video",target:"m1",targetHandle:"image"}, {id:"e7",source:"m1",sourceHandle:"image-out",target:"v1",targetHandle:"image"}]' <<<"$SNAP")
want_code "成环 → 1003" 1003 "$(call PUT "/api/v1/canvas/canvases/$CID" "$(save "$BAD" "" true)")"

G=$(ok GET "/api/v1/canvas/canvases/$CID")
check "取画布：snapshot 外包一层、未知顶层键保留、旧运行态已剥离（m1 无 run 前保留成品）" \
  "$(jq -r '[.revision==env.REV2, (.snapshot.viewport.zoom==1), (.snapshot.nodes|length==7), (.snapshot.nodes[]|select(.id=="m1")|.data.status==null)]|all' <<<"$G" REV2=$REV2 2>/dev/null || jq -r --arg r "$REV2" '[.revision==$r, (.snapshot.viewport.zoom==1), (.snapshot.nodes|length==7), (.snapshot.nodes[]|select(.id=="m1")|.data.status==null)]|all' <<<"$G")" true

echo "== 执行节点：image（mock ai-gateway）"
RUN_BODY=$(jq -nc --arg img "$IMG" '{run_id:"__ID__",kind:"image",model:"Banana",model_id:"banana-v1",prompt:"",ratio:"1:1",size:"1K",count:1,cfg:0.95,
  references:[{handle:"prompt",node_id:"t1",text:"白底棚拍，商品居中"},{handle:"image",index:1,node_id:"i1",image:$img,asset_id:"88001"}]}')
KEY=$(uuidgen); B=${RUN_BODY/__ID__/$KEY}
want_code "缺 Idempotency-Key → 1002" 1002 "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$B")"
K2=$(uuidgen); want_code "count=5 超上限 → 1003" 1003 "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$(jq -c --arg k "$K2" '.count=5|.run_id=$k' <<<"$B")" "$K2")"
K2=$(uuidgen); want_code "引用没连线的节点 → 1003" 1003 "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$(jq -c --arg k "$K2" '.references[1].node_id="c1"|.run_id=$k' <<<"$B")" "$K2")"
K2=$(uuidgen); want_code "Idempotency-Key 与 run_id 不一致 → 1003" 1003 "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$B" "$K2")"
RN=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$B" "$KEY")
RID=$(jq -r .request_id <<<"$RN")
check "提交返回 request_id、未重放" "$(jq -r '(.request_id|test("^[0-9]+$")) and (.replayed==false) and (.run_id==env.KEY)' <<<"$RN" KEY=$KEY 2>/dev/null || jq -r --arg k "$KEY" '(.request_id|test("^[0-9]+$")) and (.replayed==false) and (.run_id==$k)' <<<"$RN")" true
check "同 key 同 body → 重放原 run" "$(ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$B" "$KEY" | jq -r '"\(.replayed),\(.request_id)"')" "true,$RID"
want_code "同 key 不同 body → 4002" 4002 "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$(jq -c '.prompt="改了"' <<<"$B")" "$KEY")"
D=$(poll_run "$RID")
check "run 到 succeeded，候选是对象数组 {url,width,height}" "$(jq -r '"\(.status),\(.candidates|length),\(.candidates[0].width),\(.progress),\(.requested_count)"' <<<"$D")" "succeeded,1,64,100,1"
CAND=$(jq -r '.candidates[0].url' <<<"$D")
check "候选已转存素材库桶（mock-media 公有地址）且可下载" "$( [[ $CAND == http://127.0.0.1:$MEDIA_PORT/obj/* ]] && curl -s -o /dev/null -w '%{http_code}' "$CAND")" 200
G=$(ok GET "/api/v1/canvas/canvases/$CID")
check "GetCanvas 节点 m1 回填 status=done / candidates / image / runId" \
  "$(jq -r --arg c "$CAND" --arg k "$KEY" '.snapshot.nodes[]|select(.id=="m1")|.data|"\(.status),\(.candidates[0]==$c),\(.image==$c),\(.runId==$k)"' <<<"$G")" "done,true,true,true"
check "画布 run 列表" "$(ok GET "/api/v1/canvas/canvases/$CID/runs?node_id=m1" | jq -r '.items|length')" 1
want_code "取消已结束的 run → 4002" 4002 "$(call POST "/api/v1/canvas/runs/$RID/cancel")"
if [[ $BILLING == 1 ]]; then
  check "计费：image count=1 → 预留单 CONFIRMED 1 × 单价、run 已结算" \
    "$(bill_res_status "$USER_ID" "$(run_req "$RID")"),$(bill_res_points "$USER_ID" "$(run_req "$RID")"),$(sql "SELECT CONCAT(billing_status,',',points) FROM afv_canvas_node_run WHERE id=$RID")" \
    "CONFIRMED,$U_IMG1K,settled,$U_IMG1K"
fi
check "任务中心已同步（origin_ref cv:<id>、entry canvas_image、成功）" \
  "$(sql "SELECT CONCAT(entry_code,',',status) FROM afv_creation_task WHERE origin_ref='cv:$RID' AND user_id=$USER_ID")" "canvas_image,2"
check "素材库入库（source_task_id=task_id、entry canvas_image、relatedIds 来自 asset_id）" \
  "$(sql "SELECT CONCAT(a.entry_code,',',JSON_EXTRACT(a.properties,'$.relatedIds[0]')) FROM afv_asset a JOIN afv_canvas_node_run r ON r.task_id=a.source_task_id WHERE r.id=$RID")" 'canvas_image,"88001"'

echo "== 执行节点：视频 / 反推 / 裁剪 / 放大（Java）"
SAVE_AFTER=$(ok PUT "/api/v1/canvas/canvases/$CID" "$(save "$SNAP" "" true)")
check "保存时 m1 有 run → 成品字段剥离、GET 时回填（快照库里没有 candidates）" \
  "$(sql "SELECT JSON_EXTRACT(snapshot,'$.nodes[2].data.candidates') IS NULL FROM afv_canvas WHERE id='$CID'")" 1
VB=$(jq -nc --arg c "$CAND" '{run_id:"v",kind:"video",model:"Seedance 2.0",model_id:"seedance-2-0",prompt:"走秀",ratio:"16:9",size:"720p",duration:"4s",sound:false,count:1,
  references:[{handle:"image1",node_id:"m1",image:"https://stale/x.png",source_handle:"candidate-1",candidate_index:0}]}')
K=$(uuidgen); VR=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/v1/run" "$(jq -c --arg k "$K" '.run_id=$k' <<<"$VB")" "$K")
V=$(poll_run "$(jq -r .request_id <<<"$VR")")
if [[ $BILLING == 1 ]]; then
  VID=$(jq -r .request_id <<<"$VR"); VREQ=$(run_req "$VID"); VPTS=$(bill_res_points "$USER_ID" "$VREQ")
  check "计费：视频按秒冻结并结算（CONFIRMED，预留点数 = run.points > 0）" \
    "$(bill_res_status "$USER_ID" "$VREQ"),$( [[ $VPTS == $(sql "SELECT points FROM afv_canvas_node_run WHERE id=$VID") && $VPTS -gt 0 ]] && echo ok)" "CONFIRMED,ok"
fi
check "视频 run 成功：video_url + 封面候选；首帧取服务端记录的上游候选" "$(jq -r '"\(.status),\(.video_url!=""),\(.candidates|length)"' <<<"$V")" "succeeded,true,1"
check "视频首帧经媒体服务拉成 mr://（gateway 收到的 first_frame 不是前端传的过期地址）" \
  "$(curl -s "http://127.0.0.1:$AIG_PORT/debug/calls" | jq -r '[.calls[]|select(.model_group=="vid.seedance-2")]|length>0')" true
IB=$(jq -nc --arg img "$IMG" '{run_id:"x",kind:"tool",tool:"i2p",references:[{handle:"image",node_id:"i1",image:$img}],params:{language:"zh"}}')
K=$(uuidgen); IR=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/p1/run" "$(jq -c --arg k "$K" '.run_id=$k' <<<"$IB")" "$K")
I=$(poll_run "$(jq -r .request_id <<<"$IR")")
check "反推（chat.completion 多模态）→ text 非空" "$(jq -r '"\(.status),\(.text|length>0),\(.candidates|length)"' <<<"$I")" "succeeded,true,0"
if [[ $BILLING == 1 ]]; then
  IREQ=$(run_req "$(jq -r .request_id <<<"$IR")")
  check "计费：i2p 由 Go 冻结并结算（CONFIRMED ${U_I2P} 点、run 已结算）" \
    "$(bill_res_status "$USER_ID" "$IREQ"),$(bill_res_points "$USER_ID" "$IREQ"),$(sql "SELECT billing_status FROM afv_canvas_node_run WHERE id=$(jq -r .request_id <<<"$IR")")" "CONFIRMED,$U_I2P,settled"
fi
CB=$(jq -nc --arg img "$IMG" '{run_id:"x",kind:"tool",tool:"crop",references:[{handle:"image",node_id:"i1",image:$img}],params:{shape:"rect",x:0.25,y:0.25,w:0.5,h:0.5}}')
K=$(uuidgen); CR=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/c1/run" "$(jq -c --arg k "$K" '.run_id=$k' <<<"$CB")" "$K")
if [[ $BILLING == 1 ]]; then
  check "计费：裁剪不计费（无计费请求号、billing_status=none、该用户预留单仍是 image/video/i2p 三张）" \
    "$(run_req "$(jq -r .request_id <<<"$CR")")|$(sql "SELECT billing_status FROM afv_canvas_node_run WHERE id=$(jq -r .request_id <<<"$CR")")|$(bill_sql "SELECT COUNT(*) FROM afv_billing_reservation WHERE user_id=$USER_ID")" "|none|3"
fi
check "裁剪同步完成：32×32（64 图取中间一半）、不计点" "$(ok GET "/api/v1/canvas/runs/$(jq -r .request_id <<<"$CR")" | jq -r '"\(.status),\(.candidates[0].width)x\(.candidates[0].height),\(.credits)"')" "succeeded,32x32,0"
UB=$(jq -nc --arg img "$IMG" '{run_id:"x",kind:"tool",tool:"upscale",references:[{handle:"image",node_id:"i1",image:$img}],params:{scale:2}}')
K=$(uuidgen); UR=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/u1/run" "$(jq -c --arg k "$K" '.run_id=$k' <<<"$UB")" "$K")
U=$(poll_run "$(jq -r .request_id <<<"$UR")")
check "放大经 Java 兼容接口到 succeeded，结果已转存" "$(jq -r '"\(.status),\(.candidates|length)"' <<<"$U")" "succeeded,1"
SEEN=$(curl -s "http://127.0.0.1:$JAVA_PORT/debug" | jq -c '.[-1]')
check "Java 收到用户 Bearer 原文、Idempotency-Key=canvas-<task_id>、驼峰字段 imageUrl/resolution=2k" \
  "$(jq -r --arg t "Bearer $USER_TOKEN" '"\(.path),\(.auth==$t),\(.idem|startswith("canvas-cv_")),\(.body.imageUrl!=null),\(.body.resolution)"' <<<"$SEEN")" \
  "/api/v1/tools/ai-image-upscaler,true,true,true,2k"
check "Java 工具计费归 Java（billing_status=java）" "$(sql "SELECT billing_status FROM afv_canvas_node_run WHERE id=$(jq -r .request_id <<<"$UR")")" java
if [[ $BILLING == 1 ]]; then
  check "计费：Java 工具不由 Go 冻结（无计费请求号、按 canvas-<task_id> 查不到预留单）" \
    "$(run_req "$(jq -r .request_id <<<"$UR")")|$(bill_res_count "$USER_ID" "canvas-$(sql "SELECT task_id FROM afv_canvas_node_run WHERE id=$(jq -r .request_id <<<"$UR")")")" "|0"
fi
TB=$(jq -nc --arg img "$IMG" '{run_id:"x",kind:"tool",tool:"tryon",params:{garment_image:$img,models:[{id:"m1",image:$img}]}}')
K=$(uuidgen); TR=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/u1/run" "$(jq -c --arg k "$K" '.run_id=$k' <<<"$TB")" "$K")
TRID=$(jq -r .request_id <<<"$TR")
check "试穿：本地没有 studio-rpc → 提交暂时失败留在排队（轮询器重提）" "$(jq -r .status <<<"$TR"),$(sql "SELECT billing_status FROM afv_canvas_node_run WHERE id=$TRID")" "queued,studio"
check "取消排队中的 run → cancelled（「已取消执行」）" "$(ok POST "/api/v1/canvas/runs/$TRID/cancel" | jq -r .status),$(ok GET "/api/v1/canvas/runs/$TRID" | jq -r .error)" "cancelled,已取消执行"

echo "== 回填与候选选择（状态取最近 run、成品取最近成功 run；选中写回 run）"
BAL2=$( [[ $BILLING == 1 ]] && bill_snap "$USER_ID" || true)
K2=$(uuidgen); R2=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$(jq -c --arg k "$K2" '.run_id=$k|.count=2' <<<"$B")" "$K2")
D2=$(poll_run "$(jq -r .request_id <<<"$R2")")
check "count=2 的 run 成功，2 张候选" "$(jq -r '"\(.status),\(.candidates|length)"' <<<"$D2")" "succeeded,2"
if [[ $BILLING == 1 ]]; then
  R2REQ=$(run_req "$(jq -r .request_id <<<"$R2")")
  check "计费：count=2 全成功按 2 张结算（CONFIRMED $((2 * U_IMG1K)) 点）" \
    "$(bill_res_status "$USER_ID" "$R2REQ"),$(bill_res_points "$USER_ID" "$R2REQ"),$(bill_logs "$USER_ID" "$R2REQ")" \
    "CONFIRMED,$((2 * U_IMG1K)),FREEZE:-$((2 * U_IMG1K)) CONSUME:-$((2 * U_IMG1K))"
  assert_balance_delta "计费：余额减少 2 × 单价" "$USER_ID" "$BAL2" $((-2 * U_IMG1K)) 0
fi
C2=$(jq -r '.candidates[1].url' <<<"$D2")
SEL=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/select-candidate" "$(jq -nc --arg r "$K2" '{run_id:$r,index:1}')")
check "选中第 2 张：写回 selected_index=1" "$(jq -r .selected_index <<<"$SEL")" 1
want_code "选中越界 → 1003" 1003 "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/select-candidate" "$(jq -nc --arg r "$K2" '{run_id:$r,index:5}')")"
K3=$(uuidgen)
want_code "第三次 run 输入图拉取失败 → 1003（run 置失败）" 1003 \
  "$(call POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$(jq -c --arg k "$K3" --arg u "http://127.0.0.1:$AIG_PORT/missing.png" '.run_id=$k|.references[1].image=$u' <<<"$B")" "$K3")"
check "第三次 run 已落库为失败（PARAM_INVALID，task_id 落库即有）" "$(sql "SELECT CONCAT(status,',',error_code,',',task_id IS NOT NULL) FROM afv_canvas_node_run WHERE run_id='$K3' AND user_id=$USER_ID")" "3,PARAM_INVALID,1"
G=$(ok GET "/api/v1/canvas/canvases/$CID")
check "最近 run 失败后：m1 状态 error，仍显示上一次成功的 2 张候选" \
  "$(jq -r --arg k "$K3" '.snapshot.nodes[]|select(.id=="m1")|.data|"\(.status),\(.runId==$k),\(.candidates|length)"' <<<"$G")" "error,true,2"
check "刷新后选中仍是第 2 张（selectedCandidate=1、image=第 2 张）" \
  "$(jq -r --arg c "$C2" '.snapshot.nodes[]|select(.id=="m1")|.data|"\(.selectedCandidate),\(.image==$c)"' <<<"$G")" "1,true"
G=$(ok GET "/api/v1/canvas/canvases/$CID")
check "再刷新一次仍是第 2 张" "$(jq -r '.snapshot.nodes[]|select(.id=="m1")|.data.selectedCandidate' <<<"$G")" 1
want_code "不带 if_match_revision 又不 force 的保存 → 1002" 1002 "$(call PUT "/api/v1/canvas/canvases/$CID" "$(save "$SNAP" "" false)")"
check "同节点在途唯一（uk_active_node 生成列存在）" "$(sql "SELECT COUNT(*) FROM information_schema.statistics WHERE table_schema=DATABASE() AND table_name='afv_canvas_node_run' AND index_name='uk_active_node'")" 1

echo "== Seedream 4K：受理即按 2K 归一（快照 / 冻结 / gateway 请求一致），回 warnings"
KS=$(uuidgen)
SB=$(jq -c --arg k "$KS" '.run_id=$k|.model="SD5P"|.model_id="sd5p-v1"|.size="4K"|.ratio="16:9"' <<<"$B")
SR=$(ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$SB" "$KS"); SRID=$(jq -r .request_id <<<"$SR")
check "RunNode 回 warnings（Seedream 4K → 2K）" "$(jq -r '"\(.warnings|length),\(.warnings[0]|test("4K"))"' <<<"$SR")" "1,true"
check "run.request 快照存归一值 size=2K、模型组 seedream" \
  "$(sql "SELECT CONCAT(JSON_UNQUOTE(JSON_EXTRACT(request,'\$.size')),',',model_group) FROM afv_canvas_node_run WHERE id=$SRID")" "2K,img.seedream-5-pro"
check "同 key 同 body（仍是 4K）→ 重放原 run、同样回 warnings" \
  "$(ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$SB" "$KS" | jq -r '"\(.replayed),\(.request_id),\(.warnings|length)"')" "true,${SRID},1"
check "Seedream run 成功" "$(poll_run "$SRID" | jq -r .status)" succeeded
check "非 Seedream 不回告警（warnings 为 []）" "$(jq -r '.warnings|length' <<<"$RN")" 0
if [[ $BILLING == 1 ]]; then
  U_IMG2K=$(bill_unit_price canvas_image 2K); U_IMG4K=$(bill_unit_price canvas_image 4K); SREQ=$(run_req "$SRID")
  check "计费：Seedream 4K 冻结额 = 2K 单价（${U_IMG2K}，4K 单价 ${U_IMG4K}）、结算 CONFIRMED、run.points 一致" \
    "$(bill_res_status "$USER_ID" "$SREQ"),$(bill_res_points "$USER_ID" "$SREQ"),$(sql "SELECT points FROM afv_canvas_node_run WHERE id=$SRID")" \
    "CONFIRMED,${U_IMG2K},${U_IMG2K}"
fi

echo "== 目录 / 模板 / 另存为"
CAT=$(ok GET /api/v1/canvas/catalog)
check "目录：Banana 参考图上限取 gateway（mock=10）、6 个工具、Java 价目透传" \
  "$(jq -r '"\([.image_models[]|select(.model=="Banana")][0].max_image_refs),\(.tools|length),\(.java_image_configs[0].points)"' <<<"$CAT")" "10,6,12"
T=$(ok GET '/api/v1/canvas/templates?scope=official')
TID=$(jq -r '.items[0].id' <<<"$T")
check "官方模板 3 个；列表不带快照（只给节点数），详情才带" \
  "$(jq -r '"\(.items|length),\(.items[0].snapshot==null),\(.items[0].node_count>0)"' <<<"$T"),$(ok GET "/api/v1/canvas/templates/$TID" | jq -r '.snapshot.nodes|length>0')" "3,true,true,true"
ER=$(jq -r '.items[]|select(.title=="元素替换")|.id' <<<"$T")
check "元素替换模板：参考视频接 video 口（不再占首帧口）" \
  "$(ok GET "/api/v1/canvas/templates/$ER" | jq -r '[.snapshot.edges[]|select(.id=="tpl-element-replace-e2")][0].targetHandle')" video
F=$(ok POST /api/v1/canvas/canvases/from-template "{\"template_id\":$TID}")
check "从模板建画布：节点 / 连线 id 全部重映射，内部连线保持" \
  "$(jq -r '[(.snapshot.nodes[].id|startswith("tpl-")|not), (.snapshot.edges[]|.id|startswith("tpl-")|not)]|all' <<<"$F"),$(jq -r '[.snapshot.edges[] as $e | [.snapshot.nodes[].id] | (index($e.source)!=null and index($e.target)!=null)]|all' <<<"$F")" "true,true"
SA=$(ok POST "/api/v1/canvas/canvases/$CID/save-as" '{}')
check "另存为：新 id、标题加「 - 副本」、副本无运行态" \
  "$(jq -r --arg id "$CID" '"\(.id!=$id),\(.title),\([.snapshot.nodes[].data.status]|map(select(.!=null))|length)"' <<<"$SA")" "true,e2e 画布 - 副本,0"
MT=$(ok POST /api/v1/canvas/templates "{\"canvas_id\":\"$CID\",\"title\":\"我的模板\"}")
check "存为我的模板，scope=user 可见、可删" "$(ok GET '/api/v1/canvas/templates?scope=user' | jq -r '.items|length'),$(code_of "$(call DELETE "/api/v1/canvas/templates/$(jq -r .id <<<"$MT")")")" "1,0"
want_code "删官方模板 → 4002" 4002 "$(call DELETE "/api/v1/canvas/templates/$TID")"

echo "== 提示词库"
L=$(ok GET '/api/v1/prompt-library/prompts?page_size=50')
OFF=$(jq -r '[.items[]|select(.owner=="official")]|length' <<<"$L")
check "官方提示词可见（启用的 ${OFF} 条）且都是 official" "$( [[ $OFF -ge 1 ]] && echo ok)" ok
P=$(ok POST /api/v1/prompt-library/prompts '{"title":"我的 · 香水冷调主图","prompt":"白瓷香水瓶放在浅蓝色背景上","models":["通用","GPT"],"tags":["香水"]}')
PID=$(jq -r .id <<<"$P")
check "新增我的提示词：vertical=mine group=favorite owner=mine" "$(jq -r '"\(.vertical),\(.group),\(.owner)"' <<<"$P")" "mine,favorite,mine"
check "vertical=mine 只看自己的" "$(ok GET '/api/v1/prompt-library/prompts?vertical=mine' | jq -r '"\(.total),\(.items[0].id)"')" "1,$PID"
check "按模型 + 关键词筛" "$(ok GET "/api/v1/prompt-library/prompts?model=GPT&q=%E9%A6%99%E6%B0%B4" | jq -r --argjson id "$PID" '[.items[].id]|index($id)!=null')" true
check "修改自己的" "$(ok PUT "/api/v1/prompt-library/prompts/$PID" '{"title":"改名","prompt":"新正文","tags":["a","b"]}' | jq -r '"\(.title),\(.tags|length)"')" "改名,2"
OID=$(jq -r '[.items[]|select(.owner=="official")][0].id' <<<"$L")
want_code "改官方提示词 → 4002" 4002 "$(call PUT "/api/v1/prompt-library/prompts/$OID" '{"title":"x","prompt":"y"}')"
want_code "正文超 3000 字 → 1003" 1003 "$(call POST /api/v1/prompt-library/prompts "$(jq -nc --arg p "$(printf '字%.0s' $(seq 1 3001))" '{title:"t",prompt:$p}')")"
check "删除自己的" "$(code_of "$(call DELETE "/api/v1/prompt-library/prompts/$PID")"),$(ok GET '/api/v1/prompt-library/prompts?vertical=mine' | jq -r .total)" "0,0"

echo "== 运营后台（/api/v1/canvas/admin、/api/v1/prompt-library/admin）"
adm() { curl -sS -X "$1" -H "Authorization: Bearer $2" -H 'Content-Type: application/json' ${4:+--data-binary "$4"} "$ADMIN$3"; }
A=$(adm GET "$ADMIN_TOKEN" "/api/v1/canvas/admin/runs?user_id=$USER_ID&page_size=50")
check "super_admin 看 run 列表（含 backend / billing_status / request）" "$(jq -r '"\(.code),\(.data.total>=5),\([.data.list[].backend]|unique|sort|join("/"))"' <<<"$A")" "0,true,gateway/java/media/studio"
check "后台成本汇总" "$(adm GET "$ADMIN_TOKEN" '/api/v1/canvas/admin/cost-summary?group_by=kind' | jq -r '.code')" 0
check "后台提示词列表（官方 19 条含未启用）" "$(adm GET "$ADMIN_TOKEN" '/api/v1/prompt-library/admin/prompts?page_size=50' | jq -r '"\(.code),\(.data.total)"')" "0,19"
want_code "无 canvas 权限的管理员 → 2005（canvas）" 2005 "$(adm GET "$NOPERM_TOKEN" /api/v1/canvas/admin/runs)"
want_code "无 canvas 权限的管理员 → 2005（prompt-library）" 2005 "$(adm GET "$NOPERM_TOKEN" /api/v1/prompt-library/admin/prompts)"
want_code "后台未登录 → 2001" 2001 "$(curl -sS "$ADMIN/api/v1/canvas/admin/runs")"

if [[ $BILLING == 1 ]]; then
  AIG=http://127.0.0.1:$AIG_PORT
  aig_inject() { curl -sS -X POST "$AIG/debug/inject" -d "$(jq -nc --arg m "$1" --arg o "$2" --arg c "${3:-}" --argjson t "${4:-0}" '{match:$m,outcome:$o,class:$c,times:$t}')" >/dev/null; }
  run_m1() { # run_m1 COUNT PROMPT → RunNode 回包 data（m1 节点，参考同 B）
    local k; k=$(uuidgen)
    ok POST "/api/v1/canvas/canvases/$CID/nodes/m1/run" "$(jq -c --arg k "$k" --arg p "$2" --argjson n "$1" '.run_id=$k|.count=$n|.prompt=$p' <<<"$B")" "$k"
  }

  echo "== 计费：image count=2 一张失败 → 按 1 张结算"
  MK="E2EPART$RANDOM"; aig_inject "$MK" FAILED INPUT_REJECTED 1
  BAL=$(bill_snap "$USER_ID")
  RR=$(run_m1 2 "部分失败 $MK"); RRID=$(jq -r .request_id <<<"$RR"); RREQ=$(run_req "$RRID")
  check "冻结 2 张（credits）" "$(jq -r .credits <<<"$RR")" $((2 * U_IMG1K))
  DD=$(poll_run "$RRID")
  check "run 成功、1 张候选" "$(jq -r '"\(.status),\(.candidates|length)"' <<<"$DD")" "succeeded,1"
  check "预留单 PARTIALLY_REFUNDED：确认 1 张、退回 1 张" \
    "$(bill_res_status "$USER_ID" "$RREQ"),$(bill_logs "$USER_ID" "$RREQ")" \
    "PARTIALLY_REFUNDED,FREEZE:-$((2 * U_IMG1K)) CONSUME:-$U_IMG1K REFUND:$U_IMG1K"
  check "run 计费镜像已结算、points = 1 张" "$(sql "SELECT CONCAT(billing_status,',',points) FROM afv_canvas_node_run WHERE id=$RRID")" "settled,$U_IMG1K"
  assert_balance_delta "只扣成功的 1 张" "$USER_ID" "$BAL" "-$U_IMG1K" 0

  echo "== 计费：取消排队中的 run → 退款"
  MK="E2E503$RANDOM"; aig_inject "$MK" SUBMIT_503 "" 0 # 提交一直 503：run 留在排队
  BAL=$(bill_snap "$USER_ID")
  RR=$(run_m1 1 "排队取消 $MK"); RRID=$(jq -r .request_id <<<"$RR"); RREQ=$(run_req "$RRID")
  check "提交暂时失败 → run 排队、已冻结 1 张" "$(jq -r .status <<<"$RR"),$(bill_res_status "$USER_ID" "$RREQ")" "queued,HELD"
  assert_balance_delta "排队时点数冻结" "$USER_ID" "$BAL" "-$U_IMG1K" "$U_IMG1K"
  check "取消 → cancelled" "$(ok POST "/api/v1/canvas/runs/$RRID/cancel" | jq -r .status)" cancelled
  check "预留单 REFUNDED、run 计费镜像 refunded" "$(bill_res_status "$USER_ID" "$RREQ"),$(sql "SELECT billing_status FROM afv_canvas_node_run WHERE id=$RRID")" "REFUNDED,refunded"
  assert_balance_delta "取消后全部退回" "$USER_ID" "$BAL" 0 0
  curl -s -o /dev/null "$AIG/debug/inject?reset=1"

  echo "== 计费：UNKNOWN → 待对账，到期由 Reclaimer 释放"
  MK="E2EUNK$RANDOM"; aig_inject "$MK" UNKNOWN "" 1
  BAL=$(bill_snap "$USER_ID")
  RR=$(run_m1 1 "结果未知 $MK"); RRID=$(jq -r .request_id <<<"$RR"); RREQ=$(run_req "$RRID")
  DD=$(poll_run "$RRID")
  check "run 失败（UPSTREAM_UNKNOWN）、计费镜像 review、预留单仍 HELD" \
    "$(jq -r .status <<<"$DD"),$(sql "SELECT CONCAT(error_code,',',billing_status) FROM afv_canvas_node_run WHERE id=$RRID"),$(bill_res_status "$USER_ID" "$RREQ")" \
    "failed,UPSTREAM_UNKNOWN,review,HELD"
  assert_balance_delta "点数仍冻结" "$USER_ID" "$BAL" "-$U_IMG1K" "$U_IMG1K"
  bill_expire "$USER_ID" "$RREQ"
  check "到期后回收器释放（HELD → EXPIRED）" "$(bill_wait_status "$USER_ID" "$RREQ" EXPIRED 90)" EXPIRED
  check "流水：冻结 → 过期解冻" "$(bill_logs "$USER_ID" "$RREQ")" "FREEZE:-$U_IMG1K UNFREEZE:$U_IMG1K"
  assert_balance_delta "余额回到原值" "$USER_ID" "$BAL" 0 0
  # Poller 每分钟的计费镜像对账：终态 + review 的 run 按预留单真实状态回写（EXPIRED → refunded）
  MIR=""
  for _ in $(seq 1 90); do
    MIR=$(sql "SELECT billing_status FROM afv_canvas_node_run WHERE id=$RRID"); [[ $MIR == refunded ]] && break; sleep 1
  done
  check "释放后计费镜像 review → refunded（守护按预留单 EXPIRED 回写）" "$MIR" refunded

  echo "== 计费：对账"
  check "画布 run 计费镜像与预留单一致：0 行不一致" "$(bill_canvas_mismatch "$USER_ID")" 0
  assert_ledger_consistent "账务自洽" "$USER_ID" $BILL_INIT
  echo "  测试用户：$(bill_snap "$USER_ID" | awk -v i=$BILL_INIT '{printf "初始 %d → 可用 %d、冻结 %d（共扣 %d）", i, $1, $2, i-$1-$2}')"
fi

echo "== 删除"
check "删除画布后取不到（4001）" "$(code_of "$(call DELETE "/api/v1/canvas/canvases/$CID")"),$(code_of "$(call GET "/api/v1/canvas/canvases/$CID")")" "0,4001"

green "全部通过：$PASS 项"

# ---------------- Java 工具真实取消（追加段：依赖本脚本 START=1 起的 mock Java 控制口 /debug/*） ----------------
# 用户取消 → mock 收到取消（带用户 Bearer）且 run=cancelled；取消时 Java 已成功 → 按成功交付；
# BearerCache 为空（Java 401 让 canvas-rpc 清掉缓存）时运营强制失败 → 退化为停止跟踪、error_reason 写明、不调 Java。
if [[ $START == 1 ]]; then
  echo "== Java 工具取消（POST /api/v1/creation/{taskId}/cancel）"
  JM=http://127.0.0.1:$JAVA_PORT
  jdbg() { curl -s "$JM/debug/$1"; }
  jref() { sql "SELECT backend_ref FROM afv_canvas_node_run WHERE id=$1"; }
  jcancels() { jdbg cancels | jq -r --arg t "$1" --arg a "Bearer $USER_TOKEN" '[.cancels[]|select(.tid==$t)]|"\(length),\(all(.auth==$a))"'; }
  JC=$(ok POST /api/v1/canvas/canvases '{"title":"e2e Java 取消"}'); JCID=$(jq -r .id <<<"$JC")
  JSNAP=$(jq -nc --arg img "$IMG" '{nodes:[
    {id:"i1",type:"image",position:{x:0,y:0},data:{kind:"input",title:"图片",image:$img}},
    {id:"u1",type:"tool",position:{x:400,y:0},data:{kind:"tool",tool:"upscale",title:"放大1",sourceNodeId:"i1"}},
    {id:"u2",type:"tool",position:{x:400,y:200},data:{kind:"tool",tool:"upscale",title:"放大2",sourceNodeId:"i1"}},
    {id:"u3",type:"tool",position:{x:400,y:400},data:{kind:"tool",tool:"upscale",title:"放大3",sourceNodeId:"i1"}}],
   edges:[],viewport:{x:0,y:0,zoom:1}}')
  ok PUT "/api/v1/canvas/canvases/$JCID" "$(save "$JSNAP" "" true)" >/dev/null
  jrun() { local k; k=$(uuidgen); ok POST "/api/v1/canvas/canvases/$JCID/nodes/$1/run" "$(jq -c --arg k "$k" '.run_id=$k' <<<"$UB")" "$k"; }

  jdbg "mode?m=hold" >/dev/null
  JR=$(jrun u1); JRID=$(jq -r .request_id <<<"$JR"); JT=$(jref "$JRID")
  check "Java 工具已提交、运行中（mock 任务一直 RUNNING）" "$(jq -r .status <<<"$JR"),$( [[ $JT == JT* ]] && echo ok)" "running,ok"
  check "用户取消 → cancelled" "$(ok POST "/api/v1/canvas/runs/$JRID/cancel" | jq -r .status)" cancelled
  check "mock Java 收到 1 次取消、带用户 Bearer" "$(jcancels "$JT")" "1,true"
  check "run 置 canceled、error_reason 注明 Java 已取消并由 Java 退款、计费仍归 Java" \
    "$(sql "SELECT CONCAT(status,',',error_code,',',billing_status,',',error_reason LIKE '%Java 任务已取消，点数由 Java 退回%') FROM afv_canvas_node_run WHERE id=$JRID")" "4,CANCELED,java,1"
  check "GetRun：cancelled、提示里不再是「只停止跟踪」" "$(ok GET "/api/v1/canvas/runs/$JRID" | jq -r '"\(.status),\(.error|test("Java 任务已取消"))"')" "cancelled,true"

  jdbg "mode?m=finish_on_cancel" >/dev/null
  JR=$(jrun u2); JRID=$(jq -r .request_id <<<"$JR"); JT=$(jref "$JRID")
  check "取消时 Java 已成功（3001）→ 按成功交付" "$(ok POST "/api/v1/canvas/runs/$JRID/cancel" | jq -r .status)" succeeded
  check "交付结果：succeeded、1 张候选（已转存）、mock 收到取消" \
    "$(ok GET "/api/v1/canvas/runs/$JRID" | jq -r '"\(.status),\(.candidates|length)"'),$(jcancels "$JT")" "succeeded,1,1,true"

  jdbg "mode?m=hold" >/dev/null
  JR=$(jrun u3); JRID=$(jq -r .request_id <<<"$JR"); JT=$(jref "$JRID")
  R0=$(jdbg "reject?on=1" | jq -r .rejected)
  # 等 Poller 查一次任务吃到 401 → canvas-rpc 清掉该用户缓存的令牌（期间不发任何带用户 Bearer 的请求）
  for _ in $(seq 1 30); do (( $(jdbg cancels | jq -r .rejected) > R0 )) && break; sleep 0.5; done
  check "Poller 查 Java 任务吃到 401（缓存令牌已清）" "$( (( $(jdbg cancels | jq -r .rejected) > R0 )) && echo ok)" ok
  FF=$(adm POST "$ADMIN_TOKEN" "/api/v1/canvas/admin/runs/$JRID/force-fail" '{"reason":"e2e"}')
  jdbg "reject?on=0" >/dev/null; jdbg "mode?m=normal" >/dev/null
  check "BearerCache 为空时强制失败 → 成功受理" "$(code_of "$FF")" 0
  check "退化为停止跟踪：run 失败（ADMIN_FORCE_FAIL）、error_reason 写明未能取消、未调 Java 取消" \
    "$(sql "SELECT CONCAT(status,',',error_code,',',error_reason LIKE '%Java 任务未能取消（无有效登录态），可能仍会执行并扣费%') FROM afv_canvas_node_run WHERE id=$JRID"),$(jdbg cancels | jq -r --arg t "$JT" '[.cancels[]|select(.tid==$t)]|length')" \
    "3,ADMIN_FORCE_FAIL,1,0"
  check "canvas-rpc 打了退化 WARN（check=canvas.java_cancel_degraded）" "$(grep -c "java_cancel_degraded" "$TMP/canvas-rpc.log" | awk '{print ($1>0)?"ok":"none"}')" ok
  code_of "$(call DELETE "/api/v1/canvas/canvases/$JCID")" >/dev/null
  green "Java 取消用例通过；累计 $PASS 项"
fi

# ---------------- Java 工具：提交暂时失败（backend_ref 没写上）后取消 → 同幂等键逐字节重放找回任务号再取消（追加段） ----------------
if [[ $START == 1 ]]; then
  echo "== Java 工具：提交 502 后取消 → 按幂等键重放找回任务号再取消"
  JC2=$(ok POST /api/v1/canvas/canvases '{"title":"e2e Java 找回"}'); JC2ID=$(jq -r .id <<<"$JC2")
  ok PUT "/api/v1/canvas/canvases/$JC2ID" "$(save "$JSNAP" "" true)" >/dev/null
  jrun2() { local k; k=$(uuidgen); ok POST "/api/v1/canvas/canvases/$JC2ID/nodes/$1/run" "$(jq -c --arg k "$k" '.run_id=$k' <<<"$UB")" "$k"; }
  jidem() { sql "SELECT JSON_UNQUOTE(JSON_EXTRACT(backend_request,'\$.idem')) FROM afv_canvas_node_run WHERE id=$1"; }
  jsubs() { jdbg cancels | jq -c --arg k "$1" '[.submits[]|select(.idem==$k)]'; }
  jsubs_of() { jdbg cancels | jq -r --arg t "$1" --arg k "$2" '[.submits[]|select(.tid==$t)]|(length>0) and all(.idem==$k)'; } # tid 幂等键 → 该 tid 的提交全用这个键
  jdbg "mode?m=hold" >/dev/null

  jdbg "fail?m=after_create" >/dev/null # Java 建了任务、回包丢了
  JR=$(jrun2 u1); JRID=$(jq -r .request_id <<<"$JR"); JK=$(jidem "$JRID")
  check "提交 502 → run 留在排队、首次发送前已落库原始请求（幂等键 canvas-<task_id>）" \
    "$(jq -r .status <<<"$JR"),$JK" "queued,canvas-$(sql "SELECT task_id FROM afv_canvas_node_run WHERE id=$JRID")"
  check "用户取消 → cancelled" "$(ok POST "/api/v1/canvas/runs/$JRID/cancel" | jq -r .status)" cancelled
  # Poller 可能在取消前先做一次暂时性重提（同键重放），提交记录条数不固定：不断言条数，断言「至少一次重放、
  # 首次之后全是同键重放（原始字节一致才会命中，否则 mock 回 1003 不记录）、只有一个 tid、该 tid 只被取消一次」
  SUBS=$(jsubs "$JK"); JT=$(jq -r '.[0].tid' <<<"$SUBS")
  check "mock 收到同键重放（原始字节一致）并返回原 taskId；全程只有一个 tid" \
    "$(jq -r '"\(length>=2),\(.[0].replay),\([.[1:][]|.replay]|all),\([.[].tid]|unique|length)"' <<<"$SUBS")" "true,false,true,1"
  check "该 tid 的所有提交都用同一幂等键" "$(jsubs_of "$JT" "$JK")" true
  check "原 taskId 收到取消（带用户 Bearer）、run 回写 backend_ref" "$(jcancels "$JT"),$(jref "$JRID")" "1,true,${JT}"
  check "run canceled、error_reason 注明 Java 已取消" \
    "$(sql "SELECT CONCAT(status,',',error_code,',',error_reason LIKE '%Java 任务已取消，点数由 Java 退回%') FROM afv_canvas_node_run WHERE id=$JRID")" "4,CANCELED,1"

  jdbg "fail?m=before_create" >/dev/null # Java 根本没收到首次请求
  JR=$(jrun2 u2); JRID=$(jq -r .request_id <<<"$JR"); JK=$(jidem "$JRID")
  check "提交 502（Java 未收到）→ 排队" "$(jq -r .status <<<"$JR")" queued
  check "用户取消 → cancelled" "$(ok POST "/api/v1/canvas/runs/$JRID/cancel" | jq -r .status)" cancelled
  # 同上：Poller 的暂时性重提可能先于取消在 Java 建任务，之后的取消重放命中同键（replay=true）；条数不固定
  SUBS=$(jsubs "$JK"); JT=$(jq -r '[.[]|select(.tid!=null)][0].tid' <<<"$SUBS")
  check "重放在 Java 新建了任务（首次 tid 为空；建任务的非 replay 提交恰好 1 次；之后同键命中同一 tid）" \
    "$(jq -r '"\(length>=2),\(.[0].tid),\([.[]|select(.tid!=null and .replay==false)]|length),\([.[].tid|select(.!=null)]|unique|length)"' <<<"$SUBS")" "true,null,1,1"
  check "该 tid 的所有提交都用同一幂等键" "$(jsubs_of "$JT" "$JK")" true
  check "新建的任务随即被取消（由 Java 退款）、run canceled" "$(jcancels "$JT"),$(sql "SELECT status FROM afv_canvas_node_run WHERE id=$JRID")" "1,true,4"

  jdbg "mode?m=normal" >/dev/null
  code_of "$(call DELETE "/api/v1/canvas/canvases/$JC2ID")" >/dev/null
  green "Java 找回任务号用例通过；累计 $PASS 项"
fi

green "== 汇总：通过 ${PASS} 项，失败 ${E2E_FAIL} 项（BILLING=${BILLING}）"
e2e_finish

#!/usr/bin/env bash
# ai-gateway 请求签名（HMAC-SHA256 v1，七头 X-Aig-*）。只依赖 python3 标准库。
# 规则出处：ai-gateway/contract/hmac.md（canonical 共 9 行，\n 连接，无尾换行）。
#
# 用法（被 source）：
#   source lib/aig-sign.sh
#   AIG_SECRET=... aig_sign GET /v1/catalog/readiness '' [body文件]    # 设置 AIG_TS / AIG_NONCE / AIG_SIG
#   aig_selftest                                                      # 用 hmac.md 的测试向量自测，通过返回 0
# 直接运行：bash lib/aig-sign.sh --selftest
#
# 密钥只从环境变量 AIG_SECRET 读，不进命令行参数（ps 看不到），也不打印。
# 预设 AIG_TS / AIG_NONCE 时按预设值签（自测用）；否则取当前时间与随机 nonce。
# 其余输入（环境变量）：AIG_CALLER（默认 fusion-go）、AIG_PURPOSE（默认 catalog）。

_AIG_PY='
import hashlib, hmac, os, re, secrets, sys, time
from urllib.parse import quote, unquote

method, path, query, body_file = sys.argv[1:5]
secret = os.environ["AIG_SECRET"].encode()
caller = os.environ.get("AIG_CALLER", "fusion-go")
purpose = os.environ.get("AIG_PURPOSE", "catalog")
ts = os.environ.get("AIG_TS") or str(int(time.time()))
nonce = os.environ.get("AIG_NONCE") or secrets.token_hex(16)

# path：去 query/fragment，连续 / 折成一个，去尾 /（根除外）；冒号后缀路由原样保留
path = re.split(r"[?#]", path, 1)[0]
path = re.sub(r"/+", "/", path)
if len(path) > 1:
    path = path.rstrip("/") or "/"

# canonical query：键值各自 RFC3986 编码（空格 %20、大写十六进制），按 (键, 值) 排序
pairs = []
for part in filter(None, query.split("&")):
    k, _, v = part.partition("=")
    pairs.append((quote(unquote(k), safe=""), quote(unquote(v), safe="")))
cq = "&".join("%s=%s" % p for p in sorted(pairs))

body = b""
if body_file:
    with open(body_file, "rb") as f:
        body = f.read()
canonical = "\n".join(["v1", caller, purpose, method.upper(), path, cq, ts, nonce,
                       hashlib.sha256(body).hexdigest()])
sig = hmac.new(secret, canonical.encode(), hashlib.sha256).hexdigest()
print(ts); print(nonce); print(sig)
'

# aig_sign METHOD PATH QUERY [BODY_FILE]
aig_sign() {
  local out
  [[ -n ${AIG_SECRET:-} ]] || { echo "aig_sign: AIG_SECRET 为空" >&2; return 2; }
  out=$(python3 -I -c "$_AIG_PY" "$1" "$2" "${3:-}" "${4:-}") || return 2
  { read -r AIG_TS; read -r AIG_NONCE; read -r AIG_SIG; } <<<"$out"
  # 预设值被原样回显；需要新鲜签名的调用方在每次请求前 unset AIG_TS AIG_NONCE
}

AIG_VECTOR_SIG=c85d585c8ce54c561674ca404ba99cff9570cfd7b32e50191edba2a0a6991d89

# hmac.md 测试向量：secret/caller/key_id/purpose/method/path/query/ts/nonce/body 全固定
aig_selftest() {
  local f got
  f=$(mktemp); printf '%s' '{"operation":"image.generate"}' >"$f"
  got=$(AIG_SECRET=test-secret-please-rotate AIG_CALLER=fusion-java AIG_PURPOSE=calls \
        AIG_TS=1790000000 AIG_NONCE=nonce-0123456789abcdef \
        python3 -I -c "$_AIG_PY" POST /v1/calls 'wait=25&retry_failed=true' "$f" | tail -n1)
  rm -f "$f"
  echo "签名自测  期望 $AIG_VECTOR_SIG"
  echo "          实得 $got"
  [[ $got == "$AIG_VECTOR_SIG" ]] && { echo "签名自测通过"; return 0; }
  echo "签名自测失败" >&2; return 1
}

if [[ ${BASH_SOURCE[0]} == "$0" ]]; then
  set -euo pipefail
  case ${1:-} in
    --selftest) aig_selftest ;;
    *) echo "用法: bash $0 --selftest" >&2; exit 2 ;;
  esac
fi

#!/usr/bin/env bash
# e2e 计费助手（source 使用）：查点数账户 / 预留单 / 流水，断言余额变化与预留单状态，跑对账查询。
#
# 依赖调用方已定义：
#   check 名称 实际 期望     —— 不相等即退出（e2e-studio.sh / e2e-canvas.sh 都有）
# 可选环境变量：
#   MYSQL_CONTAINER   MySQL 容器名（默认 compose-mysql-1）
#   E2E_DB            库名（默认 fusion_aigc）
#
# 预留单的定位规则与 pkg/billing 的 Charge.bizID 一致（在 SQL 里算，不在 shell 里算）：
#   biz_type = 'LOCALLIFE'
#   biz_id   = {user_id}_{request_id}；超过 64 字节折叠成 {user_id}_{SHA-256(原串) 十六进制} 截前 64 位
# request_id 即业务表里的 billing_request_id（studio-<task_id>[-r<n>] / canvas-<task_id>）。
#
# 账务口径（pkg/billing/ledger.go）：
#   FREEZE   balance -= P, frozen += P    流水 amount = -P
#   CONSUME  frozen  -= 实扣               流水 amount = -实扣（balance 不动）
#   REFUND   frozen  -= P, balance += P   流水 amount = +P（业务失败）
#   UNFREEZE frozen  -= P, balance += P   流水 amount = +P（预留过期回收，单子 → EXPIRED）

BILL_LIB_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=strict.sh
source "$BILL_LIB_DIR/strict.sh"   # 严格模式 + ERR trap（已加载则跳过）；调用方的 cleanup 须保留退出码，见 strict.sh
BILL_ROOT=$(cd "$BILL_LIB_DIR/../../.." && pwd)
BILL_MYSQL_CONTAINER=${MYSQL_CONTAINER:-compose-mysql-1}
BILL_DB=${E2E_DB:-fusion_aigc}

bill_sql() { docker exec -i "$BILL_MYSQL_CONTAINER" mysql --default-character-set=utf8mb4 -uroot -p123456 "$BILL_DB" -N -e "$1" 2>/dev/null; }
# bill_sql_strict 同 bill_sql，但 SQL 出错时输出「SQL_ERROR: …」（对账查询用：查询本身坏了不能当成 0 行）
bill_sql_strict() {
  local out
  if ! out=$(docker exec -i "$BILL_MYSQL_CONTAINER" mysql --default-character-set=utf8mb4 -uroot -p123456 "$BILL_DB" -N -e "$1" 2>&1 | grep -v 'Using a password'); then :; fi
  if grep -q '^ERROR' <<<"$out"; then echo "SQL_ERROR: $out"; return 1; fi
  echo "$out"
}

# bill_biz_expr 用户 请求号 → biz_id 的 SQL 表达式（折叠规则同 pkg/billing）
bill_biz_expr() {
  local raw="CONCAT('$1','_','$2')"
  echo "(CASE WHEN CHAR_LENGTH($raw) <= 64 THEN $raw ELSE LEFT(CONCAT('$1','_',SHA2($raw,256)),64) END)"
}
bill_biz_id() { bill_sql "SELECT $(bill_biz_expr "$1" "$2")"; }

# ---------------- 准备 / 清理 ----------------

# bill_seed 用户 点数 —— 建点数账户 + 一个不过期批次（source_id=E2E 便于识别）。只用于 e2e 的随机用户：先清掉该用户的旧账。
bill_seed() {
  local u=$1 n=$2
  bill_cleanup "$u"
  bill_sql "INSERT INTO afv_points_account (user_id,balance,frozen_balance,total_received,version,deleted) VALUES ($u,$n,0,$n,0,0);
            INSERT INTO afv_points_batch (account_id,user_id,amount,remaining,frozen,source,source_id,expire_time,status,deleted)
              SELECT id,$u,$n,$n,0,'ADMIN_GIFT','E2E',NULL,'ACTIVE',0 FROM afv_points_account WHERE user_id=$u"
  [[ $(bill_balance "$u") == "$n" ]] || { echo "bill_seed: 用户 $u 建账失败" >&2; return 1; }
}

# bill_cleanup 用户 —— 删掉该用户的账户、批次、流水（含明细）、预留单（含明细）。只用于 e2e 的随机用户。
bill_cleanup() {
  local u=$1
  bill_sql "DELETE ll FROM afv_points_log_line ll JOIN afv_points_log pl ON pl.id=ll.ledger_id WHERE pl.user_id=$u;
            DELETE FROM afv_points_log_line WHERE user_id=$u;
            DELETE FROM afv_points_log WHERE user_id=$u;
            DELETE rl FROM afv_billing_reservation_line rl JOIN afv_billing_reservation r ON r.reservation_no=rl.reservation_no WHERE r.user_id=$u;
            DELETE FROM afv_billing_reservation WHERE user_id=$u;
            DELETE FROM afv_points_batch WHERE user_id=$u;
            DELETE FROM afv_points_account WHERE user_id=$u" || true
}

# ---------------- 查询 ----------------

bill_balance() { bill_sql "SELECT COALESCE((SELECT balance FROM afv_points_account WHERE user_id=$1 AND deleted=0),0)"; }
bill_frozen()  { bill_sql "SELECT COALESCE((SELECT frozen_balance FROM afv_points_account WHERE user_id=$1 AND deleted=0),0)"; }
# bill_snap 用户 → "可用 冻结"
bill_snap() { bill_sql "SELECT CONCAT(COALESCE(MAX(balance),0),' ',COALESCE(MAX(frozen_balance),0)) FROM afv_points_account WHERE user_id=$1 AND deleted=0"; }

# bill_res 用户 请求号 列 → 预留单某列（无单为空）
bill_res() {
  bill_sql "SELECT $3 FROM afv_billing_reservation WHERE biz_type='LOCALLIFE' AND deleted=0 AND biz_id=$(bill_biz_expr "$1" "$2") LIMIT 1"
}
bill_res_status() { bill_res "$1" "$2" status; }
bill_res_points() { bill_res "$1" "$2" price_points; }
bill_res_count()  { bill_sql "SELECT COUNT(*) FROM afv_billing_reservation WHERE biz_type='LOCALLIFE' AND deleted=0 AND biz_id=$(bill_biz_expr "$1" "$2")"; }
# bill_held_count 用户 → 该用户还在 HELD 的单数
bill_held_count() { bill_sql "SELECT COUNT(*) FROM afv_billing_reservation WHERE user_id=$1 AND status='HELD' AND deleted=0"; }

# bill_logs 用户 请求号 → 流水行 "FREEZE:-10 CONSUME:-10 …"（按 id 顺序）
bill_logs() {
  bill_sql "SELECT COALESCE(GROUP_CONCAT(CONCAT(change_type,':',amount) ORDER BY id SEPARATOR ' '),'')
            FROM afv_points_log WHERE user_id=$1 AND biz_type='LOCALLIFE' AND biz_id=$(bill_biz_expr "$1" "$2")"
}

# bill_unit_price 入口码 分辨率 → 该入口按张的单价（与 pkg/billing/price.go 的单价行选择同序：分辨率精确 → 不限分辨率 → id）
bill_unit_price() {
  bill_sql "SELECT p.price_points FROM afv_billing_offer o
            JOIN afv_billing_offer_variant v ON v.offer_code=o.offer_code AND v.deleted=0 AND v.status='ENABLED'
            JOIN afv_billing_charge_component c ON c.variant_code=v.variant_code AND c.deleted=0 AND c.cost_type='BILLED'
            JOIN afv_model_pricing p ON p.model_id=COALESCE(c.model_id,v.default_model_id) AND p.action_type=c.action_type
                 AND p.status=1 AND p.deleted=0
            WHERE o.entry_code='$1' AND o.deleted=0 AND o.status='ENABLED'
            ORDER BY (p.resolution='$2') DESC, (p.resolution IS NULL) DESC, p.id LIMIT 1"
}

# bill_expire 用户 请求号 —— 把预留单的到期时间改到过去（模拟租约过期，等回收器释放）
bill_expire() {
  bill_sql "UPDATE afv_billing_reservation SET expire_time = NOW() - INTERVAL 1 MINUTE
            WHERE biz_type='LOCALLIFE' AND deleted=0 AND biz_id=$(bill_biz_expr "$1" "$2")"
}

# bill_wait_status 用户 请求号 状态 [超时秒，默认 90] → 等到预留单变成该状态；返回最终状态
bill_wait_status() {
  local st
  for _ in $(seq 1 "${4:-90}"); do
    st=$(bill_res_status "$1" "$2")
    [[ $st == "$3" ]] && { echo "$st"; return 0; }
    sleep 1
  done
  echo "$st"
}

# ---------------- 断言 ----------------

# assert_balance_delta 名称 用户 前值("可用 冻结") 可用变化 [冻结变化，默认 0]
assert_balance_delta() {
  local name=$1 u=$2 before=$3 db=$4 df=${5:-0} b0 f0 b1 f1
  read -r b0 f0 <<<"$before"
  read -r b1 f1 <<<"$(bill_snap "$u")"
  check "${name}（可用 ${b0}→${b1}、冻结 ${f0}→${f1}）" "$((b1 - b0)),$((f1 - f0))" "$db,$df"
}

# assert_reservation_status 名称 用户 请求号 状态 [点数]
assert_reservation_status() {
  local name=$1 u=$2 req=$3 want=$4 pts=${5:-}
  local got; got=$(bill_res_status "$u" "$req")
  if [[ -n $pts ]]; then
    check "${name}（${req}）" "$got,$(bill_res_points "$u" "$req")" "$want,$pts"
  else
    check "${name}（${req}）" "$got" "$want"
  fi
}

# assert_ledger_consistent 名称 用户 初始点数
#   没有 HELD、冻结为 0、账户 = 批次合计、按流水重放余额对得上、守恒（初始 = 可用 + 冻结 + 已消费）
assert_ledger_consistent() {
  local name=$1 u=$2 init=$3 r
  r=$(bill_sql "SELECT CONCAT_WS(',',
      (SELECT COUNT(*) FROM afv_billing_reservation WHERE user_id=$u AND status='HELD' AND deleted=0),
      a.frozen_balance,
      a.balance = (SELECT COALESCE(SUM(remaining),0) FROM afv_points_batch WHERE user_id=$u AND deleted=0),
      a.frozen_balance = (SELECT COALESCE(SUM(frozen),0) FROM afv_points_batch WHERE user_id=$u AND deleted=0),
      a.balance = $init + (SELECT COALESCE(SUM(amount),0) FROM afv_points_log WHERE user_id=$u AND change_type IN ('FREEZE','REFUND','UNFREEZE')),
      a.frozen_balance = (SELECT COALESCE(SUM(CASE change_type WHEN 'FREEZE' THEN -amount WHEN 'CONSUME' THEN amount ELSE -amount END),0)
                          FROM afv_points_log WHERE user_id=$u),
      $init = a.balance + a.frozen_balance + a.total_consumed)
    FROM afv_points_account a WHERE a.user_id=$u")
  check "${name}（HELD 数,冻结,账户=批次可用,账户=批次冻结,流水重放可用,流水重放冻结,守恒）" "$r" "0,0,1,1,1,1,1"
}

# bill_studio_mismatch 用户 → 跑 pkg/auditdb 的 StudioTasksBillingMismatch（原文取自 db/audit/query/studio_reconcile.sql），
# 只看该用户，返回不一致行数；有行时把明细打到 stderr。
bill_studio_mismatch() {
  local q
  q=$(awk '/^-- name: StudioTasksBillingMismatch/{on=1;next} on&&/^--/{next} on{print} on&&/;[[:space:]]*$/{exit}' \
        "$BILL_ROOT/db/audit/query/studio_reconcile.sql" | sed 's/LIMIT ?;/LIMIT 100000/')
  local rows; rows=$(bill_sql_strict "SELECT * FROM ($q) x WHERE x.user_id=$1") || { echo "$rows" >&2; echo SQL_ERROR; return; }
  [[ -n $rows ]] && echo "$rows" >&2
  if [[ -z $rows ]]; then echo 0; else wc -l <<<"$rows" | tr -d ' '; fi
}

# bill_canvas_mismatch 用户 → 画布 run 的计费镜像与预留单不一致的行数（口径同 studio 对账）：
#   java / studio / none 不该有 Go 的预留单；settled 要 CONFIRMED / PARTIALLY_REFUNDED；refunded 要 REFUNDED / EXPIRED / CANCELLED；
#   review 只能是 HELD（待回收）或 EXPIRED（已回收）；终态 run 不该还是 held；失败 / 取消的不该已结算；成功的不该 unsettled。
bill_canvas_mismatch() {
  local u=$1 raw="CONCAT(r.user_id,'_',r.billing_request_id)"
  local rows; rows=$(bill_sql_strict "SELECT r.id, r.kind, r.status, r.billing_status, COALESCE(res.status,'-')
    FROM afv_canvas_node_run r
    LEFT JOIN afv_billing_reservation res ON res.biz_type='LOCALLIFE' AND res.deleted=0
      AND r.billing_request_id IS NOT NULL AND r.billing_request_id <> ''
      AND res.biz_id = (CASE WHEN CHAR_LENGTH($raw) <= 64 THEN $raw ELSE LEFT(CONCAT(r.user_id,'_',SHA2($raw,256)),64) END)
    WHERE r.user_id=$u AND r.status IN (2,3,4) AND (
         (r.billing_status IN ('java','studio','none') AND res.id IS NOT NULL)
      OR (r.billing_status = 'settled' AND (res.id IS NULL OR res.status NOT IN ('CONFIRMED','PARTIALLY_REFUNDED')))
      OR (r.billing_status = 'refunded' AND (res.id IS NULL OR res.status NOT IN ('REFUNDED','EXPIRED','CANCELLED')))
      OR (r.billing_status = 'review' AND (res.id IS NULL OR res.status NOT IN ('HELD','EXPIRED')))
      OR r.billing_status = 'held'
      OR (r.status IN (3,4) AND res.status IN ('CONFIRMED','PARTIALLY_REFUNDED'))
      OR (r.status = 2 AND r.billing_status = 'unsettled'))") || { echo "$rows" >&2; echo SQL_ERROR; return; }
  [[ -n $rows ]] && echo "$rows" >&2
  if [[ -z $rows ]]; then echo 0; else wc -l <<<"$rows" | tr -d ' '; fi
}

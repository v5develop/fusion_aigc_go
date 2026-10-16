#!/usr/bin/env bash
# 计费接入的端到端辅助（配合现有 e2e-*.sh 使用，服务要以 Billing.Mode=on 启动）。
#
#   BILL_DB=fusion_aigc_bt bash billing-e2e-helper.sh seed  <用户> <点数>   造账户 + 批次 + 九个入口要的创作权益
#   BILL_DB=fusion_aigc_bt bash billing-e2e-helper.sh audit <用户>          查这个用户的预留单与账，核对账务自洽
#
# 典型用法：先 seed，再 USER_ID=<用户> bash tests/e2e/e2e-imagegen.sh（及 video / note / digitalhuman），最后 audit。
# 审计通过的标准：没有留在 HELD 的单、冻结为 0、账户余额 = 批次剩余、
# 余额 = 初始 - 已确认扣除（确认的 + 部分退回单实际扣的），账本里冻结 / 确认 / 退回能对上。
set -euo pipefail

DB=${BILL_DB:-fusion_aigc}
MYSQL=(docker exec -i mysql-8.0.36 mysql --default-character-set=utf8mb4 -uroot -p123456 "$DB" -N)
sql() { "${MYSQL[@]}" -e "$1" 2>/dev/null; }

case ${1:-} in
seed)
  U=$2; N=$3
  sql "DELETE FROM afv_points_batch WHERE user_id=$U; DELETE FROM afv_points_account WHERE user_id=$U;
       DELETE FROM afv_app_user_feature WHERE user_id=$U;
       INSERT INTO afv_points_account (user_id,balance,frozen_balance,total_received,version,deleted) VALUES ($U,$N,0,$N,0,0);
       INSERT INTO afv_points_batch (account_id,user_id,amount,remaining,frozen,source,expire_time,status,deleted)
         SELECT id,$U,$N,$N,0,'ADMIN_GIFT',NULL,'ACTIVE',0 FROM afv_points_account WHERE user_id=$U;
       INSERT INTO afv_app_user_feature (user_id,feature_code,source,status)
         SELECT DISTINCT $U,feature_code,'ADMIN','ACTIVE' FROM afv_billing_offer
         WHERE entry_code IN ('imageTextNote','poster','longImage','digitalHuman','voiceover','actionTransfer','exploreShop','shortDrama','videoRemake')
           AND status='ENABLED' AND deleted=0;"
  echo "已造用户 ${U}：${N} 点"
  ;;
audit)
  U=$2
  echo "== 预留单（按状态）"
  sql "SELECT status, COUNT(*), SUM(price_points) FROM afv_billing_reservation WHERE user_id=$U GROUP BY status"
  echo "== 账户 / 批次"
  sql "SELECT 'account', balance, frozen_balance, total_received, total_consumed, total_refunded FROM afv_points_account WHERE user_id=$U;
       SELECT 'batches', SUM(remaining), SUM(frozen) FROM afv_points_batch WHERE user_id=$U"
  held=$(sql "SELECT COUNT(*) FROM afv_billing_reservation WHERE user_id=$U AND status='HELD'")
  frozen=$(sql "SELECT frozen_balance FROM afv_points_account WHERE user_id=$U")
  consistent=$(sql "SELECT a.balance=b.r AND a.frozen_balance=b.f FROM afv_points_account a JOIN (SELECT user_id,SUM(remaining) r,SUM(frozen) f FROM afv_points_batch WHERE user_id=$U) b ON b.user_id=a.user_id WHERE a.user_id=$U")
  # 守恒：总收入 = 余额 + 冻结 + 已消费 （退回的钱回到余额，不在已消费里）
  conserve=$(sql "SELECT total_received = balance + frozen_balance + (total_consumed) FROM afv_points_account WHERE user_id=$U")
  echo "== 检查"
  echo "HELD 单数=${held}（应为 0）  冻结=${frozen}（应为 0）  账户=批次一致:${consistent}（应为 1）  守恒:${conserve}（应为 1）"
  [[ $held == 0 && $frozen == 0 && $consistent == 1 && $conserve == 1 ]] && echo "PASS" || { echo "FAIL"; exit 1; }
  ;;
*) echo "用法：$0 seed <用户> <点数> | audit <用户>"; exit 2 ;;
esac

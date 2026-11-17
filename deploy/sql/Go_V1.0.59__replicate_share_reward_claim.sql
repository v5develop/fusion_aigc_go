-- ============================================================
-- 视频做同款：分享奖励「先占位再发放」两阶段（CONTRACT §U）
--   原流程：管理端先调 Java 加积分，再调 Go 标记已发放；期间若奖励被关闭 / 月上限被并发发放占满，
--   Java 已加点而 Go 拒收 → 「已发放但未标记」。
--   新流程：Go claim（同一事务内锁用户投稿行，校验开关 / 月上限，reward_status none → granting，记点数快照）
--   → Java 加积分（幂等键 = rewardKey，点数 = 快照）→ Go mark（granting → granted，无条件，不再校验开关 / 上限）。
--   取消发放（Java 明确失败）用 release：granting → none。
--
--   * reward_status 新增取值 granting（发放中）：none / granting / granted。
--   * reward_points：占位时的点数快照（重试沿用同一个数，Java 幂等键不变所以不会重复加点）；老的已发放行为 0。
--   * reward_claimed_by / reward_claimed_at：占位的管理员与时间；月次数按 COALESCE(reward_claimed_at, reward_at) 落月、
--     只数 granted + granting，保证占位即占额度。
--
-- 编号：Go_V1.0.59（所有分支 deploy/sql 已提交最高 Go_V1.0.58）。上线前在各目标库核对已执行版本。
-- 只改 Go 自有表；列已存在则报重复列，需重复执行时先自行判断（与同目录其它加列迁移一致）。
-- ============================================================
SET NAMES utf8mb4;

ALTER TABLE `afv_replicate_share`
  MODIFY COLUMN `reward_status` varchar(16) NOT NULL DEFAULT 'none' COMMENT 'none / granting（已占位、Java 加积分中）/ granted（已发放）',
  ADD COLUMN `reward_points`     int         NOT NULL DEFAULT '0' COMMENT '占位时的奖励点数快照（Java 加积分用；老数据 0）' AFTER `reward_status`,
  ADD COLUMN `reward_claimed_by` bigint      NOT NULL DEFAULT '0' COMMENT '占位的管理员 id' AFTER `reward_points`,
  ADD COLUMN `reward_claimed_at` datetime(3) DEFAULT NULL COMMENT '占位时间（月次数落月依据）' AFTER `reward_claimed_by`;

-- ============================================================
-- 画布 run：用户取消请求标记（afv_canvas_node_run.cancel_request）
--
-- 产品决策：退款只有一个主人，且只在确认没发给供应商之后退。
--   用户在排队 / 提交中取消时不再直接结束并退款，只打标记 cancel_request=1（接口 cancel_state=requested，前端「取消中」）：
--     * 派发器发请求前用一条 CAS 把 run 从排队改成提交中（条件含 cancel_request=0）；发现已请求取消 → 由持有 run 的一方
--       确认网关上没有已发出的调用（按幂等键取回并取消）后结束并退款；
--     * 已开始提交的：提交返回后转运行中的 CAS 也带 cancel_request=0，没抢到就由派发器取消本次拿到的调用——全部取消成功才退款，
--       有被网关拒绝（已发给供应商）的 → run 按运行中跟踪、按实际结算，cancel_request=2（接口 cancel_state=refused）。
--     * 派发器宕机：提交中兜底 / 排队推进在确认网关无已发出调用后再处理。
--   取值：0 没有；1 已请求取消（待确认）；2 取消被拒（已发给供应商，照常完成、按实际结算）。
--
-- 编号：Go_V1.0.61（所有分支 deploy/sql 最高 Go_V1.0.60）。上线前在各目标库核对已执行版本。
-- 上线顺序：先执行本迁移，再发布 canvas-rpc（新代码读写该列；列缺失时读 run 会报 Unknown column）。
-- 不支持新旧 canvas-rpc 混跑：旧实例的提交中兜底会把新实例刚抢到（排队 → 提交中）的 run 当成冻结卡住而失败退款，
-- 旧派发器也不认取消标记（照常提交）。发布时先执行迁移、再整体替换全部 canvas-rpc 实例（不要滚动混跑）。
-- 只给 Go 自有表补一列，不改任何 Java 表；可重复执行。
-- ============================================================
SET NAMES utf8mb4;

DROP PROCEDURE IF EXISTS `go_v1061_add_column`;
DELIMITER $$
CREATE PROCEDURE `go_v1061_add_column`(IN p_table VARCHAR(64), IN p_col VARCHAR(64), IN p_def TEXT)
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table)
       AND NOT EXISTS (SELECT 1 FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = p_table AND COLUMN_NAME = p_col) THEN
        SET @ddl := CONCAT('ALTER TABLE `', p_table, '` ADD COLUMN `', p_col, '` ', p_def);
        PREPARE stmt FROM @ddl;
        EXECUTE stmt;
        DEALLOCATE PREPARE stmt;
    END IF;
END$$
DELIMITER ;

CALL `go_v1061_add_column`('afv_canvas_node_run', 'cancel_request',
  'tinyint NOT NULL DEFAULT ''0'' COMMENT ''用户取消请求：0 没有；1 已请求取消（排队 / 提交中，待确认网关无已发出调用后结束并退款）；2 取消被拒（已发给供应商，照常完成、按实际结算）'' AFTER `attempt`');

DROP PROCEDURE IF EXISTS `go_v1061_add_column`;

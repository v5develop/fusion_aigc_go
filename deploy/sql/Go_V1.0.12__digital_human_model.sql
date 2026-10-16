-- 数字人出片模型：seedance2.5（云端 85）/ lite（本地 45）。
-- 之前固定走 85，没有这一列；产品 9/27 确认要给用户选，历史记录一律算云端。
ALTER TABLE `afv_digital_human`
  ADD COLUMN `model` VARCHAR(64) NOT NULL DEFAULT 'seedance2.5'
  COMMENT '出片模型：seedance2.5(云端 85) / lite(本地 45)' AFTER `duration`;

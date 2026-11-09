-- studio 子任务的校色参数（商拍换装 / 姿态复刻结果页「校色」）。
--
-- 校色在前端浏览器里烘成 PNG，服务端不处理像素，只存用户最后一次保存的五个滑杆值，
-- 便于再次打开结果时还原：{"hue":3,"saturation":-16,"value":-6,"brightness":-17,"contrast":10}。
-- 写入口只有 PATCH /api/v1/studio/tasks/:task_id/color-fix（仅本人、仅成功的任务，范围在 studio-rpc 校验）。
-- 「默认校色」开关不建列：随提交写进 afv_studio_batch.payload 的 color_fix。
--
-- 只增不改：加一个可空 JSON 列，旧数据为 NULL（= 没保存过）。
SET NAMES utf8mb4;

ALTER TABLE `afv_studio_task`
  ADD COLUMN `color_fix_params` json DEFAULT NULL COMMENT '校色参数 {hue,saturation,value,brightness,contrast}；NULL=没保存过' AFTER `error_reason`;

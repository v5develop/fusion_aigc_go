-- 转存 outbox 自带产物来源（画布 run 硬删除兜底 + studio 成品已换兜底）
--
-- 1) afv_canvas_copy_job.src_snapshot：落 outbox 时把入素材库需要的 run 字段一并存下
--    {kind, model_group, run_id, canvas_id, node_id, request, candidates, video_url}（candidates / video_url 是置成功时写进 run 的
--    mr:// 引用）。run 行被硬删除（只有人工改库会这样）时按本列照常复制、入素材库（同 source_task_id 只写一次），不再作废。
--    NULL = 本列上线前落的行（或落行时本列还不存在）：run 行没了仍按旧行为作废 + ERROR check=canvas.library_copy_orphaned。
-- 2) afv_studio_copy_job.billing_request_id：落 outbox 时子任务的计费请求号（这张成品的预留单）。转存时子任务的计费请求号
--    已不是它 = 子任务已重新生成 / 重做（成品已换）；按本列查预留单，已确认扣费（CONFIRMED / PARTIALLY_REFUNDED）的旧成品
--    照常入素材库（带 supersededSrcRid 标记，不参与子任务 asset_id / regen_asset_floor），未扣费的作废。
--    计费未开启（off / shadow）时没有扣费可核对，交付过的旧成品照常入库。
--    NULL = 本列上线前落的行：按结构判定（子任务重做过且有更晚的转存行 / 当前成品是另一个私有引用），旧成品属于第 0..attempt-1 轮，
--    这些轮的计费请求号确定（studio-<task_id>、studio-<task_id>-r<n>）：全部已扣费 → 入库，全部未扣费 → 作废，
--    两种都有 → 作废 + ERROR check=studio.library_copy_superseded reason=ambiguous_pre_migration；结构上判定不了时走原逻辑。
--
-- 只增不改：两个可空列，旧数据不动。上线顺序无要求：列不存在时 canvas-rpc / studio-rpc 落 outbox 自动去掉这一列重写
-- （同一事务内，INSERT 报 1054 不影响事务），读这一列报 1054 按 NULL 处理。
SET NAMES utf8mb4;

ALTER TABLE `afv_canvas_copy_job`
  ADD COLUMN `src_snapshot` json DEFAULT NULL COMMENT '入素材库所需的 run 字段快照（run 行硬删除时兜底）；NULL=旧行' AFTER `user_id`;

ALTER TABLE `afv_studio_copy_job`
  ADD COLUMN `billing_request_id` varchar(128) DEFAULT NULL COMMENT '这张成品的计费请求号（成品已换时据此判断旧成品是否已付费）；NULL=旧行' AFTER `ref`;

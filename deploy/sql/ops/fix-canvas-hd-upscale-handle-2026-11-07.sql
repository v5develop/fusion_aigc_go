-- 修正官方画布模板「高清放大」的连线目标口（2026-11-07）。可重复执行。
--
-- 问题：seed/canvas-official-templates.sql 第 6 段最初把原图 → 高清放大工具节点的连线写成 targetHandle = "image-in"，
--   工具节点的输入口实际叫 "image"，前端套用模板后这条线挂不上。种子已改成 "image"，但种子是
--   INSERT … WHERE NOT EXISTS，已经落过库的环境不会被改，需要跑这份脚本。
-- 只改：scope=official、title=高清放大、未删除，且 edges[0] 仍是 tpl-hd-upscale-e1 / targetHandle=image-in 的行；
--   已修过的、运营改过结构的（edges[0] 不是这条线）都不动。
-- 所属库：fusion_aigc（afv_canvas_template，Go_V1.0.25__canvas.sql 建表）。
-- 跑完自查：最后一条 SELECT 的 target_handle 应为 image。

UPDATE afv_canvas_template
SET snapshot = JSON_SET(snapshot, '$.edges[0].targetHandle', 'image')
WHERE scope = 'official' AND title = '高清放大' AND deleted = 0
  AND JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[0].id')) = 'tpl-hd-upscale-e1'
  AND JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[0].targetHandle')) = 'image-in';

SELECT id, title,
       JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[0].id'))           AS edge_id,
       JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[0].targetHandle')) AS target_handle
FROM afv_canvas_template
WHERE scope = 'official' AND title = '高清放大' AND deleted = 0;

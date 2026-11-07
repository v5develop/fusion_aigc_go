-- 官方画布模板种子（3 个，scope=official）
-- 来源：前端原型 CanvasView.vue 的 insertCanvasTemplate（outfit / three-view / element-replace）
-- 表：afv_canvas_template（Go_V1.0.25__canvas.sql）。
--
-- 快照 = Vue Flow 原生结构 {nodes[{id,type,position,data}], edges[{id,source,target,sourceHandle,targetHandle,data{order}}]}。
-- 节点 id 为稳定前缀 tpl-<模板>-nN，边 id 为 tpl-<模板>-eN；服务端 from-template 建画布时会重新生成 id。
-- 所有模块框（module）原点为 (0,0)，其余节点坐标等于原型里 base 偏移后的相对值。
--
-- 演示素材已清除：原型里的 image URL（/images/library/*.jpg）与 assetId（a_model_1 等）、
-- 参考视频的 video URL / assetId 全部置空；参考视频节点 status 由原型的 done 改为 idle（没有视频不能算完成）。
-- cover_url 留空。【运营需在后台替换封面与示例图】（description 里已写明）。
--
-- 可重放：INSERT ... SELECT ... WHERE NOT EXISTS（scope='official' + title 判重）。已存在的不会被改写；
-- 要更新快照请手工 UPDATE。sort 升序，步长 10。
-- 用法：mysql ... < deploy/sql/seed/canvas-official-templates.sql

SET NAMES utf8mb4;

-- 1. outfit 模特换装：5 个节点（含 1 个 module 框），3 条连线
INSERT INTO afv_canvas_template (scope, user_id, title, description, snapshot, cover_url, sort, enabled, created_at)
SELECT 'official', NULL, '模特换装', '人物图 + 单品图，保脸保姿势，只换衣服。运营需在后台替换封面与示例图。',
  '{"nodes":[{"id":"tpl-outfit-n1","type":"module","position":{"x":0,"y":0},"draggable":false,"selectable":false,"zIndex":-1,"data":{"title":"模块 · 模特换装","subtitle":"模板模块","kind":"module","color":"rgba(142, 102, 249, 0.10)","width":1000,"height":700}},{"id":"tpl-outfit-n2","type":"text","position":{"x":50,"y":70},"data":{"text":"保持人物脸、姿势与场景不变，只把服装换成参考图中的单品；版型、颜色与图案一致。","title":"提示词","subtitle":"文字","kind":"text","mentions":[],"width":320,"height":180}},{"id":"tpl-outfit-n3","type":"image","position":{"x":50,"y":330},"data":{"image":"","title":"人物图","subtitle":"参考图 · 人物","kind":"input"}},{"id":"tpl-outfit-n4","type":"image","position":{"x":320,"y":330},"data":{"image":"","title":"目标单品","subtitle":"参考图 · 单品","kind":"input"}},{"id":"tpl-outfit-n5","type":"model","position":{"x":640,"y":250},"data":{"title":"模特换装","subtitle":"Banana · 1K","kind":"model","model":"Banana","maxImageRefs":5,"ratio":"1:1","size":"1K","quality":"中","cfg":0.95,"count":1,"prompt":"","status":"idle"}}],"edges":[{"id":"tpl-outfit-e1","type":"canvas-bezier","source":"tpl-outfit-n2","target":"tpl-outfit-n5","sourceHandle":"text","targetHandle":"prompt","data":{"color":"#22c55e"}},{"id":"tpl-outfit-e2","type":"canvas-bezier","source":"tpl-outfit-n3","target":"tpl-outfit-n5","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":1}},{"id":"tpl-outfit-e3","type":"canvas-bezier","source":"tpl-outfit-n4","target":"tpl-outfit-n5","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":2}}]}',
  '', 10, 1, NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_canvas_template WHERE scope = 'official' AND title = '模特换装');

-- 2. three-view 商品三视图：4 个节点（含 1 个 module 框），2 条连线
INSERT INTO afv_canvas_template (scope, user_id, title, description, snapshot, cover_url, sort, enabled, created_at)
SELECT 'official', NULL, '商品三视图', '一张商品图，生成正/侧/背三视图，白底一致。运营需在后台替换封面与示例图。',
  '{"nodes":[{"id":"tpl-three-view-n1","type":"module","position":{"x":0,"y":0},"draggable":false,"selectable":false,"zIndex":-1,"data":{"title":"模块 · 商品三视图","subtitle":"模板模块","kind":"module","color":"rgba(20, 184, 166, 0.10)","width":860,"height":700}},{"id":"tpl-three-view-n2","type":"text","position":{"x":50,"y":60},"data":{"text":"基于商品参考图生成同一商品的正视图、侧视图、背视图，白底棚拍，保持材质与颜色一致。","title":"提示词","subtitle":"文字","kind":"text","mentions":[],"width":320,"height":170}},{"id":"tpl-three-view-n3","type":"image","position":{"x":50,"y":300},"data":{"image":"","title":"商品图","subtitle":"参考图 · 商品","kind":"input"}},{"id":"tpl-three-view-n4","type":"model","position":{"x":460,"y":240},"data":{"title":"商品三视图","subtitle":"Image · 1K","kind":"model","model":"Image","maxImageRefs":14,"ratio":"1:1","size":"1K","quality":"中","cfg":0.95,"count":3,"prompt":"","status":"idle"}}],"edges":[{"id":"tpl-three-view-e1","type":"canvas-bezier","source":"tpl-three-view-n2","target":"tpl-three-view-n4","sourceHandle":"text","targetHandle":"prompt","data":{"color":"#22c55e"}},{"id":"tpl-three-view-e2","type":"canvas-bezier","source":"tpl-three-view-n3","target":"tpl-three-view-n4","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":1}}]}',
  '', 20, 1, NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_canvas_template WHERE scope = 'official' AND title = '商品三视图');

-- 3. element-replace 元素替换：7 个节点（含 1 个 module 框），4 条连线
--   参考视频接视频生成节点的 video 口（参考视频），元素图接 image 口（参考图）。原型里参考视频接的是 image1（首帧口），
--   而首尾帧不能与参考图同用（gateway 审计），模板一运行就会被拒，2026-10-31 改为 video 口。
--   与原型一致只接「提示词 + 参考视频 + 元素图 1/2」4 条线，元素图 3 无连线（参考图上限 3，产品确认后可补第 3 条）。
INSERT INTO afv_canvas_template (scope, user_id, title, description, snapshot, cover_url, sort, enabled, created_at)
SELECT 'official', NULL, '元素替换', '参考视频 + 3 张元素图，只换局部元素。运营需在后台替换封面与示例图。',
  '{"nodes":[{"id":"tpl-element-replace-n1","type":"module","position":{"x":0,"y":0},"draggable":false,"selectable":false,"zIndex":-1,"data":{"title":"模块 · 元素替换","subtitle":"模板模块","kind":"module","color":"rgba(249, 115, 22, 0.10)","width":1180,"height":980}},{"id":"tpl-element-replace-n2","type":"text","position":{"x":50,"y":760},"data":{"text":"把参考视频里的帽子替换成图1/图2/图3的帽款；场景、动作、人物保持不变。","title":"提示词","subtitle":"文字","kind":"text","mentions":[],"width":320,"height":170}},{"id":"tpl-element-replace-n3","type":"model","position":{"x":50,"y":100},"data":{"title":"参考视频","subtitle":"Seedance 2.5 · 4s","kind":"video","model":"Seedance 2.5","maxImageRefs":3,"ratio":"16:9","size":"720p","quality":"中","duration":"4s","sound":true,"cfg":0.95,"count":1,"prompt":"","status":"idle","inputOnly":true}},{"id":"tpl-element-replace-n4","type":"image","position":{"x":410,"y":100},"data":{"image":"","title":"元素图 1","subtitle":"参考图 · 元素","kind":"input"}},{"id":"tpl-element-replace-n5","type":"image","position":{"x":660,"y":100},"data":{"image":"","title":"元素图 2","subtitle":"参考图 · 元素","kind":"input"}},{"id":"tpl-element-replace-n6","type":"image","position":{"x":910,"y":100},"data":{"image":"","title":"元素图 3","subtitle":"参考图 · 元素","kind":"input"}},{"id":"tpl-element-replace-n7","type":"model","position":{"x":410,"y":380},"data":{"title":"元素替换","subtitle":"Seedance 2.5 · 4s","kind":"video","model":"Seedance 2.5","maxImageRefs":3,"ratio":"16:9","size":"720p","quality":"中","duration":"4s","sound":true,"cfg":0.95,"count":1,"prompt":"","status":"idle"}}],"edges":[{"id":"tpl-element-replace-e1","type":"canvas-bezier","source":"tpl-element-replace-n2","target":"tpl-element-replace-n7","sourceHandle":"text","targetHandle":"prompt","data":{"color":"#22c55e"}},{"id":"tpl-element-replace-e2","type":"canvas-bezier","source":"tpl-element-replace-n3","target":"tpl-element-replace-n7","sourceHandle":"video","targetHandle":"video","data":{"color":"#a855f7","order":1}},{"id":"tpl-element-replace-e3","type":"canvas-bezier","source":"tpl-element-replace-n4","target":"tpl-element-replace-n7","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":2}},{"id":"tpl-element-replace-e4","type":"canvas-bezier","source":"tpl-element-replace-n5","target":"tpl-element-replace-n7","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":3}}]}',
  '', 30, 1, NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_canvas_template WHERE scope = 'official' AND title = '元素替换');

-- 修正已按旧种子落库的「元素替换」：e2（参考视频 → 视频生成）从 image1 首帧口改到 video 口（可重放：只改仍是 image1 的）。
UPDATE afv_canvas_template
SET snapshot = JSON_SET(snapshot, '$.edges[1].sourceHandle', 'video', '$.edges[1].targetHandle', 'video', '$.edges[1].data.color', '#a855f7')
WHERE scope = 'official' AND title = '元素替换' AND deleted = 0
  AND JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[1].id')) = 'tpl-element-replace-e2'
  AND JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[1].targetHandle')) = 'image1';

-- 4. product-main 商品主图（2026-11-06 二轮 3→6）：4 个节点（含 1 个 module 框），2 条连线
INSERT INTO afv_canvas_template (scope, user_id, title, description, snapshot, cover_url, sort, enabled, created_at)
SELECT 'official', NULL, '商品主图', '一张商品图，出白底 / 场景两版 1:1 主图。运营需在后台替换封面与示例图。',
  '{"nodes":[{"id":"tpl-product-main-n1","type":"module","position":{"x":0,"y":0},"draggable":false,"selectable":false,"zIndex":-1,"data":{"title":"模块 · 商品主图","subtitle":"模板模块","kind":"module","color":"rgba(20, 184, 166, 0.10)","width":980,"height":720}},{"id":"tpl-product-main-n2","type":"text","position":{"x":50,"y":60},"data":{"text":"商品主图：白底棚拍与场景两张，主体居中、边缘留白，真实材质与颜色，1:1。","title":"主图提示词","subtitle":"文字","kind":"text","mentions":[],"width":320,"height":170}},{"id":"tpl-product-main-n3","type":"image","position":{"x":50,"y":300},"data":{"image":"","title":"商品图","subtitle":"参考图 · 商品","kind":"input"}},{"id":"tpl-product-main-n4","type":"model","position":{"x":460,"y":240},"data":{"title":"商品主图","subtitle":"Banana · 1K","kind":"model","model":"Banana","maxImageRefs":5,"ratio":"1:1","size":"1K","quality":"中","cfg":0.95,"count":2,"prompt":"","status":"idle"}}],"edges":[{"id":"tpl-product-main-e1","type":"canvas-bezier","source":"tpl-product-main-n2","target":"tpl-product-main-n4","sourceHandle":"text","targetHandle":"prompt","data":{"color":"#22c55e"}},{"id":"tpl-product-main-e2","type":"canvas-bezier","source":"tpl-product-main-n3","target":"tpl-product-main-n4","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":1}}]}',
  '', 40, 1, NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_canvas_template WHERE scope = 'official' AND title = '商品主图');

-- 5. short-video 带货短片（2026-11-06 二轮 3→6）：4 个节点（含 1 个 module 框），2 条连线
--   竖屏 9:16 / 8s；商品图接首帧口（image1），运行时以首帧 + 提示词驱动视频生成。
INSERT INTO afv_canvas_template (scope, user_id, title, description, snapshot, cover_url, sort, enabled, created_at)
SELECT 'official', NULL, '带货短片', '商品图 + 卖点，出 8 秒竖屏带货短片。运营需在后台替换封面与示例图。',
  '{"nodes":[{"id":"tpl-short-video-n1","type":"module","position":{"x":0,"y":0},"draggable":false,"selectable":false,"zIndex":-1,"data":{"title":"模块 · 带货短片","subtitle":"模板模块","kind":"module","color":"rgba(106, 72, 123, 0.10)","width":980,"height":760}},{"id":"tpl-short-video-n2","type":"text","position":{"x":50,"y":60},"data":{"text":"8 秒竖屏带货短片：镜头从商品特写推到手持展示，暖光，节奏轻快，结尾出现卖点字幕。","title":"短片提示词","subtitle":"文字","kind":"text","mentions":[],"width":320,"height":180}},{"id":"tpl-short-video-n3","type":"image","position":{"x":50,"y":320},"data":{"image":"","title":"商品图","subtitle":"参考图 · 商品","kind":"input"}},{"id":"tpl-short-video-n4","type":"model","position":{"x":500,"y":240},"data":{"title":"带货短片","subtitle":"Seedance 2.5 · 8s","kind":"video","model":"Seedance 2.5","maxImageRefs":3,"ratio":"9:16","size":"720p","quality":"中","duration":"8s","sound":true,"cfg":0.95,"count":1,"prompt":"","status":"idle"}}],"edges":[{"id":"tpl-short-video-e1","type":"canvas-bezier","source":"tpl-short-video-n2","target":"tpl-short-video-n4","sourceHandle":"text","targetHandle":"prompt","data":{"color":"#22c55e"}},{"id":"tpl-short-video-e2","type":"canvas-bezier","source":"tpl-short-video-n3","target":"tpl-short-video-n4","sourceHandle":"image","targetHandle":"image1","data":{"color":"#38bdf8","order":1}}]}',
  '', 50, 1, NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_canvas_template WHERE scope = 'official' AND title = '带货短片');

-- 6. hd-upscale 高清放大（2026-11-06 二轮 3→6）：3 个节点（含 1 个 module 框），1 条连线
--   工具节点 upscale 走 Java 计价（免费预扣口径由 Java 侧定），原图接工具节点的 image 口。
INSERT INTO afv_canvas_template (scope, user_id, title, description, snapshot, cover_url, sort, enabled, created_at)
SELECT 'official', NULL, '高清放大', '模糊素材放到 2K，保留原构图与比例。运营需在后台替换封面与示例图。',
  '{"nodes":[{"id":"tpl-hd-upscale-n1","type":"module","position":{"x":0,"y":0},"draggable":false,"selectable":false,"zIndex":-1,"data":{"title":"模块 · 高清放大","subtitle":"模板模块","kind":"module","color":"rgba(59, 130, 246, 0.10)","width":900,"height":680}},{"id":"tpl-hd-upscale-n2","type":"image","position":{"x":50,"y":120},"data":{"image":"","title":"原图","subtitle":"待放大素材","kind":"input"}},{"id":"tpl-hd-upscale-n3","type":"tool","position":{"x":420,"y":140},"data":{"title":"高清放大","subtitle":"2x 放大","kind":"tool","tool":"upscale","scale":2,"sourceNodeId":"tpl-hd-upscale-n2","status":"idle"}}],"edges":[{"id":"tpl-hd-upscale-e1","type":"canvas-bezier","source":"tpl-hd-upscale-n2","target":"tpl-hd-upscale-n3","sourceHandle":"image","targetHandle":"image","data":{"color":"#38bdf8","order":1}}]}',
  '', 60, 1, NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_canvas_template WHERE scope = 'official' AND title = '高清放大');

-- 修正已按旧种子落库的「高清放大」：e1（原图 → 高清放大）从 image-in 改到 image 口（可重放：只改仍是 image-in 的；
-- 同 ops/fix-canvas-hd-upscale-handle-2026-11-07.sql）。
UPDATE afv_canvas_template
SET snapshot = JSON_SET(snapshot, '$.edges[0].targetHandle', 'image')
WHERE scope = 'official' AND title = '高清放大' AND deleted = 0
  AND JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[0].id')) = 'tpl-hd-upscale-e1'
  AND JSON_UNQUOTE(JSON_EXTRACT(snapshot, '$.edges[0].targetHandle')) = 'image-in';

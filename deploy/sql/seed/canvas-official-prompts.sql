-- 官方提示词库种子（19 条，user_id=NULL）
-- 来源：前端原型 src/data/prompt-library.ts，排除 mine-1 / mine-2 两条演示数据。
-- 表：afv_prompt_library_item（Go_V1.0.25__canvas.sql）。
--
-- 【上线前必须确认】19 条里有 8 条来自第三方 GitHub 来源（Awesome GPT Image、YouMind、
-- Freestylefly、DavidWu、Awesome GPT-4o、Banana Prompt Quicker），授权已于 2026-10-31 确认。这 8 条在下文以「第三方」标出：
--   * ecom-product-1  白底商品主图 · 柔和棚拍  （Awesome GPT Image）
--   * general-photo-1  柔光棚拍 · 产品级  （Freestylefly GPT Image 2）
--   * general-photo-2  自然光 · 窗边  （DavidWu GPT Image 2）
--   * general-comp-1  三分法 · 留白构图  （Awesome GPT-4o）
--   * general-style-1  极简高级感  （Awesome GPT Image）
--   * general-style-2  胶片质感  （Banana Prompt Quicker）
--   * general-mat-1  金属与玻璃材质  （YouMind GPT Image 2）
--   * general-text-1  海报标题排版  （YouMind Nano Banana Pro）
-- 其余 11 条为小岐AI 自有（10 条内置 + 1 条智能体提示词）。
--
-- 开关：下面的 @third_party_enabled 控制这 8 条的 enabled。2026-10-31 已确认授权，默认 1（展示）；
-- 已存在的行重放不会被改写
-- （按 user_id IS NULL + title 判重），所以改开关后要用文末的 UPDATE。
--
-- 可重放：INSERT ... SELECT ... WHERE NOT EXISTS。title 在官方范围内视为唯一键。
-- sort 按原型顺序，步长 10，升序展示。
-- 用法：mysql ... < deploy/sql/seed/canvas-official-prompts.sql

SET NAMES utf8mb4;
SET @third_party_enabled = 1;  -- 2026-10-31 用户确认授权，直接启用

-- 01 ecom-model-1
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '同一模特 · 身份锁', '同一位模特，参考图第 1 张为唯一身份基准。保持脸型、五官比例、发型发色、肤色、体型不变；只换服装、场景、姿势与镜头。禁止换脸、改变年龄或性别。', 'ecommerce', 'model',
  '["通用","GPT","Seedream"]', '["模特一致性","身份锁","换装"]', '小岐AI 内置', 1, 10, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '同一模特 · 身份锁');

-- 02 ecom-model-2
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '同一模特 · 多角度', '同一模特，正面 / 侧面 / 3/4 侧 / 背面四视图，统一中性表情、自然站姿、纯色背景、柔和棚拍光，保持五官与发型一致。', 'ecommerce', 'model',
  '["通用","GPT"]', '["多角度","角色设定图"]', '小岐AI 内置', 1, 20, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '同一模特 · 多角度');

-- 03 ecom-scene-1
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '商品换背景 · 暖木桌面', '把商品放到暖色木质桌面，晨光从左上方斜射，背景有虚化绿植，浅景深，保留商品原有形状、颜色与材质。', 'ecommerce', 'scene',
  '["通用","GPT","Seedream"]', '["换背景","暖调","自然光"]', '小岐AI 内置', 1, 30, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '商品换背景 · 暖木桌面');

-- 04 ecom-scene-2
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '城市街头 · 通勤场景', '城市街头，阴天柔光，玻璃幕墙与行人虚化背景，主体居中，半身构图，真实街拍质感，适合通勤品类。', 'ecommerce', 'scene',
  '["通用","Seedream"]', '["街头","通勤","阴天"]', '小岐AI 内置', 1, 40, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '城市街头 · 通勤场景');

-- 05 ecom-product-1  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '白底商品主图 · 柔和棚拍', 'Studio product photo of a product on a seamless light-gray backdrop, soft key light from upper left, gentle contact shadow, crisp edges, true-to-life color, e-commerce hero shot. No text, letters or logos.', 'ecommerce', 'product',
  '["通用","GPT","Banana"]', '["白底","主图","棚拍"]', 'Awesome GPT Image', @third_party_enabled, 50, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '白底商品主图 · 柔和棚拍');

-- 06 ecom-product-2
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '卖点图 · 45° 俯拍', '45° 俯拍商品，纯色背景，硬光突出材质纹理与高光，画面留出标题区，电商卖点图，干净利落。', 'ecommerce', 'product',
  '["通用","GPT"]', '["卖点图","俯拍","硬光"]', '小岐AI 内置', 1, 60, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '卖点图 · 45° 俯拍');

-- 07 ecom-detail-1
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '详情页主屏主视觉', '商品详情页首屏主视觉，产品居中偏右，左侧留白放标题与卖点，柔和渐变背景，高级质感，统一光源。', 'ecommerce', 'detail',
  '["通用","GPT","Seedream"]', '["详情页","主视觉","留白"]', '小岐AI 内置', 1, 70, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '详情页主屏主视觉');

-- 08 ecom-detail-2
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '详情页 · 核心卖点模块', '详情页核心卖点模块，商品局部特写 + 3 个图标位 + 简短文案区，浅色背景，信息层级清晰，保持与主视觉同一光源和色调。', 'ecommerce', 'detail',
  '["通用","GPT"]', '["详情页","卖点","模块化"]', '小岐AI 内置', 1, 80, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '详情页 · 核心卖点模块');

-- 09 ecom-detail-3
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '详情页 · 固定模特身份锁（智能体提示词）', '你是电商详情页生图助手。每次生成前先确认「固定模特」：以用户上传的模特图第 1 张为唯一身份基准，保持脸型、五官比例、发型发色、肤色、体型不变；换服装、场景、姿势与镜头时，不得换脸、改变年龄或性别。若用户指定了模特名称，先查固定模特库；没有参考图就先要求用户上传。商品必须保持版型、面料纹理、颜色与 logo 位置。输出前自检：同一人、同一商品、光源一致；禁止在画面里生成不存在的文字或水印。', 'ecommerce', 'detail',
  '["通用","GPT","Banana","Seedream"]', '["智能体提示词","固定模特","身份锁","详情页"]', '小岐AI 智能体提示词', 1, 90, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '详情页 · 固定模特身份锁（智能体提示词）');

-- 10 ecom-detail-4
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '详情页 · 多模块同一模特', '同一模特完成详情页的 4 个模块：主视觉、卖点特写、使用场景、尺寸说明。四个模块保持同一张脸、同一发型、同一肤色与体型；服装/商品的颜色、材质和 logo 位置一致；光线方向统一。每个模块单独出图，不要拼成一张四宫格。', 'ecommerce', 'detail',
  '["通用","GPT","Seedream"]', '["详情页","多模块","一致性"]', '小岐AI 内置', 1, 100, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '详情页 · 多模块同一模特');

-- 11 ecom-cloth-1
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '服装智能上身', '让这位模特穿上这件服装，保持模特脸型、发型、肤色、体型不变；服装版型、面料纹理、颜色准确；自然站姿，棚拍白底。', 'ecommerce', 'apparel',
  '["通用","Seedream","Banana"]', '["智能上身","服装","一致性"]', '小岐AI 内置', 1, 110, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '服装智能上身');

-- 12 ecom-cloth-2
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '服装重设造型', '保留模特身份与服装主体，重设发型、妆容与配饰，整体更利落；保持面料纹理与颜色，半身构图，棚拍柔光。', 'ecommerce', 'apparel',
  '["通用","Seedream"]', '["重设造型","发型","妆容"]', '小岐AI 内置', 1, 120, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '服装重设造型');

-- 13 general-photo-1  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '柔光棚拍 · 产品级', 'Soft studio lighting, large diffused key light, subtle fill, seamless background, gentle contact shadow, high micro-detail, product-grade rendering.', 'general', 'photo',
  '["通用","GPT","Seedream","Banana"]', '["柔光","棚拍","产品级"]', 'Freestylefly GPT Image 2', @third_party_enabled, 130, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '柔光棚拍 · 产品级');

-- 14 general-photo-2  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '自然光 · 窗边', 'Window light from the left, soft falloff, warm highlights, realistic shadows, natural color, shallow depth of field.', 'general', 'photo',
  '["通用","GPT","Banana"]', '["自然光","窗边","浅景深"]', 'DavidWu GPT Image 2', @third_party_enabled, 140, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '自然光 · 窗边');

-- 15 general-comp-1  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '三分法 · 留白构图', 'Rule-of-thirds composition, subject on the right third, generous negative space on the left for copy, balanced visual weight.', 'general', 'composition',
  '["通用","GPT"]', '["三分法","留白","构图"]', 'Awesome GPT-4o', @third_party_enabled, 150, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '三分法 · 留白构图');

-- 16 general-style-1  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '极简高级感', 'Minimalist premium aesthetic, muted palette, clean lines, generous negative space, subtle texture, editorial lighting.', 'general', 'style',
  '["通用","GPT","Seedream"]', '["极简","高级","编辑感"]', 'Awesome GPT Image', @third_party_enabled, 160, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '极简高级感');

-- 17 general-style-2  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '胶片质感', 'Analog film look, fine grain, soft contrast, slightly faded highlights, warm shadows, 35mm lens character.', 'general', 'style',
  '["通用","Banana"]', '["胶片","颗粒","暖调"]', 'Banana Prompt Quicker', @third_party_enabled, 170, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '胶片质感');

-- 18 general-mat-1  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '金属与玻璃材质', 'Brushed metal and clear glass materials, accurate reflections, crisp specular highlights, subtle fingerprints, physically plausible light transport.', 'general', 'material',
  '["通用","GPT","Seedream"]', '["金属","玻璃","反射"]', 'YouMind GPT Image 2', @third_party_enabled, 180, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '金属与玻璃材质');

-- 19 general-text-1  【第三方，2026-10-31 已确认授权】
INSERT INTO afv_prompt_library_item (user_id, title, prompt, vertical, group_key, models, tags, source, enabled, sort, created_at, updated_at)
SELECT NULL, '海报标题排版', 'Poster layout with a bold headline on the upper left, supporting copy below, product image on the right, clear grid, high contrast, no gibberish text.', 'general', 'text',
  '["通用","GPT","Banana"]', '["海报","标题","排版"]', 'YouMind Nano Banana Pro', @third_party_enabled, 190, NOW(), NOW()
FROM DUAL WHERE NOT EXISTS (SELECT 1 FROM afv_prompt_library_item WHERE user_id IS NULL AND title = '海报标题排版');

-- 授权确认后一次性启用这 8 条（或确认不用时把 enabled 改回 0）：
-- UPDATE afv_prompt_library_item SET enabled = 1, updated_at = NOW()
--  WHERE user_id IS NULL AND title IN (
--   '白底商品主图 · 柔和棚拍',
--   '柔光棚拍 · 产品级',
--   '自然光 · 窗边',
--   '三分法 · 留白构图',
--   '极简高级感',
--   '胶片质感',
--   '金属与玻璃材质',
--   '海报标题排版'
-- );

-- 已按旧版种子（第三方 enabled=0）落库的环境：一次性启用脚本见 deploy/sql/ops/canvas-enable-third-party-prompts-2026-10-31.sql。
-- 本文件只插入不存在的行，重放不会改写已有行（运营在后台下架的条目不会被重新上架）。

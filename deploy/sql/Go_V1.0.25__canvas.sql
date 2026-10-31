-- 画布（canvas）+ 提示词库（prompt library），设计方案 docs/design/xiaoqi-2026-10-30/00-需求分流与设计方案.md §6
--
-- 新建 4 张表（只新增 afv_canvas* / afv_prompt_library_item，不改任何现有表结构），外加素材库分类与入口归属的配置数据。
-- 全部可重复执行：建表用 IF NOT EXISTS，配置数据用 NOT EXISTS 判重。
--
-- 约定：
--   * 排序规则统一 utf8mb4_unicode_ci（见 Go_V1.0.20：混用 0900_ai_ci 会让任务中心同步报 1267）。
--   * 时间列 DATETIME(3)（bun 模型，毫秒精度，接口返回 unix 毫秒）。
--   * 画布文档整段存前端 Vue Flow 快照（{nodes,edges}），运行态（status/progress/candidates/image/video/runId…）
--     剥离到 afv_canvas_node_run，GET 画布时由服务端按最近一次 run 回填。
--   * 一次节点执行 = 一行 afv_canvas_node_run = 一张计费预留单（RequestID = canvas-<task_id>，仅 gateway 后端）。
--   * 官方模板 / 官方提示词的种子数据在 deploy/sql/seed/canvas-official-*.sql，不在本文件。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_canvas` (
  `id`           varchar(40)   NOT NULL COMMENT '画布ID（字符串，前端 workflowId），cv_ + 16 位十六进制',
  `user_id`      bigint        NOT NULL COMMENT '所属用户ID',
  `workspace_id` bigint        NOT NULL DEFAULT '0' COMMENT '所属工作空间ID；0=个人空间',
  `title`        varchar(100)  NOT NULL DEFAULT '' COMMENT '标题',
  `revision`     varchar(32)   NOT NULL COMMENT '乐观锁版本号：<unix毫秒>-<4位随机hex>，每次保存换新；PUT 带 if_match_revision 比对',
  `node_count`   int           NOT NULL DEFAULT '0' COMMENT '节点数（含 module 框），保存时由快照计算',
  `cover_rid`    varchar(128)  DEFAULT NULL COMMENT '封面资源号（媒体服务 rid，可空）',
  `cover_url`    varchar(1024) DEFAULT NULL COMMENT '封面地址：快照里第一张可用图（保存时计算）',
  `snapshot`     json          NOT NULL COMMENT '整段 Vue Flow 快照 {nodes,edges}（运行态已剥离，≤2MB）',
  `deleted`      tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除：1-已删除',
  `created_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`   datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  KEY `idx_user_updated` (`user_id`,`deleted`,`updated_at`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布文档';

CREATE TABLE IF NOT EXISTS `afv_canvas_node_run` (
  `id`                 bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID（对外 request_id）',
  `canvas_id`          varchar(40)   NOT NULL COMMENT '所属画布（afv_canvas.id）',
  `node_id`            varchar(64)   NOT NULL COMMENT '节点ID（快照 nodes[].id）',
  `user_id`            bigint        NOT NULL COMMENT '提交用户ID',
  `run_id`             varchar(64)   NOT NULL COMMENT '前端 run_id（= Idempotency-Key），同用户唯一',
  `kind`               enum('image','video','tool_crop','tool_upscale','tool_outpaint','tool_inpaint','tool_i2p','tool_tryon') NOT NULL COMMENT '执行类型',
  `request`            json          NOT NULL COMMENT 'RunNodeRequest 全字段快照（下划线化）',
  `model_group`        varchar(64)   NOT NULL DEFAULT '' COMMENT 'ai-gateway model group（gateway 后端）；Java/studio/media 后端为工具标识',
  `backend`            enum('gateway','media','java','studio') NOT NULL COMMENT '执行后端',
  `backend_ref`        varchar(255)  DEFAULT NULL COMMENT '后端引用：gateway call_id（count>1 时逗号分隔，每张一个调用）/ Java taskUid / studio batch_id；media 为空',
  `backend_request`    json          DEFAULT NULL COMMENT 'Java 兼容工具首次发送前落库的原始请求 {path,idem,body}（body 为发出的原始字节）：同幂等键重提 / 找回任务号时逐字节重发；其它后端为空',
  `status`             tinyint       NOT NULL DEFAULT '0' COMMENT '状态：0-排队 1-运行中 2-成功 3-失败 4-已取消 5-提交中（已落库、正在冻结点数，轮询器只做 5 分钟兜底）',
  `progress`           int           NOT NULL DEFAULT '0' COMMENT '进度 0-100（估算）',
  `candidates`         json          DEFAULT NULL COMMENT '候选 [{rid,url,w,h}]；视频首项为成片（cover 另列）',
  `selected_index`     int           NOT NULL DEFAULT '0' COMMENT '选中的候选下标',
  `result_text`        mediumtext    COMMENT '文本结果（tool_i2p 反推提示词）',
  `video_url`          varchar(1024) DEFAULT NULL COMMENT '视频成品地址（kind=video）',
  `requested_count`    int           NOT NULL DEFAULT '1' COMMENT '请求的候选张数',
  `points`             bigint        NOT NULL DEFAULT '0' COMMENT '本 run 冻结 / 实扣点数（gateway 后端；Java/studio 由对方计费为 0）',
  `cost_cny`           decimal(12,6) NOT NULL DEFAULT '0.000000' COMMENT '上游成本（人民币，gateway call.cost.amount_cny 合计；Java / studio 后端为 0）',
  `billing_request_id` varchar(96)   DEFAULT NULL COMMENT '计费请求号 canvas-<task_id>（仅 gateway 后端）',
  `billing_status`     varchar(16)   NOT NULL DEFAULT 'none' COMMENT '计费镜像：none/held/settled/unsettled/refunded/shadow/java/studio/review（review=UNKNOWN 待对账，冻结到期由 Reclaimer 自动退回；unsettled=已交付但确认扣费失败，待人工对账）',
  `error_code`         varchar(64)   DEFAULT NULL COMMENT '机器可读失败码（TIMEOUT / CONTENT_BLOCKED / UPSTREAM_FAILED …）',
  `error_reason`       varchar(255)  DEFAULT NULL COMMENT '失败原因（可展示）',
  `task_id`            varchar(64)   DEFAULT NULL COMMENT '对外任务号 cv_<任务中心编号>_run_<4hex>，与插入同一事务回填（提交中之前就已生成）；任务中心坐标前缀 cv',
  `attempt`            int           NOT NULL DEFAULT '0' COMMENT '提交次数（暂时性错误重提 +1）',
  `deleted`            tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除（任务中心同步用；画布删除不级联）',
  `lease_owner`        varchar(64)   DEFAULT NULL COMMENT '租约持有者（canvas-rpc 实例 hostname-pid-随机）：同一 run 同一时刻只由一个实例提交 / 轮询 / 收尾；多实例部署用',
  `lease_until`        datetime(3)   DEFAULT NULL COMMENT '租约到期时间（库时钟 NOW(3)+3 分钟，心跳续租）；NULL 或已过期 = 可被任一实例抢占（崩溃后自然接管）',
  `created_at`         datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `started_at`         datetime(3)   DEFAULT NULL COMMENT '提交到后端的时间',
  `finished_at`        datetime(3)   DEFAULT NULL COMMENT '进终态的时间',
  `updated_at`         datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间（任务中心窗口同步用；轮询器每轮会触碰在途 run，避免队头饥饿）',
  `active_node`        varchar(110)  GENERATED ALWAYS AS (IF(`status` IN (0,1,5), CONCAT(`canvas_id`,'/',`node_id`), NULL)) STORED COMMENT '生成列：未结束（提交中/排队/运行中）时为 canvas_id/node_id，终态为 NULL；唯一键保证同一节点同一时刻只有一个在途 run',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_run` (`user_id`,`run_id`) USING BTREE,
  UNIQUE KEY `uk_task_id` (`task_id`) USING BTREE,
  UNIQUE KEY `uk_active_node` (`active_node`) USING BTREE,
  KEY `idx_status` (`status`,`updated_at`) USING BTREE,
  KEY `idx_status_lease` (`status`,`lease_until`) USING BTREE,
  KEY `idx_canvas` (`canvas_id`,`node_id`,`id`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布节点执行（一次 run）';

CREATE TABLE IF NOT EXISTS `afv_canvas_template` (
  `id`          bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `scope`       enum('official','user') NOT NULL COMMENT '范围：official-官方 user-用户自建',
  `user_id`     bigint        DEFAULT NULL COMMENT '用户模板的所属用户；官方为 NULL',
  `title`       varchar(100)  NOT NULL COMMENT '标题',
  `description` varchar(500)  NOT NULL DEFAULT '' COMMENT '说明',
  `snapshot`    json          NOT NULL COMMENT 'Vue Flow 快照（运行态已剥离）',
  `cover_url`   varchar(1024) NOT NULL DEFAULT '' COMMENT '封面地址',
  `sort`        int           NOT NULL DEFAULT '0' COMMENT '排序（升序）',
  `enabled`     tinyint       NOT NULL DEFAULT '1' COMMENT '是否上架：1-上架 0-下架',
  `deleted`     tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除',
  `created_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at`  datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  KEY `idx_scope_sort` (`scope`,`enabled`,`sort`) USING BTREE,
  KEY `idx_user` (`user_id`,`created_at`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布模板';

CREATE TABLE IF NOT EXISTS `afv_prompt_library_item` (
  `id`         bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`    bigint        DEFAULT NULL COMMENT '所属用户；NULL=官方',
  `title`      varchar(100)  NOT NULL COMMENT '标题（≤100 字）',
  `prompt`     text          NOT NULL COMMENT '正文（≤3000 字）',
  `vertical`   enum('ecommerce','general','mine') NOT NULL COMMENT '一级分类：ecommerce-电商 general-通用 mine-我的',
  `group_key`  varchar(32)   NOT NULL DEFAULT '' COMMENT '二级分类英文 key（model/scene/product/detail/apparel/photo/composition/style/material/text/favorite）',
  `models`     json          DEFAULT NULL COMMENT '适用模型（通用/GPT/Banana/Seedream）',
  `tags`       json          DEFAULT NULL COMMENT '标签（≤10）',
  `source`     varchar(64)   NOT NULL DEFAULT '' COMMENT '来源说明',
  `enabled`    tinyint       NOT NULL DEFAULT '1' COMMENT '是否启用',
  `sort`       int           NOT NULL DEFAULT '0' COMMENT '排序（升序）',
  `deleted`    tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除',
  `created_at` datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  `updated_at` datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) ON UPDATE CURRENT_TIMESTAMP(3) COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  KEY `idx_user_vertical` (`user_id`,`vertical`,`group_key`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='提示词库（官方 + 用户）';

-- 素材库：画布生成的图片 / 视频 / 裁剪图的聚合分类与入口归属。
-- entry_code 与计费 Offer 同名（canvas_image / canvas_video / canvas_tool_i2p 由 Go 计费，pkg/billing/audit.go Targets 已登记；
-- canvas_tool_crop 不计费，只做素材归类）。Java 兼容工具与 studio 试穿的成品由对方入库，不在这里。
INSERT INTO `afv_asset_category` (`category_code`,`category_name`,`parent_id`,`level`,`media_type`,`sort_order`,`status`)
SELECT * FROM (
  SELECT 'canvas_image' AS category_code, '画布生图' AS category_name, (SELECT c.id FROM afv_asset_category c WHERE c.category_code='image' AND c.level=1 AND c.deleted=0 LIMIT 1) AS parent_id, 2 AS level, '' AS media_type, 110 AS sort_order, 1 AS status
  UNION ALL SELECT 'canvas_tool_crop', '画布裁剪', (SELECT c.id FROM afv_asset_category c WHERE c.category_code='image' AND c.level=1 AND c.deleted=0 LIMIT 1), 2, '', 120, 1
  UNION ALL SELECT 'canvas_video', '画布视频', (SELECT c.id FROM afv_asset_category c WHERE c.category_code='video' AND c.level=1 AND c.deleted=0 LIMIT 1), 2, '', 110, 1
) t
WHERE NOT EXISTS (SELECT 1 FROM `afv_asset_category` c WHERE c.`category_code` = t.category_code AND c.`deleted` = 0);

INSERT INTO `afv_asset_category_entry` (`category_id`,`entry_code`,`sort_order`)
SELECT c.`id`, c.`category_code`, 10 FROM `afv_asset_category` c
WHERE c.`category_code` IN ('canvas_image','canvas_tool_crop','canvas_video') AND c.`deleted` = 0
  AND NOT EXISTS (SELECT 1 FROM `afv_asset_category_entry` e WHERE e.`category_id` = c.`id` AND e.`entry_code` = c.`category_code`);

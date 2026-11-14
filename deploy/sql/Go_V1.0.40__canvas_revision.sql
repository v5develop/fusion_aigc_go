-- 画布快照历史：保留每张画布最近 20 个 revision 的快照，供前端三方合并（base = 本地编辑起点 revision）
--
-- 设计见 docs/design/canvas-agent-tools.md §11（P0-8 画布实时同步）。
--   * 每次画布 revision 成功变化（用户保存、智能体写入、新建 / 另存 / 从模板新建）后由 canvas-rpc 写一行：
--     存储态快照（运行态已剥离，与 afv_canvas.snapshot 同一份内容）的 gzip，写入后按画布裁剪到最近 Limits.RevisionKeep（默认 20）个。
--   * GET /api/v1/canvas/canvases/:id?revision=<rev> 读这里；超出保留范围返回 4001 + reason CANVAS_REVISION_NOT_RETAINED。
--   * 写历史是尽力而为：失败（含本表还不存在）只记 ERROR 日志，不影响保存本身；历史缺失时前端按「超出保留范围」处理（整画布载入最新版）。
--   * 归属不在本表判：先按 afv_canvas (id, user_id, deleted=0) 校验，再按 (canvas_id, revision) 取。
--
-- 版本号：Go_V1.0.40（39 = 同批视频做同款后台）。只新增一张表，不改现有表；CREATE TABLE IF NOT EXISTS，可重复执行。
-- 上线顺序无要求：表不存在时历史写入失败只记日志，?revision= 查询返回「超出保留范围」。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_canvas_revision` (
  `id`             bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID（写入顺序，裁剪按它）',
  `canvas_id`      varchar(32)   NOT NULL COMMENT '画布（afv_canvas.id）',
  `user_id`        bigint        NOT NULL COMMENT '画布所属用户（冗余，排查用）',
  `revision`       varchar(32)   NOT NULL COMMENT '该版本的 revision（与 afv_canvas.revision 同格式）',
  `source`         varchar(16)   NOT NULL DEFAULT '' COMMENT '谁产生的：user / agent',
  `actor`          varchar(64)   NOT NULL DEFAULT '' COMMENT '具体执行者：user:<id> / svc:<keyId> / <client>:<agent>',
  `node_count`     int           NOT NULL DEFAULT '0' COMMENT '节点数',
  `snapshot_bytes` int           NOT NULL DEFAULT '0' COMMENT '快照解压后字节数',
  `snapshot`       mediumblob    NOT NULL COMMENT 'gzip(存储态快照 JSON)，快照 ≤2MB',
  `created_at`     datetime(3)   NOT NULL DEFAULT CURRENT_TIMESTAMP(3) COMMENT '创建时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_canvas_revision` (`canvas_id`,`revision`) USING BTREE,
  KEY `idx_canvas_id` (`canvas_id`,`id`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='画布快照历史（最近 N 个 revision，三方合并 base）';

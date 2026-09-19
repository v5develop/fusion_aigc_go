-- AI图文笔记模块：新增 1 张表与 AI帮写提示词
-- 只新建表，不修改任何现有表结构。
-- 构思走 AIGC 执行面 74（出标题/正文/话题 + 每张图的出图提示词），确认生成走 75（按提示词出图）。
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_image_text_note` (
  `id`               bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`          bigint        NOT NULL COMMENT '创建用户ID',
  `note_type`        varchar(16)   NOT NULL COMMENT '笔记类型：explore-探店 seeding-种草 emotion-情感 knowledge-知识',
  `topic`            varchar(1024) NOT NULL COMMENT '笔记主题/思路（用户输入）',
  `material_urls`    json          DEFAULT NULL COMMENT '用户上传的参考素材 URL 数组（最多 3 张）',
  `digital_human_id` bigint        DEFAULT NULL COMMENT '选用的数字人（afv_digital_human.id）',
  `person_url`       varchar(1024) DEFAULT NULL COMMENT '数字人形象图（74 的 person_url）',
  `plan_task_id`     varchar(64)   DEFAULT NULL COMMENT '构思任务ID（AIGC 执行面 task_type=74）',
  `plan_script`      json          DEFAULT NULL COMMENT '74 返回的完整 script（images/images_url 原样转交 75）',
  `title`            varchar(255)  NOT NULL DEFAULT '' COMMENT '笔记标题（构思生成，用户可改）',
  `content`          text          COMMENT '笔记正文（构思生成，用户可改）',
  `tags`             json          DEFAULT NULL COMMENT '话题数组，如 ["#夏日护肤"]',
  `image_count`      int           NOT NULL DEFAULT '4' COMMENT '套图数量（前端传入，不设上限；构思时传给 74，生成时不超过构思张数）',
  `gen_task_id`      varchar(64)   DEFAULT NULL COMMENT '出图任务ID（AIGC 执行面 task_type=75）',
  `image_urls`       json          DEFAULT NULL COMMENT '笔记图片 URL 数组（生成结果，用户可增删）',
  `request_id`       varchar(64)   DEFAULT NULL COMMENT '客户端幂等键（X-Request-Id，开始构思时）',
  `status`           int           NOT NULL DEFAULT '0' COMMENT '状态：0-构思中 1-待确认 2-构思失败 3-生成中 4-已完成 5-生成失败',
  `fail_reason`      varchar(512)  DEFAULT NULL COMMENT '失败原因',
  `deleted`          tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`      datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`      datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_request` (`user_id`,`request_id`) USING BTREE,
  KEY `idx_user_time` (`user_id`,`deleted`,`id` DESC) USING BTREE,
  KEY `idx_status_time` (`status`,`update_time`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='AI图文笔记';

-- AI帮写新增场景：图文笔记主题/思路（放在 AI文案的提示词表里，运营可改）
INSERT INTO `afv_copywriting_option` (`option_type`,`code`,`label`,`value`,`sort_order`) VALUES
 ('PROMPT','ai_write_note_topic','图文笔记主题-系统提示词','你帮本地生活商家构思一篇小红书风格图文笔记的主题和思路：说清楚写什么、突出哪些亮点、面向什么人群，两到三句话，只输出主题思路本身。',7);

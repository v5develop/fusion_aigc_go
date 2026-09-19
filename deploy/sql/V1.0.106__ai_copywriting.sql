-- AI文案模块：新增文案表与选项字典表
-- 只新建表，不修改任何现有表结构
-- 依赖的配置数据（afv_feature / afv_billing_* / afv_model_pricing）见 V1.0.107

CREATE TABLE IF NOT EXISTS `afv_copywriting` (
  `id`              bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`         bigint       NOT NULL COMMENT '创建用户ID',
  `owner_type`      int          DEFAULT NULL COMMENT '拥有者类型：1-个人 2-团队',
  `owner_id`        bigint       DEFAULT NULL COMMENT '拥有者ID',
  `source`          varchar(32)  NOT NULL COMMENT '来源：AI_WRITE-AI帮写 IMITATE-爆款仿写 MANUAL-自主撰写',
  `content_type`    varchar(32)  DEFAULT NULL COMMENT '内容类型 code（afv_copywriting_option）',
  `style`           varchar(32)  DEFAULT NULL COMMENT '文案风格 code',
  `industry`        varchar(32)  DEFAULT NULL COMMENT '行业 code',
  `title`           varchar(255) NOT NULL DEFAULT '' COMMENT '文案标题',
  `body`            text         NOT NULL COMMENT '文案正文',
  `word_count`      int          NOT NULL DEFAULT '0' COMMENT '正文字数（列表展示用）',
  `topic`           varchar(512) DEFAULT NULL COMMENT '用户输入的主题/商品卖点',
  `reference_text`  text         COMMENT '爆款仿写的参考范文',
  `target_words`    int          DEFAULT NULL COMMENT '期望字数：100/200/500/自定义',
  `shop_profile_id` bigint       DEFAULT NULL COMMENT '关联店铺档案ID',
  `model_id`        bigint       DEFAULT NULL COMMENT '生成所用模型ID（afv_ai_model）',
  `task_id`         varchar(64)  DEFAULT NULL COMMENT '生成任务ID（afv_creation_task.task_id）',
  `request_id`      varchar(64)  DEFAULT NULL COMMENT '客户端幂等键（X-Request-Id），自主撰写为空',
  `status`          int          NOT NULL DEFAULT '0' COMMENT '状态：0-生成中 1-已完成 2-生成失败',
  `fail_reason`     varchar(512) DEFAULT NULL COMMENT '生成失败原因',
  `deleted`         tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_request` (`user_id`,`request_id`) USING BTREE,
  KEY `idx_user_time` (`user_id`,`deleted`,`id` DESC) USING BTREE,
  KEY `idx_task` (`task_id`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='AI文案表';

CREATE TABLE IF NOT EXISTS `afv_copywriting_option` (
  `id`          bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `option_type` varchar(32)  NOT NULL COMMENT '选项类型：CONTENT_TYPE/STYLE/INDUSTRY/WORD_COUNT/PROMPT/BLOCKWORD',
  `code`        varchar(32)  NOT NULL COMMENT '选项编码',
  `label`       varchar(64)  NOT NULL COMMENT '展示名称',
  `value`       text         COMMENT '附加值：提示词片段、字数，或 PROMPT 类型的完整模板',
  `sort_order`  int          NOT NULL DEFAULT '0' COMMENT '排序权重',
  `status`      int          NOT NULL DEFAULT '1' COMMENT '状态：0-停用 1-启用',
  `deleted`     tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `deleted_id`  bigint       NOT NULL DEFAULT '0' COMMENT '删除占位：未删除为0，删除时置为行ID（同现有表约定）',
  `create_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_type_code` (`option_type`,`code`,`deleted_id`) USING BTREE,
  KEY `idx_type_sort` (`option_type`,`status`,`sort_order`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='AI文案选项字典表';

-- 选项种子数据（来自设计稿）
INSERT INTO `afv_copywriting_option` (`option_type`,`code`,`label`,`value`,`sort_order`) VALUES
 ('CONTENT_TYPE','general','通用','',1),
 ('CONTENT_TYPE','explore','探店','探店体验视角，突出环境、服务与招牌产品',2),
 ('CONTENT_TYPE','promotion','带货','带货种草视角，突出卖点、价格与下单理由',3),
 ('CONTENT_TYPE','knowledge','知识','知识科普视角，条理清晰、给出可执行要点',4),
 ('CONTENT_TYPE','emotion','情感','情感共鸣视角，以故事和感受带出内容',5),
 ('STYLE','recommend','种草分享','亲切真诚的种草口吻，多用第一人称体验',1),
 ('STYLE','review','测评硬核','硬核测评口吻，摆参数、讲实测、结论明确',2),
 ('STYLE','compare','对比推荐','横向对比多个选择，给出推荐结论',3),
 ('STYLE','opinion','观点犀利','观点鲜明、有态度，敢下判断',4),
 ('STYLE','story','情感故事','以故事展开，重场景与情绪',5),
 ('INDUSTRY','general','通用','',1),
 ('INDUSTRY','foot_massage','足疗按摩','',2),
 ('INDUSTRY','beauty','美业','',3),
 ('INDUSTRY','education','教育','',4),
 ('INDUSTRY','maternal','母婴','',5),
 ('WORD_COUNT','w100','100字','100',1),
 ('WORD_COUNT','w200','200字','200',2),
 ('WORD_COUNT','w500','500字','500',3);

-- 提示词模板（运营可在后台修改；缺失时回落到代码内置模板）
INSERT INTO `afv_copywriting_option` (`option_type`,`code`,`label`,`value`,`sort_order`) VALUES
 ('PROMPT','generate_ai_write','AI帮写-系统提示词','你是本地生活商家的社媒文案写手。输出可直接发布的中文文案，不要出现解释、标题标号或 markdown 语法。',1),
 ('PROMPT','generate_imitate','爆款仿写-系统提示词','你是本地生活商家的社媒文案写手，擅长模仿给定范文的结构、节奏和语气来写新文案。输出可直接发布的中文文案，不要解释。',2),
 ('PROMPT','ai_write_topic','主题补写-系统提示词','你帮商家把粗略想法补写成一句清晰的文案主题，20 字以内，只输出主题本身。',3),
 ('PROMPT','ai_write_selling_point','卖点优化-系统提示词','你帮商家把商品卖点整理得更有吸引力，保留原有事实（价格、时间、地点等），只输出整理后的卖点文本。',4),
 ('PROMPT','generate_suffix','生成-输出格式要求','先输出一行标题，再空一行输出正文。标题不超过 30 字，不要加「标题：」这类前缀。',5);

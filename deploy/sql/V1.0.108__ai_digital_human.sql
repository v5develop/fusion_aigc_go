-- AI数字人模块：新增 5 张表与初始数据
-- 只新建表，不修改任何现有表结构。
-- 依赖的现有配置（只插数据，需数据库负责人确认后在各环境执行，本文件不包含）：
--   afv_ai_model 一行 code=qwen-audio-3.1-tts-flash，model_type=4（语音合成），ai_provider_id=百炼
--   afv_system_config 一行 ai_tts_model_id = 上面那行的 id
-- 公共数字人不建表，复用现有模板素材（运营在现有后台「模板」里维护，本服务只读）：
--   afv_public_template_assets：title=名字，url=形象图，extra={"role":"店长","avatarUrl":"头像，可空"}
--   afv_public_template_asset_placement：biz_type=localLife，module_code=digitalHuman，slot_code=publicModel（排序、上下架）
SET NAMES utf8mb4;

CREATE TABLE IF NOT EXISTS `afv_digital_human` (
  `id`              bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`         bigint        NOT NULL COMMENT '创建用户ID',
  `name`            varchar(64)   NOT NULL DEFAULT '' COMMENT '名称；未填时取公共数字人名或音色名',
  `portrait_source` varchar(16)   NOT NULL COMMENT '形象来源：UPLOAD-上传照片 PUBLIC-公共数字人 AI-AI定制',
  `portrait_ref_id` bigint        DEFAULT NULL COMMENT 'PUBLIC=afv_public_template_assets.id，AI=afv_digital_human_portrait.id',
  `image_url`       varchar(1024) NOT NULL COMMENT '形象图公网URL',
  `voice_id`        bigint        NOT NULL COMMENT '音色（afv_digital_human_voice.id）',
  `script`          varchar(255)  NOT NULL COMMENT '配音内容',
  `audio_url`       varchar(1024) DEFAULT NULL COMMENT 'TTS 产物（已转存）',
  `video_url`       varchar(1024) DEFAULT NULL COMMENT '口播视频',
  `duration`        int           DEFAULT NULL COMMENT '视频秒数',
  -- 后加的列，同时写在 V1.0.119（老库走那条 ALTER）。这里也留一份：新装的库要建对，
  -- 而且 make model 是拿这个文件生成 app/digitalhuman/model 的，漏了会被覆盖掉。
  `model`           varchar(64)   NOT NULL DEFAULT 'seedance2.5' COMMENT '出片模型：seedance2.5(云端 85) / lite(本地 45)',
  `tts_task_id`     varchar(64)   DEFAULT NULL COMMENT 'TTS 任务ID（百炼直连，自生成）',
  `video_task_id`   varchar(64)   DEFAULT NULL COMMENT '数字人视频任务ID（AIGC 执行面 task_type=85）',
  `request_id`      varchar(64)   DEFAULT NULL COMMENT '客户端幂等键（X-Request-Id）',
  `status`          int           NOT NULL DEFAULT '0' COMMENT '状态：0-配音中 1-视频生成中 2-已完成 3-失败',
  `fail_reason`     varchar(512)  DEFAULT NULL COMMENT '失败原因',
  `deleted`         tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time`     datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time`     datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_request` (`user_id`,`request_id`) USING BTREE,
  KEY `idx_user_time` (`user_id`,`deleted`,`id` DESC) USING BTREE,
  KEY `idx_status_time` (`status`,`update_time`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='AI数字人';

CREATE TABLE IF NOT EXISTS `afv_digital_human_portrait` (
  `id`          bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`     bigint       NOT NULL COMMENT '创建用户ID',
  `gender`      varchar(8)   NOT NULL COMMENT '性别：female/male',
  `age`         varchar(32)  NOT NULL COMMENT '年龄 code（选项 AGE）',
  `skin_tone`   varchar(32)  NOT NULL COMMENT '肤色 code（选项 SKIN_TONE）',
  `role`        varchar(32)  DEFAULT NULL COMMENT '职业角色 code（选项 ROLE）',
  `description` varchar(255) DEFAULT NULL COMMENT '创意描述',
  `task_id`     varchar(64)  DEFAULT NULL COMMENT '出图任务ID（AIGC 执行面 task_type=75）',
  `image_urls`  json         DEFAULT NULL COMMENT '结果图 URL 数组（4 张）',
  `request_id`  varchar(64)  DEFAULT NULL COMMENT '客户端幂等键（X-Request-Id）',
  `status`      int          NOT NULL DEFAULT '0' COMMENT '状态：0-生成中 1-已完成 2-失败',
  `fail_reason` varchar(512) DEFAULT NULL COMMENT '失败原因',
  `deleted`     tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `create_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_request` (`user_id`,`request_id`) USING BTREE,
  KEY `idx_status_time` (`status`,`update_time`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='AI定制数字人形象';

CREATE TABLE IF NOT EXISTS `afv_digital_human_voice` (
  `id`          bigint        NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `name`        varchar(64)   NOT NULL COMMENT '展示名，如 于小云',
  `voice`       varchar(128)  NOT NULL COMMENT 'TTS 音色参数值，如 yuxiaoyun_v3.1（须属于 ai_tts_model_id 指向的模型）',
  `gender`      varchar(8)    NOT NULL COMMENT '性别：female/male',
  `style`       varchar(64)   NOT NULL DEFAULT '' COMMENT '风格描述，如 元气、亲切、自然',
  `categories`  json          NOT NULL COMMENT '所属分类 code 数组（选项 VOICE_CATEGORY），一个音色可在多个分类下',
  `preview_url` varchar(1024) DEFAULT NULL COMMENT '试听音频',
  `sort_order`  int           NOT NULL DEFAULT '0' COMMENT '排序权重',
  `status`      int           NOT NULL DEFAULT '1' COMMENT '状态：0-停用 1-启用',
  `deleted`     tinyint       NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `deleted_id`  bigint        NOT NULL DEFAULT '0' COMMENT '删除占位：未删除为0，删除时置为行ID',
  `create_time` datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time` datetime      NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_voice` (`voice`,`deleted_id`) USING BTREE,
  KEY `idx_status_sort` (`status`,`sort_order`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='数字人系统音色';

CREATE TABLE IF NOT EXISTS `afv_digital_human_option` (
  `id`          bigint       NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `option_type` varchar(32)  NOT NULL COMMENT '选项类型：AGE/SKIN_TONE/ROLE/VOICE_CATEGORY/PROMPT',
  `code`        varchar(32)  NOT NULL COMMENT '选项编码',
  `label`       varchar(64)  NOT NULL COMMENT '展示名称（AI定制时也拼进提示词）',
  `value`       text         COMMENT '附加值：肤色为色块颜色，PROMPT 为完整模板',
  `sort_order`  int          NOT NULL DEFAULT '0' COMMENT '排序权重',
  `status`      int          NOT NULL DEFAULT '1' COMMENT '状态：0-停用 1-启用',
  `deleted`     tinyint      NOT NULL DEFAULT '0' COMMENT '逻辑删除标志',
  `deleted_id`  bigint       NOT NULL DEFAULT '0' COMMENT '删除占位：未删除为0，删除时置为行ID（同现有表约定）',
  `create_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
  `update_time` datetime     NOT NULL DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_type_code` (`option_type`,`code`,`deleted_id`) USING BTREE,
  KEY `idx_type_sort` (`option_type`,`status`,`sort_order`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='数字人选项字典表';

CREATE TABLE IF NOT EXISTS `afv_user_agreement` (
  `id`             bigint      NOT NULL AUTO_INCREMENT COMMENT '主键ID',
  `user_id`        bigint      NOT NULL COMMENT '用户ID',
  `agreement_code` varchar(32) NOT NULL COMMENT '协议编码，如 aigc',
  `version`        int         NOT NULL COMMENT '协议版本；条款改版后 +1，需重新同意',
  `accept_time`    datetime    NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '同意时间',
  PRIMARY KEY (`id`) USING BTREE,
  UNIQUE KEY `uk_user_agreement` (`user_id`,`agreement_code`,`version`) USING BTREE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC COMMENT='用户协议同意记录';

-- 选项初始数据（来自设计稿）
INSERT INTO `afv_digital_human_option` (`option_type`,`code`,`label`,`value`,`sort_order`,`status`) VALUES
 ('AGE','child','儿童',NULL,1,1),
 ('AGE','youth','青年',NULL,2,1),
 ('AGE','elder','老人',NULL,3,1),
 ('SKIN_TONE','fair','白皙','#F9DCCB',1,1),
 ('SKIN_TONE','natural','自然','#F0C3A0',2,1),
 ('SKIN_TONE','wheat','小麦','#E3A77F',3,1),
 ('SKIN_TONE','tan','健康','#D08E64',4,1),
 ('ROLE','store_manager','门店店长',NULL,1,1),
 ('ROLE','host','主播',NULL,2,1),
 ('ROLE','teacher','老师',NULL,3,1),
 ('ROLE','shopping_guide','导购',NULL,4,1),
 ('ROLE','top_sales','销冠',NULL,5,1),
 ('ROLE','beauty_blogger','美妆博主',NULL,6,1),
 ('ROLE','fashion_blogger','穿搭博主',NULL,7,1),
 ('ROLE','explore_blogger','探店博主',NULL,8,1),
 ('ROLE','presenter','主持人',NULL,9,1),
 ('ROLE','customer_service','客服',NULL,10,1),
 -- 方言、外语：精品中文音色中没有对应音色，先停用
 ('VOICE_CATEGORY','female','女声',NULL,1,1),
 ('VOICE_CATEGORY','male','男声',NULL,2,1),
 ('VOICE_CATEGORY','broadcast','口播',NULL,3,1),
 ('VOICE_CATEGORY','emotion','情感',NULL,4,1),
 ('VOICE_CATEGORY','selling','带货',NULL,5,1),
 ('VOICE_CATEGORY','knowledge','知识',NULL,6,1),
 ('VOICE_CATEGORY','dialect','方言',NULL,7,0),
 ('VOICE_CATEGORY','narration','解说',NULL,8,1),
 ('VOICE_CATEGORY','foreign','外语',NULL,9,0),
 ('PROMPT','portrait','AI定制-形象提示词',
  '真实摄影风格的人物半身像，竖版 9:16 构图。人物：{age}{gender}，{skin}肤色，职业形象为{role}。{description}面向镜头、神情自然亲切，光线柔和均匀，背景简洁干净，画面清晰，适合作为数字人口播形象。',1,1),
 ('PROMPT','portrait_variants','AI定制-4张图的差异描述（每行一条）',
  '穿着得体的职业装，站在明亮的门店里\n穿着休闲日常装，背景为浅色纯色墙面\n穿着时尚简约的服装，背景为温馨的室内环境\n穿着清爽的浅色上衣，背景为户外自然光街景',2,1);

-- 系统音色：百炼 Qwen-Audio-TTS 精品中文音色（qwen-audio-3.1-tts-flash）
-- https://help.aliyun.com/zh/model-studio/qwen-audio-tts-voice-list
--
-- 2026-09-21 实测：下面 6 个用当前账号合成会返回 411（陆小燕、许如远、易安宁、楚景川、顾小然在官网有列但接口拒绝；
-- 凌星朗官网已不在列表里），因此建表时就置为停用（status=0），App 端看不到。
-- 账号开通后把 status 改回 1、再用 deploy/scripts/gen-voice-previews.sh 生成试听即可。
INSERT INTO `afv_digital_human_voice` (`name`,`voice`,`gender`,`style`,`categories`,`sort_order`,`status`) VALUES
 ('于小云','yuxiaoyun_v3.1','female','元气、亲切、自然','["female", "broadcast", "selling"]',1,1),
 ('乔小娇','qiaoxiaojiao_v3.1','female','俏丽、可爱','["female", "broadcast", "selling"]',2,1),
 ('陆小燕','luxiaoyan_v3.1','female','明亮、清亮','["female", "broadcast"]',3,0),
 ('夏小晨','xiaxiaochen_v3.1','female','元气、明亮','["female", "broadcast", "selling"]',4,1),
 ('安明远','anmingyuan_v3.1','male','清亮、自然','["male", "broadcast", "narration"]',5,1),
 ('温怀清','wenhuaiqing_v3.1','female','清亮、柔和','["female", "knowledge"]',6,1),
 ('安小岚','anxiaolan_v3.1','female','清甜、纯净','["female"]',7,1),
 ('许如远','xuruyuan_v3.1','female','细腻、柔和','["female", "narration"]',8,0),
 ('谢舒柔','xieshurou_v3.1','female','柔和、自然、知性','["female", "emotion"]',9,1),
 ('易安宁','yianning_v3.1','female','气声、亲切、口音特别','["female", "emotion"]',10,0),
 ('白清岚','baiqinglan_v3.1','female','明亮、清纯','["female"]',11,1),
 ('许玉远','xuyuyuan_v3.1','female','知性、成熟、质感','["female", "broadcast", "knowledge"]',12,1),
 ('安若柔','anruorou_v3.1','female','气声、知性','["female", "emotion"]',13,1),
 ('闻怀之','wenhuaizhi_v3.1','female','稳重、成熟','["female", "knowledge"]',14,1),
 ('萧行之','xiaoxingzhi_v3.1','female','端庄、贵气','["female", "knowledge"]',15,1),
 ('顾云舒','guyunshu_v3.1','female','成熟、稳重','["female", "narration"]',16,1),
 ('霍拙石','huozhuoshi_v3.1','male','清亮','["male"]',17,1),
 ('叶清禾','yeqinghe_v3.1','female','亲切、温柔','["female", "emotion"]',18,1),
 ('云欢欢','yunhuanhuan_v3.1','female','高亢、热情','["female", "selling"]',19,1),
 ('徐小俏','xuxiaoqiao_v3.1','female','自然、俏皮','["female"]',20,1),
 ('白安然','baianran_v3.1','female','低沉、浑厚、气声','["female", "emotion", "narration"]',21,1),
 ('许言初','xuyanchu_v3.1','female','沉稳、磁性','["female", "knowledge"]',22,1),
 ('楚景川','chujingchuan_v3.1','male','浑厚、爽朗','["male", "narration"]',23,0),
 ('顾小然','guxiaoran_v3.1','female','邻家姐姐','["female"]',24,0),
 ('叶知晴','yezhiqing_v3.1','female','轻快、自然','["female"]',25,1),
 ('凌星朗','lingxinglang_v3.1','female','活泼、元气','["female", "selling"]',26,0),
 ('安迪','andi_v3.1','male','ABC 口音','["male"]',27,1),
 ('安语晴','anyuqing_v3.1','female','甜妹','["female"]',28,1);

-- AI帮写新增场景：数字人配音内容（放在 AI文案的提示词表里，运营可改）
INSERT INTO `afv_copywriting_option` (`option_type`,`code`,`label`,`value`,`sort_order`) VALUES
 ('PROMPT','ai_write_dh_script','数字人配音-系统提示词','你帮商家写数字人口播的配音文案：口语化、亲切自然，适合直接念出来，一到两句话，不要表情符号和话题标签，只输出配音文案本身。',6);

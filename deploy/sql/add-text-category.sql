-- 给计费加一个「文本」分类 + 文本类创作能力。
--
-- 结论先说：**不用改 Java 代码**。
-- afv_billing_offer 的 category 和 billing_scene 在 Java 里都是普通字符串
-- （BillingOffer.java 里就是 private String，只在后台回显和 EntryOption 里透传），
-- 没有枚举校验。所以直接写数据就生效。
-- 唯一要留意的是**后台页面的下拉框可能是前端写死的**——下拉里没有 TEXT 的话，
-- 要么让前端补上，要么就用下面这两条 SQL 直接改。

-- 1) 文本类创作能力。action_type 复用已有的 TEXT_CHAT（afv_feature 里 ai_chat 用的就是它）。
--    多语言名称字段是 JSON，照现有行的格式写。
INSERT INTO `afv_feature` (`feature_code`, `feature_name`, `description`, `action_type`, `status`, `deleted`)
SELECT 'locallife_text_gen',
       JSON_OBJECT('zh-CN', '本地生活文案创作', 'en-US', 'Local Life Copywriting'),
       JSON_OBJECT('zh-CN', '本地生活的 AI 帮写文案', 'en-US', 'AI copywriting for local life'),
       'TEXT_CHAT', 1, 0
WHERE NOT EXISTS (SELECT 1 FROM `afv_feature` WHERE `feature_code` = 'locallife_text_gen' AND `deleted` = 0);

-- 2) 把 AI文案场景的分类从借用的 IMAGE 改成 TEXT，能力码换成上面新建的。
--    ⚠️ 能力码换了之后，如果以后打开权益校验（我们这边的 Billing.CheckEntitlement），
--    用户必须被发放 locallife_text_gen 才能用 AI文案。现在校验默认是关的，先不影响。
UPDATE `afv_billing_offer`
   SET `category` = 'TEXT',
       `billing_scene` = 'TEXT_GEN',
       `feature_code` = 'locallife_text_gen'
 WHERE `entry_code` = 'aiCopywriting' AND `deleted` = 0;

-- 3) 自查
SELECT `offer_code`, `entry_code`, `feature_code`, `category`, `billing_scene`, `status`
  FROM `afv_billing_offer` WHERE `entry_code` = 'aiCopywriting' AND `deleted` = 0;

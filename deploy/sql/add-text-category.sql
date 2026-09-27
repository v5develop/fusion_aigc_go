-- 给 AI文案加「文本」分类。
--
-- ⚠️ 2026-09-27 改过：原来这份 SQL 会新建一个 locallife_text_gen 能力码。
-- 产品定了「开权益」之后那样做是错的——新建的码**一个用户都没有被发放过**，
-- 开权益等于把 AI文案对所有人关掉。
-- 改成复用 ai_chat（联盟发了 14 人 + 手工 3 人 = 16 个有效用户）。
--
-- 不用改 Java 代码：afv_billing_offer 的 category 和 billing_scene 在它那边
-- 都是普通字符串（BillingOffer.java 里就是 private String，只在后台回显和
-- EntryOption 里透传），没有枚举校验。写数据就生效。
-- 唯一要留意的是**后台页面的分类下拉框可能是前端写死的**——下拉里没有 TEXT 的话，
-- 就用这条 SQL 直接改库。

UPDATE `afv_billing_offer`
   SET `category` = 'TEXT',
       `billing_scene` = 'TEXT_GEN',
       `feature_code` = 'ai_chat'   -- 不要换成新建的码，见上面的说明
 WHERE `entry_code` = 'aiCopywriting' AND `deleted` = 0;

-- 自查：category=TEXT、billing_scene=TEXT_GEN、feature_code=ai_chat
SELECT `offer_code`, `entry_code`, `feature_code`, `category`, `billing_scene`, `status`
  FROM `afv_billing_offer` WHERE `entry_code` = 'aiCopywriting' AND `deleted` = 0;

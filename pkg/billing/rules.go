package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// Java 在算完基础价之后还会过一层规则引擎（PricingRuleEngine）：
// 从 afv_billing_charge_rule 取**已发布且在生效窗口内**的规则，按因子匹配后加减点数。
//
//	因子有 6 个：userLevel / offerCode / qualityTier / referenceCount / outputCount / durationSeconds
//	效果有 ADD_POINTS / MULTIPLY / DISCOUNT（后两者 2026-09 从 double 截断改成了 HALF_UP）
//
// 其中 `userLevel` 最麻烦：它要按 afv_recharge_order 的充值记录加权打分、匹配
// afv_user_level_rule 的档位、再被 afv_user_level_assignment 的人工指定覆盖，
// 还有 5 分钟的本地缓存。**我们不打算复刻这一套。**
//
// 所以这里只做一件事：**发现有规则会命中我们，就拒绝算价**。
// 宁可当场报错让人看见，也不要算出一个和 Java 不一样的价——
// 那种错是静默的，用户看到的价和实际扣的对不上，等发现时账已经乱了一片。
//
// 库里现有两条规则的 scope_code 都是 `*`（通配），只是还停在 DRAFT。
// 哪天运营把任何一条发布了，我们这边立刻就会报错，而不是悄悄算错。

type ruleRow struct {
	RuleCode  string `db:"rule_code"`
	ScopeType string `db:"scope_type"`
	ScopeCode string `db:"scope_code"`
	FactorKey string `db:"factor_key"`
}

// checkNoPricingRules 确认没有已发布的规则会作用到这个 offer / 档位上。
func checkNoPricingRules(ctx context.Context, conn sqlx.SqlConn, offerCode, variantCode string) error {
	var rows []ruleRow
	// 匹配口径照 Java 的 matchesScope：通配 `*` 或对上 offer_code / variant_code。
	// 生效窗口：effective_time 为空或已到，expire_time 为空或未到。
	const q = "SELECT `rule_code`,`scope_type`,`scope_code`,`factor_key` FROM `afv_billing_charge_rule` " +
		"WHERE `status` = 'PUBLISHED' AND `deleted` = 0 " +
		"AND (`effective_time` IS NULL OR `effective_time` <= NOW()) " +
		"AND (`expire_time` IS NULL OR `expire_time` > NOW()) " +
		"AND (`scope_code` = '*' OR `scope_code` = ? OR `scope_code` = ?)"
	if err := conn.QueryRowsCtx(ctx, &rows, q, offerCode, variantCode); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	var names []string
	for _, r := range rows {
		names = append(names, fmt.Sprintf("%s(%s/%s，因子 %s)", r.RuleCode, r.ScopeType, r.ScopeCode, r.FactorKey))
	}
	return fmt.Errorf("%w: %s 命中了已发布的计价规则 %s。"+
		"Java 会按这些规则加减点数，我们没有实现规则引擎，算出来的价会和它不一致——"+
		"要么把规则的作用域排除掉本地生活的入口码，要么我们补上规则引擎。在此之前不计费",
		ErrPricingRule, offerCode, strings.Join(names, "、"))
}

// featureCodeOf 入口码挂的是哪个创作能力。只读一行，给准入校验用。
func featureCodeOf(ctx context.Context, conn sqlx.SqlConn, entryCode string) (string, error) {
	var code string
	const q = "SELECT `feature_code` FROM `afv_billing_offer` " +
		"WHERE `entry_code` = ? AND `status` = 'ENABLED' AND `deleted` = 0 LIMIT 1"
	switch err := conn.QueryRowCtx(ctx, &code, q, entryCode); {
	case errors.Is(err, sqlx.ErrNotFound):
		return "", fmt.Errorf("%w: %s", ErrNoOffer, entryCode)
	case err != nil:
		return "", err
	}
	return code, nil
}

// hasEntitlement 查用户有没有这个创作能力（afv_app_user_feature）。
//
// ⚠️ Java 的权益闸门在**报价**那一步（BillingPricingServiceImpl.validateFeatureAvailable），
// 不在 reserve 里。我们直接写表、不走它的报价接口，等于绕过了这道闸。
//
// 开关是 Conf.CheckEntitlement，产品 2026-09-27 定了开。
// 前提是入口码挂的能力码真有人被发放过，别挂 locallife_image_gen（只有 2 个手工发放）
// 也别新建能力码——新码零发放，开了就是把那个功能对所有人关掉。
func hasEntitlement(ctx context.Context, conn sqlx.SqlConn, userID int64, featureCode string) (bool, error) {
	if strings.TrimSpace(featureCode) == "" {
		return true, nil
	}
	var n int64
	const q = "SELECT COUNT(*) FROM `afv_app_user_feature` " +
		"WHERE `user_id` = ? AND `feature_code` = ? AND `status` = 'ACTIVE' AND `deleted` = 0 " +
		"AND (`effective_time` IS NULL OR `effective_time` <= NOW()) " +
		"AND (`expire_time` IS NULL OR `expire_time` > NOW())"
	if err := conn.QueryRowCtx(ctx, &n, q, userID, featureCode); err != nil {
		return false, err
	}
	return n > 0, nil
}

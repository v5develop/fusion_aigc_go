package billing

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 上线前的计费配置检查。
//
// 为什么要有：计费配置在 Java 的后台里，缺了或配错了，用户那头只会表现成"免费用"或"扣双份"，
// 没人会第一时间看见。这里把每个会计费的入口 × 档位真的算一遍价，算不出来就是配置有问题。
// 同一套 price() 线上也在用，所以检查通过 = 线上提交时不会因为缺配置而失败，
// 反过来，缺配置的入口线上提交会被拒（ErrNoOffer 等），不会静默放行。

// Target 一个要计费的入口。Tiers 为空表示不分档（图片类）。
type Target struct {
	Entry string
	Name  string
	Tiers []string
	Kind  Kind
}

// Targets 本地生活所有要计费的入口。**新增计费功能必须加在这里**，检查才会覆盖到它。
// AI文案不计费（产品 2026-10-11 定），不在列。
var Targets = []Target{
	{"imageTextNote", "AI图文笔记", nil, KindQuick},
	{"poster", "AI海报", nil, KindQuick},
	{"longImage", "AI长图", nil, KindQuick},
	{"digitalHuman", "AI数字人", []string{TierCloud, TierLite}, KindVideo},
	{"voiceover", "数字人口播", []string{TierCloud, TierLite}, KindVideo},
	{"actionTransfer", "动作迁移", []string{TierCloud, TierLite}, KindVideo},
	{"exploreShop", "探店视频", []string{TierCloud, TierLite}, KindVideo},
	{"shortDrama", "带货短剧", []string{TierCloud, TierLite}, KindVideo},
	{"videoRemake", "爆款视频复刻", []string{TierCloud, TierLite}, KindVideo},
	// studio（ai-gateway 生图）：每张图一张预留单。按出图模型分价：提交 / 报价 / 重做都带 Charge.ModelGroup，
	// 按档位默认模型的 gateway_model_group 选档（见 ModelGroupEntries）；分辨率差价在单价行的 resolution 上。
	{"studio_tryon", "商拍换装", nil, KindQuick},
	{"studio_pose", "姿态复刻", nil, KindQuick},
	{"studio_imagegen", "图片生成", nil, KindQuick},
	// canvas（画布）：每个 node run 一张预留单。只登记 Go 计费的三个：图片生成、视频生成、反推提示词；
	// 裁剪不计费，放大 / 扩图 / 局部重绘由 Java 兼容工具计费，试穿走 studio_tryon。
	{"canvas_image", "画布生图", nil, KindQuick},
	{"canvas_video", "画布视频", nil, KindVideo},
	{"canvas_tool_i2p", "画布反推提示词", nil, KindQuick},
}

// ModelGroupEntries 按 Charge.ModelGroup 分模型计价的入口：检查时除了默认档位，还把每个「默认模型带 gateway_model_group」
// 的档位各算一遍价（线上按模型组选到的就是这些档位）。没有任何分模型档位时报 WARN：所有模型都会按默认档位同价扣。
var ModelGroupEntries = map[string]bool{"studio_tryon": true, "studio_pose": true, "studio_imagegen": true}

// Level 问题级别。ERROR：这个入口现在提交会被拒或会错扣，必须修；WARN：能算出价，但看着不对，需要人看一眼。
type Level string

const (
	LevelError Level = "ERROR"
	LevelWarn  Level = "WARN"
)

// Finding 一条检查结果。
type Finding struct {
	Level  Level
	Entry  string
	Name   string
	Tier   string
	Detail string
}

func (f Finding) String() string {
	tier := f.Tier
	if tier == "" {
		tier = "-"
	}
	return fmt.Sprintf("[%s] %s(%s) 档位 %s：%s", f.Level, f.Name, f.Entry, tier, f.Detail)
}

// HasError 有没有必须修的问题。
func HasError(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == LevelError {
			return true
		}
	}
	return false
}

// Audit 逐个入口、逐个档位算一次价，并检查几处典型的配错。只读，不动账。
func Audit(ctx context.Context, conn sqlx.SqlConn) []Finding {
	var out []Finding
	for _, t := range Targets {
		tiers := t.Tiers
		if len(tiers) == 0 {
			tiers = []string{""}
		}
		for _, tier := range tiers {
			out = append(out, auditOne(ctx, conn, t, tier)...)
		}
		if ModelGroupEntries[t.Entry] {
			out = append(out, auditGroups(ctx, conn, t)...)
		}
	}
	return out
}

// auditGroups 分模型计价的入口：逐个模型组档位按 Charge.ModelGroup 试算（与线上提交同一个 price()），
// 并核对选中的确实是那个组的档位（没选中说明组码对不上，线上会退回默认档位按错的价扣）。
func auditGroups(ctx context.Context, conn sqlx.SqlConn, t Target) []Finding {
	add := func(l Level, group, d string, a ...any) Finding {
		return Finding{Level: l, Entry: t.Entry, Name: t.Name, Tier: "组 " + group, Detail: fmt.Sprintf(d, a...)}
	}
	var groups []string
	const q = "SELECT DISTINCT m.`gateway_model_group` FROM `afv_billing_offer` o " +
		"JOIN `afv_billing_offer_variant` v ON v.`offer_code` = o.`offer_code` AND v.`status` = 'ENABLED' AND v.`deleted` = 0 " +
		"JOIN `afv_ai_model` m ON m.`id` = v.`default_model_id` AND m.`deleted` = 0 " +
		"WHERE o.`entry_code` = ? AND o.`status` = 'ENABLED' AND o.`deleted` = 0 " +
		"AND m.`gateway_model_group` IS NOT NULL AND m.`gateway_model_group` <> '' ORDER BY 1"
	if err := conn.QueryRowsCtx(ctx, &groups, q, t.Entry); err != nil {
		return []Finding{add(LevelError, "-", "查分模型档位失败：%v", err)}
	}
	if len(groups) == 0 {
		return []Finding{add(LevelWarn, "-", "没有分模型档位（默认模型带 gateway_model_group 的档位），所有模型按默认档位同价扣")}
	}
	var out []Finding
	for _, g := range groups {
		r, err := price(ctx, conn, Charge{UserID: 1, RequestID: "audit", EntryCode: t.Entry, ModelGroup: g, Kind: t.Kind, Count: 1, Seconds: 15})
		switch {
		case err != nil:
			out = append(out, add(LevelError, g, "算不出价：%v", err))
		case r.ModelGroup != g:
			out = append(out, add(LevelError, g, "没有按组选中档位（选到 %s）", r.VariantCode))
		}
	}
	return out
}

func auditOne(ctx context.Context, conn sqlx.SqlConn, t Target, tier string) []Finding {
	add := func(l Level, d string, a ...any) Finding {
		return Finding{Level: l, Entry: t.Entry, Name: t.Name, Tier: tier, Detail: fmt.Sprintf(d, a...)}
	}
	// 用 15 秒 / 1 张试算，和线上真实提交走同一个 price()。
	q, err := price(ctx, conn, Charge{UserID: 1, RequestID: "audit", EntryCode: t.Entry, Tier: tier,
		Kind: t.Kind, Count: 1, Seconds: 15})
	if err != nil {
		switch {
		case errors.Is(err, ErrNoOffer):
			return []Finding{add(LevelError, "算不出价（缺场景 / 档位 / 步骤 / 单价）：%v", err)}
		case errors.Is(err, ErrPricingRule):
			return []Finding{add(LevelError, "命中未实现的计价规则：%v", err)}
		default:
			return []Finding{add(LevelError, "试算失败：%v", err)}
		}
	}
	var out []Finding
	if strings.TrimSpace(q.FeatureCode) == "" {
		out = append(out, add(LevelWarn, "场景没挂创作能力码，开了权益准入也拦不住没权益的人"))
	} else if n, err := issuedCount(ctx, conn, q.FeatureCode); err == nil && n == 0 {
		out = append(out, add(LevelError, "能力码 %s 没有任何人被发放过，开了准入等于对所有人关掉这个功能", q.FeatureCode))
	}

	// 一个档位里收费步骤超过一个会叠加扣费：要么确实是"基础费+按秒费"，要么是配重了。让人看一眼。
	var billed []string
	for _, l := range q.Lines {
		if !l.CostOnly {
			billed = append(billed, fmt.Sprintf("%s=%d点", l.ComponentCode, l.Points))
		}
	}
	if len(billed) > 1 {
		out = append(out, add(LevelWarn, "有 %d 个收费步骤会叠加扣费（合计 %d 点）：%s", len(billed), q.PricePoints, strings.Join(billed, " + ")))
	}
	// 步骤名带 lite 却挂在 cloud 档（或反过来）：典型的配错档位，会按错的价扣。
	for _, l := range q.Lines {
		code := strings.ToLower(l.ComponentCode)
		if tier == TierCloud && strings.Contains(code, "lite") {
			out = append(out, add(LevelError, "云端档下挂了 lite 的步骤 %s", l.ComponentCode))
		}
		if tier == TierLite && strings.Contains(code, "cloud") {
			out = append(out, add(LevelError, "lite 档下挂了云端的步骤 %s", l.ComponentCode))
		}
	}
	return out
}

func issuedCount(ctx context.Context, conn sqlx.SqlConn, featureCode string) (int64, error) {
	var n int64
	const q = "SELECT COUNT(*) FROM `afv_app_user_feature` WHERE `feature_code` = ? AND `status` = 'ACTIVE' AND `deleted` = 0 " +
		"AND (`expire_time` IS NULL OR `expire_time` > NOW())"
	err := conn.QueryRowCtx(ctx, &n, q, featureCode)
	return n, err
}

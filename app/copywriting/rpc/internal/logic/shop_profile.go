package logic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/config"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/xerr"
)

// charLen 按设计稿便签的口径数字数：汉字等全角字符算 2，其余算 1。
func charLen(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
			unicode.Is(unicode.Hangul, r) || (r >= 0xFF00 && r <= 0xFFEF) || (r >= 0x3000 && r <= 0x303F) {
			n += 2
			continue
		}
		n++
	}
	return n
}

// shopProfileForm 校验并整理表单；返回可直接落库的行。
func shopProfileForm(cfg config.ShopProfileConf, in *pb.ShopProfileForm) (*model.AfvShopProfile, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, xerr.Param("请输入店铺名称")
	}
	fields := []struct {
		label string
		value *string
		max   int
	}{
		{"店铺名称", &name, cfg.MaxName},
		{"开店时间", &in.OpenDate, cfg.MaxOpenDate},
		{"店铺地址", &in.Address, cfg.MaxAddress},
		{"提供商品/服务", &in.Products, cfg.MaxProducts},
		{"核心卖点", &in.SellingPoints, cfg.MaxSellingPoints},
		{"如何称呼自己", &in.SelfTitle, cfg.MaxSelfTitle},
		{"主要目标客群", &in.TargetAudience, cfg.MaxTargetAudience},
	}
	for _, f := range fields {
		*f.value = strings.TrimSpace(*f.value)
		if charLen(*f.value) > f.max {
			return nil, xerr.Newf(xerr.ParamError, "%s最多 %d 个字符（一个汉字算 2 个）", f.label, f.max)
		}
	}
	return &model.AfvShopProfile{
		Id: in.Id, UserId: in.UserId, Name: name,
		OpenDate: nullStr(in.OpenDate), Address: nullStr(in.Address), Products: nullStr(in.Products),
		SellingPoints: nullStr(in.SellingPoints), SelfTitle: nullStr(in.SelfTitle),
		TargetAudience: nullStr(in.TargetAudience),
	}, nil
}

func toShopProfile(p *model.AfvShopProfile) *pb.ShopProfile {
	return &pb.ShopProfile{
		Id: p.Id, Name: p.Name, OpenDate: p.OpenDate.String, Address: p.Address.String,
		Products: p.Products.String, SellingPoints: p.SellingPoints.String, SelfTitle: p.SelfTitle.String,
		TargetAudience: p.TargetAudience.String,
		CreateTime:     p.CreateTime.UnixMilli(), UpdateTime: p.UpdateTime.UnixMilli(),
	}
}

func findOwnedProfile(ctx context.Context, svcCtx *svc.ServiceContext, id, userID int64) (*model.AfvShopProfile, error) {
	p, err := svcCtx.ShopProfile.FindOwned(ctx, id, userID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("店铺档案")
	}
	return p, err
}

// profileText 把档案拼成给执行面的 shop_profile 文本；空字段跳过。
// 开店时间能解析成日期时，顺带算出已开店多久（对模型比一个日期更有用）。
func profileText(p *model.AfvShopProfile) string {
	var b strings.Builder
	add := func(label, v string) {
		if v = strings.TrimSpace(v); v != "" {
			fmt.Fprintf(&b, "%s：%s\n", label, v)
		}
	}
	add("店铺名称", p.Name)
	add("开店时间", openDateText(p.OpenDate.String))
	add("店铺地址", p.Address.String)
	add("提供商品/服务", p.Products.String)
	add("核心卖点", p.SellingPoints.String)
	add("如何称呼自己", p.SelfTitle.String)
	add("主要目标客群", p.TargetAudience.String)
	return strings.TrimRight(b.String(), "\n")
}

// openDateText 2025-10-03 → 「2025-10-03（已开店 1 年 2 个月）」；解析不了就原样返回。
func openDateText(v string) string {
	v = strings.TrimSpace(v)
	t, err := time.Parse("2006-01-02", v)
	if err != nil || !t.Before(time.Now()) {
		return v
	}
	months := monthsSince(t)
	switch {
	case months < 1:
		return fmt.Sprintf("%s（刚开店）", v)
	case months < 12:
		return fmt.Sprintf("%s（已开店 %d 个月）", v, months)
	case months%12 == 0:
		return fmt.Sprintf("%s（已开店 %d 年）", v, months/12)
	default:
		return fmt.Sprintf("%s（已开店 %d 年 %d 个月）", v, months/12, months%12)
	}
}

// shopProfileTextOf 供其他逻辑复用：取某条档案拼好的文本；id 为 0 或档案不存在返回空串。
func shopProfileTextOf(ctx context.Context, svcCtx *svc.ServiceContext, id, userID int64) string {
	if id <= 0 {
		return ""
	}
	p, err := svcCtx.ShopProfile.FindOwned(ctx, id, userID)
	if err != nil {
		return ""
	}
	return profileText(p)
}

// monthsSince 按自然年月算相差几个月（当月天数不够时不算满一个月）。
func monthsSince(t time.Time) int {
	now := time.Now()
	months := (now.Year()-t.Year())*12 + int(now.Month()-t.Month())
	if now.Day() < t.Day() {
		months--
	}
	if months < 0 {
		months = 0
	}
	return months
}

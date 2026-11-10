package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type QuoteLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewQuoteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *QuoteLogic {
	return &QuoteLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Quote 提交前报价（只读）。数字人按配音实际时长计费，时长要等配音（TTS）出来才知道，所以报不出准数：
//   - points     = 最短时长（minVideoSeconds）的价，与提交前余额快拦（Enough）同一个计费请求；
//   - points_max = 该档最长时长（maxSecondsOf）的价，配音更长会直接失败、不收费；
//   - exact=false，stage=submit（冻结在提交后的配音完成时，用户不用再操作）；
//   - sufficient_max = 余额够不够 points_max：不够时前端只提示「余额可能不足以覆盖较长配音」，不拦提交。
func (l *QuoteLogic) Quote(in *pb.QuoteReq) (*pb.QuoteResp, error) {
	mdl, err := normalizeModel(in.Model)
	if err != nil {
		_, msg, _ := xerr.Parse(err)
		return &pb.QuoteResp{Reason: msg, Sufficient: true, Stage: "submit"}, nil
	}
	lo := l.svcCtx.Billing.Estimate(l.ctx, minHumanCharge(in.UserId, mdl))
	out := toQuoteResp(lo)
	if lo.Available && lo.Mode != billing.ModeOff {
		hi := l.svcCtx.Billing.Estimate(l.ctx, humanCharge(&model.AfvDigitalHuman{UserId: in.UserId, Model: mdl}, "", maxSecondsOf(mdl)))
		if hi.Available {
			out.PointsMax = hi.Points
		}
	}
	// 按秒计价时才是「要等时长」；档位若是按条定价，最低价就是准数
	out.Exact = lo.Mode == billing.ModeOff || (lo.Available && lo.PriceUnit != "PER_SECOND")
	if out.Exact {
		out.PointsMax = 0
	}
	out.SufficientMax = sufficientMax(out)
	return out, nil
}

func toQuoteResp(e billing.Estimate) *pb.QuoteResp {
	return &pb.QuoteResp{
		Available: e.Available, Reason: e.Reason, BillingMode: e.Mode, Charged: e.Charged,
		Points: e.Points, UnitPoints: e.UnitPoints, PriceUnit: e.PriceUnit, Quantity: int32(e.Quantity),
		Exact: true, Stage: "submit", Balance: e.Balance, BalanceKnown: e.BalanceKnown, Sufficient: e.Sufficient,
	}
}

// sufficientMax 余额够不够最高价：只有真扣、余额已知、有最高价时才比；否则同 sufficient（准数 / 不扣 / 不知道）。
func sufficientMax(r *pb.QuoteResp) bool {
	if !r.Exact && r.Charged && r.BalanceKnown && r.PointsMax > 0 {
		return r.Balance >= r.PointsMax
	}
	return r.Sufficient
}

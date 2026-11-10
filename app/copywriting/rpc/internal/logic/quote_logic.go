package logic

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"

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

// Quote 提交前报价（只读）：报的是**提交生成**的价——只有 Generate（source=AI_WRITE / IMITATE，异步出整篇文案）
// 和 Regenerate（重新生成，算新的一次）冻结点数，计费请求与 Generate 的 copywritingCharge 同一个（入口码 BillingEntryCode）。
// 不收费、不该挂这个价的：AiWrite（各输入框旁的「AI 帮写 / AI 优化」同步辅助，含数字人配音、笔记主题、海报信息等场景）、
// 手动保存（Create / Update）。
func (l *QuoteLogic) Quote(in *pb.QuoteReq) (*pb.QuoteResp, error) {
	e := l.svcCtx.Billing.Estimate(l.ctx, copywritingCharge(l.svcCtx.Config.BillingEntryCode, in.UserId, ""))
	return &pb.QuoteResp{
		Available: e.Available, Reason: e.Reason, BillingMode: e.Mode, Charged: e.Charged,
		Points: e.Points, UnitPoints: e.UnitPoints, PriceUnit: e.PriceUnit, Quantity: int32(e.Quantity),
		Exact: true, Stage: "submit", Balance: e.Balance, BalanceKnown: e.BalanceKnown, Sufficient: e.Sufficient,
	}, nil
}

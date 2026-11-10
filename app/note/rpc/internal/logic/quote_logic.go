package logic

import (
	"context"

	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/app/note/rpc/pb"
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

// Quote 提交前报价（只读）：构思（74）不收费，出图（75）按张冻结。
//   - 不带 id（填表阶段）：张数与构思同一个归一（noteImageCount）；
//   - 带 id（配图阶段，方案已出）：按记录的张数（image_count 非 0 时覆盖），并与出图同口径校验不超过方案张数
//     （generateImageCount），不合法回 available=false + 与出图相同的提示。
//
// 计费请求与出图同一个 noteCharge，所以报出的就是出图会冻结的点数。
func (l *QuoteLogic) Quote(in *pb.QuoteReq) (*pb.QuoteResp, error) {
	count, err := l.count(in)
	if err != nil {
		if _, msg, ok := xerr.Parse(err); ok {
			return &pb.QuoteResp{Reason: msg, Sufficient: true, Stage: "generate"}, nil
		}
		return nil, err // 查库失败等内部错误照常报错
	}
	e := l.svcCtx.Billing.Estimate(l.ctx, noteCharge(in.UserId, "", int(count)))
	return toQuoteResp(e), nil
}

func (l *QuoteLogic) count(in *pb.QuoteReq) (int64, error) {
	if in.Id <= 0 {
		return noteImageCount(l.svcCtx.Config.Limits.DefaultImages, in.ImageCount)
	}
	n, err := findOwned(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return 0, err
	}
	if n.Status != model.StatusPlanned && n.Status != model.StatusGenFailed {
		// 还在构思（方案未出）或已在出图 / 已完成：没有方案可校验，按记录张数（或覆盖值）报
		return noteImageCount(int(n.ImageCount), in.ImageCount)
	}
	s, err := parseScript([]byte(n.PlanScript.String))
	if err != nil {
		l.Errorf("note %d parse script: %v", n.Id, err)
		return 0, xerr.New(xerr.Conflict, "构思结果异常，请重新构思") // 与出图同一句
	}
	c, err := generateImageCount(n.ImageCount, in.ImageCount, len(s.Images))
	return int64(c), err
}

// noteImageCount 套图张数：不传用默认，至少 1 张（不设上限）。构思与报价共用。
func noteImageCount(def int, n int32) (int64, error) {
	count := int64(n)
	if count == 0 {
		count = int64(def)
	}
	if count < 1 {
		return 0, xerr.Param("套图数量至少 1 张")
	}
	return count, nil
}

func toQuoteResp(e billing.Estimate) *pb.QuoteResp {
	return &pb.QuoteResp{
		Available: e.Available, Reason: e.Reason, BillingMode: e.Mode, Charged: e.Charged,
		Points: e.Points, UnitPoints: e.UnitPoints, PriceUnit: e.PriceUnit, Quantity: int32(e.Quantity),
		Exact: true, Stage: "generate", Balance: e.Balance, BalanceKnown: e.BalanceKnown, Sufficient: e.Sufficient,
	}
}

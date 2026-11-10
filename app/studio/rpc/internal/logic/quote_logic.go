package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
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

// 提交前报价（只读）：与提交同一套模型 / 分辨率归一与计费入口，返回本次提交会冻结的点数；不落库、不冻结、不校验素材
func (l *QuoteLogic) Quote(in *studio.QuoteReq) (*studio.QuoteResp, error) {
	k := model.Kind(in.Kind)
	if !k.Valid() {
		return nil, xerr.Param("kind 只能是 tryon / pose / imagegen")
	}
	c := l.svcCtx.Config
	var q quoter
	if l.svcCtx.Billing != nil {
		q = l.svcCtx.Billing
	}
	return quoteStudio(l.ctx, q, svc.EntryCode(c, k), k, in, c.ModelGroupMap()), nil
}

// quoter 报价只要算价与计费开关（*billing.Service 满足）。
type quoter interface {
	Mode() string
	Quote(ctx context.Context, c billing.Charge) (*billing.Quote, error)
}

// quoteStudio 报价核心：输入归一与提交（ValidateTryon / ValidatePose / ValidateImageGen）同一组函数，
// 计费请求与提交逐张冻结的 charge 同形（入口码 + 模型组 + Count=1 + 分辨率档），所以单价 × 张数 = 提交回的 estimated_points。
// 只读：不冻结、不查余额、不做准入（准入失败在提交时照常拦）。报不出价不算错误，回 available=false + 原因。
func quoteStudio(ctx context.Context, q quoter, entry string, k model.Kind, in *studio.QuoteReq, groups map[string]string) *studio.QuoteResp {
	total := in.TaskTotal
	if total < 0 {
		total = 0
	}
	out := &studio.QuoteResp{PriceUnit: "PER_IMAGE", TaskTotal: total, Warnings: []string{}}
	unavailable := func(reason string) *studio.QuoteResp {
		out.Available, out.Points, out.UnitPoints, out.Reason = false, 0, 0, reason
		return out
	}

	res, err := NormalizeResolution(in.Resolution)
	if err != nil {
		return unavailable(errText(err))
	}
	def := DefaultAspectRatio
	if k == model.KindImageGen {
		def = DefaultImageGenAspectRatio
	}
	ar, err := NormalizeAspectRatio(in.AspectRatio, def)
	if err != nil {
		return unavailable(errText(err))
	}
	_, group, err := NormalizeModel(in.Model, groups)
	if err != nil {
		return unavailable(errText(err))
	}
	if res, out.Warnings, err = pipeline.NormalizeForGroup(group, res, ar); err != nil {
		return unavailable(errText(err))
	}
	if out.Warnings == nil {
		out.Warnings = []string{}
	}
	out.Resolution = res

	if q == nil {
		out.BillingMode = billing.ModeOff
		out.Available = true
		return out
	}
	out.BillingMode = q.Mode()
	out.Charged = out.BillingMode == billing.ModeOn
	if out.BillingMode == billing.ModeOff {
		// 不计费：提交不冻结（estimated_points=0），这里如实回 0 点
		out.Available = true
		return out
	}
	r, err := q.Quote(ctx, quoteCharge(entry, in.UserId, group, res))
	if err != nil {
		logx.WithContext(ctx).Errorf("studio: 报价失败 用户 %d 入口 %s 模型组 %s 分辨率 %s: %v", in.UserId, entry, group, res, err)
		if errors.Is(err, billing.ErrNoOffer) {
			return unavailable(billing.MsgNoOffer)
		}
		return unavailable("暂时无法报价，请稍后重试")
	}
	out.Available = true
	out.UnitPoints = r.PricePoints
	out.Points = r.PricePoints * int64(total)
	// ACTUAL_COST（如 GPT Image 2.6）：points 是冻结上限，成功后逐张按供应商实报成本结算、退回差额
	out.SettleMode, out.MaxPoints = r.SettleMode, out.Points
	if out.SettleMode == "" {
		out.SettleMode = billing.SettleFixed
	}
	if r.PriceUnit != "" {
		out.PriceUnit = r.PriceUnit
	}
	return out
}

// errText 参数错误给用户看的那句话。
func errText(err error) string {
	var e *xerr.CodeError
	if errors.As(err, &e) {
		return e.Msg
	}
	return err.Error()
}

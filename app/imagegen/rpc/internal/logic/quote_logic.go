package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
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

// 冻结发生在哪一步（QuoteResp.stage）
const (
	stageSubmit   = "submit"   // 提交即冻结：海报 / 长图 / 动作迁移 / 口播
	stageGenerate = "generate" // 构思（出剧本 / 解析）免费，确认出片时冻结：探店 / 短剧 / 复刻
)

// Quote 提交前报价（只读）：参数归一与提交同一组函数，计费请求用提交时同一个 imageCharge / videoCharge 构造，
// 所以报出的点数就是提交（或出片）会冻结的点数。参数不合法不算接口错误，回 available=false + 原因。
func (l *QuoteLogic) Quote(in *pb.QuoteReq) (*pb.QuoteResp, error) {
	q, err := quoteChargeOf(l.ctx, l.svcCtx, in)
	if err != nil {
		return &pb.QuoteResp{Reason: errText(err), Sufficient: true, Stage: q.stage}, nil
	}
	e := l.svcCtx.Billing.Estimate(l.ctx, q.charge)
	out := toQuoteResp(e, q.stage)
	if q.secondsUnknown && e.PriceUnit == "PER_SECOND" {
		// 复刻没解析出原片时长：points 报最短计费时长（minRemakeSeconds，解析回填时也会抬到这个下限）的价，单价看 unit_points
		out.Exact = false
	}
	return out, nil
}

type imageQuote struct {
	charge billing.Charge
	stage  string
	// secondsUnknown 时长要等提交后才知道（复刻未解析）：按最短计费时长 minRemakeSeconds 报最低价
	secondsUnknown bool
}

// quoteChargeOf 把报价参数归一成提交时的那个计费请求。
func quoteChargeOf(ctx context.Context, svcCtx *svc.ServiceContext, in *pb.QuoteReq) (imageQuote, error) {
	switch in.Kind {
	case model.KindPoster, model.KindLongImage:
		count, err := imageCountOf(svcCtx.Config.Limits.MaxImages, in.ImageCount)
		if err != nil {
			return imageQuote{stage: stageSubmit}, err
		}
		// 海报的模型档位不参与计价（imageCharge 不看 Model），这里也不校验
		return imageQuote{stage: stageSubmit, charge: imageCharge(&model.LocalLifeCreation{
			UserId: in.UserId, Kind: in.Kind, ImageCount: count,
		})}, nil
	case model.KindActionTransfer:
		if !validModel(optionsOf(ctx, svcCtx, genActionModels, actionModels), in.Model) {
			return imageQuote{stage: stageSubmit}, xerr.Param("请选择模型")
		}
		// 动作视频的时长提交时也不知道（Duration=0），由档位默认时长计价——与提交一致
		return imageQuote{stage: stageSubmit, charge: imageCharge(&model.LocalLifeCreation{
			UserId: in.UserId, Kind: in.Kind, Model: in.Model, ImageCount: 1,
		})}, nil
	case model.KindVoiceover:
		mdl, duration, err := voiceoverModelDuration(ctx, svcCtx, in.Model, in.Duration)
		if err != nil {
			return imageQuote{stage: stageSubmit}, err
		}
		return imageQuote{stage: stageSubmit, charge: imageCharge(&model.LocalLifeCreation{
			UserId: in.UserId, Kind: in.Kind, Model: mdl, Duration: int64(duration), ImageCount: 1,
		})}, nil
	case model.KindExploreShop, model.KindShortDrama, model.KindVideoRemake:
		return videoQuoteOf(ctx, svcCtx, in)
	}
	return imageQuote{}, xerr.Newf(xerr.ParamError, "不支持的类型 %s", in.Kind)
}

// videoQuoteOf 视频类：出片时冻结，计费请求即 videoCharge。
//   - 带 id（已构思 / 已解析的记录）：按记录的档位与时长报（复刻可带 model 覆盖，同出片的 applyRemakeOverrides）；
//   - 不带 id：探店 / 短剧按表单的档位与时长报；复刻原片时长未知，按最短计费时长（4 秒）报最低价（exact=false）。
func videoQuoteOf(ctx context.Context, svcCtx *svc.ServiceContext, in *pb.QuoteReq) (imageQuote, error) {
	q := imageQuote{stage: stageGenerate}
	mdl := strings.TrimSpace(in.Model)
	if in.Kind == model.KindVideoRemake && mdl != "" && !validRemakeModel(ctx, svcCtx, mdl) {
		return q, xerr.Newf(xerr.ParamError, "不支持的模型 %s", mdl)
	}
	if in.Id > 0 {
		c, err := findOwnedVideo(ctx, svcCtx, in.Id, in.UserId)
		if err != nil {
			return q, err
		}
		if c.Kind != in.Kind {
			return q, xerr.NotFoundf("记录")
		}
		if in.Kind == model.KindVideoRemake && mdl != "" {
			c.Model = mdl
		}
		q.charge = videoCharge(c, "")
		q.secondsUnknown = c.Duration <= 0
		if q.secondsUnknown {
			q.charge.Seconds = minRemakeSeconds // 解析回填的时长经 clampRemakeSeconds 至少这么长，报 1 秒会把最低价报成 1/4
		}
		return q, nil
	}
	if in.Kind == model.KindVideoRemake {
		// 出片时 c.Duration 是解析回填的原片时长，这里还不知道；档位按出片时的规则（不传 = 记录上的空档位 → 云端）
		q.charge = videoCharge(&model.AfvVideoCreation{UserId: in.UserId, Kind: in.Kind, Model: mdl, Duration: minRemakeSeconds}, "")
		q.secondsUnknown = true
		return q, nil
	}
	cfg, err := videoKindConf(svcCtx, in.Kind)
	if err != nil {
		return q, err
	}
	duration, err := videoDurationOf(ctx, svcCtx, cfg, in.Duration)
	if err != nil {
		return q, err
	}
	if mdl, err = videoModelOf(ctx, svcCtx, cfg, mdl); err != nil {
		return q, err
	}
	q.charge = videoCharge(&model.AfvVideoCreation{UserId: in.UserId, Kind: in.Kind, Model: mdl, Duration: int64(duration)}, "")
	return q, nil
}

func toQuoteResp(e billing.Estimate, stage string) *pb.QuoteResp {
	return &pb.QuoteResp{
		Available: e.Available, Reason: e.Reason, BillingMode: e.Mode, Charged: e.Charged,
		Points: e.Points, UnitPoints: e.UnitPoints, PriceUnit: e.PriceUnit, Quantity: int32(e.Quantity),
		Exact: true, Stage: stage, Balance: e.Balance, BalanceKnown: e.BalanceKnown, Sufficient: e.Sufficient,
	}
}

// errText 参数错误给用户看的那句话。
func errText(err error) string {
	if _, msg, ok := xerr.Parse(err); ok && msg != "" {
		return msg
	}
	return "暂时无法报价，请稍后重试"
}

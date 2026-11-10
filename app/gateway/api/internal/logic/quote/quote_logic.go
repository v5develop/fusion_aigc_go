// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package quote

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type QuoteLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 提交前报价（只读，不冻结）：按工具与当前表单参数，返回提交（或出片 / 出图）会冻结的点数、余额与是否足够；与提交走同一套计费请求构造与算价
func NewQuoteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *QuoteLogic {
	return &QuoteLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// Quote 按工具分发给真正冻结点数的那个服务——报价和冻结在同一个进程、用同一个计费请求构造函数，
// 网关只做转发与字段映射，不自己算价。
func (l *QuoteLogic) Quote(req *types.LocalLifeQuoteReq) (resp *types.LocalLifeQuoteResp, err error) {
	uid := ctxdata.UserID(l.ctx)
	var out quoteResult
	switch req.Tool {
	case "poster", "longImage", "actionTransfer", "voiceover", "exploreShop", "shortDrama", "videoRemake":
		out, err = l.svcCtx.ImageGenRpc.Quote(l.ctx, &imagegen.QuoteReq{
			UserId: uid, Kind: req.Tool, Model: req.Model, Duration: req.Duration, ImageCount: req.ImageCount, Id: req.Id,
		})
	case "digitalHuman":
		out, err = l.svcCtx.DigitalHumanRpc.Quote(l.ctx, &digitalhuman.QuoteReq{UserId: uid, Model: req.Model})
	case "imageTextNote":
		out, err = l.svcCtx.NoteRpc.Quote(l.ctx, &note.QuoteReq{UserId: uid, ImageCount: req.ImageCount, Id: req.Id})
	case "copywriting":
		out, err = l.svcCtx.CopywritingRpc.Quote(l.ctx, &copywriting.QuoteReq{UserId: uid})
	default:
		return nil, xerr.Param("不支持的工具")
	}
	if err != nil {
		return nil, err
	}
	return toResp(out), nil
}

// quoteResult 四个服务的 QuoteResp 同形（数字人多一个 sufficient_max，见 sufficientMax）（各自 proto 里定义），按 getter 取值。
type quoteResult interface {
	GetAvailable() bool
	GetReason() string
	GetBillingMode() string
	GetCharged() bool
	GetPoints() int64
	GetUnitPoints() int64
	GetPriceUnit() string
	GetQuantity() int32
	GetExact() bool
	GetPointsMax() int64
	GetStage() string
	GetBalance() int64
	GetBalanceKnown() bool
	GetSufficient() bool
}

func toResp(r quoteResult) *types.LocalLifeQuoteResp {
	return &types.LocalLifeQuoteResp{
		Available: r.GetAvailable(), Reason: r.GetReason(), BillingMode: r.GetBillingMode(), Charged: r.GetCharged(),
		Points: r.GetPoints(), UnitPoints: r.GetUnitPoints(), PriceUnit: r.GetPriceUnit(), Quantity: r.GetQuantity(),
		Exact: r.GetExact(), PointsMax: r.GetPointsMax(), Stage: r.GetStage(),
		Balance: r.GetBalance(), BalanceKnown: r.GetBalanceKnown(), Sufficient: r.GetSufficient(),
		SufficientMax: sufficientMax(r),
		FreeQuota:     0, // 本地生活冻结不扣免费额度（pkg/billing.Estimate 注释）
	}
}

// sufficientMax 只有数字人报 sufficient_max（按配音最长时长的价比余额）；其余工具没有最高价，同 sufficient。
func sufficientMax(r quoteResult) bool {
	if m, ok := r.(interface{ GetSufficientMax() bool }); ok {
		return m.GetSufficientMax()
	}
	return r.GetSufficient()
}

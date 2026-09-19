// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AI帮写或爆款仿写：提交生成
func NewGenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateLogic {
	return &GenerateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GenerateLogic) Generate(req *types.GenerateReq) (resp *types.Copywriting, err error) {
	out, err := l.svcCtx.CopywritingRpc.Generate(l.ctx, &copywriting.GenerateReq{
		UserId: ctxdata.UserID(l.ctx), Source: req.Source, ContentType: req.ContentType,
		Style: req.Style, Industry: req.Industry, Topic: req.Topic, ReferenceText: req.ReferenceText,
		TargetWords: req.TargetWords, ShopProfileId: req.ShopProfileId, RequestId: req.RequestId,
	})
	if err != nil {
		return nil, err
	}
	return toCopywriting(out), nil
}

// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 文案选项：内容类型/风格/行业/字数 + 当前模型
func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OptionsLogic) Options() (resp *types.OptionsResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.Options(l.ctx, &copywriting.Empty{})
	if err != nil {
		return nil, err
	}
	resp = &types.OptionsResp{
		ContentTypes: toOptions(out.ContentTypes), Styles: toOptions(out.Styles),
		Industries: toOptions(out.Industries), WordCounts: toOptions(out.WordCounts),
		MaxTopicLen: out.MaxTopicLen, MaxReferenceLen: out.MaxReferenceLen,
	}
	if out.Model != nil {
		resp.Model = types.ModelInfo{Name: out.Model.Name, Code: out.Model.Code}
	}
	return resp, nil
}

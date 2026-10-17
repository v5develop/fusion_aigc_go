// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package imagegen

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ImportActionVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 导入动作视频（前端直传后回传地址）
func NewImportActionVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ImportActionVideoLogic {
	return &ImportActionVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ImportActionVideoLogic) ImportActionVideo(req *types.ImportActionVideoReq) (resp *types.ActionVideo, err error) {
	out, err := l.svcCtx.ImageGenRpc.ImportActionVideo(l.ctx, &imagegen.ImportActionVideoReq{
		UserId: ctxdata.UserID(l.ctx), Url: req.Url, CoverUrl: req.CoverUrl, Name: req.Name,
	})
	if err != nil {
		return nil, err
	}
	return &types.ActionVideo{Id: out.Id, Name: out.Name, Url: out.Url, CoverUrl: out.CoverUrl, Mine: out.Mine}, nil
}

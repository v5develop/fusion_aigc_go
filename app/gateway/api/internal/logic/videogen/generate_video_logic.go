// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package videogen

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/imagegen/rpc/imagegen"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateVideoLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 确认剧本，开始生成视频（探店 73 / 短剧 88）
func NewGenerateVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateVideoLogic {
	return &GenerateVideoLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GenerateVideoLogic) GenerateVideo(req *types.GenerateVideoReq) (resp *types.VideoCreation, err error) {
	out, err := l.svcCtx.ImageGenRpc.GenerateVideo(l.ctx, &imagegen.GenerateVideoReq{
		Id: req.Id, UserId: ctxdata.UserID(l.ctx),
		Prompt: req.Prompt, Model: req.Model, Size: req.Size,
	})
	if err != nil {
		return nil, err
	}
	return toVideoCreation(out), nil
}

package logic

import (
	"context"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitImageGenLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitImageGenLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitImageGenLogic {
	return &SubmitImageGenLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 图片生成：count 张，一张一个子任务
func (l *SubmitImageGenLogic) SubmitImageGen(in *studio.SubmitImageGenReq) (*studio.SubmitResp, error) {
	v, err := ValidateImageGen(in, l.svcCtx.Config.ModelGroupMap())
	if err != nil {
		return nil, err
	}
	// rewrite_prompt 记在 payload 里，由编排在生图前先进「反推中」润色一次
	return newDeps(l.svcCtx).submit(l.ctx, imageGenSpec(in, v))
}

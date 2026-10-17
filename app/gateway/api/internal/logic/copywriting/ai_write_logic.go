// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type AiWriteLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AI帮写 / AI优化（同步）
func NewAiWriteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AiWriteLogic {
	return &AiWriteLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *AiWriteLogic) AiWrite(req *types.AiWriteReq) (resp *types.AiWriteResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.AiWrite(l.ctx, &copywriting.AiWriteReq{
		UserId: ctxdata.UserID(l.ctx), Scene: req.Scene, ContentType: req.ContentType,
		Industry: req.Industry, Context: req.Context, MaxLen: req.MaxLen, NoteType: req.NoteType,
	})
	if err != nil {
		return nil, err
	}
	return &types.AiWriteResp{Text: out.Text}, nil
}

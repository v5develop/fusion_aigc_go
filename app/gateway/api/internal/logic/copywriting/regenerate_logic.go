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

type RegenerateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 重新生成
func NewRegenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegenerateLogic {
	return &RegenerateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RegenerateLogic) Regenerate(req *types.RegenerateReq) (resp *types.Copywriting, err error) {
	out, err := l.svcCtx.CopywritingRpc.Regenerate(l.ctx, &copywriting.RegenerateReq{
		Id: req.Id, UserId: ctxdata.UserID(l.ctx), RequestId: req.RequestId,
	})
	if err != nil {
		return nil, err
	}
	return toCopywriting(out), nil
}

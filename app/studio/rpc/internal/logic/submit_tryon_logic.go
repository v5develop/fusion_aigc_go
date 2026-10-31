package logic

import (
	"context"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"

	"github.com/zeromicro/go-zero/core/logx"
)

type SubmitTryonLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewSubmitTryonLogic(ctx context.Context, svcCtx *svc.ServiceContext) *SubmitTryonLogic {
	return &SubmitTryonLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 用户侧 ----
func (l *SubmitTryonLogic) SubmitTryon(in *studio.SubmitTryonReq) (*studio.SubmitResp, error) {
	v, err := ValidateTryon(in, l.svcCtx.Config.ModelGroupMap())
	if err != nil {
		return nil, err
	}
	// 编排（切图 → 反推 → 生图）交给 Poller；这里只收单、冻结点数
	return newDeps(l.svcCtx).submit(l.ctx, tryonSpec(in, v))
}

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
	// 参考图上限预警取 gateway 目录（与 /studio/models 同一份缓存），与生图同口径
	v, err := ValidateTryonCap(in, l.svcCtx.Config.ModelGroupMap(), func(group string) int {
		return catalogRefCap(studioCatalog.get(l.ctx, l.svcCtx.Gateway), group)
	})
	if err != nil {
		return nil, err
	}
	// 编排（切图 → 反推 → 生图）交给 Poller；这里只收单、冻结点数
	return newDeps(l.svcCtx).submit(l.ctx, tryonSpec(in, v))
}

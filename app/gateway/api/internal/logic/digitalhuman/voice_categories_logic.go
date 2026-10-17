// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type VoiceCategoriesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 系统音色分类
func NewVoiceCategoriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VoiceCategoriesLogic {
	return &VoiceCategoriesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *VoiceCategoriesLogic) VoiceCategories() (resp *types.VoiceCategoriesResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.VoiceCategories(l.ctx, &digitalhuman.Empty{})
	if err != nil {
		return nil, err
	}
	return &types.VoiceCategoriesResp{List: toOptions(out.List)}, nil
}

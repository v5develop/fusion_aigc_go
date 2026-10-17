// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListVoicesLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 系统音色列表
func NewListVoicesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListVoicesLogic {
	return &ListVoicesLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListVoicesLogic) ListVoices(req *types.VoiceListReq) (resp *types.VoiceListResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AdminListVoices(l.ctx, &digitalhuman.ListVoicesReq{Category: req.Category})
	if err != nil {
		return nil, err
	}
	resp = &types.VoiceListResp{List: make([]types.Voice, 0, len(out.List))}
	for _, v := range out.List {
		resp.List = append(resp.List, *toVoice(v))
	}
	return resp, nil
}

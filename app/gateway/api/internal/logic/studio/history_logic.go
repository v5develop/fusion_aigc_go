// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package studio

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type HistoryLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 历史（游标分页），含提交快照供回填
func NewHistoryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *HistoryLogic {
	return &HistoryLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *HistoryLogic) History(req *types.StudioHistoryReq) (resp *types.StudioHistoryResp, err error) {
	out, err := l.svcCtx.StudioRpc.ListHistory(l.ctx, &studio_client.ListHistoryReq{
		UserId: ctxdata.UserID(l.ctx), Kind: req.Kind, Cursor: req.Cursor, Limit: req.Limit,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.StudioHistoryResp{List: make([]types.StudioBatchDetail, 0, len(out.Items)), NextCursor: out.NextCursor}
	for _, it := range out.Items {
		resp.List = append(resp.List, toDetail(it))
	}
	return resp, nil
}

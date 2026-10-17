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

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 文案列表（含搜索）
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.ListReq) (resp *types.ListResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.List(l.ctx, &copywriting.ListReq{
		UserId: ctxdata.UserID(l.ctx), Keyword: req.Keyword, ContentType: req.ContentType,
		Status: req.Status, Page: req.Page, PageSize: req.PageSize,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.ListResp{Total: out.Total, List: make([]types.Copywriting, 0, len(out.List))}
	for _, c := range out.List {
		resp.List = append(resp.List, *toCopywriting(c))
	}
	return resp, nil
}

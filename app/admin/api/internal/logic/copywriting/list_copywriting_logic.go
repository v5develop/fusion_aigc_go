// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package copywriting

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/copywriting/rpc/copywriting"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListCopywritingLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户文案列表
func NewListCopywritingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListCopywritingLogic {
	return &ListCopywritingLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListCopywritingLogic) ListCopywriting(req *types.CopywritingListReq) (resp *types.CopywritingListResp, err error) {
	out, err := l.svcCtx.CopywritingRpc.AdminListCopywriting(l.ctx, &copywriting.AdminListReq{
		Page: req.Page, PageSize: req.PageSize, UserId: req.UserId, Keyword: req.Keyword,
	})
	if err != nil {
		return nil, err
	}
	resp = &types.CopywritingListResp{Total: out.Total, List: make([]types.Copywriting, 0, len(out.List))}
	for _, c := range out.List {
		resp.List = append(resp.List, types.Copywriting{
			Id: c.Id, Source: c.Source, ContentType: c.ContentType, ContentTypeLabel: c.ContentTypeLabel,
			Title: c.Title, Body: c.Body, WordCount: c.WordCount, Status: c.Status,
			FailReason: c.FailReason, CreateTime: c.CreateTime,
		})
	}
	return resp, nil
}

// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"context"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/app/note/rpc/note"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用户图文笔记列表（只读，排查用）
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.NoteListReq) (resp *types.NoteListResp, err error) {
	out, err := l.svcCtx.NoteRpc.AdminList(l.ctx, &note.ListReq{UserId: req.UserId, Status: req.Status, Page: req.Page, PageSize: req.PageSize})
	if err != nil {
		return nil, err
	}
	resp = &types.NoteListResp{List: make([]types.Note, 0, len(out.List)), Total: out.Total}
	for _, n := range out.List {
		resp.List = append(resp.List, types.Note{
			Id: n.Id, UserId: n.UserId, NoteType: n.NoteType, NoteTypeLabel: n.NoteTypeLabel, Topic: n.Topic,
			Title: n.Title, Content: n.Content, Tags: orEmpty(n.Tags), ImageUrls: orEmpty(n.ImageUrls),
			Status: n.Status, FailReason: n.FailReason, CreateTime: n.CreateTime,
		})
	}
	return resp, nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

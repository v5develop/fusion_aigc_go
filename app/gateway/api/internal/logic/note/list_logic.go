// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"context"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/app/note/rpc/note"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 笔记列表（素材库接入前使用）
func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ListLogic) List(req *types.ListNotesReq) (resp *types.NoteListResp, err error) {
	out, err := l.svcCtx.NoteRpc.List(l.ctx, &note.ListReq{UserId: ctxdata.UserID(l.ctx), Status: req.Status, Page: req.Page, PageSize: req.PageSize})
	if err != nil {
		return nil, err
	}
	resp = &types.NoteListResp{List: make([]types.Note, 0, len(out.List)), Total: out.Total}
	for _, n := range out.List {
		resp.List = append(resp.List, *toNote(n))
	}
	return resp, nil
}

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

type UpdateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 编辑笔记：标题、正文、话题、图片
func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	return &UpdateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *UpdateLogic) Update(req *types.UpdateNoteReq) (resp *types.Note, err error) {
	out, err := l.svcCtx.NoteRpc.Update(l.ctx, &note.UpdateReq{
		Id: req.Id, UserId: ctxdata.UserID(l.ctx), Title: req.Title, Content: req.Content, Tags: req.Tags, ImageUrls: req.ImageUrls,
	})
	if err != nil {
		return nil, err
	}
	return toNote(out), nil
}

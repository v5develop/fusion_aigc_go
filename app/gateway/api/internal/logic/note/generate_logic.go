// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package note

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/app/note/rpc/note"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 确认开始生成；生成失败后再次调用即重新生成
func NewGenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateLogic {
	return &GenerateLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GenerateLogic) Generate(req *types.GenerateNoteReq) (resp *types.Note, err error) {
	out, err := l.svcCtx.NoteRpc.Generate(l.ctx, &note.GenerateReq{
		Id: req.Id, UserId: ctxdata.UserID(l.ctx), Title: req.Title, Content: req.Content, Tags: req.Tags, ImageCount: req.ImageCount,
	})
	if err != nil {
		return nil, err
	}
	return toNote(out), nil
}

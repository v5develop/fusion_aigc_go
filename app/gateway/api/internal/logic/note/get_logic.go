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

type GetLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 笔记详情（构思中/生成中轮询此接口）
func NewGetLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetLogic {
	return &GetLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetLogic) Get(req *types.IdPath) (resp *types.Note, err error) {
	out, err := l.svcCtx.NoteRpc.Get(l.ctx, &note.IdUserReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx)})
	if err != nil {
		return nil, err
	}
	return toNote(out), nil
}

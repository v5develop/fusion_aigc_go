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

type ReplanLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 规划页「重新生成」：重新构思
func NewReplanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ReplanLogic {
	return &ReplanLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *ReplanLogic) Replan(req *types.ReplanNoteReq) (resp *types.Note, err error) {
	out, err := l.svcCtx.NoteRpc.Replan(l.ctx, &note.ReplanReq{Id: req.Id, UserId: ctxdata.UserID(l.ctx), ImageCount: req.ImageCount})
	if err != nil {
		return nil, err
	}
	return toNote(out), nil
}

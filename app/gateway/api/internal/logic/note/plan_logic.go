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

type PlanLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 开始构思笔记（异步，轮询详情；完成后进入规划页）
func NewPlanLogic(ctx context.Context, svcCtx *svc.ServiceContext) *PlanLogic {
	return &PlanLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *PlanLogic) Plan(req *types.PlanNoteReq) (resp *types.Note, err error) {
	out, err := l.svcCtx.NoteRpc.Plan(l.ctx, &note.PlanReq{
		CopywritingId: req.CopywritingId,
		UserId:        ctxdata.UserID(l.ctx), NoteType: req.NoteType, Topic: req.Topic, MaterialUrls: req.MaterialUrls,
		DigitalHumanId: req.DigitalHumanId, ShopProfileId: req.ShopProfileId, RequestId: req.RequestId, ImageCount: req.ImageCount,
	})
	if err != nil {
		return nil, err
	}
	return toNote(out), nil
}

package logic

import (
	"context"

	"aigc-life/app/note/rpc/internal/svc"
	"aigc-life/app/note/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListLogic {
	return &AdminListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 运营后台 ----
func (l *AdminListLogic) AdminList(in *pb.ListReq) (*pb.NoteList, error) {
	return list(l.ctx, l.svcCtx, in)
}

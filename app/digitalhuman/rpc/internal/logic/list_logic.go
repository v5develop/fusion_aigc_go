package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/pagex"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListLogic {
	return &ListLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListLogic) List(in *pb.ListReq) (*pb.DigitalHumanList, error) {
	limit, offset := pagex.Offset(in.Page, in.PageSize)
	list, total, err := l.svcCtx.Human.List(l.ctx, model.DhFilter{UserID: in.UserId, Status: int64(in.Status), Limit: limit, Offset: offset})
	if err != nil {
		return nil, err
	}
	names := voiceNames(l.ctx, l.svcCtx, list)
	resp := &pb.DigitalHumanList{List: make([]*pb.DigitalHumanInfo, 0, len(list)), Total: total}
	for _, h := range list {
		resp.List = append(resp.List, toInfo(h, names))
	}
	return resp, nil
}

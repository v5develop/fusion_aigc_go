package logic

import (
	"context"

	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListOptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListOptionsLogic {
	return &AdminListOptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 运营后台 ----
func (l *AdminListOptionsLogic) AdminListOptions(in *pb.AdminListOptionsReq) (*pb.OptionList, error) {
	list, err := l.svcCtx.Option.ListByType(l.ctx, in.OptionType, false)
	if err != nil {
		return nil, err
	}
	resp := &pb.OptionList{List: make([]*pb.OptionInfo, 0, len(list))}
	for _, o := range list {
		resp.List = append(resp.List, toOption(o))
	}
	return resp, nil
}

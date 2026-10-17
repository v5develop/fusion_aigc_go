package logic

import (
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminGenOptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminGenOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminGenOptionsLogic {
	return &AdminGenOptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 生成选项（模型 / 比例 / 时长）的运营配置：直接读库（含停用项），不走缓存。
func (l *AdminGenOptionsLogic) AdminGenOptions(in *pb.Empty) (*pb.GenOptionList, error) {
	rows, err := l.svcCtx.GenOpts.ListGenOptions(l.ctx)
	if err != nil {
		return nil, err
	}
	kinds, err := l.svcCtx.GenOpts.ListGenKinds(l.ctx)
	if err != nil {
		return nil, err
	}
	out := &pb.GenOptionList{List: make([]*pb.GenOptionItem, 0, len(rows)), Kinds: make([]*pb.GenKindItem, 0, len(kinds))}
	for _, r := range rows {
		out.List = append(out.List, genItem(r))
	}
	for _, k := range kinds {
		out.Kinds = append(out.Kinds, genKindItem(k))
	}
	return out, nil
}

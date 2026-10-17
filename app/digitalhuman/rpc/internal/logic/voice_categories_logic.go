package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type VoiceCategoriesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewVoiceCategoriesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *VoiceCategoriesLogic {
	return &VoiceCategoriesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *VoiceCategoriesLogic) VoiceCategories(in *pb.Empty) (*pb.OptionList, error) {
	list, err := l.svcCtx.Option.ListByType(l.ctx, model.OptionVoiceCategory, true)
	if err != nil {
		return nil, err
	}
	resp := &pb.OptionList{List: make([]*pb.OptionInfo, 0, len(list))}
	for _, o := range list {
		resp.List = append(resp.List, toOption(o))
	}
	return resp, nil
}

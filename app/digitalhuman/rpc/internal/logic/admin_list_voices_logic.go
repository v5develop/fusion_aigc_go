package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListVoicesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListVoicesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListVoicesLogic {
	return &AdminListVoicesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminListVoicesLogic) AdminListVoices(in *pb.ListVoicesReq) (*pb.VoiceList, error) {
	list, err := l.svcCtx.Voice.List(l.ctx, in.Category, false)
	if err != nil {
		return nil, err
	}
	resp := &pb.VoiceList{List: make([]*pb.VoiceInfo, 0, len(list))}
	for _, v := range list {
		resp.List = append(resp.List, toVoice(v, true))
	}
	return resp, nil
}

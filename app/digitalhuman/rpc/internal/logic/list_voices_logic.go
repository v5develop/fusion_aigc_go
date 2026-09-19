package logic

import (
	"context"

	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListVoicesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListVoicesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListVoicesLogic {
	return &ListVoicesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *ListVoicesLogic) ListVoices(in *pb.ListVoicesReq) (*pb.VoiceList, error) {
	list, err := l.svcCtx.Voice.List(l.ctx, in.Category, true)
	if err != nil {
		return nil, err
	}
	resp := &pb.VoiceList{List: make([]*pb.VoiceInfo, 0, len(list))}
	for _, v := range list {
		resp.List = append(resp.List, toVoice(v, false))
	}
	return resp, nil
}

package logic

import (
	"context"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListVideosLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListVideosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListVideosLogic {
	return &AdminListVideosLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AdminListVideosLogic) AdminListVideos(in *pb.VideoListReq) (*pb.VideoList, error) {
	limit, offset := pagexOffset(in.Page, in.PageSize)
	rows, total, err := l.svcCtx.Video.List(l.ctx, model.VideoListFilter{
		UserID: in.UserId, Kind: in.Kind, Status: int64(in.Status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	resp := &pb.VideoList{Total: total, List: make([]*pb.VideoInfo, 0, len(rows))}
	for _, c := range rows {
		resp.List = append(resp.List, toVideoInfo(c))
	}
	return resp, nil
}

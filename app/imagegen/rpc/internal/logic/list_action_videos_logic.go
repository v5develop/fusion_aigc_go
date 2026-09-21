package logic

import (
	"context"

	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type ListActionVideosLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListActionVideosLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListActionVideosLogic {
	return &ListActionVideosLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 动作迁移：动作视频（预置来自模板素材表，我的来自用户导入）----
func (l *ListActionVideosLogic) ListActionVideos(in *pb.IdUserReq) (*pb.ActionVideoList, error) {
	cfg := &l.svcCtx.Config.ActionTransfer
	presets, err := l.svcCtx.Shared.ListTemplates(l.ctx, templateSlot(cfg), "", "")
	if err != nil {
		return nil, err
	}
	resp := &pb.ActionVideoList{Presets: make([]*pb.ActionVideo, 0, len(presets)), Mine: []*pb.ActionVideo{}}
	for _, t := range presets {
		resp.Presets = append(resp.Presets, &pb.ActionVideo{Id: t.Id, Name: t.Title, Url: t.Url})
	}
	mine, err := l.svcCtx.ActionVideo.ListByUser(l.ctx, in.UserId)
	if err != nil {
		return nil, err
	}
	for _, v := range mine {
		resp.Mine = append(resp.Mine, &pb.ActionVideo{Id: v.Id, Name: v.Name, Url: v.Url, CoverUrl: v.CoverUrl.String, Mine: true})
	}
	return resp, nil
}

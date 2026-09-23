package logic

import (
	"context"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGenerateVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateVideoLogic {
	return &GenerateVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GenerateVideo 确认剧本，出成片（73 / 88）
func (l *GenerateVideoLogic) GenerateVideo(in *pb.IdUserReq) (*pb.VideoInfo, error) {
	c, err := findOwnedVideo(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if c.Status != model.VideoPlanned && c.Status != model.VideoFailed {
		return nil, xerr.New(xerr.Conflict, "请先等剧本生成完成")
	}
	if !c.PlanScript.Valid || c.PlanScript.String == "" {
		return nil, xerr.New(xerr.Conflict, "还没有剧本，请先构思")
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	var task aigc.Task
	if c.Kind == model.KindVideoRemake {
		if task, err = remakeTask(c); err != nil {
			return nil, err
		}
	} else {
		cfg, kerr := videoKindConf(l.svcCtx, c.Kind)
		if kerr != nil {
			return nil, kerr
		}
		if task, err = genTask(c, cfg); err != nil {
			return nil, err
		}
	}
	taskID := newTaskID(l.svcCtx)
	ok, err := l.svcCtx.Video.StartGenerate(l.ctx, c.Id, taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.New(xerr.Conflict, "当前状态不能生成")
	}
	if _, err := l.svcCtx.Aigc.Submit(l.ctx, taskID, task); err != nil {
		l.Errorf("submit %s video %d: %v", c.Kind, c.Id, err)
		failVideo(l.ctx, l.svcCtx, c.Id, model.VideoGenerating, "提交生成失败，请重试")
		return nil, xerr.New(xerr.ServerError, "提交生成失败，请重试")
	}
	// TODO 计费：出片预扣 100 点，失败退回
	cur, err := findOwnedVideo(l.ctx, l.svcCtx, c.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(cur), nil
}

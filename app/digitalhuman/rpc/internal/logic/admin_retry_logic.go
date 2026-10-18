package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/taskcenter"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminRetryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminRetryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminRetryLogic {
	return &AdminRetryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminRetry 运营重做生成失败的数字人。失败处在哪一段，看记录上有没有音频：
//   - 有音频：配音已经完成，只是出片失败，用同一段音频重新出片（重新冻结点数）；
//   - 没有音频：配音失败，重新配音，配音完成后再出片。配音阶段不收费。
func (l *AdminRetryLogic) AdminRetry(in *pb.IdReq) (*pb.Empty, error) {
	h, err := l.svcCtx.Human.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("数字人记录")
	}
	if err != nil {
		return nil, err
	}
	if h.Status != model.DhStatusFailed {
		return nil, xerr.New(xerr.Conflict, "数字人不是生成失败，不能重试")
	}
	if h.AudioUrl.Valid {
		if err := l.retryRendering(h); err != nil {
			return nil, err
		}
		return &pb.Empty{}, nil
	}
	if err := l.retryDubbing(h); err != nil {
		return nil, err
	}
	return &pb.Empty{}, nil
}

// retryDubbing 配音失败：先确认音色还能用，再把记录退回配音中，重新跑配音和后续出片。
func (l *AdminRetryLogic) retryDubbing(h *model.AfvDigitalHuman) error {
	v, err := l.svcCtx.Voice.FindAlive(l.ctx, h.VoiceId)
	if errors.Is(err, model.ErrNotFound) || (err == nil && v.Status != 1) {
		return xerr.Param("音色不可用，请重新选择")
	}
	if err != nil {
		return err
	}
	ok, err := l.svcCtx.Human.Finish(l.ctx, h.Id, model.DhStatusFailed, model.DhStatusDubbing, "", "")
	if err != nil {
		return err
	}
	if !ok {
		return xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
	}
	h.Status = model.DhStatusDubbing
	startDigitalHuman(l.svcCtx, h, v.Voice)
	return nil
}

// retryRendering 出片失败：音频沿用，重新冻结点数、换新的出片任务号重新提交。
func (l *AdminRetryLogic) retryRendering(h *model.AfvDigitalHuman) error {
	secs := max(int(h.Duration.Int64), minVideoSeconds)
	imagePath, audioPath, msg := humanMedia(l.ctx, l.svcCtx, h, h.AudioUrl.String)
	if msg != "" {
		return xerr.New(xerr.ServerError, msg)
	}
	taskID := aigc.ExactTaskID(l.svcCtx.Config.TaskIDPrefix, taskcenter.Ref("dh", h.Id), true) // 出片失败重做，任务号必须不同
	if _, err := l.svcCtx.Billing.Hold(l.ctx, humanCharge(h, taskID, secs)); err != nil {
		return err
	}
	ok, err := l.svcCtx.Human.RestartRendering(l.ctx, h.Id, taskID)
	if err != nil || !ok {
		l.svcCtx.Billing.Fail(l.ctx, h.UserId, taskID, "未能开始重做")
		if err != nil {
			return err
		}
		return xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
	}
	submitRendering(l.ctx, l.svcCtx, h, imagePath, audioPath, secs, taskID)
	return nil
}

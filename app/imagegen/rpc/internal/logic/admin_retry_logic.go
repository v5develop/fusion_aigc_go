package logic

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
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

// AdminRetry 运营重做生成失败的图片 / 长图：按原内容重新冻结点数、重新提交。
// 换新的请求号：点数的预留单和执行面任务号都是新的，旧的失败记录（及其已退回的预留单）不受影响。
func (l *AdminRetryLogic) AdminRetry(in *pb.IdReq) (*pb.Empty, error) {
	c, err := l.svcCtx.Creation.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("创作记录")
	}
	if err != nil {
		return nil, err
	}
	if c.Status != model.StatusFailed {
		return nil, xerr.New(xerr.Conflict, "记录不是生成失败，不能重试")
	}
	cfg, err := kindConf(l.svcCtx, c.Kind)
	if err != nil {
		return nil, err
	}
	attempt := fmt.Sprintf("r%d", time.Now().UnixNano())
	c.RequestId = sql.NullString{String: "retry-" + attempt, Valid: true}
	if _, err := l.svcCtx.Billing.Hold(l.ctx, imageCharge(c)); err != nil {
		return nil, err
	}
	ok, err := l.svcCtx.Creation.RestartGenerate(l.ctx, in.Id, c.RequestId.String)
	if err != nil || !ok {
		l.svcCtx.Billing.Fail(l.ctx, c.UserId, c.RequestId.String, "未能开始重做")
		if err != nil {
			return nil, err
		}
		return nil, xerr.New(xerr.Conflict, "任务状态已变化，请刷新后再看")
	}
	tasks := submit(l.ctx, l.svcCtx, c, cfg, posterTypeLabel(l.ctx, l.svcCtx, cfg.BizType, c.PosterType.String), attempt)
	if _, err := l.svcCtx.Creation.UpdateTasks(l.ctx, in.Id, dumpTasks(tasks), model.StatusGenerating, "", ""); err != nil {
		l.Errorf("save tasks %d: %v", in.Id, err)
	}
	return &pb.Empty{}, nil
}

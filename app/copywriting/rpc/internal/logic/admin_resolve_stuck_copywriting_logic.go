package logic

import (
	"context"
	"strings"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminResolveStuckCopywritingLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminResolveStuckCopywritingLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminResolveStuckCopywritingLogic {
	return &AdminResolveStuckCopywritingLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AdminResolveStuckCopywriting 人工处理「生成中、点数已扣、结果未落库」的文案。
//
// 只处理自动回收不敢动的那一类：
//   - 预留单已确认 / 部分退回：先把已扣的点数全部退回，再标记失败；
//   - 预留单已退回（上次人工处理中途失败）：点数已经在账上，只补标记失败。
//
// 其他情况直接拒绝：仍冻结的交给自动回收（会退款），没有预留单或已结束的不需要处理。
// 顺序是先退款后标失败：退款成功后即使崩溃，记录仍是生成中，下一次处理会走「已退回」分支补标记，不会重复退款。
func (l *AdminResolveStuckCopywritingLogic) AdminResolveStuckCopywriting(in *pb.AdminResolveReq) (*pb.AdminResolveResp, error) {
	note := strings.TrimSpace(in.Note)
	if note == "" {
		return nil, xerr.Param("请填写处理说明")
	}
	c, err := l.svcCtx.Copywriting.FindOne(l.ctx, in.Id)
	if err != nil {
		return nil, err
	}
	if c.Status != model.StatusGenerating {
		return nil, xerr.Newf(xerr.Conflict, "文案 %d 已结束（状态 %d），无需人工处理", c.Id, c.Status)
	}
	requestID := billingRequestID(c.Id)
	status, err := l.svcCtx.Billing.Status(l.ctx, c.UserId, requestID)
	if err != nil {
		return nil, err
	}
	var refunded int64
	switch status {
	case "CONFIRMED", "PARTIALLY_REFUNDED":
		refunded, err = l.svcCtx.Billing.RefundConfirmed(l.ctx, c.UserId, requestID, "文案生成未落库："+note)
		if err != nil {
			return nil, err
		}
		status = "REFUNDED"
	case "REFUNDED":
		// 上次人工处理已退回，只补标记失败
	case "HELD":
		return nil, xerr.Newf(xerr.Conflict, "文案 %d 的点数仍在冻结中，由自动回收处理，不能人工标记", c.Id)
	default:
		return nil, xerr.Newf(xerr.Conflict, "文案 %d 没有已扣费的预留单（状态 %q），由自动回收处理", c.Id, status)
	}
	ok, err := l.svcCtx.Copywriting.FinishGenerate(l.ctx, c.Id, model.StatusFailed, "", "", 0, "人工处理："+note)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.Newf(xerr.Conflict, "文案 %d 状态已变化，请刷新后重试", c.Id)
	}
	l.Infof("resolved stuck copywriting %d, refunded=%d, reservation=%s, note=%q", c.Id, refunded, status, note)
	return &pb.AdminResolveResp{ReservationStatus: status, RefundedPoints: refunded}, nil
}

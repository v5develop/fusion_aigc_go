package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/app/note/rpc/pb"
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

// AdminRetry 运营重做生成失败的笔记：沿用用户的「重新生成」，按原构思结果冻结点数、重新提交。
// 失败时点数已经退回，这里只是再冻结一次。
func (l *AdminRetryLogic) AdminRetry(in *pb.IdReq) (*pb.Empty, error) {
	n, err := l.svcCtx.Note.FindOne(l.ctx, in.Id)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("笔记")
	}
	if err != nil {
		return nil, err
	}
	if n.Status != model.StatusGenFailed {
		return nil, xerr.New(xerr.Conflict, "笔记不是生成失败，不能重试")
	}
	if _, err := NewGenerateLogic(l.ctx, l.svcCtx).Generate(&pb.GenerateReq{Id: in.Id, UserId: n.UserId}); err != nil {
		return nil, err
	}
	return &pb.Empty{}, nil
}

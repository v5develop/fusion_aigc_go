package logic

import (
	"context"
	"errors"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type RegenerateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRegenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RegenerateLogic {
	return &RegenerateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 重新生成：用原参数再来一次
// 重新生成：用原参数再来一次
func (l *RegenerateLogic) Regenerate(in *pb.RegenerateReq) (*pb.CopywritingInfo, error) {
	old, err := l.svcCtx.Copywriting.FindOwned(l.ctx, in.Id, in.UserId)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("文案")
	}
	if err != nil {
		return nil, err
	}
	if old.Source == model.SourceManual {
		return nil, xerr.New(xerr.Forbidden, "自主撰写的文案不能重新生成")
	}
	if old.Status == model.StatusGenerating {
		return nil, xerr.New(xerr.Conflict, "上一次生成还未完成")
	}
	return NewGenerateLogic(l.ctx, l.svcCtx).Generate(&pb.GenerateReq{
		UserId: old.UserId, OwnerType: old.OwnerType.Int64, OwnerId: old.OwnerId.Int64,
		Source: old.Source, ContentType: old.ContentType.String, Style: old.Style.String,
		Industry: old.Industry.String, Topic: old.Topic.String, ReferenceText: old.ReferenceText.String,
		TargetWords: int32(old.TargetWords.Int64), ShopProfileId: old.ShopProfileId.Int64,
		RequestId: in.RequestId,
	})
}

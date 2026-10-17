package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/copywriting/model"
	"aigc-local-life/app/copywriting/rpc/internal/svc"
	"aigc-local-life/app/copywriting/rpc/pb"
	"aigc-local-life/pkg/pagex"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type DerivativesLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDerivativesLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DerivativesLogic {
	return &DerivativesLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// Derivatives 生成历史：这条文案被拿去生成过哪些成品。
//
// 只返回**已完成**的：这个列表是拿来点开看成品的，生成中和失败的给出去没用。
// 记录靠各业务表上的 copywriting_id 关联（提交时带上），只有从「选择AI文案」进来的才有。
func (l *DerivativesLogic) Derivatives(in *pb.DerivativesReq) (*pb.DerivativeList, error) {
	if _, err := l.svcCtx.Copywriting.FindOwned(l.ctx, in.CopywritingId, in.UserId); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, xerr.NotFoundf("文案")
		}
		return nil, err
	}
	limit, offset := pagex.Offset(in.Page, in.PageSize)
	list, total, err := l.svcCtx.Shared.ListDerivatives(l.ctx, in.CopywritingId, in.UserId, derivativeKinds(in.Kind), limit, offset)
	if err != nil {
		return nil, err
	}
	resp := &pb.DerivativeList{Total: total, List: make([]*pb.Derivative, 0, len(list))}
	for _, d := range list {
		resp.List = append(resp.List, &pb.Derivative{
			Id: d.Id, Kind: d.Kind, CoverUrl: d.CoverUrl, ResultUrl: d.ResultUrl,
			IsVideo: d.IsVideo == 1, Size: d.Size, CreateTime: d.CreateTime.UnixMilli(),
		})
	}
	return resp, nil
}

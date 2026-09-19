package logic

import (
	"context"
	"errors"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/pagex"
	"aigc-life/pkg/xerr"

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

// 生成历史：该文案衍生出的内容
// 生成历史：该文案衍生出的内容
//
// TODO 本版不交付。依赖两件未定的事：各标签对应哪些 afv_asset.type；
// 下游模块发任务时是否把 afv_creation_task.external_ref 写成文案 id。
func (l *DerivativesLogic) Derivatives(in *pb.DerivativesReq) (*pb.DerivativeList, error) {
	if _, err := l.svcCtx.Copywriting.FindOwned(l.ctx, in.CopywritingId, in.UserId); err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, xerr.NotFoundf("文案")
		}
		return nil, err
	}
	limit, offset := pagex.Offset(in.Page, in.PageSize)
	list, total, err := l.svcCtx.Shared.ListDerivatives(l.ctx, in.CopywritingId, in.UserId, assetTypesOf(in.Kind), limit, offset)
	if err != nil {
		return nil, err
	}
	resp := &pb.DerivativeList{Total: total, List: make([]*pb.Derivative, 0, len(list))}
	for _, d := range list {
		resp.List = append(resp.List, &pb.Derivative{
			Id: d.Id, Kind: kindOfAssetType(d.Type), AssetType: d.Type, CoverUrl: d.CoverUrl,
			ResultUrl: d.ResultUrl, IsVideo: isVideoType(d.Type), CreateTime: d.CreateTime.UnixMilli(),
		})
	}
	return resp, nil
}

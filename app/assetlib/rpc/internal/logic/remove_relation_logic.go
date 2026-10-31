package logic

import (
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RemoveRelationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRemoveRelationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RemoveRelationLogic {
	return &RemoveRelationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RemoveRelationLogic) RemoveRelation(in *assetlib.RelationReq) (*assetlib.Empty, error) {
	if err := checkRelation(l.ctx, l.svcCtx, in); err != nil {
		return nil, err
	}
	n, err := l.svcCtx.Repo.Relation.Remove(l.ctx, in.Id, in.RelatedAssetId, in.UserId, in.RelationType)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, xerr.New(xerr.NotFound, "关系不存在")
	}
	return &assetlib.Empty{}, nil
}

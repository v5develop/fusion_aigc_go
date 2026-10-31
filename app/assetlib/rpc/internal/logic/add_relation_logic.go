package logic

import (
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddRelationLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddRelationLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddRelationLogic {
	return &AddRelationLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *AddRelationLogic) AddRelation(in *assetlib.RelationReq) (*assetlib.Empty, error) {
	if err := checkRelation(l.ctx, l.svcCtx, in); err != nil {
		return nil, err
	}
	if _, err := l.svcCtx.Repo.Relation.Add(l.ctx, in.Id, in.RelatedAssetId, in.RelationType); err != nil {
		return nil, err
	}
	return &assetlib.Empty{}, nil
}

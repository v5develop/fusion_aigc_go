package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type DeleteTagLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewDeleteTagLogic(ctx context.Context, svcCtx *svc.ServiceContext) *DeleteTagLogic {
	return &DeleteTagLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *DeleteTagLogic) DeleteTag(in *assetlib.TagIdReq) (*assetlib.Empty, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	t, err := l.svcCtx.Repo.Tag.GetOwned(l.ctx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, xerr.New(xerr.NotFound, "标签不存在")
	}
	err = l.svcCtx.Repo.RunInTx(l.ctx, func(ctx context.Context, tx *repo.Repo) error {
		_, err := tx.Tag.Delete(ctx, t.ID, in.UserId)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &assetlib.Empty{}, nil
}

package logic

import (
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/pagex"
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type RenameTagLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewRenameTagLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RenameTagLogic {
	return &RenameTagLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *RenameTagLogic) RenameTag(in *assetlib.RenameTagReq) (*assetlib.Tag, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	name, err := cleanTagName(in.Name, l.svcCtx.Config.Limits.TagNameLen)
	if err != nil {
		return nil, err
	}
	t, err := l.svcCtx.Repo.Tag.GetOwned(l.ctx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if t == nil {
		return nil, xerr.New(xerr.NotFound, "标签不存在")
	}
	if err := l.svcCtx.Repo.Tag.Rename(l.ctx, t.ID, in.UserId, name); err != nil {
		if repo.IsDuplicate(err) {
			return nil, xerr.New(xerr.Conflict, "标签已存在")
		}
		return nil, err
	}
	return &assetlib.Tag{Id: t.ID, Name: name, CreatedAt: pagex.Ms(t.CreatedAt)}, nil
}

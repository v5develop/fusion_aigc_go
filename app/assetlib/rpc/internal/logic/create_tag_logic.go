package logic

import (
	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/pagex"
	"aigc-local-life/pkg/xerr"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateTagLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateTagLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateTagLogic {
	return &CreateTagLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

func (l *CreateTagLogic) CreateTag(in *assetlib.CreateTagReq) (*assetlib.Tag, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	name, err := cleanTagName(in.Name, lim.TagNameLen)
	if err != nil {
		return nil, err
	}
	n, err := l.svcCtx.Repo.Tag.Count(l.ctx, in.UserId)
	if err != nil {
		return nil, err
	}
	if n >= int64(lim.TagsPerUser) {
		return nil, xerr.Newf(xerr.ParamError, "标签最多 %d 个", lim.TagsPerUser)
	}
	t := &model.Tag{UserID: in.UserId, Name: name}
	if err := l.svcCtx.Repo.Tag.Insert(l.ctx, t); err != nil {
		if repo.IsDuplicate(err) {
			return nil, xerr.New(xerr.Conflict, "标签已存在")
		}
		return nil, err
	}
	return &assetlib.Tag{Id: t.ID, Name: t.Name, CreatedAt: pagex.Ms(t.CreatedAt)}, nil
}

package logic

import (
	"aigc-local-life/pkg/pagex"
	"context"
	"strings"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminListTagsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminListTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminListTagsLogic {
	return &AdminListTagsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- 运营后台 ----
func (l *AdminListTagsLogic) AdminListTags(in *assetlib.AdminListTagsReq) (*assetlib.AdminListTagsResp, error) {
	limit, offset := listPage(in.Page, in.PageSize)
	rows, total, err := l.svcCtx.Repo.Tag.AdminList(l.ctx, in.UserId, strings.TrimSpace(in.Q), limit, offset)
	if err != nil {
		return nil, err
	}
	out := &assetlib.AdminListTagsResp{Items: make([]*assetlib.AdminTag, 0, len(rows)), Total: total}
	for _, t := range rows {
		out.Items = append(out.Items, &assetlib.AdminTag{Id: t.ID, UserId: t.UserID, Name: t.Name, AssetCount: t.AssetCount, CreatedAt: pagex.Ms(t.CreatedAt)})
	}
	return out, nil
}

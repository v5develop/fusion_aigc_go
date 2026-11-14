package logic

import (
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// maxSysTags 系统标签字典最多返回几个。
const maxSysTags = 500

type ListSysTagsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewListSysTagsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *ListSysTagsLogic {
	return &ListSysTagsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 系统标签字典：本人资产（未删除、非草稿）里出现过的系统标签及资产数，按数量倒序、名字升序，最多 500 个
func (l *ListSysTagsLogic) ListSysTags(in *assetlib.UserReq) (*assetlib.ListSysTagsResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Repo.Asset.SysTagCounts(l.ctx, in.UserId, maxSysTags)
	if err != nil {
		return nil, err
	}
	out := &assetlib.ListSysTagsResp{Items: make([]*assetlib.SysTag, 0, len(rows))}
	for _, r := range rows {
		out.Items = append(out.Items, &assetlib.SysTag{Name: r.Name, AssetCount: r.N})
	}
	return out, nil
}

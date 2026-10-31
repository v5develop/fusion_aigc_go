package logic

import (
	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
	"context"
	"database/sql"
	"strings"
	"time"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AddVersionLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAddVersionLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AddVersionLogic {
	return &AddVersionLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 追加版本（version_no 取 MAX+1），并设为当前版本
func (l *AddVersionLogic) AddVersion(in *assetlib.AddVersionReq) (*assetlib.Version, error) {
	row, err := mustOwn(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	if err := checkOwnedURL(ownsURL(l.svcCtx), "image_url", in.ImageUrl); err != nil {
		return nil, err
	}
	if runeLen(in.Prompt) > lim.PromptLen {
		return nil, xerr.Newf(xerr.ParamError, "提示词最多 %d 字", lim.PromptLen)
	}
	if runeLen(in.Model) > 64 || len(in.ImageRid) > 128 {
		return nil, xerr.Param("model / image_rid 过长")
	}
	params, err := checkJSON("params", in.Params)
	if err != nil {
		return nil, err
	}
	v := &model.Version{AssetID: row.ID, ImageURL: strings.TrimSpace(in.ImageUrl), Model: strings.TrimSpace(in.Model), Params: params,
		Prompt: sql.NullString{String: in.Prompt, Valid: in.Prompt != ""}, ImageRid: sql.NullString{String: in.ImageRid, Valid: in.ImageRid != ""}}
	// 版本号 MAX+1；并发追加撞 uk_asset_version 时重试
	for attempt := 0; ; attempt++ {
		err = l.svcCtx.Repo.RunInTx(l.ctx, func(ctx context.Context, tx *repo.Repo) error {
			// 先锁旁表行（有的话），同一资产的追加在这里串行；没有旁表行时靠 uk_asset_version 兜底重试
			e, err := tx.Ext.Get(ctx, row.ID, in.UserId, true)
			if err != nil {
				return err
			}
			if e == nil {
				e = &model.Ext{AssetID: row.ID, UserID: in.UserId}
			}
			n, err := tx.Version.NextNo(ctx, row.ID, in.UserId)
			if err != nil {
				return err
			}
			if int(n) > lim.VersionsPerAsset {
				return xerr.Newf(xerr.ParamError, "每个资产最多 %d 个版本", lim.VersionsPerAsset)
			}
			v.ID, v.VersionNo = 0, n
			if err := tx.Version.Insert(ctx, in.UserId, v); err != nil {
				return err
			}
			// 新版本即当前版本
			e.CurrentVersionID = sql.NullInt64{Int64: v.ID, Valid: true}
			return tx.Ext.Upsert(ctx, in.UserId, e)
		})
		if err == nil || !repo.IsDuplicate(err) || attempt >= 2 {
			break
		}
	}
	if err != nil {
		if repo.IsDuplicate(err) {
			return nil, xerr.New(xerr.Conflict, "版本追加冲突，请重试")
		}
		return nil, err
	}
	v.CreatedAt = time.Now()
	return toVersion(v), nil
}

package logic

import (
	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateLogic {
	return &UpdateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 改标题（写 Java afv_asset.name）/ 分类 / 收藏 / 标签（整组替换）/ 元数据 / 授权 / 当前版本
func (l *UpdateLogic) Update(in *assetlib.UpdateReq) (*assetlib.Asset, error) {
	row, err := mustOwn(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits

	// ---- 校验（全部通过才动库）----
	var title string
	if in.Title != nil {
		title = strings.TrimSpace(*in.Title)
		if title == "" {
			return nil, xerr.New(xerr.ParamMissing, "标题不能为空")
		}
		if runeLen(title) > lim.TitleLen {
			return nil, xerr.Newf(xerr.ParamError, "标题最多 %d 字", lim.TitleLen)
		}
	}
	if in.Kind != nil && *in.Kind != "" && !model.Kind(*in.Kind).Valid() {
		return nil, xerr.Param("kind 只能是 model / scene / makeup / hair / accessory，空串表示未分类")
	}
	if in.License != nil && *in.License != "" && !slices.Contains(model.Licenses, *in.License) {
		return nil, xerr.Param("license 只能是 cleared / internal / unknown")
	}
	var meta json.RawMessage
	if in.Metadata != nil {
		m := model.Metadata{Industry: strings.TrimSpace(in.Metadata.Industry), Style: strings.TrimSpace(in.Metadata.Style),
			Shot: strings.TrimSpace(in.Metadata.Shot), Color: strings.TrimSpace(in.Metadata.Color)}
		for _, v := range []string{m.Industry, m.Style, m.Shot, m.Color} {
			if runeLen(v) > lim.MetadataLen {
				return nil, xerr.Newf(xerr.ParamError, "metadata 每项最多 %d 字", lim.MetadataLen)
			}
		}
		if m != (model.Metadata{}) {
			meta, _ = json.Marshal(m)
		}
	}
	var tagNames []string
	if in.Tags != nil {
		seen := map[string]bool{}
		for _, n := range in.Tags.Names {
			name, err := cleanTagName(n, lim.TagNameLen)
			if err != nil {
				return nil, err
			}
			if key := strings.ToLower(name); !seen[key] {
				seen[key] = true
				tagNames = append(tagNames, name)
			}
		}
		if len(tagNames) > lim.TagsPerAsset {
			return nil, xerr.Newf(xerr.ParamError, "每个资产最多 %d 个标签", lim.TagsPerAsset)
		}
	}
	extTouched := in.Kind != nil || in.Favorite != nil || in.License != nil || in.Metadata != nil || in.CurrentVersionId != nil

	err = l.svcCtx.Repo.RunInTx(l.ctx, func(ctx context.Context, tx *repo.Repo) error {
		if in.Title != nil && title != row.Name {
			if _, err := tx.Asset.Rename(ctx, row.ID, in.UserId, title); err != nil {
				return err
			}
		}
		if extTouched {
			e, err := tx.Ext.Get(ctx, row.ID, true)
			if err != nil {
				return err
			}
			if e == nil {
				e = &model.Ext{AssetID: row.ID, UserID: in.UserId}
			}
			if in.Kind != nil {
				e.Kind = sql.NullString{String: *in.Kind, Valid: *in.Kind != ""}
			}
			if in.Favorite != nil {
				e.Favorite = 0
				if *in.Favorite {
					e.Favorite = 1
				}
			}
			if in.License != nil {
				e.License = *in.License
			}
			if in.Metadata != nil {
				e.Metadata = meta
			}
			if in.CurrentVersionId != nil {
				vid := *in.CurrentVersionId
				if vid > 0 {
					v, err := tx.Version.Get(ctx, vid, row.ID)
					if err != nil {
						return err
					}
					if v == nil {
						return xerr.Param("版本不存在")
					}
				}
				e.CurrentVersionID = sql.NullInt64{Int64: vid, Valid: vid > 0}
			}
			if err := tx.Ext.Upsert(ctx, e); err != nil {
				return err
			}
		}
		if in.Tags != nil {
			ids, err := ensureTags(ctx, tx, in.UserId, tagNames, lim.TagsPerUser)
			if err != nil {
				return err
			}
			return tx.Tag.ReplaceAssetTags(ctx, row.ID, in.UserId, ids)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	row, err = mustOwn(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	out, err := toAssets(l.ctx, l.svcCtx, in.UserId, []*repo.AssetRow{row})
	if err != nil {
		return nil, err
	}
	return out[0], nil
}

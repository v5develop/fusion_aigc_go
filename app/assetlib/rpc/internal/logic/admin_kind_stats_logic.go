package logic

import (
	"aigc-local-life/app/assetlib/model"
	"context"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminKindStatsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminKindStatsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminKindStatsLogic {
	return &AdminKindStatsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 按 kind × 来源计数（全量用户，或指定 user_id）
func (l *AdminKindStatsLogic) AdminKindStats(in *assetlib.AdminKindStatsReq) (*assetlib.AdminKindStatsResp, error) {
	if err := checkMedia(in.Media); err != nil {
		return nil, err
	}
	rows, err := l.svcCtx.Repo.Asset.AdminKindStats(l.ctx, in.UserId, in.Media)
	if err != nil {
		return nil, err
	}
	out := &assetlib.AdminKindStatsResp{Kinds: categoryItems(rows)}
	bySource := map[string]int64{}
	for _, r := range rows {
		k := r.Kind.String
		if !r.Kind.Valid || k == "" {
			k = "uncategorized"
		}
		out.Rows = append(out.Rows, &assetlib.KindStat{Kind: k, Source: r.Source, Count: r.N})
		bySource[r.Source] += r.N
	}
	labels := map[string]string{model.SourceUpload: "上传", model.SourceGenerated: "AI 生成", model.SourceOfficial: "官方 / 系统"}
	for _, s := range []string{model.SourceUpload, model.SourceGenerated, model.SourceOfficial} {
		out.Sources = append(out.Sources, &assetlib.Category{Key: s, Label: labels[s], Count: bySource[s]})
	}
	return out, nil

}

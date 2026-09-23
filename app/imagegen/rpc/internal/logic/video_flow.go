package logic

import (
	"context"
	"encoding/json"
	"time"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/asset"

	"github.com/zeromicro/go-zero/core/logx"
)

// pollVideos 探店视频 / 带货短剧是两步：先等剧本（72 / 87），用户确认后再等成片（73 / 88）。
func (p *Poller) pollVideos(ctx context.Context) {
	p.pollVideoPlans(ctx)
	p.pollVideoGenerates(ctx)
}

func (p *Poller) pollVideoPlans(ctx context.Context) {
	svcCtx := p.svcCtx
	rows, err := svcCtx.Video.ListByStatus(ctx, model.VideoPlanning, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list planning videos: %v", err)
		return
	}
	for _, c := range rows {
		cfg, err := videoKindConf(svcCtx, c.Kind)
		if err != nil {
			continue
		}
		r := p.query(ctx, c.PlanTaskId.String)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess:
			if len(r.Script) == 0 {
				finishVideoPlan(ctx, svcCtx, c, "", "构思结果为空，请重试", model.VideoFailed)
				continue
			}
			finishVideoPlan(ctx, svcCtx, c, string(r.Script), "", model.VideoPlanned)
		case r != nil && r.Done():
			logx.WithContext(ctx).Infof("%s %d plan %s %s: %s", c.Kind, c.Id, c.PlanTaskId.String, r.Status, r.ErrorMsg)
			finishVideoPlan(ctx, svcCtx, c, "", "构思失败，请重试", model.VideoFailed)
		case time.Since(c.UpdateTime) > time.Duration(cfg.PlanTimeoutMinutes)*time.Minute:
			finishVideoPlan(ctx, svcCtx, c, "", "构思超时，请重试", model.VideoFailed)
		}
	}
}

func finishVideoPlan(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvVideoCreation, script, reason string, status int64) {
	if _, err := svcCtx.Video.FinishPlan(ctx, c.Id, script, reason, status); err != nil {
		logx.WithContext(ctx).Errorf("finish plan %s %d: %v", c.Kind, c.Id, err)
	}
}

func (p *Poller) pollVideoGenerates(ctx context.Context) {
	svcCtx := p.svcCtx
	rows, err := svcCtx.Video.ListByStatus(ctx, model.VideoGenerating, time.Now(), pollBatch)
	if err != nil {
		logx.WithContext(ctx).Errorf("list generating videos: %v", err)
		return
	}
	for _, c := range rows {
		cfg, err := videoKindConf(svcCtx, c.Kind)
		if err != nil {
			continue
		}
		r := p.query(ctx, c.GenTaskId.String)
		switch {
		case r != nil && r.Status == aigc.StatusSuccess && r.GeneratedImagePath != "":
			// 执行面产物在上游私有桶，转存到公有桶后再落库；转存失败下一轮重试
			u, terr := svcCtx.Storage.Transfer(ctx, r.GeneratedImagePath, c.Kind, c.UserId)
			if terr != nil {
				logx.WithContext(ctx).Errorf("%s %d 转存失败: %v", c.Kind, c.Id, terr)
				continue
			}
			ok, err := svcCtx.Video.FinishGenerate(ctx, c.Id, model.VideoDone, u, "")
			if err != nil {
				logx.WithContext(ctx).Errorf("finish generate %s %d: %v", c.Kind, c.Id, err)
				continue
			}
			if ok {
				saveVideoAsset(ctx, svcCtx, c, u)
			}
			// TODO 计费：成功结算
		case r != nil && r.Done():
			logx.WithContext(ctx).Infof("%s %d generate %s %s: %s", c.Kind, c.Id, c.GenTaskId.String, r.Status, r.ErrorMsg)
			failVideo(ctx, svcCtx, c.Id, model.VideoGenerating, "生成失败，请重试")
		case time.Since(c.UpdateTime) > time.Duration(cfg.GenTimeoutMinutes)*time.Minute:
			failVideo(ctx, svcCtx, c.Id, model.VideoGenerating, "生成超时，请重试")
		}
	}
}

// saveVideoAsset 成品写进素材库，封面用出镜人物图（没有就用成片地址）。
func saveVideoAsset(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvVideoCreation, url string) {
	if !svcCtx.Config.Asset.Enabled {
		return
	}
	cfg, err := videoKindConf(svcCtx, c.Kind)
	if err != nil {
		return
	}
	cover := url
	if c.CharUrl.Valid && c.CharUrl.String != "" {
		cover = c.CharUrl.String
	}
	name := c.UserText.String
	props, _ := json.Marshal(map[string]any{"videoCreationId": c.Id, "kind": c.Kind, "duration": c.Duration})
	a := asset.Asset{
		UserID: c.UserId, SourceTaskID: c.GenTaskId.String, Type: cfg.AssetType, EntryCode: cfg.AssetEntryCode,
		Name: name, CoverURL: cover, ResultURL: url, AspectRatio: c.Size, AIPrompt: name,
		Properties: string(props),
	}
	if err := svcCtx.Asset.InsertSet(ctx, []asset.Asset{a}); err != nil {
		logx.WithContext(ctx).Errorf("save asset of %s %d: %v", c.Kind, c.Id, err)
	}
}

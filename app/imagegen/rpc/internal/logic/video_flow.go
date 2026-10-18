package logic

import (
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/taskcenter"
	"context"
	"encoding/json"
	"strings"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/asset"

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
		if c.Kind == model.KindVideoRemake {
			p.pollVideoParse(ctx, c)
			continue
		}
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
			aigc.LogOutcome(ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "plan", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id), ExecTaskID: c.PlanTaskId.String}, r)
			finishVideoPlan(ctx, svcCtx, c, "", "构思失败，请重试", model.VideoFailed)
		case time.Since(c.UpdateTime) > time.Duration(cfg.PlanTimeoutMinutes)*time.Minute:
			finishVideoPlan(ctx, svcCtx, c, "", "构思超时，请重试", model.VideoFailed)
		}
	}
}

// pollVideoParse 视频复刻第一步：70 返回剧情文本与原片时长。
// 时长在这里才拿得到，超过上限就到此为止，不让用户花钱去复刻一条超长视频。
func (p *Poller) pollVideoParse(ctx context.Context, c *model.AfvVideoCreation) {
	svcCtx := p.svcCtx
	cfg := &svcCtx.Config.VideoRemake
	// 链接来源的任务号是转存完才分配的。为空说明还在下载转存，或者服务在转存中途重启了——
	// 后者没人会再接手，超时就给个明确的失败，别让用户一直转圈。
	if !c.PlanTaskId.Valid || c.PlanTaskId.String == "" {
		if time.Since(c.UpdateTime) > time.Duration(cfg.FetchTimeoutMinutes)*time.Minute {
			failVideo(ctx, svcCtx, c.Id, model.VideoPlanning, "视频转存中断，请重试")
		}
		return
	}
	r := p.query(ctx, c.PlanTaskId.String)
	switch {
	case r != nil && r.Status == aigc.StatusSuccess:
		text := strings.TrimSpace(r.Text)
		if text == "" {
			failVideo(ctx, svcCtx, c.Id, model.VideoPlanning, "没能解析出剧情，换一个视频试试")
			return
		}
		seconds := remakeSeconds(r.Duration, text)
		if cfg.MaxVideoSeconds > 0 && seconds > cfg.MaxVideoSeconds {
			failVideo(ctx, svcCtx, c.Id, model.VideoPlanning, videoLimits(cfg).LimitMsg())
			return
		}
		script, _ := json.Marshal(text)
		if _, err := svcCtx.Video.FinishParse(ctx, c.Id, string(script), int64(seconds)); err != nil {
			logx.WithContext(ctx).Errorf("finish parse %d: %v", c.Id, err)
		}
	case r != nil && r.Done():
		aigc.LogOutcome(ctx, aigc.Trace{Service: "imagegen", Kind: "videoRemake", Stage: "parse", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id), ExecTaskID: c.PlanTaskId.String}, r)
		failVideo(ctx, svcCtx, c.Id, model.VideoPlanning, "视频解析失败，换一个视频试试")
	case time.Since(c.UpdateTime) > time.Duration(cfg.ParseTimeoutMinutes)*time.Minute:
		failVideo(ctx, svcCtx, c.Id, model.VideoPlanning, "视频解析超时，请重试")
	}
}

func finishVideoPlan(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvVideoCreation, script, reason string, status int64) {
	if status == model.VideoFailed {
		// 构思失败：和 failVideo 一样宣告 task_failed，坐标直接用当前记录，不再查库
		t := aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "plan", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id)}
		aigc.LogTaskFailed(ctx, t, reason, nil)
	}
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
		genTimeout := svcCtx.Config.VideoRemake.GenTimeoutMinutes
		if c.Kind != model.KindVideoRemake {
			cfg, err := videoKindConf(svcCtx, c.Kind)
			if err != nil {
				continue
			}
			genTimeout = cfg.GenTimeoutMinutes
		}
		// 未超时的在途任务续期预留（见 pollGenerates 的说明）
		if time.Since(c.UpdateTime) <= time.Duration(genTimeout)*time.Minute {
			if err := svcCtx.Billing.Renew(ctx, c.UserId, c.GenTaskId.String); err != nil {
				logx.WithContext(ctx).Errorf("renew video %d: %v", c.Id, err)
			}
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
			// 先确认扣费，确认成功才交付；预留已失效则不交付
			if err := svcCtx.Billing.Settle(ctx, c.UserId, c.GenTaskId.String, 0); err != nil {
				logx.WithContext(ctx).Errorf("settle video %s %d: %v", c.Kind, c.Id, err)
				aigc.LogTaskFailed(ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "generate", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id)}, billing.DeliverBlockedReason, err)
				failVideo(ctx, svcCtx, c.Id, model.VideoGenerating, billing.DeliverBlockedReason)
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
		case r != nil && r.Done():
			aigc.LogOutcome(ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "generate", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id), ExecTaskID: c.GenTaskId.String}, r)
			failVideo(ctx, svcCtx, c.Id, model.VideoGenerating, "生成失败，请重试")
		case time.Since(c.UpdateTime) > time.Duration(genTimeout)*time.Minute:
			failVideo(ctx, svcCtx, c.Id, model.VideoGenerating, "生成超时，请重试")
		}
	}
}

// saveVideoAsset 成品写进素材库，封面用出镜人物图（没有就用成片地址）。
func saveVideoAsset(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvVideoCreation, url string) {
	if !svcCtx.Config.Asset.Enabled {
		return
	}
	assetType, entryCode := svcCtx.Config.VideoRemake.AssetType, svcCtx.Config.VideoRemake.AssetEntryCode
	if c.Kind != model.KindVideoRemake {
		cfg, err := videoKindConf(svcCtx, c.Kind)
		if err != nil {
			return
		}
		assetType, entryCode = cfg.AssetType, cfg.AssetEntryCode
	}
	cover := url
	switch {
	case c.CharUrl.Valid && c.CharUrl.String != "":
		cover = c.CharUrl.String // 探店 / 短剧：出镜人物图
	case c.CoverUrl.Valid && c.CoverUrl.String != "":
		cover = c.CoverUrl.String // 视频复刻：原视频封面
	}
	name := c.UserText.String
	if c.Kind == model.KindVideoRemake {
		name = c.Title.String
		if name == "" {
			name = "爆款视频复刻"
		}
	}
	props, _ := json.Marshal(map[string]any{"videoCreationId": c.Id, "kind": c.Kind, "duration": c.Duration})
	a := asset.Asset{
		UserID: c.UserId, SourceTaskID: c.GenTaskId.String, Type: assetType, EntryCode: entryCode,
		Name: name, CoverURL: cover, ResultURL: url, AspectRatio: c.Size, AIPrompt: name,
		Properties: string(props),
	}
	if err := svcCtx.Asset.InsertSet(ctx, []asset.Asset{a}); err != nil {
		logx.WithContext(ctx).Errorf("save asset of %s %d: %v", c.Kind, c.Id, err)
	}
}

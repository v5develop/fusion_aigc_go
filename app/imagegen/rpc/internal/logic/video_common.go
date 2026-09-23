package logic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"aigc-life/app/copywriting/rpc/copywriting"
	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/config"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// videoModels 探店视频 / 带货短剧的模型选择（设计稿「生成设置」弹窗，两个模块一样）。
// 值直接作为执行面 73 / 88 的 model：seedance2.5 走云端（81 / 89），lite 走执行面本地 GPU。
var videoModels = []*pb.Option{
	{Code: "seedance2.5", Label: "seedance 2.5"},
	{Code: "lite", Label: "XQ Video 1.0 Lite"},
}

// videoDurations 视频时长（秒）；videoSizes 视频比例
var (
	videoDurations = []int32{15, 30}
	videoSizes     = []*pb.Option{{Code: "9:16", Label: "9:16"}, {Code: "16:9", Label: "16:9"}}
)

func videoKindConf(svcCtx *svc.ServiceContext, kind string) (*config.VideoKindConf, error) {
	switch kind {
	case model.KindExploreShop:
		return &svcCtx.Config.ExploreShop, nil
	case model.KindShortDrama:
		return &svcCtx.Config.ShortDrama, nil
	}
	return nil, xerr.Newf(xerr.ParamError, "不支持的类型 %s", kind)
}

func toVideoInfo(c *model.AfvVideoCreation) *pb.VideoInfo {
	return &pb.VideoInfo{
		Id: c.Id, UserId: c.UserId, Kind: c.Kind, UserText: c.UserText.String,
		SellingPoints: c.SellingPoints.String, ShopProfileId: c.ShopProfileId.Int64,
		CharUrl: c.CharUrl.String, Env1Url: c.Env1Url.String, Env2Url: c.Env2Url.String, PropUrl: c.PropUrl.String,
		Duration: int32(c.Duration), Size: c.Size, Model: c.Model, Script: c.PlanScript.String,
		VideoUrl: c.VideoUrl.String, Status: int32(c.Status), FailReason: c.FailReason.String,
		CreateTime: c.CreateTime.UnixMilli(),
	}
}

func findOwnedVideo(ctx context.Context, svcCtx *svc.ServiceContext, id, userID int64) (*model.AfvVideoCreation, error) {
	c, err := svcCtx.Video.FindOwned(ctx, id, userID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("记录")
	}
	return c, err
}

// materialURL 素材图：只接受本存储签发的上传地址，空表示没传。
func materialURL(svcCtx *svc.ServiceContext, u, label string) (string, error) {
	if u = strings.TrimSpace(u); u == "" {
		return "", nil
	}
	if err := storage.Owned(svcCtx.Storage, u); err != nil {
		return "", xerr.Newf(xerr.ParamError, "%s请先通过上传接口上传", label)
	}
	return u, nil
}

// videoShopProfileText 取关联店铺档案拼好的文本；没关联或取不到就当没选（档案是选填项）。
func videoShopProfileText(ctx context.Context, svcCtx *svc.ServiceContext, profileID, userID int64) string {
	if profileID <= 0 {
		return ""
	}
	out, err := svcCtx.CopywritingRpc.ProfileText(ctx, &copywriting.IdUserReq{Id: profileID, UserId: userID})
	if err != nil {
		logx.WithContext(ctx).Errorf("video: 取店铺档案 %d 失败: %v", profileID, err)
		return ""
	}
	return out.Text
}

// planTask 第一步：出剧本（探店 72 / 短剧 87）。
func planTask(c *model.AfvVideoCreation, shopProfile string) aigc.Task {
	if c.Kind == model.KindShortDrama {
		return aigc.DramaScript{
			PropURL: c.PropUrl.String, EnvURL: c.Env1Url.String, CharURL: c.CharUrl.String,
			Theme: c.UserText.String, SellingPoints: c.SellingPoints.String, ShopProfile: shopProfile,
			Duration: int(c.Duration), AspectRatio: c.Size,
		}
	}
	return aigc.ShopScript{
		UserText: c.UserText.String, Duration: int(c.Duration), ShopProfile: shopProfile,
		PersonURL: c.CharUrl.String, Env1URL: c.Env1Url.String, Env2URL: c.Env2Url.String, PropURL: c.PropUrl.String,
	}
}

// genTask 第二步：把剧本原样回传出片（探店 73 / 短剧 88）。
func genTask(c *model.AfvVideoCreation, cfg *config.VideoKindConf) (aigc.Task, error) {
	var script any
	if err := json.Unmarshal([]byte(c.PlanScript.String), &script); err != nil {
		return nil, xerr.New(xerr.ServerError, "剧本内容异常，请重新构思")
	}
	if c.Kind == model.KindShortDrama {
		return aigc.DramaVideo{
			Script: script, PropURL: c.PropUrl.String, EnvURL: c.Env1Url.String, CharURL: c.CharUrl.String,
			Duration: int(c.Duration), AspectRatio: c.Size, Resolution: cfg.Resolution, Model: c.Model,
		}, nil
	}
	return aigc.ShopVideo{
		Script: script, PersonURL: c.CharUrl.String, Env1URL: c.Env1Url.String, Env2URL: c.Env2Url.String,
		PropURL: c.PropUrl.String, Duration: int(c.Duration), AspectRatio: c.Size,
		Resolution: cfg.Resolution, Model: c.Model,
	}, nil
}

// submitPlan 提交出剧本的任务；失败直接标记失败（还没开始构思，没有可挽回的中间状态）。
func submitPlan(ctx context.Context, svcCtx *svc.ServiceContext, c *model.AfvVideoCreation, shopProfile string) error {
	if _, err := svcCtx.Aigc.Submit(ctx, c.PlanTaskId.String, planTask(c, shopProfile)); err != nil {
		logx.WithContext(ctx).Errorf("submit %s plan %d: %v", c.Kind, c.Id, err)
		return xerr.New(xerr.ServerError, "提交构思失败，请重试")
	}
	// TODO 计费：等计费方案确认后，构思预扣 10 点、出片预扣 100 点，失败退回
	return nil
}

// failVideo 把记录标记为失败（只在它还处于 from 状态时生效）。
func failVideo(ctx context.Context, svcCtx *svc.ServiceContext, id, from int64, reason string) {
	var err error
	switch from {
	case model.VideoPlanning:
		_, err = svcCtx.Video.FinishPlan(ctx, id, "", reason, model.VideoFailed)
	case model.VideoGenerating:
		_, err = svcCtx.Video.FinishGenerate(ctx, id, model.VideoFailed, "", reason)
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("fail video %d: %v", id, err)
	}
}

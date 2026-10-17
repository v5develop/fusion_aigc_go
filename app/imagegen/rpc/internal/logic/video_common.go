package logic

import (
	"aigc-local-life/pkg/taskcenter"
	"context"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/copywriting/rpc/copywriting"
	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// videoModels 探店视频 / 带货短剧的模型选择（设计稿「生成设置」弹窗，两个模块一样）。
// 值直接作为执行面 73 / 88 的 model：seedance2.5 走云端（81 / 89），lite 走执行面本地 GPU。
var videoModels = []*pb.Option{
	{Code: "seedance2.5", Label: "seedance 2.5"},
	{Code: "lite", Label: "XQ Video 1.0 Lite"},
}

// maxRemakePromptLen 视频复刻里用户可改的剧情文本上限。70 解析出来的本身就很长（几千字），
// 给足余量，只拦明显异常的输入。
const maxRemakePromptLen = 20000

// maxCoverBytes 封面图的下载上限，封面就是张缩略图，2M 绰绰有余
const maxCoverBytes = 2 << 20

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

// videoLimits 源视频限制（视频复刻）。Formats 留空时用内置白名单。
func videoLimits(cfg *config.VideoRemakeConf) aigc.VideoLimits {
	formats := cfg.Formats
	if len(formats) == 0 {
		formats = []string{"mp4", "avi", "mkv", "mov", "flv", "wmv"}
	}
	return aigc.VideoLimits{MaxBytes: cfg.MaxVideoBytes, MaxSeconds: cfg.MaxVideoSeconds, Formats: formats}
}

func toVideoInfo(c *model.AfvVideoCreation) *pb.VideoInfo {
	return &pb.VideoInfo{
		Id: c.Id, UserId: c.UserId, Kind: c.Kind, UserText: c.UserText.String,
		SellingPoints: c.SellingPoints.String, ShopProfileId: c.ShopProfileId.Int64,
		CharUrl: c.CharUrl.String, Env1Url: c.Env1Url.String, Env2Url: c.Env2Url.String, PropUrl: c.PropUrl.String,
		Duration: int32(c.Duration), Size: c.Size, Model: c.Model, Script: scriptForAPI(c),
		VideoUrl: c.VideoUrl.String, Status: int32(c.Status), FailReason: c.FailReason.String,
		Source: c.Source.String, SourceUrl: c.SourceUrl.String, SourceVideoUrl: c.SourceVideoUrl.String,
		Title: c.Title.String, CoverUrl: c.CoverUrl.String,
		CreateTime: c.CreateTime.UnixMilli(),
	}
}

// scriptForAPI 对外输出的剧本。
//
// 探店 / 短剧是结构化剧本（JSON 对象，含 scenes），原样返回；
// 视频复刻存的是 70 解析出来的**一段文本**，为了塞进 json 列才包了一层 JSON 字符串——
// 这里把那层剥掉，前端直接拿到可读文本，不用先 parse 出一个字符串再渲染。
func scriptForAPI(c *model.AfvVideoCreation) string {
	if c.Kind != model.KindVideoRemake || !c.PlanScript.Valid {
		return c.PlanScript.String
	}
	var text string
	if err := json.Unmarshal([]byte(c.PlanScript.String), &text); err != nil {
		return c.PlanScript.String // 解不出来就原样给，总比空着强
	}
	return text
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

// planTask 第一步：探店 72 出剧本 / 短剧 87 出剧本 / 视频复刻 70 解析原片。
func planTask(c *model.AfvVideoCreation, shopProfile string) aigc.Task {
	if c.Kind == model.KindVideoRemake {
		return aigc.VideoParse{VideoURL: c.SourceVideoUrl.String}
	}
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

// remakeTask 视频复刻第二步：把剧情文本交给 71。
// model / aspectRatio 是用户在剧情分析页选的，没选就留空用执行面默认（成片跟原片走）。
func remakeTask(c *model.AfvVideoCreation) (aigc.Task, error) {
	var text string
	if err := json.Unmarshal([]byte(c.PlanScript.String), &text); err != nil {
		return nil, xerr.New(xerr.ServerError, "剧情内容异常，请重新解析")
	}
	return aigc.VideoRemake{
		Script: text, Duration: clampRemakeSeconds(int(c.Duration)),
		AspectRatio: c.Size, Model: c.Model,
	}, nil
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
		aigc.LogSubmitFailed(ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "plan", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id), ExecTaskID: c.PlanTaskId.String}, err)
		return xerr.New(xerr.ServerError, "提交构思失败，请重试")
	}
	// TODO 计费：等计费方案确认后，构思预扣 10 点、出片预扣 100 点，失败退回
	return nil
}

// failVideo 把记录标记为失败（只在它还处于 from 状态时生效）。

// lookupCtx 失败日志要补的查库上下文：原 ctx 可能已经超时，单独给一个短超时，查不到也不影响业务。
func lookupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// videoTrace 视频失败的排障坐标：带上所属用户与类型（查库失败则留空）。
func videoTrace(ctx context.Context, svcCtx *svc.ServiceContext, id int64, stage string) aigc.Trace {
	t := aigc.Trace{Service: "imagegen", Stage: stage, RecordID: id, TaskCenterID: taskcenter.Ref("vid", id)}
	lctx, cancel := lookupCtx(ctx)
	defer cancel()
	if c, err := svcCtx.Video.FindOne(lctx, id); err == nil {
		t.UserID, t.Kind = c.UserId, c.Kind
	} else {
		aigc.LogLookupFailed(ctx, t, err)
	}
	return t
}

func failVideo(ctx context.Context, svcCtx *svc.ServiceContext, id, from int64, reason string) {
	stage := "plan"
	if from == model.VideoGenerating {
		stage = "generate"
	}
	aigc.LogTaskFailed(ctx, videoTrace(ctx, svcCtx, id, stage), reason, nil)
	var err error
	switch from {
	case model.VideoPlanning:
		_, err = svcCtx.Video.FinishPlan(ctx, id, "", reason, model.VideoFailed)
	case model.VideoGenerating:
		var ok bool
		if ok, err = svcCtx.Video.FinishGenerate(ctx, id, model.VideoFailed, "", reason); err == nil && ok {
			// 出片失败 / 超时：退回出片时冻结的点数（构思、解析阶段不收费，没有冻结）
			if c, ferr := svcCtx.Video.FindOne(ctx, id); ferr == nil {
				svcCtx.Billing.Fail(ctx, c.UserId, c.GenTaskId.String, reason)
			} else {
				logx.WithContext(ctx).Errorf("fail video %d: 找不到记录，无法退回点数: %v", id, ferr)
			}
		}
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("fail video %d: %v", id, err)
	}
}

// minRemakeSeconds 71 底层的 Seedance 要求时长 ≥ 4 秒（2026-10-10 测试服实测：传 1 会报校验错误）。
const minRemakeSeconds = 4

func clampRemakeSeconds(s int) int {
	if s < minRemakeSeconds {
		return minRemakeSeconds
	}
	return s
}

var scriptTotalRe = regexp.MustCompile(`【总时长】[：:]\s*([0-9]+(?:\.[0-9]+)?)`)

// remakeSeconds 原片时长。70 有时不回 duration（实测出现过），这时从剧情文本里的「【总时长】：5.0 秒」取；
// 都没有就按最短档，不能是 0 或 1。
func remakeSeconds(parsed float64, script string) int {
	if parsed <= 0 {
		if m := scriptTotalRe.FindStringSubmatch(script); m != nil {
			parsed, _ = strconv.ParseFloat(m[1], 64)
		}
	}
	return clampRemakeSeconds(int(math.Round(parsed)))
}

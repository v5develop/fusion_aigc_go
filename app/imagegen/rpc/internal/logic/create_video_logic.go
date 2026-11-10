package logic

import (
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/taskcenter"
	"time"

	"aigc-local-life/pkg/linkparse"
	"context"

	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"errors"
	"net/http"
	"strings"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateVideoLogic {
	return &CreateVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// CreateVideo 提交表单，出剧本（72 / 87），轮询 GetVideo
// 提交表单，出剧本（探店 72 / 短剧 87），之后轮询 GetVideo
func (l *CreateVideoLogic) CreateVideo(in *pb.CreateVideoReq) (*pb.VideoInfo, error) {
	if in.RequestId == "" {
		return nil, xerr.Param("缺少 X-Request-Id")
	}
	// 幂等：同一用户同一 request_id 只生成一次
	if old, err := l.svcCtx.Video.FindByRequestID(l.ctx, in.UserId, in.RequestId); err == nil {
		return toVideoInfo(old), nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	if in.Kind == model.KindVideoRemake {
		return l.createRemake(in)
	}
	cfg, err := videoKindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.VideoLimits
	userText := strings.TrimSpace(in.UserText)
	sellingPoints := strings.TrimSpace(in.SellingPoints)
	label := "视频文案"
	if in.Kind == model.KindShortDrama {
		label = "短剧主题"
	}
	switch {
	case userText == "":
		return nil, xerr.Newf(xerr.ParamError, "请输入%s", label)
	case runeLen(userText) > lim.MaxUserTextLen:
		return nil, xerr.Newf(xerr.ParamError, "%s最多 %d 字", label, lim.MaxUserTextLen)
	case runeLen(sellingPoints) > lim.MaxSellingPointsLen:
		return nil, xerr.Newf(xerr.ParamError, "商品卖点最多 %d 字", lim.MaxSellingPointsLen)
	}
	if err := checkText(l.ctx, l.svcCtx, in.Kind+"_input", userText+"\n"+sellingPoints); err != nil {
		return nil, err
	}

	duration, err := videoDurationOf(l.ctx, l.svcCtx, cfg, in.Duration)
	if err != nil {
		return nil, err
	}
	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = defaultSizeOf(l.ctx, l.svcCtx, in.Kind, cfg.DefaultSize)
	}
	if !validOption(l.ctx, l.svcCtx, genVideoSizes, videoSizes, size) {
		return nil, xerr.Param("视频比例无效")
	}
	mdl, err := videoModelOf(l.ctx, l.svcCtx, cfg, in.Model)
	if err != nil {
		return nil, err
	}

	// 出镜人物图：选填，三种来源交给数字人服务解析
	charURL := ""
	if in.PortraitSource != "" {
		p, err := l.svcCtx.DigitalHumanRpc.ResolvePortrait(l.ctx, &digitalhuman.ResolvePortraitReq{
			UserId: in.UserId, PortraitSource: in.PortraitSource, ImageUrl: in.ImageUrl,
			PublicId: in.PublicId, PortraitId: in.PortraitId, ImageIndex: in.ImageIndex,
		})
		if err != nil {
			return nil, err
		}
		charURL = p.ImageUrl
	}
	env1Label, propLabel := "门店图", "道具图"
	if in.Kind == model.KindShortDrama {
		env1Label, propLabel = "环境图", "商品图"
	}
	env1URL, err := materialURL(l.svcCtx, in.Env1Url, env1Label)
	if err != nil {
		return nil, err
	}
	env2URL, err := materialURL(l.svcCtx, in.Env2Url, "店内图")
	if err != nil {
		return nil, err
	}
	propURL, err := materialURL(l.svcCtx, in.PropUrl, propLabel)
	if err != nil {
		return nil, err
	}
	// 短剧的商品图必填（87 要求；设计稿会同步改成必填）
	if in.Kind == model.KindShortDrama && propURL == "" {
		return nil, xerr.Param("请上传商品图")
	}

	row := &model.AfvVideoCreation{
		UserId: in.UserId, Kind: in.Kind, UserText: nullStr(userText), SellingPoints: nullStr(sellingPoints),
		ShopProfileId: nullInt(in.ShopProfileId), CopywritingId: nullInt(in.CopywritingId),
		CharUrl: nullStr(charURL), Env1Url: nullStr(env1URL),
		Env2Url: nullStr(env2URL), PropUrl: nullStr(propURL), Duration: int64(duration), Size: size, Model: mdl,
		PlanTaskId: nullStr(newTaskID(l.svcCtx)), RequestId: nullStr(in.RequestId), Status: model.VideoPlanning,
	}
	res, err := l.svcCtx.Video.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) { // 并发重复提交
			old, ferr := l.svcCtx.Video.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toVideoInfo(old), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	refPlanTaskID(l.ctx, l.svcCtx, row)
	if err := submitPlan(l.ctx, l.svcCtx, row, videoShopProfileText(l.ctx, l.svcCtx, in.ShopProfileId, in.UserId)); err != nil {
		failVideo(l.ctx, l.svcCtx, row.Id, model.VideoPlanning, "提交构思失败，请重试")
		return nil, err
	}
	cur, err := findOwnedVideo(l.ctx, l.svcCtx, row.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(cur), nil
}

// createRemake 爆款视频复刻：拿到一个能下载的视频地址，交给执行面 70 解析。
//
// 两种来源：上传本地视频（直传后的地址，我们自己的桶）、粘贴分享链接（走解析服务，没配地址时不开放）。
// 提交前先探一下体积与格式，超限直接拒，不让用户白等一场解析（时长 HEAD 拿不到，等 70 返回再判）。
func (l *CreateVideoLogic) createRemake(in *pb.CreateVideoReq) (*pb.VideoInfo, error) {
	cfg := &l.svcCtx.Config.VideoRemake
	var row *model.AfvVideoCreation
	var linked *linkparse.Video // 链接来源：解析结果，下载转存在后台继续
	switch in.Source {
	case model.SourceUpload:
		videoURL := strings.TrimSpace(in.VideoUrl)
		if videoURL == "" {
			return nil, xerr.Param("请先上传视频")
		}
		if err := storage.Owned(l.svcCtx.Storage, videoURL); err != nil {
			return nil, xerr.Param("请先通过上传接口上传视频")
		}
		if err := videoLimits(cfg).ProbeVideo(l.ctx, http.DefaultClient, videoURL); err != nil {
			return nil, xerr.Param(err.Error())
		}
		row = &model.AfvVideoCreation{
			Source: nullStr(model.SourceUpload), SourceVideoUrl: nullStr(videoURL),
		}
	case model.SourceLink:
		link := strings.TrimSpace(in.Link)
		if link == "" {
			return nil, xerr.Param("请粘贴视频链接")
		}
		if !l.svcCtx.LinkParse.Enabled() {
			return nil, xerr.New(xerr.NotReady, "链接解析暂未开放，请先用「上传视频」")
		}
		// 只在这里同步做解析（几秒）：能立刻校验链接、拿到标题封面。
		// 下载 + 转存可能要几十秒（最大 50M），放后台做，不然提交接口会超时。
		v, err := l.parseLink(link)
		if err != nil {
			return nil, err
		}
		linked = v
		row = &model.AfvVideoCreation{
			Source: nullStr(model.SourceLink), SourceUrl: nullStr(orDefault(v.SourceURL, link)),
			Title: nullStr(v.Title),
		}
	default:
		return nil, xerr.Param("请选择分析方式")
	}

	row.UserId, row.Kind = in.UserId, in.Kind
	if linked == nil { // 链接来源的任务号等转存完再分配
		row.PlanTaskId = nullStr(newTaskID(l.svcCtx))
	}
	row.RequestId = nullStr(in.RequestId)
	row.Status = model.VideoPlanning
	row.Duration = 0 // 原片时长等 70 返回后回填
	res, err := l.svcCtx.Video.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) {
			old, ferr := l.svcCtx.Video.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toVideoInfo(old), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	if linked != nil {
		// 后台：下载直链（会过期，所以当场下）→ 转存到我们的桶 → 再交给执行面 70
		go l.fetchAndSubmit(row.Id, in.UserId, linked)
	} else {
		refPlanTaskID(l.ctx, l.svcCtx, row)
		if err := submitPlan(l.ctx, l.svcCtx, row, ""); err != nil {
			failVideo(l.ctx, l.svcCtx, row.Id, model.VideoPlanning, "提交解析失败，请重试")
			return nil, err
		}
	}
	cur, err := findOwnedVideo(l.ctx, l.svcCtx, row.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(cur), nil
}

// parseLink 解析分享链接。只做解析（抖音 4–8 秒，其余更快），拿到直链、标题、封面。
//
// 时长这里能拿到就先判，省得白下一场（B站 不返回时长，只能等执行面 70 回报再判）。
func (l *CreateVideoLogic) parseLink(link string) (*linkparse.Video, error) {
	cfg := &l.svcCtx.Config.VideoRemake
	v, err := l.svcCtx.LinkParse.Parse(l.ctx, link)
	switch {
	case errors.Is(err, linkparse.ErrUnsupported):
		return nil, xerr.Param("这个链接解析不了，换一条分享链接或直接上传视频（支持抖音、快手、小红书、B站）")
	case errors.Is(err, linkparse.ErrNoVideo):
		return nil, xerr.Param("这个作品里没有视频，换一个再试")
	case err != nil:
		l.Errorf("parse link %q: %v", link, err)
		return nil, xerr.New(xerr.ServerError, "链接解析失败，请稍后重试")
	}
	if cfg.MaxVideoSeconds > 0 && int(v.Duration) > cfg.MaxVideoSeconds {
		return nil, xerr.Param(videoLimits(cfg).LimitMsg())
	}
	return v, nil
}

// fetchAndSubmit 后台：把直链下下来转存，再交给执行面 70。
//
// 为什么不在提交接口里做完：一个 50M 的视频下载 + 上传 OSS 可能几十秒，
// 而 RPC 有超时（imagegen-rpc 的 Timeout），同步做会超时报「服务繁忙」——踩过一次。
//
// 直链带签名、几小时就过期，所以必须当场下、转存到我们自己的桶：
// 直接把直链丢给执行面，等它排到队可能已经 403，用户之后回看原片也没了。
func (l *CreateVideoLogic) fetchAndSubmit(id, userID int64, v *linkparse.Video) {
	// 脱离请求的 ctx：接口早就返回了，用它会立刻被取消
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(l.svcCtx.Config.VideoRemake.FetchTimeoutMinutes)*time.Minute)
	defer cancel()
	log := logx.WithContext(ctx)
	cfg := &l.svcCtx.Config.VideoRemake

	data, ct, err := linkparse.Download(ctx, http.DefaultClient, v, cfg.MaxVideoBytes)
	switch {
	case errors.Is(err, linkparse.ErrTooLarge):
		failVideo(ctx, l.svcCtx, id, model.VideoPlanning, videoLimits(cfg).LimitMsg())
		return
	case err != nil:
		aigc.LogStepFailed(ctx, aigc.Trace{Service: "imagegen", Kind: "videoRemake", Stage: "fetch", RecordID: id, UserID: userID, TaskCenterID: taskcenter.Ref("vid", id)}, "download", err)
		failVideo(ctx, l.svcCtx, id, model.VideoPlanning, "视频下载失败，请重试")
		return
	}
	key := storage.ObjectKey(l.svcCtx.Config.Storage.Prefix, "video", userID, extOf(ct))
	videoURL, err := l.svcCtx.Storage.Put(ctx, key, ct, data)
	if err != nil {
		aigc.LogStepFailed(ctx, aigc.Trace{Service: "imagegen", Stage: "fetch", RecordID: id, UserID: userID, TaskCenterID: taskcenter.Ref("vid", id)}, "store", err)
		failVideo(ctx, l.svcCtx, id, model.VideoPlanning, "视频转存失败，请重试")
		return
	}
	cover := l.saveCover(ctx, userID, v)

	taskID := newRefTaskID(l.svcCtx, "vid", id, "plan")
	if err := l.svcCtx.Video.FinishFetch(ctx, id, videoURL, cover, taskID); err != nil {
		aigc.LogStepFailed(ctx, aigc.Trace{Service: "imagegen", Stage: "fetch", RecordID: id, UserID: userID, TaskCenterID: taskcenter.Ref("vid", id)}, "finish", err)
		failVideo(ctx, l.svcCtx, id, model.VideoPlanning, "视频转存失败，请重试")
		return
	}
	c, err := l.svcCtx.Video.FindOwned(ctx, id, userID)
	if err != nil {
		log.Errorf("find video %d: %v", id, err)
		return
	}
	if err := submitPlan(ctx, l.svcCtx, c, ""); err != nil {
		failVideo(ctx, l.svcCtx, id, model.VideoPlanning, "提交解析失败，请重试")
	}
}

// saveCover 封面也转存一份（也是会过期的直链）。封面没了不影响生成，失败就不要。
func (l *CreateVideoLogic) saveCover(ctx context.Context, userID int64, v *linkparse.Video) string {
	if v.CoverURL == "" {
		return ""
	}
	cover := &linkparse.Video{URL: v.CoverURL, UserAgent: v.UserAgent}
	data, _, err := linkparse.Download(ctx, http.DefaultClient, cover, maxCoverBytes)
	if err != nil {
		logx.WithContext(ctx).Infof("下载封面失败，跳过: %v", err)
		return ""
	}
	key := storage.ObjectKey(l.svcCtx.Config.Storage.Prefix, "image", userID, "jpg")
	url, err := l.svcCtx.Storage.Put(ctx, key, "image/jpeg", data)
	if err != nil {
		logx.WithContext(ctx).Infof("转存封面失败，跳过: %v", err)
		return ""
	}
	return url
}

// extOf 按 content-type 取扩展名，认不出就当 mp4（四个平台的视频作品都是 mp4）。
func extOf(contentType string) string {
	switch contentType {
	case "video/quicktime":
		return "mov"
	case "video/x-msvideo":
		return "avi"
	default:
		return "mp4"
	}
}

func orDefault(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// videoDurationOf 探店 / 短剧的视频时长归一（不传取后台默认，取值必须在选项里）。提交与报价共用。
func videoDurationOf(ctx context.Context, svcCtx *svc.ServiceContext, cfg *config.VideoKindConf, duration int32) (int32, error) {
	if duration == 0 {
		duration = defaultDurationOf(ctx, svcCtx, genVideoDurations, int32(cfg.DefaultDuration))
	}
	if !containsDuration(durationsOf(ctx, svcCtx, genVideoDurations, videoDurations), duration) {
		return 0, xerr.Param("视频时长无效")
	}
	return duration, nil
}

// videoModelOf 探店 / 短剧的模型档位归一。提交与报价共用。
func videoModelOf(ctx context.Context, svcCtx *svc.ServiceContext, cfg *config.VideoKindConf, mdl string) (string, error) {
	mdl = strings.TrimSpace(mdl)
	if mdl == "" {
		mdl = defaultOf(ctx, svcCtx, genVideoModels, cfg.DefaultModel)
	}
	if !validModel(optionsOf(ctx, svcCtx, genVideoModels, videoModels), mdl) {
		return "", xerr.Param("模型无效")
	}
	return mdl, nil
}

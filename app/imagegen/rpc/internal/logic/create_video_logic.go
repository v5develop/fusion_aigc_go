package logic

import (
	"aigc-life/pkg/linkparse"
	"context"

	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"errors"
	"net/http"
	"strings"

	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/imagegen/model"
	"aigc-life/pkg/storage"
	"aigc-life/pkg/xerr"

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

	duration := in.Duration
	if duration == 0 {
		duration = int32(cfg.DefaultDuration)
	}
	if duration != 15 && duration != 30 {
		return nil, xerr.Param("视频时长为 15 或 30 秒")
	}
	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = cfg.DefaultSize
	}
	if size != "9:16" && size != "16:9" {
		return nil, xerr.Param("视频比例无效")
	}
	mdl := strings.TrimSpace(in.Model)
	if mdl == "" {
		mdl = cfg.DefaultModel
	}
	if !validModel(videoModels, mdl) {
		return nil, xerr.Param("模型无效")
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
		ShopProfileId: nullInt(in.ShopProfileId), CharUrl: nullStr(charURL), Env1Url: nullStr(env1URL),
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
		v, err := l.resolveLink(in.UserId, link)
		if err != nil {
			return nil, err
		}
		row = &model.AfvVideoCreation{
			Source: nullStr(model.SourceLink), SourceUrl: nullStr(orDefault(v.SourceURL, link)),
			SourceVideoUrl: nullStr(v.VideoURL), Title: nullStr(v.Title), CoverUrl: nullStr(v.CoverURL),
		}
	default:
		return nil, xerr.Param("请选择分析方式")
	}

	row.UserId, row.Kind = in.UserId, in.Kind
	row.PlanTaskId = nullStr(newTaskID(l.svcCtx))
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
	if err := submitPlan(l.ctx, l.svcCtx, row, ""); err != nil {
		failVideo(l.ctx, l.svcCtx, row.Id, model.VideoPlanning, "提交解析失败，请重试")
		return nil, err
	}
	cur, err := findOwnedVideo(l.ctx, l.svcCtx, row.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(cur), nil
}

// linkVideo 解析并转存后的结果，存的都是我们自己桶里的地址。
type linkVideo struct {
	VideoURL  string
	CoverURL  string
	Title     string
	SourceURL string
}

// resolveLink 分享链接 → 我们桶里的视频地址。
//
// 解析服务给的直链**带签名、几小时就过期**，所以必须当场下下来转存，
// 不能把直链直接丢给执行面——等它排到队可能已经 403 了，用户还要回看原片。
// 下载是有上限的（和上传同一个 50M），超了立刻断，不让一个几百兆的视频把内存撑爆。
func (l *CreateVideoLogic) resolveLink(userID int64, link string) (*linkVideo, error) {
	cfg := &l.svcCtx.Config.VideoRemake
	lim := videoLimits(cfg)

	v, err := l.svcCtx.LinkParse.Parse(l.ctx, link)
	switch {
	case errors.Is(err, linkparse.ErrUnsupported):
		return nil, xerr.Param("这个链接解析不了，目前支持抖音、快手、小红书、B站的分享链接")
	case errors.Is(err, linkparse.ErrNoVideo):
		return nil, xerr.Param("这个作品里没有视频，换一个再试")
	case err != nil:
		l.Errorf("parse link %q: %v", link, err)
		return nil, xerr.New(xerr.ServerError, "链接解析失败，请稍后重试")
	}
	// 时长这里能拿到就先判，省得下完 50M 才发现超时长（拿不到的等 70 返回再判）
	if cfg.MaxVideoSeconds > 0 && int(v.Duration) > cfg.MaxVideoSeconds {
		return nil, xerr.Param(lim.LimitMsg())
	}

	data, ct, err := linkparse.Download(l.ctx, http.DefaultClient, v, cfg.MaxVideoBytes)
	switch {
	case errors.Is(err, linkparse.ErrTooLarge):
		return nil, xerr.Param(lim.LimitMsg())
	case err != nil:
		l.Errorf("download %s: %v", v.URL, err)
		return nil, xerr.New(xerr.ServerError, "视频下载失败，请稍后重试")
	}
	key := storage.ObjectKey(l.svcCtx.Config.Storage.Prefix, "video", userID, extOf(ct))
	videoURL, err := l.svcCtx.Storage.Put(l.ctx, key, ct, data)
	if err != nil {
		l.Errorf("put video: %v", err)
		return nil, xerr.New(xerr.ServerError, "视频转存失败，请稍后重试")
	}
	return &linkVideo{
		VideoURL: videoURL, Title: v.Title, SourceURL: v.SourceURL,
		CoverURL: l.saveCover(userID, v),
	}, nil
}

// saveCover 封面也转存一份（也是会过期的直链）。封面没了不影响生成，失败就不要，别挡着用户。
func (l *CreateVideoLogic) saveCover(userID int64, v *linkparse.Video) string {
	if v.CoverURL == "" {
		return ""
	}
	cover := &linkparse.Video{URL: v.CoverURL, UserAgent: v.UserAgent}
	data, _, err := linkparse.Download(l.ctx, http.DefaultClient, cover, maxCoverBytes)
	if err != nil {
		l.Infof("下载封面失败，跳过: %v", err)
		return ""
	}
	key := storage.ObjectKey(l.svcCtx.Config.Storage.Prefix, "image", userID, "jpg")
	url, err := l.svcCtx.Storage.Put(l.ctx, key, "image/jpeg", data)
	if err != nil {
		l.Infof("转存封面失败，跳过: %v", err)
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

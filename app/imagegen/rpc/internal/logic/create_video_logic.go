package logic

import (
	"context"

	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"errors"
	"strings"

	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/imagegen/model"
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
	cfg, err := videoKindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	if err := requireAigc(l.svcCtx); err != nil {
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

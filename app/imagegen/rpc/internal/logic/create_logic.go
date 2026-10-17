package logic

import (
	"context"
	"errors"
	"strings"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateLogic {
	return &CreateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 提交生成（异步，轮询 Get）。一张图一个执行面任务。
func (l *CreateLogic) Create(in *pb.CreateReq) (*pb.CreationInfo, error) {
	if in.RequestId == "" {
		return nil, xerr.Param("缺少 X-Request-Id")
	}
	// 幂等：同一用户同一 request_id 只生成一次
	if old, err := l.svcCtx.Creation.FindByRequestID(l.ctx, in.UserId, in.RequestId); err == nil {
		return toInfo(old), nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	cfg, err := kindConf(l.svcCtx, in.Kind)
	if err != nil {
		return nil, err
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	switch in.Kind {
	case model.KindActionTransfer:
		return l.createActionTransfer(in, cfg)
	case model.KindVoiceover:
		return l.createVoiceover(in, cfg)
	}
	lim := l.svcCtx.Config.Limits
	content := strings.TrimSpace(in.Content)
	switch {
	case content == "":
		return nil, xerr.Param("请输入内容")
	case runeLen(content) > lim.MaxContentLen:
		return nil, xerr.Newf(xerr.ParamError, "内容最多 %d 字", lim.MaxContentLen)
	}
	if err := checkText(l.ctx, l.svcCtx, in.Kind+"_input", content); err != nil {
		return nil, err
	}
	mats, err := materials(l.ctx, l.svcCtx, in.MaterialUrls)
	if err != nil {
		return nil, err
	}

	// 模板：海报、长图都必选（按配置 TemplateRequired）。下单时把地址快照下来，之后模板下架不影响。
	var templateURL string
	if in.TemplateId > 0 {
		t, err := l.svcCtx.Shared.FindTemplate(l.ctx, templateSlot(cfg), in.TemplateId)
		if errors.Is(err, model.ErrNotFound) {
			return nil, xerr.Param("该模板已下架，请重新选择")
		}
		if err != nil {
			return nil, err
		}
		templateURL = t.Url
	} else if templateRequired(l.ctx, l.svcCtx, in.Kind, cfg.TemplateRequired) {
		return nil, xerr.Param("请选择模板")
	}

	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = defaultSizeOf(l.ctx, l.svcCtx, in.Kind, cfg.DefaultSize)
	}
	if in.Kind == model.KindPoster {
		if !validOption(l.ctx, l.svcCtx, genPosterStyles, posterStyles, in.PosterStyle) {
			return nil, xerr.Param("海报风格无效")
		}
		if !validSize(l.ctx, l.svcCtx, size) {
			return nil, xerr.Param("图片比例无效")
		}
	} else {
		size = defaultSizeOf(l.ctx, l.svcCtx, in.Kind, cfg.DefaultSize) // 长图比例固定
	}
	count := int64(in.ImageCount)
	if count == 0 {
		count = 1
	}
	if count < 1 || count > int64(lim.MaxImages) {
		return nil, xerr.Newf(xerr.ParamError, "生成数量为 1–%d 张", lim.MaxImages)
	}

	// 海报可以指定档位（options 下发的 models）；不传用配置里的默认。长图没有 model 参数
	mdl := cfg.Model // 长图没有 model 参数，留空
	if in.Kind == model.KindPoster {
		mdl = defaultOf(l.ctx, l.svcCtx, genPosterModels, cfg.Model)
	}
	if in.Kind == model.KindPoster && in.Model != "" {
		if !validModel(optionsOf(l.ctx, l.svcCtx, genPosterModels, posterModels), in.Model) {
			return nil, xerr.Newf(xerr.ParamError, "不支持的模型 %s", in.Model)
		}
		mdl = in.Model
	}

	row := &model.LocalLifeCreation{
		UserId: in.UserId, Kind: in.Kind, Content: content, MaterialUrls: nullStr(jsonList(mats)),
		TemplateAssetId: nullInt(in.TemplateId), TemplateUrl: nullStr(templateURL),
		PosterType: nullStr(in.PosterType), PosterStyle: nullStr(in.PosterStyle), Size: size, Model: mdl,
		CopywritingId: nullInt(in.CopywritingId),
		ImageCount:    count, RequestId: nullStr(in.RequestId), Status: model.StatusGenerating,
	}
	return l.insertAndSubmit(row, cfg, posterTypeLabel(l.ctx, l.svcCtx, cfg.BizType, in.PosterType))
}

// insertAndSubmit 落记录后提交执行面任务。同一 request_id 并发重复提交时返回先到的那条。
func (l *CreateLogic) insertAndSubmit(row *model.LocalLifeCreation, cfg *config.KindConf, typeLabel string) (*pb.CreationInfo, error) {
	// 先冻结点数（点数不够就到此为止，不落记录）。同一请求号重复提交，冻结是幂等的
	if _, err := l.svcCtx.Billing.Hold(l.ctx, imageCharge(row)); err != nil {
		return nil, err
	}
	res, err := l.svcCtx.Creation.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) {
			old, ferr := l.svcCtx.Creation.FindByRequestID(l.ctx, row.UserId, row.RequestId.String)
			if ferr != nil {
				return nil, ferr
			}
			return toInfo(old), nil
		}
		l.svcCtx.Billing.Fail(l.ctx, row.UserId, row.RequestId.String, "落库失败")
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		l.svcCtx.Billing.Fail(l.ctx, row.UserId, row.RequestId.String, "落库失败")
		return nil, err
	}
	tasks := submit(l.ctx, l.svcCtx, row, cfg, typeLabel)
	if _, err := l.svcCtx.Creation.UpdateTasks(l.ctx, row.Id, dumpTasks(tasks), model.StatusGenerating, "", ""); err != nil {
		l.Errorf("save tasks %d: %v", row.Id, err)
	}
	if cur, err := l.svcCtx.Creation.FindOne(l.ctx, row.Id); err == nil {
		row = cur
	}
	return toInfo(row), nil
}

func validSize(ctx context.Context, svcCtx *svc.ServiceContext, size string) bool {
	return validModel(optionsOf(ctx, svcCtx, genPosterSizes, posterSizes), size)
}

// createActionTransfer 动作迁移：人物图（上传/公共数字人/AI定制形象）+ 动作视频（预置或我导入的）→ 视频。
func (l *CreateLogic) createActionTransfer(in *pb.CreateReq, cfg *config.KindConf) (*pb.CreationInfo, error) {
	// 人物图交给数字人服务解析，三种来源的校验逻辑不重复写
	p, err := l.svcCtx.DigitalHumanRpc.ResolvePortrait(l.ctx, &digitalhuman.ResolvePortraitReq{
		UserId: in.UserId, PortraitSource: in.PortraitSource, ImageUrl: in.ImageUrl,
		PublicId: in.PublicId, PortraitId: in.PortraitId, ImageIndex: in.ImageIndex,
	})
	if err != nil {
		return nil, err
	}
	videoURL, err := l.actionVideo(in, cfg)
	if err != nil {
		return nil, err
	}
	if !validModel(optionsOf(l.ctx, l.svcCtx, genActionModels, actionModels), in.Model) {
		return nil, xerr.Param("请选择模型")
	}

	row := &model.LocalLifeCreation{
		UserId: in.UserId, Kind: in.Kind, Content: "", InputImageUrl: nullStr(p.ImageUrl),
		InputVideoUrl: nullStr(videoURL), Model: in.Model, ImageCount: 1,
		RequestId: nullStr(in.RequestId), Status: model.StatusGenerating,
	}
	return l.insertAndSubmit(row, cfg, "")
}

// actionVideo 动作视频：预置（模板素材）或我导入的，二选一。
func (l *CreateLogic) actionVideo(in *pb.CreateReq, cfg *config.KindConf) (string, error) {
	switch {
	case in.ActionVideoId > 0:
		t, err := l.svcCtx.Shared.FindTemplate(l.ctx, templateSlot(cfg), in.ActionVideoId)
		if errors.Is(err, model.ErrNotFound) {
			return "", xerr.Param("该动作视频已下架，请重新选择")
		}
		return t.Url, err
	case in.MyActionVideoId > 0:
		v, err := l.svcCtx.ActionVideo.FindOwned(l.ctx, in.MyActionVideoId, in.UserId)
		if errors.Is(err, model.ErrNotFound) {
			return "", xerr.Param("动作视频不存在")
		}
		return v.Url, err
	}
	return "", xerr.Param("请选择动作视频")
}

// createVoiceover 数字人口播：口播文案 + 人物图（上传/公共数字人/AI定制形象）+ 背景图（可选）→ 视频。
// 执行面 92 一步出片，剧本由它内部生成。
func (l *CreateLogic) createVoiceover(in *pb.CreateReq, cfg *config.KindConf) (*pb.CreationInfo, error) {
	content := strings.TrimSpace(in.Content) // 字数不限（产品确认）
	if content == "" {
		return nil, xerr.Param("请输入口播文案")
	}
	vm := optionsOf(l.ctx, l.svcCtx, genVoiceoverModels, voiceoverModels)
	mdl := in.Model
	if mdl == "" {
		mdl = defaultOf(l.ctx, l.svcCtx, genVoiceoverModels, vm[0].Code)
	}
	if !validModel(vm, mdl) {
		return nil, xerr.Param("模型无效")
	}
	duration := in.Duration
	if duration == 0 {
		duration = defaultDurationOf(l.ctx, l.svcCtx, genVoiceoverDurations, voiceoverDurations[0])
	}
	if !containsDuration(durationsOf(l.ctx, l.svcCtx, genVoiceoverDurations, voiceoverDurations), duration) {
		return nil, xerr.Param("视频时长无效")
	}
	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = defaultSizeOf(l.ctx, l.svcCtx, in.Kind, cfg.DefaultSize)
	}
	if !validOption(l.ctx, l.svcCtx, genVoiceoverSizes, voiceoverSizes, size) {
		return nil, xerr.Param("视频比例无效")
	}
	if err := checkText(l.ctx, l.svcCtx, "voiceover_input", content); err != nil {
		return nil, err
	}
	p, err := l.svcCtx.DigitalHumanRpc.ResolvePortrait(l.ctx, &digitalhuman.ResolvePortraitReq{
		UserId: in.UserId, PortraitSource: in.PortraitSource, ImageUrl: in.ImageUrl,
		PublicId: in.PublicId, PortraitId: in.PortraitId, ImageIndex: in.ImageIndex,
	})
	if err != nil {
		return nil, err
	}
	bgID, bgURL, err := l.background(in, cfg)
	if err != nil {
		return nil, err
	}

	row := &model.LocalLifeCreation{
		UserId: in.UserId, Kind: in.Kind, Content: content, InputImageUrl: nullStr(p.ImageUrl),
		TemplateAssetId: nullInt(bgID), InputBgUrl: nullStr(bgURL), Size: size, Duration: int64(duration),
		Model: mdl, ImageCount: 1, RequestId: nullStr(in.RequestId), Status: model.StatusGenerating,
	}
	return l.insertAndSubmit(row, cfg, "")
}

// background 数字人口播的背景图：AI智能匹配（不传给执行面）/ 预设背景（模板素材，快照地址）/ 导入背景（直传）。
func (l *CreateLogic) background(in *pb.CreateReq, cfg *config.KindConf) (int64, string, error) {
	switch in.BackgroundSource {
	case "", "AI":
		return 0, "", nil
	case "PRESET":
		if in.BackgroundId <= 0 {
			return 0, "", xerr.Param("请选择背景")
		}
		t, err := l.svcCtx.Shared.FindTemplate(l.ctx, templateSlot(cfg), in.BackgroundId)
		if errors.Is(err, model.ErrNotFound) {
			return 0, "", xerr.Param("该背景已下架，请重新选择")
		}
		if err != nil {
			return 0, "", err
		}
		return t.Id, t.Url, nil
	case "UPLOAD":
		u := strings.TrimSpace(in.BackgroundUrl)
		if u == "" {
			return 0, "", xerr.Param("请上传背景图")
		}
		if err := storage.Owned(l.svcCtx.Storage, u); err != nil {
			return 0, "", xerr.Param(err.Error())
		}
		return 0, u, nil
	}
	return 0, "", xerr.Param("背景来源无效")
}

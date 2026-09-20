package logic

import (
	"context"
	"errors"
	"strings"

	"aigc-life/app/imagegen/model"
	"aigc-life/app/imagegen/rpc/internal/svc"
	"aigc-life/app/imagegen/rpc/pb"
	"aigc-life/pkg/xerr"

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
func (l *CreateLogic) Create(in *pb.CreateReq) (*pb.ImageInfo, error) {
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

	// 模板：海报必选；长图可不选（AI智能匹配）。下单时把地址快照下来，之后模板下架不影响。
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
	} else if cfg.TemplateRequired {
		return nil, xerr.Param("请选择模板")
	}

	size := strings.TrimSpace(in.Size)
	if size == "" {
		size = cfg.DefaultSize
	}
	if in.Kind == model.KindPoster {
		if _, ok := styleLabel(in.PosterStyle); !ok {
			return nil, xerr.Param("海报风格无效")
		}
		if !validSize(size) {
			return nil, xerr.Param("图片比例无效")
		}
	} else {
		size = cfg.DefaultSize // 长图比例固定
	}
	count := int64(in.ImageCount)
	if count == 0 {
		count = 1
	}
	if count < 1 || count > int64(lim.MaxImages) {
		return nil, xerr.Newf(xerr.ParamError, "生成数量为 1–%d 张", lim.MaxImages)
	}

	row := &model.AfvImageCreation{
		UserId: in.UserId, Kind: in.Kind, Content: content, MaterialUrls: nullStr(jsonList(mats)),
		TemplateAssetId: nullInt(in.TemplateId), TemplateUrl: nullStr(templateURL),
		PosterType: nullStr(in.PosterType), PosterStyle: nullStr(in.PosterStyle), Size: size, Model: cfg.Model,
		ImageCount: count, RequestId: nullStr(in.RequestId), Status: model.StatusGenerating,
	}
	res, err := l.svcCtx.Creation.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) { // 并发重复提交
			old, ferr := l.svcCtx.Creation.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toInfo(old), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	tasks := submit(l.ctx, l.svcCtx, row, cfg, posterTypeLabel(l.ctx, l.svcCtx, cfg.BizType, in.PosterType))
	if _, err := l.svcCtx.Creation.UpdateTasks(l.ctx, row.Id, dumpTasks(tasks), model.StatusGenerating, "", ""); err != nil {
		l.Errorf("save tasks %d: %v", row.Id, err)
	}
	if cur, err := l.svcCtx.Creation.FindOne(l.ctx, row.Id); err == nil {
		row = cur
	}
	return toInfo(row), nil
}

func validSize(size string) bool {
	for _, s := range posterSizes {
		if s.Code == size {
			return true
		}
	}
	return false
}

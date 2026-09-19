package logic

import (
	"context"
	"encoding/json"
	"errors"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/xerr"
)

const (
	genderFemale = "female"
	genderMale   = "male"
)

var genderLabels = map[string]string{genderFemale: "女性", genderMale: "男性"}

var validOptionTypes = map[string]bool{
	model.OptionAge: true, model.OptionSkinTone: true, model.OptionRole: true,
	model.OptionVoiceCategory: true, model.OptionPrompt: true,
}

// enabledOption 取启用中的选项；code 为空且 optional 时返回 nil。
func enabledOption(ctx context.Context, svcCtx *svc.ServiceContext, optionType, code, name string, optional bool) (*model.AfvDigitalHumanOption, error) {
	if code == "" {
		if optional {
			return nil, nil
		}
		return nil, xerr.Newf(xerr.ParamError, "请选择%s", name)
	}
	o, err := svcCtx.Option.FindOneByOptionTypeCodeDeletedId(ctx, optionType, code, 0)
	if errors.Is(err, model.ErrNotFound) || (err == nil && o.Status != 1) {
		return nil, xerr.Newf(xerr.ParamError, "%s选项无效", name)
	}
	return o, err
}

// resolvePortraitLabels 校验 AI 定制表单，返回各项展示名（拼提示词用）。
func resolvePortraitLabels(ctx context.Context, svcCtx *svc.ServiceContext, gender, age, skin, role string) (*portraitLabels, error) {
	g, ok := genderLabels[gender]
	if !ok {
		return nil, xerr.Param("请选择性别")
	}
	a, err := enabledOption(ctx, svcCtx, model.OptionAge, age, "年龄", false)
	if err != nil {
		return nil, err
	}
	s, err := enabledOption(ctx, svcCtx, model.OptionSkinTone, skin, "肤色", false)
	if err != nil {
		return nil, err
	}
	r, err := enabledOption(ctx, svcCtx, model.OptionRole, role, "职业角色", true)
	if err != nil {
		return nil, err
	}
	l := &portraitLabels{Gender: g, Age: a.Label, Skin: s.Label}
	if r != nil {
		l.Role = r.Label
	}
	return l, nil
}

// portraitSource 解析后的形象。
type portraitSource struct {
	ImageURL    string
	RefID       int64
	DefaultName string // 未填名称时的默认名
}

// resolvePortraitSource 三种形象来源：上传照片 / 公共数字人 / AI 定制的某一张。
func resolvePortraitSource(ctx context.Context, svcCtx *svc.ServiceContext, in *pb.CreateReq) (*portraitSource, error) {
	switch in.PortraitSource {
	case model.PortraitUpload:
		// 只接受我们自己签发的上传地址，防止把任意外链交给执行面
		if in.ImageUrl == "" || !svcCtx.Storage.Owns(in.ImageUrl) {
			return nil, xerr.Param("请先上传照片")
		}
		return &portraitSource{ImageURL: in.ImageUrl}, nil

	case model.PortraitPublic:
		p, err := svcCtx.Shared.FindPublicModel(ctx, publicSlot(svcCtx), in.PublicId)
		if errors.Is(err, model.ErrNotFound) {
			return nil, xerr.Param("该公共数字人已下架，请重新选择")
		}
		if err != nil {
			return nil, err
		}
		return &portraitSource{ImageURL: p.ImageUrl, RefID: p.Id, DefaultName: p.Name}, nil

	case model.PortraitAI:
		p, err := svcCtx.Portrait.FindOwned(ctx, in.PortraitId, in.UserId)
		if errors.Is(err, model.ErrNotFound) {
			return nil, xerr.NotFoundf("形象")
		}
		if err != nil {
			return nil, err
		}
		if p.Status != model.PortraitDone {
			return nil, xerr.New(xerr.NotReady, "形象还未生成完成")
		}
		var urls []string
		_ = json.Unmarshal([]byte(p.ImageUrls.String), &urls)
		if in.ImageIndex < 0 || int(in.ImageIndex) >= len(urls) {
			return nil, xerr.Param("请选择一张形象图")
		}
		return &portraitSource{ImageURL: urls[in.ImageIndex], RefID: p.Id}, nil
	}
	return nil, xerr.Param("形象来源只能是 UPLOAD、PUBLIC 或 AI")
}

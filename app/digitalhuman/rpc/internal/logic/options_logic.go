package logic

import (
	"context"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AI 定制表单选项：性别 / 年龄 / 肤色 / 职业角色
func (l *OptionsLogic) Options(in *pb.Empty) (*pb.OptionsResp, error) {
	list, err := l.svcCtx.Option.ListByType(l.ctx, "", true)
	if err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	resp := &pb.OptionsResp{
		// 性别固定两项，不进字典
		Genders: []*pb.OptionInfo{
			{OptionType: "GENDER", Code: genderFemale, Label: "女", Status: 1},
			{OptionType: "GENDER", Code: genderMale, Label: "男", Status: 1},
		},
		Models:            videoModels,
		MaxDescriptionLen: int32(lim.MaxDescriptionLen),
		MaxScriptLen:      int32(lim.MaxScriptLen),
	}
	for _, o := range list {
		switch o.OptionType {
		case model.OptionAge:
			resp.Ages = append(resp.Ages, toOption(o))
		case model.OptionSkinTone:
			resp.SkinTones = append(resp.SkinTones, toOption(o))
		case model.OptionRole:
			resp.Roles = append(resp.Roles, toOption(o))
		}
	}
	return resp, nil
}

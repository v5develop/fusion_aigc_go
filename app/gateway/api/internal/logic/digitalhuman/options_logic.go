// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package digitalhuman

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type OptionsLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AI 定制选项：性别/年龄/肤色/职业角色
func NewOptionsLogic(ctx context.Context, svcCtx *svc.ServiceContext) *OptionsLogic {
	return &OptionsLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *OptionsLogic) Options() (resp *types.DhOptionsResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.Options(l.ctx, &digitalhuman.Empty{})
	if err != nil {
		return nil, err
	}
	return &types.DhOptionsResp{
		Genders: toOptions(out.Genders), Ages: toOptions(out.Ages), SkinTones: toOptions(out.SkinTones), Roles: toOptions(out.Roles),
		Models:            toOptions(out.Models),
		MaxDescriptionLen: out.MaxDescriptionLen, MaxScriptLen: out.MaxScriptLen,
	}, nil
}

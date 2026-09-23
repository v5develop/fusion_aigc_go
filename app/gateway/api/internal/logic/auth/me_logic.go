// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package auth

import (
	"context"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type MeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 当前登录用户
func NewMeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MeLogic {
	return &MeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 当前登录用户
func (l *MeLogic) Me() (resp *types.LoginUser, err error) {
	u, err := l.svcCtx.AppUser.FindByID(l.ctx, ctxdata.UserID(l.ctx))
	if err != nil {
		l.Errorf("find app user: %v", err)
		return nil, xerr.New(xerr.ServerError, "查询失败，请重试")
	}
	if u == nil {
		return nil, xerr.New(xerr.Unauthorized, "未登录")
	}
	return &types.LoginUser{Id: u.Id, Nickname: u.Nickname.String, Avatar: u.Avatar.String, Phone: u.Phone.String}, nil
}

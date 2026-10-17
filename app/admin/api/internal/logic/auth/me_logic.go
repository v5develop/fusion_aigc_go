// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package auth

import (
	"context"
	"errors"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/pkg/adminuser"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type MeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 当前登录的管理员
func NewMeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *MeLogic {
	return &MeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *MeLogic) Me() (resp *types.AdminInfo, err error) {
	a, err := l.svcCtx.AdminUser.FindByID(l.ctx, ctxdata.AdminID(l.ctx))
	switch {
	case errors.Is(err, adminuser.ErrNoDB):
		return nil, xerr.New(xerr.NotReady, "后台登录暂未开放")
	case err != nil:
		l.Errorf("find admin: %v", err)
		return nil, xerr.New(xerr.ServerError, "查询失败，请稍后重试")
	case a == nil:
		// 账号被删或被停用，手里的令牌就该作废
		return nil, xerr.New(xerr.Unauthorized, "账号不可用，请重新登录")
	}
	info := toAdminInfo(a)
	return &info, nil
}

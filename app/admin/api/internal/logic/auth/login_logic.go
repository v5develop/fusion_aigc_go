// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package auth

import (
	"context"
	"errors"
	"strings"

	"aigc-life/app/admin/api/internal/svc"
	"aigc-life/app/admin/api/internal/types"
	"aigc-life/pkg/adminuser"
	"aigc-life/pkg/token"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type LoginLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 后台登录：用现有后台账号，连错 5 次锁 15 分钟
func NewLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogic {
	return &LoginLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *LoginLogic) Login(req *types.AdminLoginReq) (resp *types.AdminLoginResp, err error) {
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		return nil, xerr.New(xerr.ParamError, "请输入用户名和密码")
	}
	a, err := l.svcCtx.AdminUser.Login(l.ctx, username, req.Password, clientIP(req.ForwardedFor))
	switch {
	case errors.Is(err, adminuser.ErrNoDB):
		return nil, xerr.New(xerr.NotReady, "后台登录暂未开放")
	case errors.Is(err, adminuser.ErrWrong):
		// 故意不区分「账号不存在」和「密码错」，免得被拿来试探账号
		l.Infof("admin login failed: %s", username)
		return nil, xerr.New(xerr.Unauthorized, "用户名或密码不正确")
	case errors.Is(err, adminuser.ErrLocked):
		return nil, xerr.New(xerr.Forbidden, "连续登录失败次数过多，请 15 分钟后再试")
	case errors.Is(err, adminuser.ErrDisabled):
		return nil, xerr.New(xerr.Forbidden, "账号已停用，请联系管理员")
	case err != nil:
		l.Errorf("admin login %s: %v", username, err)
		return nil, xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	t, exp, err := token.SignAdmin(l.svcCtx.Config.Auth.Jwt, a.Id)
	if err != nil {
		l.Errorf("sign admin token: %v", err)
		return nil, xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	return &types.AdminLoginResp{
		Token:     t,
		ExpiresAt: exp.UnixMilli(),
		Admin:     toAdminInfo(a),
	}, nil
}

func toAdminInfo(a *adminuser.Admin) types.AdminInfo {
	roles := a.Roles
	if roles == nil {
		roles = []string{}
	}
	return types.AdminInfo{
		Id:       a.Id,
		Username: a.Username,
		RealName: a.RealName.String,
		Roles:    roles,
	}
}

// clientIP 取 X-Forwarded-For 里最前面那个（nginx 会把真实来源放在最前），只用于记录。
func clientIP(forwardedFor string) string {
	ip, _, _ := strings.Cut(forwardedFor, ",")
	return strings.TrimSpace(ip)
}

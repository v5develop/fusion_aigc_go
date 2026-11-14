// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"aigc-local-life/app/admin/api/internal/svc"
	"aigc-local-life/app/admin/api/internal/types"
	"aigc-local-life/pkg/adminuser"
	"aigc-local-life/pkg/clientip"
	"aigc-local-life/pkg/token"
	"aigc-local-life/pkg/xerr"

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
	// 共享模式：后台账号只在 Java 后台登录，令牌写入共用的会话 Redis，这里不再签发
	if l.svcCtx.Session.Enabled() {
		return nil, xerr.New(xerr.NotReady, "请用后台统一登录入口（与 Java 后台共用账号）")
	}
	username := strings.TrimSpace(req.Username)
	if username == "" || req.Password == "" {
		return nil, xerr.New(xerr.ParamError, "请输入用户名和密码")
	}
	a, err := l.svcCtx.AdminUser.Login(l.ctx, username, req.Password, clientIPFrom(l.ctx))
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

type clientIPKey struct{}

// WithRequestClientIP 把调用方 IP 放进 ctx 供 Login 记录 last_login_ip。
// 取法见 pkg/clientip：只信可信代理写入的头，X-Forwarded-For 首段可被客户端伪造，不能直接用。
func WithRequestClientIP(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, clientIPKey{}, clientip.From(r))
}

func clientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(clientIPKey{}).(string)
	return ip
}

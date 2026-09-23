// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package auth

import (
	"context"
	"errors"
	"strings"

	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/appuser"
	"aigc-life/pkg/token"
	"aigc-life/pkg/unionauth"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type LoginLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 用联盟的临时 token 换本服务的令牌：没有的用户会自动建档
func NewLoginLogic(ctx context.Context, svcCtx *svc.ServiceContext) *LoginLogic {
	return &LoginLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

// 用联盟的临时 token 换本服务的令牌：没有的用户会自动建档
func (l *LoginLogic) Login(req *types.LoginReq) (resp *types.LoginResp, err error) {
	if !l.svcCtx.Union.Enabled() {
		return nil, xerr.New(xerr.NotReady, "登录服务暂未开放")
	}
	// 临时 token 前端自己向联盟换（GET /api/v1/user/auth/token），放 body 或 Authorization 都行
	tempToken := strings.TrimSpace(req.TempToken)
	if tempToken == "" {
		tempToken = strings.TrimSpace(strings.TrimPrefix(req.Authorization, "Bearer "))
	}
	if tempToken == "" {
		return nil, xerr.New(xerr.Unauthorized, "缺少登录凭证")
	}
	// 拿临时 token 去换用户信息（用一次就失效，所以只在这里用）
	u, err := l.svcCtx.Union.Login(l.ctx, tempToken)
	if err != nil {
		if errors.Is(err, unionauth.ErrUnauthorized) {
			l.Infof("union login rejected: %v", err)
			return nil, xerr.New(xerr.Unauthorized, "登录已失效，请重新登录")
		}
		l.Errorf("union login: %v", err)
		return nil, xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	user, err := l.svcCtx.AppUser.Upsert(l.ctx, appuser.Profile{
		UnionUserID: u.UserID.String(), Nickname: u.Nickname, Avatar: u.Avatar, Phone: u.Phone,
		Gender: u.Gender, Birthday: u.Birthday, Province: u.Province, City: u.City,
		RealNameStatus: u.RealNameStatus, Roles: u.Roles,
		Status: u.Status, Enabled: appuser.Active(u.Status),
	})
	if err != nil {
		l.Errorf("upsert app user %s: %v", u.UserID, err)
		return nil, xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	if user.Status != appuser.StatusEnabled {
		return nil, xerr.New(xerr.Forbidden, "账号已被禁用")
	}
	t, exp, err := token.Sign(l.svcCtx.Config.Auth.Jwt, user.Id, u.UserID.String())
	if err != nil {
		l.Errorf("sign token: %v", err)
		return nil, xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	return &types.LoginResp{
		Token: t, ExpiresAt: exp.UnixMilli(),
		User: types.LoginUser{Id: user.Id, Nickname: user.Nickname.String, Avatar: user.Avatar.String, Phone: user.Phone.String},
	}, nil
}

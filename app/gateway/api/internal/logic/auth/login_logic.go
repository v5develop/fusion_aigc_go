// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/appuser"
	"aigc-local-life/pkg/session"
	"aigc-local-life/pkg/token"
	"aigc-local-life/pkg/unionauth"
	"aigc-local-life/pkg/xerr"

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

// 用联盟的临时 token 换本服务的令牌：没有的用户会自动建档。
// 与 Java 的 /api/v1/auth/union-token-login 行为一致：同一套错误码（见下方注释）、同一张 afv_app_user 表。
func (l *LoginLogic) Login(req *types.LoginReq) (resp *types.LoginResp, err error) {
	if !l.svcCtx.Union.Enabled() {
		return nil, xerr.New(xerr.NotReady, "登录服务暂未开放")
	}
	// 临时 token：Java 字段名 token 优先，其次旧字段 temp_token，最后 Authorization: Bearer
	tempToken := strings.TrimSpace(req.Token)
	if tempToken == "" {
		tempToken = strings.TrimSpace(req.TempToken)
	}
	if tempToken == "" {
		tempToken = strings.TrimSpace(strings.TrimPrefix(req.Authorization, "Bearer "))
	}
	// Java 是 @NotBlank 校验，码为 PARAM_INVALID（1003），文案同 Java
	if tempToken == "" {
		return nil, xerr.Param("临时Token不能为空")
	}
	// 拿临时 token 去换用户信息（用一次就失效，所以只在这里用）
	u, err := l.svcCtx.Union.Login(l.ctx, tempToken)
	if err != nil {
		var rej *unionauth.RejectedError
		if errors.As(err, &rej) {
			// 联盟明确拒绝：PARAM_INVALID，文案取联盟返回的 message（与 Java 一致）
			l.Infof("union login rejected: %v", err)
			return nil, xerr.Param(rej.Message)
		}
		if errors.Is(err, unionauth.ErrUnavailable) {
			l.Errorf("union login unavailable: %v", err)
			return nil, xerr.New(xerr.ThirdPartyError, "联盟服务暂不可用，请稍后重试")
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
	// Java：状态不是 1 即 ACCOUNT_DISABLED（2007）
	if user.Status != appuser.StatusEnabled {
		return nil, xerr.New(xerr.AccountDisabled, "用户已被禁用")
	}
	resp = &types.LoginResp{
		User:   types.LoginUser{Id: user.Id, Nickname: user.Nickname.String, Avatar: user.Avatar.String, Phone: user.Phone.String},
		UserId: user.Id, Username: user.Username, Nickname: user.Nickname.String,
	}
	if err := l.issue(resp, user); err != nil {
		return nil, err
	}
	return resp, nil
}

// issue 发令牌。配了 Session（和 AIGC 服务共用的 Redis 会话）就发它那套——
// 一个 token 两边都认，App 可以直接调 AIGC 那边现成的接口（点数、素材库、刷新令牌…）。
// 没配则回落到我们自己签的 JWT。
func (l *LoginLogic) issue(resp *types.LoginResp, user *appuser.User) error {
	if !l.svcCtx.Session.Enabled() {
		t, exp, err := token.Sign(l.svcCtx.Config.Auth.Jwt, user.Id, user.UnionUserId.String)
		if err != nil {
			l.Errorf("sign token: %v", err)
			return xerr.New(xerr.ServerError, "登录失败，请稍后重试")
		}
		resp.Token, resp.ExpiresAt = t, exp.UnixMilli()
		resp.AccessToken, resp.ExpiresIn = t, int64(time.Until(exp).Seconds())
		return nil
	}
	// 会话里要带 currentTeamId，取法和 Java 一致（建档时已经建好团队）
	teamID, err := l.svcCtx.AppUser.CurrentTeamID(l.ctx, user.Id)
	if err != nil {
		l.Errorf("current team of %d: %v", user.Id, err)
		return xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	t, err := l.svcCtx.Session.Issue(l.ctx, session.Claims{
		UserID: user.Id, Username: user.Username, CurrentTeamID: teamID,
	})
	if err != nil {
		l.Errorf("issue session for %d: %v", user.Id, err)
		return xerr.New(xerr.ServerError, "登录失败，请稍后重试")
	}
	now := time.Now()
	resp.Token = t.AccessToken
	resp.ExpiresAt = now.Add(time.Duration(t.ExpiresIn) * time.Second).UnixMilli()
	resp.RefreshToken = t.RefreshToken
	resp.RefreshExpiresAt = now.Add(time.Duration(t.RefreshExpiresIn) * time.Second).UnixMilli()
	// Java 字段（秒数，camelCase）
	resp.AccessToken = t.AccessToken
	resp.ExpiresIn = int64(t.ExpiresIn)
	resp.RefreshTokenJava = t.RefreshToken
	resp.RefreshExpiresIn = int64(t.RefreshExpiresIn)
	return nil
}

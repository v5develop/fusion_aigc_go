package svc

import (
	"context"

	"aigc-local-life/pkg/appuser"
	"aigc-local-life/pkg/fusion"

	"github.com/zeromicro/go-zero/core/logx"
)

// fusionRegistrar 把建档交给现有 Java 服务的 /api/v1/union/user-register。
//
// 它和我们写同一张表，但还会建团队、写唯一键、以及他们以后加的别的副作用——
// 交给它建，口径永远一致，我们只管建完回查。没配密钥时不启用，由我们自己写表。
type fusionRegistrar struct{ c *fusion.Client }

func (r fusionRegistrar) Enabled() bool { return r.c.Enabled() }

func (r fusionRegistrar) Register(ctx context.Context, p appuser.Profile) error {
	roles := p.Roles
	if roles == nil {
		roles = []string{} // 对方要求必填，没有就送空数组而不是 null
	}
	_, err := r.c.UserRegister(ctx, fusion.RegisterReq{
		UnionUserID: p.UnionUserID, Nickname: p.Nickname, Avatar: p.Avatar, Phone: p.Phone,
		Birthday: p.Birthday, Gender: p.Gender, Province: p.Province, City: p.City,
		RealNameStatus: p.RealNameStatus, Roles: roles, Status: p.Status,
	})
	if fusion.IsDuplicate(err) {
		// 并发撞车：人已经建好了，调用方回查就能拿到
		logx.WithContext(ctx).Infof("fusion register: 用户 %s 已存在，按成功处理", p.UnionUserID)
		return nil
	}
	return err
}

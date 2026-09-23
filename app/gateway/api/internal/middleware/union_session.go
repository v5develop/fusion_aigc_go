package middleware

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"aigc-life/pkg/appuser"
	"aigc-life/pkg/unionauth"

	"github.com/zeromicro/go-zero/core/collection"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/syncx"
)

// UnionSession 让 App 直接拿**联盟 token** 访问我们的接口：
// 第一次请求去联盟换用户信息（顺带建档/同步资料），之后按 token 缓存一段时间，
// 这样前端只需要管一个 token，不用再拿我们签发的令牌。
//
// 缓存是有代价的：用户在联盟那边登出或被冻结后，最多还能用到缓存过期。
// 所以 TTL 别放太长（默认 30 分钟）。
type UnionSession struct {
	union *unionauth.Client
	users *appuser.Model
	cache *collection.Cache
	// single 同一个 token 并发进来时只放一个去联盟，其余等结果
	single syncx.SingleFlight
}

// ErrDisabled 账号被禁用（联盟侧冻结，或本地被运营禁掉）。
var ErrDisabled = errors.New("union session: 账号已被禁用")

func NewUnionSession(union *unionauth.Client, users *appuser.Model, ttl time.Duration) *UnionSession {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	cache, err := collection.NewCache(ttl, collection.WithName("union-session"))
	if err != nil { // 只有 TTL 不合法才会出错，这里已经兜过底
		logx.Errorf("union session: 缓存初始化失败: %v", err)
		return nil
	}
	return &UnionSession{union: union, users: users, cache: cache, single: syncx.NewSingleFlight()}
}

// Enabled 没配联盟地址时不启用（登录只能走我们自己签发的令牌）。
func (s *UnionSession) Enabled() bool { return s != nil && s.union.Enabled() }

// Resolve 联盟 token → 本服务的用户 id。
func (s *UnionSession) Resolve(ctx context.Context, unionToken string) (int64, error) {
	key := tokenKey(unionToken)
	if v, ok := s.cache.Get(key); ok {
		if id, ok := v.(int64); ok {
			return id, nil
		}
	}
	v, err := s.single.Do(key, func() (any, error) {
		id, err := s.resolve(ctx, unionToken)
		if err != nil {
			return nil, err
		}
		s.cache.Set(key, id)
		return id, nil
	})
	if err != nil {
		return 0, err
	}
	id, _ := v.(int64)
	return id, nil
}

func (s *UnionSession) resolve(ctx context.Context, unionToken string) (int64, error) {
	u, err := s.union.Login(ctx, unionToken)
	if err != nil {
		return 0, err
	}
	user, err := s.users.Upsert(ctx, appuser.Profile{
		UnionUserID: u.UserID.String(), Nickname: u.Nickname, Avatar: u.Avatar, Phone: u.Phone,
		Gender: u.Gender, Birthday: u.Birthday, Province: u.Province, City: u.City,
		RealNameStatus: u.RealNameStatus, Enabled: appuser.Active(u.Status),
	})
	if err != nil {
		return 0, err
	}
	if user.Status != appuser.StatusEnabled {
		return 0, ErrDisabled
	}
	return user.Id, nil
}

// Forget 让某个联盟 token 的缓存立刻失效（退出登录时用）。
func (s *UnionSession) Forget(unionToken string) {
	if s != nil {
		s.cache.Del(tokenKey(unionToken))
	}
}

// tokenKey 缓存用 token 的哈希做 key：别把别人的凭证明文留在内存和日志里。
func tokenKey(unionToken string) string {
	sum := sha256.Sum256([]byte(unionToken))
	return hex.EncodeToString(sum[:16])
}

// Package session 发和验「和 AIGC（Java）服务同一套」的登录会话。
//
// 我们和它是平行项目、同一个库、同一张用户表（afv_app_user），所以会话也该是同一套：
// 一个 token 两边都认，App 既能调我们的接口，也能直接调它现成的接口
// （点数余额、价目、素材库、刷新令牌…），不用我们再代理或重写一遍。
//
// 它的 token **不是 JWT**，是去掉横线的 UUID（32 位 hex），本身不含信息，
// 身份全在 Redis 里。下面这套 key 和 JSON 结构必须和它**一模一样**，
// 否则它的 TokenAuthenticationFilter 认不出来（出处：《联盟用户注册登录流程与写表.md》第五节）：
//
//	fusion:token:{access}            {"userId","username","currentTeamId","issuedAt"}  默认 2 小时
//	fusion:refresh_token:{refresh}   同上                                              默认 7 天
//	fusion:token:{access}:refresh    refresh                                           7 天
//	fusion:refresh_access:{refresh}  access                                            7 天
//	fusion:user_sessions:{userId}    ZSET，member=access，score=当前秒                  7 天
//
// 刷新令牌不用我们实现：Java 那边已经有 POST /api/v1/auth/refresh，它改的是同一份 Redis，
// 刷完我们照样认。
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Conf struct {
	// RedisAddr AIGC（Java）服务在用的那台 Redis；留空则不启用，回落到我们自己的 JWT
	RedisAddr string `json:",optional"`
	RedisPass string `json:",optional"`
	// RedisDB 必须和它一致！它跑在 10 号库（容器里的 REDIS_DB=10），
	// 填错库就是「写进去了但它读不到」——最难查的那种错。
	RedisDB int `json:",default=10"`
	// AccessHours / RefreshDays 对齐它的 TOKEN_ACCESS_HOURS / TOKEN_REFRESH_DAYS
	AccessHours int `json:",default=2"`
	RefreshDays int `json:",default=7"`
	// MaxSessions 对齐它的 max-concurrent-sessions：0 = 不限。
	// 超出时淘汰最久没刷新的那个会话（连它的全部 key 一起删）
	MaxSessions int `json:",default=0"`
}

// ErrInvalid token 不存在或已过期（Redis 里查不到）。
var ErrInvalid = errors.New("session: 令牌无效或已过期")

// Claims Redis 里存的那份 JSON。字段名必须和 Java 一致，多一个少一个都可能让它解析失败。
type Claims struct {
	UserID        int64  `json:"userId"`
	Username      string `json:"username"`
	CurrentTeamID int64  `json:"currentTeamId"`
	IssuedAt      int64  `json:"issuedAt"` // epoch 秒
}

// Tokens 发出来的一对令牌，字段对应它 LoginRespVO 里的同名项。
type Tokens struct {
	AccessToken      string
	RefreshToken     string
	ExpiresIn        int64 // 秒
	RefreshExpiresIn int64 // 秒
}

type Store struct {
	conf Conf
	rds  *redis.Client
}

// New 用 go-redis 而不是 go-zero 的封装：go-zero 那个不支持选库，
// 而它的会话在 10 号库，连错库就全对不上。
func New(c Conf) *Store {
	if strings.TrimSpace(c.RedisAddr) == "" {
		return nil
	}
	return &Store{conf: c, rds: redis.NewClient(&redis.Options{
		Addr: c.RedisAddr, Password: c.RedisPass, DB: c.RedisDB,
	})}
}

func (s *Store) Enabled() bool { return s != nil && s.rds != nil }

// Redis 会话用的 Redis 客户端（未启用返回 nil）。给同库的其它轻量用途（如按用户的调用配额）复用连接池，key 自带前缀不与会话撞键。
func (s *Store) Redis() *redis.Client {
	if !s.Enabled() {
		return nil
	}
	return s.rds
}

func (s *Store) accessTTL() time.Duration {
	h := s.conf.AccessHours
	if h <= 0 {
		h = 2
	}
	return time.Duration(h) * time.Hour
}

func (s *Store) refreshTTL() time.Duration {
	d := s.conf.RefreshDays
	if d <= 0 {
		d = 7
	}
	return time.Duration(d) * 24 * time.Hour
}

// key 名集中在这里，改动时只看这一处。
func accessKey(t string) string       { return "fusion:token:" + t }
func refreshKey(t string) string      { return "fusion:refresh_token:" + t }
func accessToRefresh(t string) string { return "fusion:token:" + t + ":refresh" }
func refreshToAccess(t string) string { return "fusion:refresh_access:" + t }
func sessionsKey(userID int64) string { return "fusion:user_sessions:" + strconv.FormatInt(userID, 10) }

// Issue 发一对令牌，并按它的规则登记会话。
func (s *Store) Issue(ctx context.Context, c Claims) (*Tokens, error) {
	access, err := newToken()
	if err != nil {
		return nil, err
	}
	refresh, err := newToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	c.IssuedAt = now.Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	accessSec := int(s.accessTTL().Seconds())
	refreshSec := int(s.refreshTTL().Seconds())

	// 五个 key 一组；任何一个写失败就当发令牌失败（宁可让用户重试，也不要留个半拉会话）
	writes := []struct {
		key, val string
		ttl      int
	}{
		{accessKey(access), string(payload), accessSec},
		{refreshKey(refresh), string(payload), refreshSec},
		{accessToRefresh(access), refresh, refreshSec},
		{refreshToAccess(refresh), access, refreshSec},
	}
	for _, w := range writes {
		if err := s.rds.Set(ctx, w.key, w.val, time.Duration(w.ttl)*time.Second).Err(); err != nil {
			return nil, fmt.Errorf("session: 写 %s 失败: %w", w.key, err)
		}
	}
	if err := s.registerSession(ctx, c.UserID, access, now); err != nil {
		return nil, err
	}
	return &Tokens{
		AccessToken: access, RefreshToken: refresh,
		ExpiresIn: int64(accessSec), RefreshExpiresIn: int64(refreshSec),
	}, nil
}

// registerSession 登记会话：先剪掉过期成员，再按上限淘汰最久没刷新的。
// 允许多端同时在线——登录不踢掉旧会话，和它一致。
func (s *Store) registerSession(ctx context.Context, userID int64, access string, now time.Time) error {
	key := sessionsKey(userID)
	if err := s.rds.ZAdd(ctx, key, redis.Z{Score: float64(now.Unix()), Member: access}).Err(); err != nil {
		return fmt.Errorf("session: 登记会话失败: %w", err)
	}
	if err := s.rds.Expire(ctx, key, s.refreshTTL()).Err(); err != nil {
		return fmt.Errorf("session: 设置会话过期失败: %w", err)
	}
	// 剪掉 score 早于「现在减 refresh 周期」的：那些 access 早就过期了，留着只占地方
	cutoff := now.Add(-s.refreshTTL()).Unix()
	if err := s.rds.ZRemRangeByScore(ctx, key, "0", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return fmt.Errorf("session: 清理过期会话失败: %w", err)
	}
	if s.conf.MaxSessions <= 0 {
		return nil // 0 = 不限
	}
	n, err := s.rds.ZCard(ctx, key).Result()
	if err != nil {
		return fmt.Errorf("session: 统计会话失败: %w", err)
	}
	// 超出上限，从最久没刷新的开始淘汰（连它的其余 key 一起删）
	for extra := int(n) - s.conf.MaxSessions; extra > 0; extra-- {
		oldest, err := s.rds.ZRange(ctx, key, 0, 0).Result()
		if err != nil || len(oldest) == 0 {
			return nil
		}
		s.revoke(ctx, oldest[0])
		if err := s.rds.ZRem(ctx, key, oldest[0]).Err(); err != nil {
			return nil
		}
	}
	return nil
}

// Parse 验令牌：Redis 里有就是有效。
func (s *Store) Parse(ctx context.Context, access string) (*Claims, error) {
	if access == "" {
		return nil, ErrInvalid
	}
	raw, err := s.rds.Get(ctx, accessKey(access)).Result()
	switch {
	case errors.Is(err, redis.Nil), err == nil && raw == "":
		return nil, ErrInvalid
	case err != nil:
		return nil, fmt.Errorf("session: 读令牌失败: %w", err)
	}
	var c Claims
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("session: 令牌内容解析失败: %w", err)
	}
	if c.UserID <= 0 {
		return nil, ErrInvalid
	}
	return &c, nil
}

// Revoke 退出登录：删掉这个 access 的全部 key，并从会话集合里摘掉。
func (s *Store) Revoke(ctx context.Context, access string) error {
	c, err := s.Parse(ctx, access)
	if err != nil {
		return err
	}
	s.revoke(ctx, access)
	_ = s.rds.ZRem(ctx, sessionsKey(c.UserID), access).Err()
	return nil
}

// revoke 删一个 access 相关的 key（access 本身、它的 refresh 映射、refresh 本身）。
func (s *Store) revoke(ctx context.Context, access string) {
	refresh, _ := s.rds.Get(ctx, accessToRefresh(access)).Result()
	keys := []string{accessKey(access), accessToRefresh(access)}
	if refresh != "" {
		keys = append(keys, refreshKey(refresh), refreshToAccess(refresh))
	}
	_ = s.rds.Del(ctx, keys...).Err()
}

// newToken 去掉横线的 UUID：32 位小写 hex，和它一致。
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	// UUID v4 的版本位与变体位，保证和「去掉横线的 UUID」形状一致
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b), nil
}

// Looks 判断一个凭证像不像这套 token：32 位小写 hex。
// 用来和我们自己的 JWT（三段点分）区分开。
func Looks(s string) bool {
	if len(s) != 32 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

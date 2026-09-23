// Package token 签发与校验 App 的访问令牌（HS256 JWT）。
//
// 登录用联盟 token 换我们自己的 token：联盟 token 由 App 持有，我们不存；
// 我们的 token 里只放本服务的用户 id，过期后 App 用联盟 token 再登录一次即可，因此不做 refresh token。
package token

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

type Conf struct {
	// Secret 签名密钥，**每个环境一份**，由部署脚本生成写进配置
	Secret string `json:",optional"`
	// ExpireHours 有效期，默认 7 天
	ExpireHours int `json:",default=168"`
}

var (
	ErrInvalid    = errors.New("token: 无效的令牌")
	ErrExpired    = errors.New("token: 令牌已过期")
	ErrNoSecret   = errors.New("token: 未配置签名密钥")
	signingMethod = jwt.SigningMethodHS256
)

// Claims 令牌里带的信息：本服务的用户 id 与联盟用户 id（排查问题用）。
type Claims struct {
	UserID      int64  `json:"uid"`
	UnionUserID string `json:"union,omitempty"`
	jwt.RegisteredClaims
}

func (c Conf) ttl() time.Duration {
	if c.ExpireHours > 0 {
		return time.Duration(c.ExpireHours) * time.Hour
	}
	return 168 * time.Hour
}

// Sign 签发令牌，返回令牌与过期时间。
func Sign(c Conf, userID int64, unionUserID string) (string, time.Time, error) {
	if c.Secret == "" {
		return "", time.Time{}, ErrNoSecret
	}
	now := time.Now()
	exp := now.Add(c.ttl())
	t := jwt.NewWithClaims(signingMethod, &Claims{
		UserID: userID, UnionUserID: unionUserID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	s, err := t.SignedString([]byte(c.Secret))
	return s, exp, err
}

// Parse 校验令牌并取出内容。
func Parse(c Conf, raw string) (*Claims, error) {
	if c.Secret == "" {
		return nil, ErrNoSecret
	}
	var claims Claims
	_, err := jwt.ParseWithClaims(raw, &claims, func(t *jwt.Token) (any, error) {
		if t.Method != signingMethod {
			return nil, fmt.Errorf("token: 签名算法不对 %v", t.Header["alg"])
		}
		return []byte(c.Secret), nil
	})
	switch {
	case errors.Is(err, jwt.ErrTokenExpired):
		return nil, ErrExpired
	case err != nil:
		return nil, ErrInvalid
	case claims.UserID <= 0:
		return nil, ErrInvalid
	}
	return &claims, nil
}

package token

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v4"
)

func TestSignParse(t *testing.T) {
	c := Conf{Secret: "s3cret", ExpireHours: 1}
	raw, exp, err := Sign(c, 42, "206757090755100672")
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) > time.Hour+time.Minute || time.Until(exp) < time.Minute {
		t.Fatalf("过期时间不对: %v", exp)
	}
	claims, err := Parse(c, raw)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UserID != 42 || claims.UnionUserID != "206757090755100672" {
		t.Fatalf("内容不对: %+v", claims)
	}
}

func TestParseRejects(t *testing.T) {
	c := Conf{Secret: "s3cret", ExpireHours: 1}
	raw, _, _ := Sign(c, 42, "")

	if _, err := Parse(Conf{Secret: "another", ExpireHours: 1}, raw); !errors.Is(err, ErrInvalid) {
		t.Errorf("换密钥应该不认: %v", err)
	}
	if _, err := Parse(c, "not-a-token"); !errors.Is(err, ErrInvalid) {
		t.Errorf("乱写的应该不认: %v", err)
	}
	if _, err := Parse(Conf{}, raw); !errors.Is(err, ErrNoSecret) {
		t.Errorf("没配密钥应该报错: %v", err)
	}
	// 已过期：直接造一个过期时间在过去的令牌
	past := time.Now().Add(-2 * time.Hour)
	expired, err := jwt.NewWithClaims(signingMethod, &Claims{
		UserID: 42,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(past),
			ExpiresAt: jwt.NewNumericDate(past.Add(time.Hour)),
		},
	}).SignedString([]byte(c.Secret))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(c, expired); !errors.Is(err, ErrExpired) {
		t.Errorf("过期的应该报过期: %v", err)
	}
}

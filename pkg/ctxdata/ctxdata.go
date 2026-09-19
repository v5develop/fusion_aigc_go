// Package ctxdata 在 context 中传递当前用户 / 管理员身份。
package ctxdata

import "context"

type key int

const (
	userIDKey key = iota
	adminIDKey
)

func WithUserID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, userIDKey, id)
}

func UserID(ctx context.Context) int64 {
	v, _ := ctx.Value(userIDKey).(int64)
	return v
}

func WithAdminID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, adminIDKey, id)
}

func AdminID(ctx context.Context) int64 {
	v, _ := ctx.Value(adminIDKey).(int64)
	return v
}

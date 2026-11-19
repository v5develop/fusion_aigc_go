package health

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DB 数据库连通性检查（PingContext）。
func DB(name string, get func() (*sql.DB, error)) Check {
	return Check{Name: name, Fn: func(ctx context.Context) error {
		db, err := get()
		if err != nil {
			return fmt.Errorf("取连接失败: %w", err)
		}
		if db == nil {
			return errors.New("未初始化")
		}
		return db.PingContext(ctx)
	}}
}

// Pinger 带 Ping 的 Redis 客户端的最小接口（redis.UniversalClient 的 Ping 返回 *StatusCmd，用 RedisPing 适配）。
type Pinger func(ctx context.Context) error

// Redis Redis 连通性检查。
func Redis(name string, ping Pinger) Check {
	return Check{Name: name, Fn: func(ctx context.Context) error { return ping(ctx) }}
}

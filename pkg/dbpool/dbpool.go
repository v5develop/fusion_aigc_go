// Package dbpool 给 go-zero sqlx 背后的 *sql.DB 设置显式连接数上限。
//
// 背景：go-zero 默认 maxOpenConns=maxIdleConns=64（每个 DSN 每个进程一份，同 DSN 的 sqlx.NewMysql 共用同一个池）。
// 多个 Go 服务 + Java + AI gateway 共用一个 MySQL，按默认值最坏情形会超过 max_connections。
// 各服务在 NewServiceContext 里拿到 conn 后调用一次 Apply；同 DSN 的其他 NewMysql（如 Reclaimer）共享这个池，限额同样生效。
package dbpool

import (
	"database/sql"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

const (
	// DefaultMaxOpen 未登记服务的兜底上限。
	DefaultMaxOpen = 10
	// MaxIdleCap 空闲连接默认上限：idle = min(open, 10)。
	MaxIdleCap = 10
	// DefaultLifetime 连接最长存活。
	DefaultLifetime = 30 * time.Minute
)

// ServiceDefaults 各服务默认 MaxOpenConns（合计见 docs/api/db-pool.md）。
var ServiceDefaults = map[string]int{
	"canvas":       30,
	"studio":       30,
	"admin-api":    10,
	"gateway-api":  20,
	"copywriting":  10,
	"assetlib":     10,
	"imagegen":     10,
	"digitalhuman": 10,
	"note":         10,
}

// Config 对应 yaml 的可选 DB 块；全部不配即用服务默认值（0 = 用默认）。
type Config struct {
	MaxOpenConns           int `json:",optional"`
	MaxIdleConns           int `json:",optional"`
	ConnMaxLifetimeSeconds int `json:",optional"`
}

// Limits 实际生效的限额。
type Limits struct {
	MaxOpen  int
	MaxIdle  int
	Lifetime time.Duration
}

// Resolve 把配置与服务默认值合成实际限额：未配或 <=0 用默认；idle 不得超过 open。
func Resolve(service string, cfg Config) Limits {
	open := cfg.MaxOpenConns
	if open <= 0 {
		open = ServiceDefaults[service]
	}
	if open <= 0 {
		open = DefaultMaxOpen
	}
	idle := cfg.MaxIdleConns
	if idle <= 0 {
		idle = min(open, MaxIdleCap)
	}
	idle = min(idle, open)
	life := DefaultLifetime
	if cfg.ConnMaxLifetimeSeconds > 0 {
		life = time.Duration(cfg.ConnMaxLifetimeSeconds) * time.Second
	}
	return Limits{MaxOpen: open, MaxIdle: idle, Lifetime: life}
}

// ApplyDB 把限额设到 *sql.DB 上。
func ApplyDB(db *sql.DB, l Limits) {
	db.SetMaxOpenConns(l.MaxOpen)
	db.SetMaxIdleConns(l.MaxIdle)
	db.SetConnMaxLifetime(l.Lifetime)
	// 空闲连接不长期占着 MySQL 名额（低于 wait_timeout）
	db.SetConnMaxIdleTime(min(l.Lifetime, 10*time.Minute))
}

// Apply 对 conn 背后的共享池设限并打一条 check=db.pool_limits 日志。conn 为 nil 时（未配 DataSource）直接跳过。
func Apply(service string, conn sqlx.SqlConn, cfg Config) (Limits, error) {
	l := Resolve(service, cfg)
	if conn == nil {
		return l, nil
	}
	raw, err := conn.RawDB()
	if err != nil {
		return l, err
	}
	ApplyDB(raw, l)
	logx.Infow("db pool limits applied",
		logx.Field("check", "db.pool_limits"),
		logx.Field("service", service),
		logx.Field("maxOpenConns", l.MaxOpen),
		logx.Field("maxIdleConns", l.MaxIdle),
		logx.Field("connMaxLifetime", l.Lifetime.String()))
	return l, nil
}

// retryInterval 启动时库不可达（RawDB 会 Ping）后的重试间隔。
var retryInterval = 5 * time.Second

// MustApply 同 Apply，不拦启动。库暂不可达时 go-zero 不会缓存池，之后首次建池会是默认 64；
// 所以失败后后台重试直到设限成功，期间日志可见。
func MustApply(service string, conn sqlx.SqlConn, cfg Config) Limits {
	l, err := Apply(service, conn, cfg)
	if err == nil {
		return l
	}
	logx.Errorw("db pool limits not applied, will retry",
		logx.Field("check", "db.pool_limits"),
		logx.Field("service", service),
		logx.Field("error", err.Error()))
	go func() {
		for {
			time.Sleep(retryInterval)
			if _, err := Apply(service, conn, cfg); err == nil {
				return
			}
		}
	}()
	return l
}

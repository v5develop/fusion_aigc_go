// Package pagex 分页参数与时间字段的转换小工具。
package pagex

import (
	"database/sql"
	"time"
)

const (
	DefaultSize = 20
	MaxSize     = 100
)

// Normalize 修正页码与每页条数。
func Normalize(page, size int64) (int64, int64) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = DefaultSize
	}
	if size > MaxSize {
		size = MaxSize
	}
	return page, size
}

// Offset 返回 limit, offset。
func Offset(page, size int64) (int64, int64) {
	page, size = Normalize(page, size)
	return size, (page - 1) * size
}

// Ms 时间转 unix 毫秒，零值返回 0。
func Ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func NullMs(t sql.NullTime) int64 {
	if !t.Valid {
		return 0
	}
	return t.Time.UnixMilli()
}

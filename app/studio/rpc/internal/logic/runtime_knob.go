package logic

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// KnobTryonNewPromptPercent afv_system_config 里的商拍提示词灰度比例（0–100）。没有这一行时用 yaml TryonPrompt.NewPercent。
const KnobTryonNewPromptPercent = "studio_tryon_new_prompt_percent"

// knobTTL 运行时开关的缓存时长：改 afv_system_config 后最多这么久生效。
const knobTTL = 60 * time.Second

// intKnob 运行时整数开关：先读 afv_system_config（与 pkg/storage 读桶配置同一张表），没有该行 / 值非法时回落 yaml 默认值。
// 查库失败时沿用上一次的值（首次失败用默认值），不影响编排。
type intKnob struct {
	key      string
	def      int
	lo, hi   int
	load     func(ctx context.Context, key string) (string, bool, error) // 返回 (值, 是否有该行, 错误)
	mu       sync.Mutex
	val      int
	at       time.Time
	now      func() time.Time
	warnedAt time.Time
}

func newIntKnob(key string, def, lo, hi int, load func(ctx context.Context, key string) (string, bool, error)) *intKnob {
	return &intKnob{key: key, def: def, lo: lo, hi: hi, load: load, val: def, now: time.Now}
}

func (k *intKnob) Get(ctx context.Context) int {
	if k == nil {
		return 0
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := k.now()
	if !k.at.IsZero() && now.Sub(k.at) < knobTTL {
		return k.val
	}
	k.at = now
	if k.load == nil {
		k.val = k.def
		return k.val
	}
	raw, ok, err := k.load(ctx, k.key)
	if err != nil {
		if now.Sub(k.warnedAt) > 10*time.Minute {
			k.warnedAt = now
			logx.WithContext(ctx).Errorf("studio: 读运行时开关 %s 失败，沿用 %d: %v", k.key, k.val, err)
		}
		return k.val
	}
	if !ok {
		k.val = k.def
		return k.val
	}
	v, perr := strconv.Atoi(strings.TrimSpace(raw))
	if perr != nil || v < k.lo || v > k.hi {
		if now.Sub(k.warnedAt) > 10*time.Minute {
			k.warnedAt = now
			logx.WithContext(ctx).Errorf("studio: 运行时开关 %s=%q 不是 %d–%d 的整数，用配置默认 %d", k.key, raw, k.lo, k.hi, k.def)
		}
		k.val = k.def
		return k.val
	}
	k.val = v
	return k.val
}

// systemConfigLoader afv_system_config 单键读取（逻辑删除的行视为没有）。
func systemConfigLoader(conn sqlx.SqlConn) func(ctx context.Context, key string) (string, bool, error) {
	return func(ctx context.Context, key string) (string, bool, error) {
		var v string
		err := conn.QueryRowCtx(ctx, &v, "SELECT COALESCE(config_value, '') FROM afv_system_config WHERE config_key = ? AND deleted = 0 LIMIT 1", key)
		if errors.Is(err, sqlx.ErrNotFound) || errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		if err != nil {
			return "", false, err
		}
		return v, true, nil
	}
}

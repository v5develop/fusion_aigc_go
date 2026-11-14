package core

import (
	"context"
	"strings"
	"time"

	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

// 收藏（DESIGN 16.5、EXPERIENCE 3.9）：
//   - 严格按行业：列表必带 industry，只返回「属于所选行业 + 此刻对用户可见」的模板；一个用户的收藏不会出现在别的行业里。
//   - 模板下线 / 移出行业后列表里不显示，但记录不删（重新上架或挂回行业后自然回来）；取消收藏不要求模板仍然可见。
//   - 每人上限 500（含已下线、暂时不显示的记录——它们仍占库）。

// MaxFavorites 每人收藏上限。
const MaxFavorites = 500

// ReasonFavoriteLimit 收藏已满（4002）。
const ReasonFavoriteLimit = "FAVORITE_LIMIT"

// Favorite 收藏行（afv_plaza_favorite，Go 独写）。
type Favorite struct {
	bun.BaseModel `bun:"table:afv_plaza_favorite,alias:fav"`

	ID         int64     `bun:"id,pk,autoincrement"`
	UserID     int64     `bun:"user_id,notnull"`
	TemplateID int64     `bun:"template_id,notnull"`
	CreateTime time.Time `bun:"create_time,notnull,default:current_timestamp"`
}

// FavoriteState 收藏 / 取消收藏的结果。
type FavoriteState struct {
	TemplateID int64 `json:"templateId"`
	Favorited  bool  `json:"favorited"`
}

// FavoriteItem 收藏列表项：模板基本字段 + 收藏时间。
type FavoriteItem struct {
	TemplateItem
	FavoritedAt string `json:"favoritedAt"`
}

// FavoriteList GET /plaza/favorites 的响应。
type FavoriteList struct {
	Items []FavoriteItem `json:"items"`
	Total int            `json:"total"`
	Limit int            `json:"limit"`
}

func errFavoriteLimit() error {
	return xerr.NewReason(xerr.Conflict, ReasonFavoriteLimit, "收藏已满，取消一些再收藏")
}

// AddFavorite 收藏一个模板（幂等）。模板必须此刻对用户可见（按 id 口径）；已收藏再收藏直接成功；满 500 且是新收藏 → FAVORITE_LIMIT。
// 上限检查与插入在持有「用户锁」的同一事务里（见 favorites_lock.go），并发下不会超、不会死锁。
func (s *Service) AddFavorite(ctx context.Context, userID, templateID int64) (*FavoriteState, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	if templateID <= 0 {
		return nil, errParam("templateId 不合法")
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := c.Get(templateID, s.now()); err != nil {
		return nil, err
	}
	err = s.withUserLock(ctx, userID, func(ctx context.Context, tx bun.Tx) error {
		exists, err := tx.NewSelect().Model((*Favorite)(nil)).
			Where("user_id = ?", userID).Where("template_id = ?", templateID).Exists(ctx)
		if err != nil || exists {
			return err // 已收藏：幂等成功
		}
		n, err := tx.NewSelect().Model((*Favorite)(nil)).Where("user_id = ?", userID).Count(ctx)
		if err != nil {
			return err
		}
		if n >= MaxFavorites {
			return errFavoriteLimit()
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO afv_plaza_favorite (user_id, template_id, create_time) VALUES (?, ?, ?)",
			userID, templateID, s.now().In(cst).Format("2006-01-02 15:04:05"))
		return err
	})
	if err != nil {
		return nil, err
	}
	return &FavoriteState{TemplateID: templateID, Favorited: true}, nil
}

// RemoveFavorite 取消收藏（幂等；不要求模板仍然可见）。
func (s *Service) RemoveFavorite(ctx context.Context, userID, templateID int64) (*FavoriteState, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	if templateID <= 0 {
		return nil, errParam("templateId 不合法")
	}
	if _, err := s.db.NewDelete().Model((*Favorite)(nil)).
		Where("user_id = ?", userID).Where("template_id = ?", templateID).Exec(ctx); err != nil {
		return nil, err
	}
	return &FavoriteState{TemplateID: templateID, Favorited: false}, nil
}

// ListFavorites 当前用户在所选行业里的收藏，收藏时间倒序。industry 必填；只含属于该范围且此刻可见的模板。
func (s *Service) ListFavorites(ctx context.Context, userID int64, industry, sub string) (*FavoriteList, error) {
	if userID <= 0 {
		return nil, xerr.New(xerr.Unauthorized, "请先登录")
	}
	if strings.TrimSpace(industry) == "" {
		return nil, ErrIndustryRequired()
	}
	if s.db == nil {
		return nil, ErrNotReady()
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	sc, err := s.resolve(c, industry, sub)
	if err != nil {
		return nil, err
	}
	var rows []Favorite
	if err := s.db.NewSelect().Model(&rows).Where("fav.user_id = ?", userID).
		Order("fav.id DESC").Limit(MaxFavorites).Scan(ctx); err != nil {
		return nil, err
	}
	now := s.now()
	usage := s.usageMap(ctx)
	out := &FavoriteList{Items: []FavoriteItem{}, Limit: MaxFavorites}
	for _, f := range rows {
		rec, ok := c.InScope(sc, f.TemplateID, now)
		if !ok {
			continue // 下线 / 不在本行业：不显示，不删记录
		}
		out.Items = append(out.Items, FavoriteItem{TemplateItem: s.item(c, rec, usage, now), FavoritedAt: f.CreateTime.Format(timeLayout)})
	}
	out.Total = len(out.Items)
	return out, nil
}

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	prefTTL    = time.Hour
	maxPrefIDs = 3 // schema 多选预留；v1 UI 恒传单个
)

func prefKey(userID int64) string { return "aigc:plaza:pref:" + strconv.FormatInt(userID, 10) }

// PreferenceResult GET /plaza/industry-preference。
type PreferenceResult struct {
	IndustryIDs       []int64      `json:"industryIds"`
	PrimaryIndustryID *int64       `json:"primaryIndustryId"`
	Primary           *IndustryRef `json:"primary"`
	Stale             bool         `json:"stale"`
}

type prefRow struct {
	IDs     []int64 `json:"ids"`
	Primary int64   `json:"primary"`
}

// GetPreference 无偏好：industryIds=[]、primary=null；偏好行业已停用 / 删除：stale=true、primary=null
// （服务端不静默改写用户行，由用户重选；PUT 时才覆盖）。
func (s *Service) GetPreference(ctx context.Context, userID int64) (*PreferenceResult, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	row, err := s.loadPref(ctx, userID)
	if err != nil {
		return nil, err
	}
	out := &PreferenceResult{IndustryIDs: []int64{}}
	if len(row.IDs) == 0 {
		return out, nil
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	out.IndustryIDs = row.IDs
	var primary *IndustryNode
	for _, id := range row.IDs {
		n := c.LookupID(id)
		if n == nil {
			out.Stale = true
			continue
		}
		if id == row.Primary {
			primary = n
		}
	}
	if primary == nil {
		out.Stale = true
		return out, nil
	}
	if out.Stale { // 部分失效：保守起见整体按失效处理，让用户重选
		return out, nil
	}
	pid := primary.ID
	out.PrimaryIndustryID = &pid
	out.Primary = &IndustryRef{ID: primary.ID, Code: primary.Code, Name: primary.Name, ParentCode: primary.parentCode()}
	return out, nil
}

func (s *Service) loadPref(ctx context.Context, userID int64) (prefRow, error) {
	if s.rd != nil {
		if raw, err := s.rd.Get(ctx, prefKey(userID)).Bytes(); err == nil {
			var r prefRow
			if json.Unmarshal(raw, &r) == nil {
				return r, nil
			}
		}
	}
	var rows []model.UserIndustryPref
	if err := s.db.NewSelect().Model(&rows).Where("uip.user_id = ?", userID).
		Order("uip.is_primary DESC", "uip.sort_order ASC", "uip.id ASC").Scan(ctx); err != nil {
		return prefRow{}, err
	}
	r := prefRow{IDs: []int64{}}
	for _, p := range rows {
		r.IDs = append(r.IDs, p.IndustryID)
		if p.IsPrimary == 1 {
			r.Primary = p.IndustryID
		}
	}
	if r.Primary == 0 && len(r.IDs) > 0 {
		r.Primary = r.IDs[0]
	}
	if s.rd != nil {
		if b, err := json.Marshal(r); err == nil {
			_ = s.rd.Set(ctx, prefKey(userID), b, prefTTL).Err()
		}
	}
	return r, nil
}

// PutPreference 整体替换（先删后插，同事务）。v1 UI 恒传单个；接受 1~3 个；primary 必须在其中，缺省取第一个；
// 每个 id 必须是当前有效（启用）的行业，允许二级；不存在「清空」语义。
// 并发 PUT 撞唯一键时重试一次（先删后插的窗口里另一个事务也插了同一行），不让它变成 500。
func (s *Service) PutPreference(ctx context.Context, userID int64, ids []int64, primary int64) (*PreferenceResult, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	ids = dedupeIDs(ids)
	if len(ids) == 0 {
		return nil, errParam("industryIds 不能为空")
	}
	if len(ids) > maxPrefIDs {
		return nil, errParam(fmt.Sprintf("最多选择 %d 个行业", maxPrefIDs))
	}
	if primary == 0 {
		primary = ids[0]
	}
	in := false
	for _, id := range ids {
		in = in || id == primary
	}
	if !in {
		return nil, errParam("primaryIndustryId 必须在 industryIds 里")
	}
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if c.LookupID(id) == nil {
			return nil, ErrIndustryNotFound()
		}
	}
	var werr error
	for attempt := 0; attempt < 2; attempt++ {
		werr = bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
			if _, err := tx.NewDelete().Model((*model.UserIndustryPref)(nil)).Where("user_id = ?", userID).Exec(ctx); err != nil {
				return err
			}
			rows := make([]model.UserIndustryPref, 0, len(ids))
			for i, id := range ids {
				p := model.UserIndustryPref{UserID: userID, IndustryID: id, SortOrder: i}
				if id == primary {
					p.IsPrimary = 1
				}
				rows = append(rows, p)
			}
			_, err := tx.NewInsert().Model(&rows).Exec(ctx)
			return err
		})
		if werr == nil || !isDuplicate(werr) {
			break
		}
		logx.WithContext(ctx).Infof("plaza: 偏好 PUT 撞唯一键，重试 user=%d", userID)
	}
	if werr != nil {
		return nil, werr
	}
	if s.rd != nil {
		_ = s.rd.Del(ctx, prefKey(userID)).Err() // 写后 evict
	}
	return s.GetPreference(ctx, userID)
}

func dedupeIDs(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

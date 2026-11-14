package service

import (
	"context"
	"time"

	"aigc-local-life/app/plaza/headline/store"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

// 行业 / 模板模块（app/plaza/core）留给头条域的三个调用点：彻底删除模板时清理头条规则与抑制（同一事务）、
// 后台模板列表行的 headlineBrief、后台模板详情的 headlineStatus。由 admin-api 装配（core.SetHeadline*），
// core 不 import 本包。

// BriefScope 模板在某个范围榜上的名次（最新版本）。
type BriefScope struct {
	Scope  string `json:"scope"`
	Name   string `json:"name"`
	Rank   int    `json:"rank"`
	Source string `json:"source"`
}

// HeadlineBrief 后台模板列表行的头条摘要：是否在任一榜上（最新版本）、在哪些范围第几名、生效规则数、是否被抑制。
type HeadlineBrief struct {
	OnBoard     bool         `json:"onBoard"`
	Version     int64        `json:"version"`
	Scopes      []BriefScope `json:"scopes"`
	ActiveRules int          `json:"activeRules"`
	Suppressed  bool         `json:"suppressed"`
}

// Briefs 批量取模板的头条摘要（列表一页一次查询）。库没配返回 (nil, nil)；没有任何榜单版本时 OnBoard=false。
func (s *Service) Briefs(ctx context.Context, ids []int64) (map[int64]*HeadlineBrief, error) {
	if s.ready() != nil || len(ids) == 0 {
		return nil, nil
	}
	out := make(map[int64]*HeadlineBrief, len(ids))
	for _, id := range ids {
		out[id] = &HeadlineBrief{Scopes: []BriefScope{}}
	}
	snap, err := s.repo.LatestSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	var cat *store.Catalog
	if snap != nil {
		items, err := s.repo.ItemsOfTemplates(ctx, snap.ID, ids)
		if err != nil {
			return nil, err
		}
		if len(items) > 0 {
			cat, _ = s.Catalog(ctx) // 只用来取范围名；取不到就回码
		}
		for _, it := range items {
			b := out[it.PlazaTemplateID]
			if b == nil {
				continue
			}
			name := it.ScopeCode
			if cat != nil {
				name = scopeName(cat, it.ScopeCode)
			}
			b.OnBoard, b.Version = true, snap.ID
			b.Scopes = append(b.Scopes, BriefScope{Scope: it.ScopeCode, Name: name, Rank: it.RankNo, Source: it.Source})
		}
		for _, b := range out {
			b.Version = snap.ID
		}
	}
	rules, blocks, err := s.rulesAndBlocks(ctx, false)
	if err != nil {
		return nil, err
	}
	for _, r := range rules {
		if b := out[r.PlazaTemplateID]; b != nil {
			b.ActiveRules++
		}
	}
	for _, bl := range blocks {
		if b := out[bl.PlazaTemplateID]; b != nil {
			b.Suppressed = true
		}
	}
	return out, nil
}

// PurgeTemplate 模板被彻底删除时在同一事务里清掉它的头条数据：人工规则逻辑删除（与 Java 的 deleted 同口径，
// v1 回滚目标也不再展示）、抑制物理删除。历史榜单条目保留（版本化快照，按保留期清理）。
func (s *Service) PurgeTemplate(ctx context.Context, tx bun.IDB, tplID int64) error {
	if tx == nil || tplID <= 0 {
		return nil
	}
	if _, err := tx.NewUpdate().Table(store.TableHeadline).Set("deleted = 1").
		Where("plaza_template_id = ?", tplID).Where("deleted = 0").Exec(ctx); err != nil {
		if store.IsMissingTable(err) {
			return nil // 头条迁移（Go_V1.0.35）还没执行：没有可清的
		}
		return err
	}
	if _, err := tx.NewDelete().Table(store.TableBlock).Where("plaza_template_id = ?", tplID).Exec(ctx); err != nil && !store.IsMissingTable(err) {
		return err
	}
	s.invalidateRules()
	s.InvalidateCatalog()
	return nil
}

// StatusForAdmin 后台模板详情的 headlineStatus：模板不在目录里（草稿 / 回收站 / 库没配）返回 nil。
func (s *Service) StatusForAdmin(ctx context.Context, tplID int64) (*TemplateStatusView, error) {
	if s.ready() != nil {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	v, err := s.TemplateStatus(ctx, tplID)
	if err != nil {
		if xerr.ReasonOf(err) == ReasonHeadlineTemplateAbsent {
			return nil, nil
		}
		return nil, err
	}
	return v, nil
}

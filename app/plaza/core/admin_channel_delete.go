package core

// 渠道字典删除（逻辑删除）与引用统计；审计检索的匹配辅助。

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
)

type channelRef struct{ total, trash int }

// channelRefCounts 每个渠道 code 被多少个模板引用（含回收站里的：恢复后会重新生效）。
// channels_json 的元素是 code 字符串或 {code,width,height} 对象，解析口径与保存时校验一致（parseChannelRefs）；
// 同一模板重复写同一 code 只算一次。
func channelRefCounts(ctx context.Context, db bun.IDB) (map[string]channelRef, error) {
	var rows []struct {
		ID       int64           `bun:"id"`
		Deleted  int8            `bun:"deleted"`
		Channels json.RawMessage `bun:"channels_json"`
	}
	if err := db.NewSelect().Model((*model.Template)(nil)).Column("id", "deleted", "channels_json").
		Where("channels_json IS NOT NULL").Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := map[string]channelRef{}
	for _, r := range rows {
		refs, err := parseChannelRefs(r.Channels)
		if err != nil {
			continue // 历史脏数据：解析不了就当没引用，不让它卡住渠道管理
		}
		seen := map[string]bool{}
		for _, c := range refs {
			if c.Code == "" || seen[c.Code] {
				continue
			}
			seen[c.Code] = true
			v := out[c.Code]
			v.total++
			if r.Deleted == 1 {
				v.trash++
			}
			out[c.Code] = v
		}
	}
	return out, nil
}

// referencingTemplateIDs 引用某渠道 code 的模板 id（升序，至多 limit 个，用于拒绝文案举例）。
func referencingTemplateIDs(ctx context.Context, db bun.IDB, code string, limit int) ([]int64, error) {
	var rows []struct {
		ID       int64           `bun:"id"`
		Channels json.RawMessage `bun:"channels_json"`
	}
	if err := db.NewSelect().Model((*model.Template)(nil)).Column("id", "channels_json").
		Where("channels_json IS NOT NULL").Order("id ASC").Scan(ctx, &rows); err != nil {
		return nil, err
	}
	var ids []int64
	for _, r := range rows {
		refs, err := parseChannelRefs(r.Channels)
		if err != nil {
			continue
		}
		for _, c := range refs {
			if c.Code == code {
				ids = append(ids, r.ID)
				break
			}
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	return ids, nil
}

// AdminDeleteChannel 删除渠道（逻辑删除：deleted=1、enabled=0，code 不可复用）。
// 被任何模板引用（含回收站）时拒绝，文案带引用数与示例模板 id；没被引用才可删。
func (s *Service) AdminDeleteChannel(ctx context.Context, op Operator, id int64) error {
	if err := s.requireDB(); err != nil {
		return err
	}
	err := bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var row model.Channel
		if err := tx.NewSelect().Model(&row).Where("ch.id = ?", id).Where("ch.deleted = 0").For("UPDATE").Scan(ctx); err != nil {
			if noRows(err) {
				return xerr.NotFoundf("渠道")
			}
			return err
		}
		refs, err := channelRefCounts(ctx, tx)
		if err != nil {
			return err
		}
		if r := refs[row.Code]; r.total > 0 {
			sample, _ := referencingTemplateIDs(ctx, tx, row.Code, 5)
			parts := make([]string, 0, len(sample))
			for _, tid := range sample {
				parts = append(parts, fmt.Sprint(tid))
			}
			msg := fmt.Sprintf("渠道「%s」被 %d 个模板引用，不能删除；请先把这些模板改投其它渠道", row.Name, r.total)
			if r.trash > 0 {
				msg += fmt.Sprintf("（其中 %d 个在回收站）", r.trash)
			}
			if len(parts) > 0 {
				msg += "，例如模板 " + strings.Join(parts, "、")
			}
			return xerr.New(xerr.Conflict, msg)
		}
		before := channelSnapshot(row)
		if _, err := tx.NewUpdate().Model((*model.Channel)(nil)).Where("id = ?", id).
			Set("deleted = 1").Set("deleted_at = NOW()").Set("enabled = 0").Exec(ctx); err != nil {
			return err
		}
		return s.audit(ctx, tx, op, "CHANNEL_DELETE", ResChannel, fmt.Sprint(id), "", before, nil)
	})
	if err != nil {
		return err
	}
	s.Invalidate(ctx)
	return nil
}

// auditMatch 审计 action / resourceType 的匹配：不含 * 为精确匹配（下拉选择沿用）；含 * 为通配
// （`TEMPLATE_*` 前缀、`*_PUBLISH` 后缀、`*MEDIA*` 包含）。返回 SQL 条件与参数。
func auditMatch(col, v string) (string, []any) {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "*") {
		return col + " = ?", []any{v}
	}
	return col + " LIKE ?", []any{strings.ReplaceAll(likeEscape(v), "*", "%")}
}

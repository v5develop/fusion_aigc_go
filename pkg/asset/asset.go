// Package asset 把生成成品写进现有系统的素材库（afv_asset / afv_asset_item）。
//
// 写法沿用现有数据的惯例：
//   - 套图（多张图属于同一次生成）：每张图一行，用 source_task_id 串起来（同 productSetImages）
//   - 定制形象：一行主资产 + 多条 afv_asset_item（afv_asset_item 注释写明只给 customizedModel 等用）
//   - source_type=2 表示 AI 生成；entry_code 与素材库聚合分类（afv_asset_category_entry）对应
//   - workspace_id 填用户的个人空间（Java V1.0.181 起素材按工作空间划归属，owner_type/owner_id 已删）；
//     没有个人空间就顺手建一个，口径同 Java 的 ensurePersonal
//   - type：图片类一律写 image、视频类写 video（画廊的「图片 / 视频」tab 按这个字面量筛），
//     细分靠 entry_code；只有定制形象用 customizedModel（Java 靠它决定带不带子资产）
//
// 各服务的 Conf.Enabled 默认关闭，等数据库负责人确认 type / entry_code 取值后开启。
package asset

import (
	"context"
	"strings"

	"aigc-local-life/pkg/workspace"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

type Conf struct {
	// Enabled 是否写素材库。关闭时所有写入变成空操作。
	Enabled bool `json:",default=false"`
	// Type afv_asset.type；EntryCode afv_asset.entry_code（同时用于素材库分类归属与计费 Offer）
	Type      string `json:",optional"`
	EntryCode string `json:",optional"`
}

// Asset 一条资产。Properties 为 JSON 字符串，可为空。
type Asset struct {
	UserID       int64
	SourceTaskID string
	Type         string
	EntryCode    string
	Name         string
	CoverURL     string
	ResultURL    string
	AspectRatio  string
	AIPrompt     string
	Properties   string
}

// Item 子资产（一个主资产下的多个媒体项）。
type Item struct {
	ItemType  string // variant / original / front …
	Name      string
	MediaURL  string
	MediaType string // image / video / audio
	SortOrder int64
	AIPrompt  string
}

type Writer struct {
	conn sqlx.SqlConn
}

func NewWriter(conn sqlx.SqlConn) *Writer {
	return &Writer{conn: conn}
}

const insertAsset = "INSERT INTO afv_asset " +
	"(user_id, workspace_id, source_task_id, type, entry_code, name, cover_url, result_url, aspect_ratio, properties, ai_prompt, " +
	"source_type, status) " +
	"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 2, 1)"

func insert(ctx context.Context, s sqlx.Session, wsID int64, a Asset) (int64, error) {
	r, err := s.ExecCtx(ctx, insertAsset, a.UserID, wsID, nullStr(a.SourceTaskID), a.Type, a.EntryCode, trim(a.Name, 128),
		nullStr(a.CoverURL), nullStr(a.ResultURL), a.AspectRatio, nullStr(a.Properties), nullStr(a.AIPrompt))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// InsertSet 写一组资产（套图：每张一行，共用 source_task_id）。一组里的 user_id 相同。
func (w *Writer) InsertSet(ctx context.Context, list []Asset) error {
	if len(list) == 0 {
		return nil
	}
	return w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		wsID, err := workspace.EnsurePersonal(ctx, s, list[0].UserID, "")
		if err != nil {
			return err
		}
		for _, a := range list {
			if _, err := insert(ctx, s, wsID, a); err != nil {
				return err
			}
		}
		return nil
	})
}

// InsertSetOnce 同 InsertSet，但同一用户、同一 source_task_id 已有资产行（含已被用户删除的）时什么都不写，返回 false。
// 给会重试的后台写入用（如画布异步转存：入库后、标记完成前崩溃，重做时不能写出第二组；用户删掉的也不能被重做写回来）。
// 查重与写入在同一事务里；并发同 source_task_id 的写入方由调用方自己互斥（画布靠 outbox 认领），这里不加锁。
func (w *Writer) InsertSetOnce(ctx context.Context, list []Asset) (bool, error) {
	if len(list) == 0 {
		return false, nil
	}
	if list[0].SourceTaskID == "" {
		return true, w.InsertSet(ctx, list)
	}
	wrote := false
	err := w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		var n int64
		if err := s.QueryRowCtx(ctx, &n, "SELECT COUNT(1) FROM afv_asset WHERE source_task_id = ? AND user_id = ?",
			list[0].SourceTaskID, list[0].UserID); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		wsID, err := workspace.EnsurePersonal(ctx, s, list[0].UserID, "")
		if err != nil {
			return err
		}
		for _, a := range list {
			if _, err := insert(ctx, s, wsID, a); err != nil {
				return err
			}
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// InsertOnceByURL 写一条资产，但同一用户、同一 source_task_id、同一 result_url 已有行（含已被用户删除的）时什么都不写，返回 false。
// 给「同一任务先后有多张成品、每张各入一次」的后台写入用（如 studio 重新生成后旧成品补入素材库）：按成品地址查重，
// 不受同任务其它成品行的影响；调用方保证同一成品重做时地址不变（复制幂等键固定）。查重不加锁，并发由调用方互斥（outbox 认领）。
func (w *Writer) InsertOnceByURL(ctx context.Context, a Asset) (bool, error) {
	if a.SourceTaskID == "" || a.ResultURL == "" {
		return true, w.InsertSet(ctx, []Asset{a})
	}
	wrote := false
	err := w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		var n int64
		if err := s.QueryRowCtx(ctx, &n, "SELECT COUNT(1) FROM afv_asset WHERE source_task_id = ? AND user_id = ? AND result_url = ?",
			a.SourceTaskID, a.UserID, a.ResultURL); err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
		wsID, err := workspace.EnsurePersonal(ctx, s, a.UserID, "")
		if err != nil {
			return err
		}
		if _, err := insert(ctx, s, wsID, a); err != nil {
			return err
		}
		wrote = true
		return nil
	})
	return wrote, err
}

// InsertWithItems 写一行主资产 + 若干子资产。
func (w *Writer) InsertWithItems(ctx context.Context, a Asset, items []Item) error {
	return w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		wsID, err := workspace.EnsurePersonal(ctx, s, a.UserID, "")
		if err != nil {
			return err
		}
		r, err := insert(ctx, s, wsID, a)
		if err != nil {
			return err
		}
		id := r
		const q = "INSERT INTO afv_asset_item (asset_id, item_type, name, media_url, media_type, sort_order, ai_prompt, source_type) " +
			"VALUES (?, ?, ?, ?, ?, ?, ?, 2)"
		for _, it := range items {
			if _, err := s.ExecCtx(ctx, q, id, it.ItemType, trim(it.Name, 128), it.MediaURL, it.MediaType, it.SortOrder, nullStr(it.AIPrompt)); err != nil {
				return err
			}
		}
		return nil
	})
}

// SoftDeleteByTask 用户删除笔记 / 数字人时，同步逻辑删除素材库里对应的资产（子资产一并删）。
func (w *Writer) SoftDeleteByTask(ctx context.Context, userID int64, sourceTaskID string) error {
	if sourceTaskID == "" {
		return nil
	}
	return w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		const items = "UPDATE afv_asset_item SET deleted = 1 WHERE deleted = 0 AND asset_id IN " +
			"(SELECT id FROM (SELECT id FROM afv_asset WHERE source_task_id = ? AND user_id = ? AND deleted = 0) x)"
		if _, err := s.ExecCtx(ctx, items, sourceTaskID, userID); err != nil {
			return err
		}
		_, err := s.ExecCtx(ctx, "UPDATE afv_asset SET deleted = 1 WHERE source_task_id = ? AND user_id = ? AND deleted = 0", sourceTaskID, userID)
		return err
	})
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

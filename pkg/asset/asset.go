// Package asset 把生成成品写进现有系统的素材库（afv_asset / afv_asset_item）。
//
// 写法沿用现有数据的惯例：
//   - 套图（多张图属于同一次生成）：每张图一行，用 source_task_id 串起来（同 productSetImages）
//   - 定制形象：一行主资产 + 多条 afv_asset_item（afv_asset_item 注释写明只给 customizedModel 等用）
//   - source_type=2 表示 AI 生成；entry_code 与素材库聚合分类（afv_asset_category_entry）对应
//
// 各服务的 Conf.Enabled 默认关闭，等数据库负责人确认 type / entry_code 取值后开启。
package asset

import (
	"context"
	"strings"

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
	"(user_id, source_task_id, type, entry_code, name, cover_url, result_url, aspect_ratio, properties, ai_prompt, " +
	"source_type, owner_type, owner_id, status) " +
	"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 2, 1, ?, 1)"

func (w *Writer) insert(ctx context.Context, a Asset) (int64, error) {
	r, err := w.conn.ExecCtx(ctx, insertAsset, a.UserID, nullStr(a.SourceTaskID), a.Type, a.EntryCode, trim(a.Name, 128),
		nullStr(a.CoverURL), nullStr(a.ResultURL), a.AspectRatio, nullStr(a.Properties), nullStr(a.AIPrompt), a.UserID)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// InsertSet 写一组资产（套图：每张一行，共用 source_task_id）。
func (w *Writer) InsertSet(ctx context.Context, list []Asset) error {
	for _, a := range list {
		if _, err := w.insert(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// InsertWithItems 写一行主资产 + 若干子资产。
func (w *Writer) InsertWithItems(ctx context.Context, a Asset, items []Item) error {
	return w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		r, err := s.ExecCtx(ctx, insertAsset, a.UserID, nullStr(a.SourceTaskID), a.Type, a.EntryCode, trim(a.Name, 128),
			nullStr(a.CoverURL), nullStr(a.ResultURL), a.AspectRatio, nullStr(a.Properties), nullStr(a.AIPrompt), a.UserID)
		if err != nil {
			return err
		}
		id, err := r.LastInsertId()
		if err != nil {
			return err
		}
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

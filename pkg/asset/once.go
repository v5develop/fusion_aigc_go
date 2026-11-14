package asset

// 付费产物入库的幂等写入（BACKLOG「付费产物入库可靠性」I1 / 对账补入）。
//
//   - InsertSetOnceTx：在调用方事务里写一组资产，按 (user_id, source_task_id, 产物序号) 幂等。各服务把「置完成」与入库放进
//     同一事务（FinishWithAssets），对账扫描补入也走它，所以「置完成后、入库前崩溃」「对账与收尾并发」都只会写一组。
//   - FinishWithAssets：置完成 + 入库同事务；入库失败时退回只置完成（入库失败不拦交付、更不把已扣费任务置失败），
//     打 ERROR check=ASSET_RECONCILE_PENDING，由本服务的对账扫描（reconcile.go）补入。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"aigc-local-life/pkg/workspace"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SeqProp 资产 properties 里的产物序号键（InsertSetOnceTx 写入，0 起，= 写入列表下标）。
const SeqProp = "outputSeq"

// ErrNoSource 没有 source_task_id：无从幂等，拒绝写入（调用方应告警，见 ASSET_RECONCILE_NO_SOURCE）。
var ErrNoSource = errors.New("asset: 缺少 source_task_id，无法幂等入库")

// InsertSetOnceTx 在调用方事务 s 里写一组资产（套图：每张一行，共用 source_task_id，一组 user_id 相同），
// 按 (user_id, source_task_id, 产物序号) 幂等，list 下标即产物序号（写进 properties.outputSeq）。返回本次写入行数。
//
// 同一用户、同一 source_task_id 已有的行（含用户已删的——删掉的不会被补回来）：
//   - 有不带序号的行（本函数上线前整组写入的行、叠字版等派生行）→ 视为整组已入库，什么都不写；
//   - 否则只补缺失的序号。
//
// 查重是锁定读（SELECT … FOR UPDATE，走 idx_asset_source_task）：并发补入同一组时后到者阻塞到前者提交后再读到已写的行；
// 两个事务同时落在空区间时 InnoDB 判死锁回滚其一（返回错误，调用方下一轮重试即可）——无论哪种都不会写出两组。
func InsertSetOnceTx(ctx context.Context, s sqlx.Session, list []Asset) (int, error) {
	if len(list) == 0 {
		return 0, nil
	}
	src, uid := list[0].SourceTaskID, list[0].UserID
	if src == "" {
		return 0, ErrNoSource
	}
	var rows []struct {
		Props sql.NullString `db:"properties"`
	}
	if err := s.QueryRowsCtx(ctx, &rows, "SELECT properties FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE", src, uid); err != nil {
		return 0, err
	}
	have := map[int]bool{}
	for _, r := range rows {
		seq, ok := seqOf(r.Props)
		if !ok {
			return 0, nil // 旧写法的整组 / 派生行：视为已入库
		}
		have[seq] = true
	}
	if len(have) >= len(list) {
		return 0, nil
	}
	wsID, err := workspace.EnsurePersonal(ctx, s, uid, "")
	if err != nil {
		return 0, err
	}
	n := 0
	for i, a := range list {
		if have[i] {
			continue
		}
		a.Properties = withSeq(a.Properties, i)
		if _, err := insert(ctx, s, wsID, a); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// InsertSetOnceIdx 同 InsertSetOnceTx，自带事务（对账补入、没有任务行要一起改的场景用）。
func (w *Writer) InsertSetOnceIdx(ctx context.Context, list []Asset) (int, error) {
	n := 0
	err := w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		var err error
		n, err = InsertSetOnceTx(ctx, s, list)
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// InsertWithItemsOnceTx 在调用方事务里写一行主资产 + 子资产；同一用户、同一 source_task_id 已有行（含已删的）时什么都不写，返回 false。
// 查重同 InsertSetOnceTx（锁定读）。
func InsertWithItemsOnceTx(ctx context.Context, s sqlx.Session, a Asset, items []Item) (bool, error) {
	if a.SourceTaskID == "" {
		return false, ErrNoSource
	}
	var ids []struct {
		ID int64 `db:"id"`
	}
	if err := s.QueryRowsCtx(ctx, &ids, "SELECT id FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE", a.SourceTaskID, a.UserID); err != nil {
		return false, err
	}
	if len(ids) > 0 {
		return false, nil
	}
	wsID, err := workspace.EnsurePersonal(ctx, s, a.UserID, "")
	if err != nil {
		return false, err
	}
	a.Properties = withSeq(a.Properties, 0)
	id, err := insert(ctx, s, wsID, a)
	if err != nil {
		return false, err
	}
	const q = "INSERT INTO afv_asset_item (asset_id, item_type, name, media_url, media_type, sort_order, ai_prompt, source_type) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, 2)"
	for _, it := range items {
		if _, err := s.ExecCtx(ctx, q, id, it.ItemType, trim(it.Name, 128), it.MediaURL, it.MediaType, it.SortOrder, nullStr(it.AIPrompt)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// InsertWithItemsOnce 同 InsertWithItemsOnceTx，自带事务。
func (w *Writer) InsertWithItemsOnce(ctx context.Context, a Asset, items []Item) (bool, error) {
	wrote := false
	err := w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		var err error
		wrote, err = InsertWithItemsOnceTx(ctx, s, a, items)
		return err
	})
	return wrote && err == nil, err
}

// assetWriteErr 入库这一步的错误（FinishWithAssets 据此区分「入库失败」与「置完成失败」）。
type assetWriteErr struct{ err error }

func (e *assetWriteErr) Error() string { return e.err.Error() }
func (e *assetWriteErr) Unwrap() error { return e.err }

// FinishResult FinishWithAssets 的结局。
type FinishResult struct {
	OK       bool  // 置完成 CAS 是否抢到（false = 别处已收尾，什么都没写）
	AssetErr error // 入库失败（已退回只置完成，待对账补入）；nil = 已入库或无需入库
}

// FinishWithAssets 置完成与入库同一事务：finish 在事务里做状态 CAS（返回是否抢到），抢到后 write 在同一事务里入库
// （write 为 nil 表示不入库，如开关关闭）。二者同生同灭：不会出现「已完成却没入库」也不会出现「入了库却没完成」。
//
// 入库失败时不拦交付：回滚后不带入库再置完成一次（finish 用连接本身当 Session），FinishResult.AssetErr 带回原因，
// 调用方打 ERROR check=ASSET_RECONCILE_PENDING（LogPending），由对账扫描按「成功且已扣费但无资产行」补入。
// 返回的 error 只表示置完成本身失败（调用方按原逻辑下一轮重试）。
func (w *Writer) FinishWithAssets(ctx context.Context, finish func(ctx context.Context, s sqlx.Session) (bool, error),
	write func(ctx context.Context, s sqlx.Session) error) (FinishResult, error) {
	var res FinishResult
	err := w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		ok, err := finish(ctx, s)
		if err != nil || !ok {
			return err
		}
		res.OK = true
		if write != nil {
			if err := write(ctx, s); err != nil {
				return &assetWriteErr{err}
			}
		}
		return nil
	})
	var ae *assetWriteErr
	if !errors.As(err, &ae) {
		if err != nil {
			return FinishResult{}, err
		}
		return res, nil
	}
	ok, err := finish(ctx, w.conn)
	if err != nil {
		return FinishResult{}, err
	}
	return FinishResult{OK: ok, AssetErr: ae.err}, nil
}

// ExistingSources 这些 source_task_id 里在 afv_asset 已有行（含已删的）的那些。对账扫描用：没有行的才需要补入。
func (w *Writer) ExistingSources(ctx context.Context, ids []string) (map[string]bool, error) {
	out := map[string]bool{}
	uniq := make([]any, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	for start := 0; start < len(uniq); start += 500 {
		part := uniq[start:min(start+500, len(uniq))]
		var rows []struct {
			Src string `db:"source_task_id"`
		}
		q := "SELECT DISTINCT source_task_id FROM afv_asset WHERE source_task_id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(part)), ",") + ")"
		if err := w.conn.QueryRowsCtx(ctx, &rows, q, part...); err != nil {
			return nil, err
		}
		for _, r := range rows {
			out[r.Src] = true
		}
	}
	return out, nil
}

// seqOf 行 properties 里的产物序号；没有 / 不是对象 / 非整数为 false。
func seqOf(raw sql.NullString) (int, bool) {
	if !raw.Valid || raw.String == "" {
		return 0, false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(raw.String), &m) != nil {
		return 0, false
	}
	v, ok := m[SeqProp]
	if !ok {
		return 0, false
	}
	var n int
	if json.Unmarshal(v, &n) != nil {
		return 0, false
	}
	return n, true
}

// withSeq 把产物序号并进 properties（JSON 对象）；原值为空 / 不是对象时只写序号（原值放进 raw 不丢）。
func withSeq(props string, seq int) string {
	m := map[string]any{}
	if strings.TrimSpace(props) != "" {
		if json.Unmarshal([]byte(props), &m) != nil || m == nil {
			m = map[string]any{"raw": props}
		}
	}
	m[SeqProp] = seq
	b, _ := json.Marshal(m)
	return string(b)
}

// ReplaceSet 用户编辑后整组重写：同一事务里逻辑删除同一用户、同一 source_task_id 的旧行（子资产一并删）并写入新的一组
// （带产物序号）。删与写同生同灭，不会出现「旧的删了、新的没写」。
func (w *Writer) ReplaceSet(ctx context.Context, list []Asset) error {
	if len(list) == 0 {
		return nil
	}
	src, uid := list[0].SourceTaskID, list[0].UserID
	if src == "" {
		return ErrNoSource
	}
	return w.conn.TransactCtx(ctx, func(ctx context.Context, s sqlx.Session) error {
		const items = "UPDATE afv_asset_item SET deleted = 1 WHERE deleted = 0 AND asset_id IN " +
			"(SELECT id FROM (SELECT id FROM afv_asset WHERE source_task_id = ? AND user_id = ? AND deleted = 0) x)"
		if _, err := s.ExecCtx(ctx, items, src, uid); err != nil {
			return err
		}
		if _, err := s.ExecCtx(ctx, "UPDATE afv_asset SET deleted = 1 WHERE source_task_id = ? AND user_id = ? AND deleted = 0", src, uid); err != nil {
			return err
		}
		wsID, err := workspace.EnsurePersonal(ctx, s, uid, "")
		if err != nil {
			return err
		}
		for i, a := range list {
			a.Properties = withSeq(a.Properties, i)
			if _, err := insert(ctx, s, wsID, a); err != nil {
				return err
			}
		}
		return nil
	})
}

package taskcenter

// 旧实现（SyncMode=legacy，当前线上默认）：每个源两条 INSERT … SELECT（主表 + 正文扩展表），
// 预留单用相关子查询、窗口谓词带 OR EXISTS(预留单)。
//
// 已知问题（也是新实现 sync_new.go 的由来）：RR 下 INSERT … SELECT 对源表与 afv_billing_reservation 的读取行加共享
// next-key 锁，计费确认 / 退款的 UPDATE 在每轮同步期间会排队；OR EXISTS 让 update_time 索引失效，每轮全扫。
// 上线路径：shadow（旧写、新算比对）→ new → 删除本文件。语义规格见 SYNC_SPEC.md，改动此处前先改规格。

import (
	"context"
	"fmt"
	"strings"
)

// billingSub 取预留单的某一列；没有预留单返回 NULL。
func (s Source) billingSub(expr string) string {
	return "(SELECT " + expr + " FROM afv_billing_reservation r WHERE r.biz_type = 'LOCALLIFE' AND r.deleted = 0" +
		" AND r.biz_id = " + s.billingKeySQL() + " LIMIT 1)"
}

// billingColumns 计费字段的 SELECT 表达式（顺序与 INSERT 列一致）。
// 预留状态映射到 Java 的 billing_status：EXPIRED 是预扣过期释放，点数已回到余额，等同退款。
func (s Source) billingColumns() []string {
	if strings.TrimSpace(s.BillingRequest) == "" {
		return []string{"'NONE'", "NULL", "NULL", "NULL", "NULL"}
	}
	status := "CASE r.status WHEN 'HELD' THEN 'HELD' WHEN 'CONFIRMED' THEN 'CONFIRMED' " +
		"WHEN 'PARTIALLY_REFUNDED' THEN 'PARTIALLY_REFUNDED' WHEN 'REFUNDED' THEN 'REFUNDED' " +
		"WHEN 'CANCELLED' THEN 'REFUNDED' WHEN 'EXPIRED' THEN 'REFUNDED' ELSE 'BILLING_PENDING_REVIEW' END"
	return []string{
		"IFNULL(" + s.billingSub(status) + ", 'NONE')",
		s.billingSub("r.price_points"),
		s.billingSub("r.feature_code"),
		s.billingSub("r.model_id"),
		s.billingSub("r.reservation_no"),
	}
}

// args 与 sql() 的占位符一一对应：窗口参数，有计费关联时多一个。
func (s Source) args(secs int64) []any {
	if strings.TrimSpace(s.BillingRequest) == "" {
		return []any{secs, secs}
	}
	return []any{secs, secs, secs}
}

// where 同步的筛选条件（窗口 + 类型 + 包含条件），sql 与 textSQL 共用，占位符参数见 args。
func (s Source) where() []string {
	where := []string{"(? = 0 OR " + s.updateCol() + " >= NOW() - INTERVAL ? SECOND)"}
	if strings.TrimSpace(s.BillingRequest) != "" {
		// 预留状态会独立变化（冻结→结算→过期），源记录没动也要同步：预留单在窗口内更新过也纳入
		where[0] = "(? = 0 OR " + s.updateCol() + " >= NOW() - INTERVAL ? SECOND OR EXISTS (SELECT 1 FROM afv_billing_reservation r " +
			"WHERE r.biz_type = 'LOCALLIFE' AND r.deleted = 0 AND r.biz_id = " + s.billingKeySQL() +
			" AND r.update_time >= NOW() - INTERVAL ? SECOND))"
	}
	if s.Kind != "" {
		where = append(where, "kind = '"+s.Kind+"'")
	}
	if s.Include != "" {
		where = append(where, "("+s.Include+")")
	}
	return where
}

// textSQL 文本产物正文的 upsert，写扩展表 afv_creation_task_text（按任务编号 1:1）。
// 任务主表不放正文：列表查询是 SELECT *，大字段会拖慢所有任务的列表。只有有正文（非空）的行才写。
func (s Source) textSQL() string {
	return "INSERT INTO " + TextTable + " (task_id, content, create_time, update_time) " +
		"SELECT " + s.refSQL() + ", " + s.ResultText + ", " + s.createCol() + ", " + s.updateCol() + " FROM " + s.Table + " s " +
		"WHERE " + strings.Join(s.where(), " AND ") + " AND COALESCE(" + s.ResultText + ", '') <> '' " +
		"ON DUPLICATE KEY UPDATE content = VALUES(content), update_time = VALUES(update_time)"
}

// sql 生成主表 upsert 语句。窗口参数：只同步 update_time 在最近 N 秒内的行（N=0 同步全部）。
func (s Source) sql() string {
	where := s.where()
	cover := s.Cover
	if strings.TrimSpace(cover) == "" {
		cover = s.Result
	}
	return "INSERT INTO afv_creation_task (" + upsertColumns + ") " +
		"SELECT " + s.refSQL() + ", s.user_id, '" + s.Type + "', " + s.Status + ", '" + s.EntryCode + "', " +
		s.requestSQL() + ", " + orNull(s.ExecTaskID) + ", " + s.execParamsSQL() + ", " +
		strings.Join(s.billingColumns(), ", ") + ", " +
		orNull(s.Result) + ", " + orNull(s.Results) + ", " + orNull(cover) + ", " + orNull(s.Fail) + ", " +
		"1, s.deleted, " + s.createCol() + ", " + s.updateCol() + ", '" + OriginLocalLife + "', " + s.originRefSQL() + " FROM " + s.Table + " s WHERE " + strings.Join(where, " AND ") + " " +
		" " + upsertUpdateClause
}

// syncLegacyOne 旧路径同步一个源（主表 + 正文表各一条语句）。secs=0 为全量。
func (y *Syncer) syncLegacyOne(ctx context.Context, s Source, secs int64) error {
	err := y.exec(ctx, s.sql(), s.args(secs))
	if err == nil && s.ResultText != "" {
		err = y.exec(ctx, s.textSQL(), s.args(secs))
	}
	if err != nil {
		return fmt.Errorf("%s/%s: %w", s.Table, s.EntryCode, err)
	}
	return nil
}

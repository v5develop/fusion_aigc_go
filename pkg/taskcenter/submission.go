package taskcenter

import (
	"context"
	"database/sql"
	"errors"

	"aigc-local-life/pkg/aigc"

	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// SubmissionTable 提交快照表：每次提交执行面写一行（按执行面任务号 upsert）。
// 任务中心的 execution_params 从这里取，所以各服务的执行参数记法一致，记的就是实际发出去的入参。
const SubmissionTable = "afv_exec_submission"

// SubmissionRecorder 实现 aigc.Recorder，把提交快照写进 afv_exec_submission。
type SubmissionRecorder struct {
	conn sqlx.SqlConn
}

func NewSubmissionRecorder(conn sqlx.SqlConn) *SubmissionRecorder {
	return &SubmissionRecorder{conn: conn}
}

func (r *SubmissionRecorder) RecordSubmission(ctx context.Context, s aigc.Submission) error {
	if s.TaskID == "" {
		return errors.New("taskcenter: 提交快照缺少执行面任务号")
	}
	var routed sql.NullInt64
	if s.RoutedType != 0 {
		routed = sql.NullInt64{Int64: int64(s.RoutedType), Valid: true}
	}
	var input, errMsg sql.NullString
	if s.Input != "" {
		input = sql.NullString{String: s.Input, Valid: true}
	}
	if s.Error != "" {
		errMsg = sql.NullString{String: s.Error, Valid: true}
	}
	_, err := r.conn.ExecCtx(ctx,
		"INSERT INTO "+SubmissionTable+" (exec_task_id, task_type, routed_task_type, input, error_msg) VALUES (?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE task_type = VALUES(task_type), routed_task_type = VALUES(routed_task_type), "+
			"input = VALUES(input), error_msg = VALUES(error_msg), updated_at = NOW()",
		s.TaskID, s.TaskType, routed, input, errMsg)
	return err
}

// execParamsSQL 执行参数 = 提交时的快照，按执行任务号取。没有快照（从未提交成功或失败过）就是 NULL，
// 不再拿源表的字段拼凑，避免「记的」和「发的」对不上。
func (s Source) execParamsSQL() string {
	return "(SELECT x.input FROM " + SubmissionTable + " x WHERE x.exec_task_id = " + orNull(s.ExecTaskID) + " LIMIT 1)"
}

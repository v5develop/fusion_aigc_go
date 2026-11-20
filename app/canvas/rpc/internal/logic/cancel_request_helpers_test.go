package logic

import (
	"fmt"
	"regexp"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectClaim 派发器发请求前抢 run：排队 → 提交中（attempt+1，条件含「未请求取消」与 attempt 未变）。ok=false 模拟没抢到。
func expectClaim(mock sqlmock.Sqlmock, id int64, attempt int32, ok bool) {
	n := int64(0)
	if ok {
		n = 1
	}
	mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_canvas_node_run` SET status = 5, attempt = %d, updated_at = NOW(3) WHERE (id = %d) AND (status = 0) AND (cancel_request = 0) AND (attempt = %d)", attempt+1, id, attempt))).
		WillReturnResult(sqlmock.NewResult(0, n))
}

// expectReleaseToQueued 提交中退回排队（无条件，非 gateway 失败收尾 / 兜底）。
func expectReleaseToQueued(mock sqlmock.Sqlmock, id int64) {
	mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_canvas_node_run` SET `status` = 0, `updated_at` = NOW(3) WHERE (`id` = %d) AND (`status` = 5)", id))).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectRequestCancel 用户在排队 / 提交中取消：只打标记。
func expectRequestCancel(mock sqlmock.Sqlmock, id int64, ok bool) {
	n := int64(0)
	if ok {
		n = 1
	}
	mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_canvas_node_run` SET cancel_request = 1 WHERE (id = %d) AND (status IN (0, 5)) AND (cancel_request = 0)", id))).
		WillReturnResult(sqlmock.NewResult(0, n))
}

// expectStaleBackToQueued 提交中兜底把派发器宕机的 run 退回排队（带抢到时的 attempt）。
func expectStaleBackToQueued(mock sqlmock.Sqlmock, id int64, attempt int32) {
	mock.ExpectExec(regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_canvas_node_run` SET status = 0, updated_at = NOW(3) WHERE (id = %d) AND (status = 5) AND (attempt = %d)", id, attempt))).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

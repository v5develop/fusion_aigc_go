package logic

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/config"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/lease"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	qRunLease   = regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET `lease_owner` = '" + lease.Owner() + "', `lease_until` = DATE_ADD(NOW(3), INTERVAL 180000000 MICROSECOND)")
	qRunRelease = regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET `lease_owner` = NULL, `lease_until` = NULL, `updated_at` = `updated_at` ")
	qFreeRun    = regexp.QuoteMeta("AND ((r.lease_until IS NULL OR r.lease_until < NOW(3))) ORDER BY r.updated_at ASC")
)

// 别的实例持有租约的 run：本轮跳过，不提交、不置失败、不 touch（即便已经超时）。
func TestAdvanceQueuedSkipsLeasedRun(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.Poll.BatchSize = 100
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.status = 0) ") + qFreeRun).
		WillReturnRows(sqlmock.NewRows([]string{"id", "canvas_id", "user_id", "status", "backend", "kind", "billing_status", "created_at"}).
			AddRow(41, "cv_1", 7, 0, "gateway", "image", "held", time.Now().Add(-time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `id` FROM `afv_canvas`")).WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 41 AND").WillReturnResult(sqlmock.NewResult(0, 0))
	c.advanceQueued(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 运行中：抢到的 run 才轮询（这里 media 后端超时置失败），抢不到的跳过；只 touch 自己处理过且仍在运行中的。
func TestPollRunningOnlyLeasedRuns(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billing.New(billing.Conf{Mode: billing.ModeOff}, nil)
	c.s.Config.Poll.BatchSize = 100
	old := time.Now().Add(-time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.status = 1) ") + qFreeRun).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status", "backend", "kind", "billing_status", "created_at"}).
			AddRow(51, 7, 1, "media", "tool_crop", "none", old).
			AddRow(52, 7, 1, "media", "tool_crop", "none", old).
			AddRow(53, 7, 1, "media", "tool_crop", "none", time.Now()))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 51 AND").WillReturnResult(sqlmock.NewResult(0, 0)) // 别的实例持有
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 52 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*`error_code` = 'TIMEOUT'.* WHERE \\(`id` = 52\\) AND \\(`status` = 1\\)").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 52 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 53 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(qRunRelease + "WHERE `id` = 53 AND").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` SET updated_at = NOW(3) WHERE (id IN (53)) AND (status = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.pollRunning(context.Background())
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// RunNode 内联提交前抢不到租约（轮询器已抢到）：不提交，留给持有者。
func TestDispatchInlineSkipsWhenLeased(t *testing.T) {
	c, mock := mockCore(t)
	run := gatewayRun(61, model.RunQueued)
	mock.ExpectExec(qRunLease + ".*WHERE `id` = 61 AND").WillReturnResult(sqlmock.NewResult(0, 0))
	if err := c.dispatchInline(context.Background(), run, imageSpec, ""); err != nil || run.Status != model.RunQueued {
		t.Fatalf("抢不到租约应原样留在排队: status=%d err=%v", run.Status, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 续期：每分钟一次，只续排队 / 运行中、held 的 gateway run。
func TestMaintainRenewsHeld(t *testing.T) {
	c, mock := mockCore(t)
	c.s.Billing = billingOn(t)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (r.status IN (0, 1)) AND (r.backend = 'gateway') AND (r.billing_status = 'held')")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "billing_request_id", "billing_status"}).AddRow(5, 7, "canvas-cv_t", "held"))
	// 同一轮接着做计费镜像对账（终态 + review / held）
	mock.ExpectQuery(regexp.QuoteMeta("AND (r.billing_status IN ('review', 'held'))")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "billing_request_id", "billing_status"}))
	c.maintain(context.Background()) // Renew 打到 billingOn 的库（这里会报错，只记日志）
	c.maintain(context.Background()) // 一分钟内不再做
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckHold(t *testing.T) {
	var c config.Config
	c.Timeouts.Image, c.Timeouts.Video, c.Timeouts.Tool, c.Timeouts.I2P = 300, 900, 600, 180
	if err := c.CheckHold(); err != nil {
		t.Fatalf("默认值应通过: %v", err)
	}
	c.Billing.Hold.DefaultMinutes, c.Billing.Hold.VideoMinutes = 5, 10
	err := c.CheckHold()
	if err == nil || !strings.Contains(err.Error(), "Timeouts.Image=300s") || !strings.Contains(err.Error(), "Timeouts.Video=900s") ||
		!strings.Contains(err.Error(), "Timeouts.Tool=600s") || strings.Contains(err.Error(), "Timeouts.I2P") {
		t.Fatalf("应指出超限的各项: %v", err)
	}
}

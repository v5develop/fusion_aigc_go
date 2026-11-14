package auditdb

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestReplicateChargeBillingMismatch(t *testing.T) {
	q, mock := newMock(t)
	older := time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)FROM afv_replicate_charge c.*JOIN afv_replicate_job j.*LEFT JOIN afv_billing_reservation r.*r\.biz_type = 'LOCALLIFE'`).
		WithArgs(older, 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "job_id", "user_id", "seg_index", "stage", "request_id", "points", "charge_status",
			"job_status", "reservation_no", "reservation_status", "reservation_points", "mismatch_type"}).
			AddRow(1, 9, 7, 2, "rest", "replicate-9-02-c1", 140, 3, 5, "BR1", "HELD", 140, "refunded_but_held"))
	got, err := q.ReplicateChargeBillingMismatch(context.Background(), ReplicateChargeBillingMismatchParams{OlderThan: older, Limit: 50})
	if err != nil || len(got) != 1 || got[0].MismatchType != "refunded_but_held" || got[0].ReservationStatus.String != "HELD" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReplicateSegmentRetryAudit(t *testing.T) {
	q, mock := newMock(t)
	after := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`(?s)FROM afv_replicate_step s.*GROUP BY s\.job_id, s\.seg_index`).
		WithArgs(after, 100).
		WillReturnRows(sqlmock.NewRows([]string{"job_id", "seg_index", "auto_retries", "user_redos", "succeeded_steps", "attempts"}).
			AddRow(9, 2, 2, 1, 1, 4))
	got, err := q.ReplicateSegmentRetryAudit(context.Background(), ReplicateSegmentRetryAuditParams{CreatedAfter: after, Limit: 100})
	if err != nil || len(got) != 1 || got[0].AutoRetries != 2 || got[0].UserRedos != 1 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

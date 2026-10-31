package auditdb

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMock(t *testing.T) (*Queries, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return New(sdb), mock
}

func TestStudioCostSummaryByKind(t *testing.T) {
	q, mock := newMock(t)
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 1, 0)
	mock.ExpectQuery(`(?s)FROM afv_studio_task t.*GROUP BY t\.kind`).
		WithArgs(from, to).
		WillReturnRows(sqlmock.NewRows([]string{"kind", "task_count", "succeeded_count", "failed_count", "cost_cny_total"}).
			AddRow("tryon", 10, 8, 2, "1.250000").
			AddRow("pose", 3, 3, 0, "0.300000"))

	got, err := q.StudioCostSummaryByKind(context.Background(), StudioCostSummaryByKindParams{DateFrom: from, DateTo: to})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != AfvStudioTaskKindTryon || got[0].SucceededCount != 8 || got[0].CostCnyTotal != "1.250000" {
		t.Fatalf("结果不对：%+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStudioTasksBillingMismatch(t *testing.T) {
	q, mock := newMock(t)
	// 关键：LEFT JOIN 预留单（LOCALLIFE + biz_id 折叠规则），且带 LIMIT
	mock.ExpectQuery(`(?s)LEFT JOIN afv_billing_reservation r.*biz_type = 'LOCALLIFE'.*SHA2\(.*LIMIT \?`).
		WithArgs(int32(50)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "user_id", "kind", "task_status", "billing_status",
			"billing_request_id", "finished_at", "reservation_no", "reservation_status", "reservation_points", "mismatch_type"}).
			AddRow(7, "tyo_x", 42, "tryon", 2, 1, "studio-tyo_x", nil, "BR1", "HELD", 30, "succeeded_not_confirmed"))

	got, err := q.StudioTasksBillingMismatch(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MismatchType != "succeeded_not_confirmed" ||
		got[0].ReservationStatus != (sql.NullString{String: "HELD", Valid: true}) || got[0].FinishedAt.Valid {
		t.Fatalf("结果不对：%+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestStudioTaskCenterMissing(t *testing.T) {
	q, mock := newMock(t)
	before := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	// 关键：按 taskcenter.Ref 规则对 task_id，并取没有投影行的
	mock.ExpectQuery(`(?s)LEFT JOIN afv_creation_task c.*AiCreation_LF_.*MD5\(.*c\.id IS NULL.*LIMIT \?`).
		WithArgs(before, int32(10)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "task_id", "user_id", "kind", "status", "created_at"}).
			AddRow(9, "pse_y", 1, "pose", 1, before.Add(-time.Hour)))

	got, err := q.StudioTaskCenterMissing(context.Background(), StudioTaskCenterMissingParams{CreatedBefore: before, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != AfvStudioTaskKindPose || got[0].ID != 9 {
		t.Fatalf("结果不对：%+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

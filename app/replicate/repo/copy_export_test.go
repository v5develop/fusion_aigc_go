package repo

import (
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

func TestAcquireCopyQuota(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("INSERT IGNORE INTO `afv_replicate_copy_quota` (`user_id`, `day`, `used`) VALUES (7, '2026-11-17', 0)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(q("UPDATE `afv_replicate_copy_quota` SET used = used + 1 WHERE (user_id = 7 AND day = '2026-11-17' AND used < 20)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(q("SELECT `used` FROM `afv_replicate_copy_quota` WHERE (user_id = 7 AND day = '2026-11-17') LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(20))
	used, ok, err := r.AcquireCopyQuota(ctx, 7, "2026-11-17", 20)
	if err != nil || ok || used != 20 {
		t.Fatalf("额度满应没占到：used=%d ok=%v err=%v", used, ok, err)
	}
}

func TestReleaseFinishDeleteCopy(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_copy_quota` SET used = used - 1 WHERE (user_id = 7 AND day = '2026-11-17' AND used > 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.ReleaseCopyQuota(ctx, 7, "2026-11-17"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_copy` SET status = 1, content = '{\"a\":1}', call_id = 'c1' WHERE (id = 5 AND status = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.FinishCopy(ctx, 5, []byte(`{"a":1}`), "c1"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	mock.ExpectExec(q("DELETE FROM `afv_replicate_copy` WHERE (id = 5 AND status = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.DeleteCopyClaim(ctx, 5); err != nil {
		t.Fatal(err)
	}
}

func TestClaimCopyDupNotStale(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec("INSERT INTO `afv_replicate_copy`").WillReturnError(&mysql.MySQLError{Number: 1062})
	mock.ExpectQuery("SELECT .* FROM `afv_replicate_copy`").WillReturnRows(sqlmock.NewRows([]string{"id", "job_id", "platform", "status"}).AddRow(9, 3, "douyin", 0))
	mock.ExpectExec(q("UPDATE `afv_replicate_copy` SET updated_at = NOW(3), call_id = '' WHERE (id = 9 AND status = 0 AND updated_at < NOW(3) - INTERVAL 180000000 MICROSECOND)")).WillReturnResult(sqlmock.NewResult(0, 0))
	if ok, err := r.ClaimCopy(ctx, &model.Copy{UserID: 7, JobID: 3, Platform: "douyin"}, 3*time.Minute); err != nil || ok {
		t.Fatalf("未超时的占坑不能接手：ok=%v err=%v", ok, err)
	}
}

func TestExportClaimFinishDelete(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec("INSERT INTO `afv_replicate_export`").WillReturnResult(sqlmock.NewResult(4, 1))
	e := &model.Export{UserID: 7, JobID: 3, Ratio: "9:16", Mode: "pad", SrcRid: "r"}
	if ok, err := r.ClaimExport(ctx, e, time.Minute); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_export` SET status = 1, video_rid = 'v', version = 10 WHERE (id = 4 AND status = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.FinishExport(ctx, 4, "v", 10); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	mock.ExpectExec(q("DELETE FROM `afv_replicate_export` WHERE (id = 4 AND status = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.DeleteExportClaim(ctx, 4); err != nil {
		t.Fatal(err)
	}
}

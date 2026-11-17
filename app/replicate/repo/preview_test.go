package repo

import (
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

// 额度：先 INSERT IGNORE 建当日行，再「used < total 才 +1」的单条条件更新；更新 0 行 = 额度满。
func TestAcquirePreviewQuota(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("INSERT IGNORE INTO `afv_replicate_preview_quota` (`user_id`, `day`, `used`) VALUES (7, '2026-11-17', 0)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(q("UPDATE `afv_replicate_preview_quota` SET used = used + 1 WHERE (user_id = 7 AND day = '2026-11-17' AND used < 3)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(q("SELECT `used` FROM `afv_replicate_preview_quota` WHERE (user_id = 7 AND day = '2026-11-17') LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(3))
	used, ok, err := r.AcquirePreviewQuota(ctx, 7, "2026-11-17", 3)
	if err != nil || ok || used != 3 {
		t.Fatalf("额度满应没占到：used=%d ok=%v err=%v", used, ok, err)
	}
}

func TestReleaseAndFinishPreview(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_replicate_preview_quota` SET used = used - 1 WHERE (user_id = 7 AND day = '2026-11-17' AND used > 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.ReleasePreviewQuota(ctx, 7, "2026-11-17"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(q("UPDATE `afv_replicate_preview` SET status = 1, image_rid = 'rid', call_id = 'call_1' WHERE (id = 5 AND status = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.FinishPreview(ctx, 5, "rid", "call_1"); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	mock.ExpectExec(q("DELETE FROM `afv_replicate_preview` WHERE (id = 5 AND status = 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := r.DeletePreviewClaim(ctx, 5); err != nil {
		t.Fatal(err)
	}
}

// 占坑撞唯一键：已出图 / 占坑未超时不能接手；超时的出图中占坑 CAS 接手。
func TestClaimPreviewDup(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec("INSERT INTO `afv_replicate_preview`").WillReturnError(&mysql.MySQLError{Number: 1062})
	mock.ExpectQuery("SELECT .* FROM `afv_replicate_preview`").WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "plan_hash", "status"}).AddRow(9, 7, "h", 0))
	mock.ExpectExec(q("UPDATE `afv_replicate_preview` SET updated_at = NOW(3), call_id = '' WHERE (id = 9 AND status = 0 AND updated_at < NOW(3) - INTERVAL 180000000 MICROSECOND)")).WillReturnResult(sqlmock.NewResult(0, 0))
	p := &model.Preview{UserID: 7, PlanHash: "h"}
	if ok, err := r.ClaimPreview(ctx, p, 3*time.Minute); err != nil || ok {
		t.Fatalf("未超时的占坑不能接手：ok=%v err=%v", ok, err)
	}
}

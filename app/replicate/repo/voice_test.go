package repo

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// 占用：流水 + 当日行 + 条件 +1 在同一事务；占到后读回已用数。
func TestAcquireVoiceQuota(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(q("INSERT IGNORE INTO `afv_replicate_voice_use` (`job_id`, `user_id`, `day`, `status`) VALUES (9, 7, '2026-11-17', 1)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("INSERT IGNORE INTO `afv_replicate_voice_quota` (`user_id`, `day`, `used`) VALUES (7, '2026-11-17', 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("UPDATE `afv_replicate_voice_quota` SET used = used + 1 WHERE (user_id = 7 AND day = '2026-11-17' AND used < 10)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(q("SELECT `used` FROM `afv_replicate_voice_quota` WHERE (user_id = 7 AND day = '2026-11-17') LIMIT 1")).WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(4))
	used, ok, err := r.AcquireVoiceQuota(ctx, 7, 9, "2026-11-17", 10)
	if err != nil || !ok || used != 4 {
		t.Fatalf("used=%d ok=%v err=%v", used, ok, err)
	}
}

// 额度满：条件更新 0 行 → 整个事务回滚（流水也撤销），没占到。
func TestAcquireVoiceQuotaFullRollsBack(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT IGNORE INTO `afv_replicate_voice_use`").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT IGNORE INTO `afv_replicate_voice_quota`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(q("UPDATE `afv_replicate_voice_quota` SET used = used + 1 WHERE (user_id = 7 AND day = '2026-11-17' AND used < 10)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()
	mock.ExpectQuery("SELECT `used` FROM `afv_replicate_voice_quota`").WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(10))
	used, ok, err := r.AcquireVoiceQuota(ctx, 7, 9, "2026-11-17", 10)
	if err != nil || ok || used != 10 {
		t.Fatalf("额度满应没占到：used=%d ok=%v err=%v", used, ok, err)
	}
}

// 重放：流水已存在（INSERT IGNORE 0 行）→ 算占到，不再动计数。
func TestAcquireVoiceQuotaReplayDoesNotCountTwice(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec("INSERT IGNORE INTO `afv_replicate_voice_use`").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	mock.ExpectQuery("SELECT `used` FROM `afv_replicate_voice_quota`").WillReturnRows(sqlmock.NewRows([]string{"used"}).AddRow(4))
	if _, ok, err := r.AcquireVoiceQuota(ctx, 7, 9, "2026-11-17", 10); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

// 退回：「占用中 → 已退回」条件更新抢到才扣回占用当天的计数；已退过 / 没占过什么都不动。
func TestReleaseVoiceQuota(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectExec(q("UPDATE `afv_replicate_voice_use` SET status = 2 WHERE (job_id = 9 AND status = 1)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("SELECT .* FROM `afv_replicate_voice_use`").WillReturnRows(sqlmock.NewRows([]string{"job_id", "user_id", "day", "status"}).AddRow(9, 7, "2026-11-16", 2))
	mock.ExpectExec(q("UPDATE `afv_replicate_voice_quota` SET used = used - 1 WHERE (user_id = 7 AND day = '2026-11-16' AND used > 0)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if ok, err := r.ReleaseVoiceQuota(ctx, 9); err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(q("UPDATE `afv_replicate_voice_use` SET status = 2 WHERE (job_id = 9 AND status = 1)")).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()
	if ok, err := r.ReleaseVoiceQuota(ctx, 9); err != nil || ok {
		t.Fatalf("已退过不应再退：ok=%v err=%v", ok, err)
	}
}

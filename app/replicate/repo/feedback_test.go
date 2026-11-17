package repo

import (
	"testing"

	"aigc-local-life/app/replicate/model"

	"github.com/DATA-DOG/go-sqlmock"
)

// 反馈覆盖写：新插入影响 1 行 = 第一次；ON DUPLICATE 改写影响 2 行 = 覆盖；值没变影响 0 行也不算第一次。
func TestUpsertFeedbackCreatedFlag(t *testing.T) {
	for affected, want := range map[int64]bool{1: true, 2: false, 0: false} {
		r, mock := newMock(t)
		mock.ExpectExec(`INSERT INTO .afv_replicate_feedback. .*VALUES \(7, 3, 2, 'down', CAST\('\["seam_jump"\]' AS JSON\), '跳了', 'smooth'\) ON DUPLICATE KEY UPDATE`).
			WillReturnResult(sqlmock.NewResult(0, affected))
		created, err := r.UpsertFeedback(ctx, &model.Feedback{UserID: 7, JobID: 3, SegIndex: 2, Rating: "down", Reasons: []byte(`["seam_jump"]`), Note: "跳了", Continuity: "smooth"})
		if err != nil || created != want {
			t.Fatalf("影响 %d 行：created=%v err=%v", affected, created, err)
		}
	}
	// up 没有原因：写 JSON null
	r, mock := newMock(t)
	mock.ExpectExec(`VALUES \(7, 3, 0, 'up', CAST\('null' AS JSON\), '', 'fast'\)`).WillReturnResult(sqlmock.NewResult(1, 1))
	if _, err := r.UpsertFeedback(ctx, &model.Feedback{UserID: 7, JobID: 3, Rating: "up", Continuity: "fast"}); err != nil {
		t.Fatal(err)
	}
}

package asset

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 同一用户、同一 source_task_id 已有资产行（含已删的）：不再写，返回 false。
func TestInsertSetOnceSkipsExisting(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(1) FROM afv_asset WHERE source_task_id = ? AND user_id = ?")).
		WithArgs("cv_t", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(2))
	mock.ExpectCommit()
	w := NewWriter(sqlx.NewSqlConnFromDB(db))
	wrote, err := w.InsertSetOnce(context.Background(), []Asset{{UserID: 7, SourceTaskID: "cv_t", Type: "image", ResultURL: "https://cdn/a"}})
	if err != nil || wrote {
		t.Fatalf("已有资产行时不应再写: wrote=%v err=%v", wrote, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

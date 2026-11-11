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

// 按成品地址查重：同一任务已有别的成品行不挡，同地址已有行（含已删的）不再写。
func TestInsertOnceByURL(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	q := regexp.QuoteMeta("SELECT COUNT(1) FROM afv_asset WHERE source_task_id = ? AND user_id = ? AND result_url = ?")
	mock.ExpectBegin()
	mock.ExpectQuery(q).WithArgs("simg_1", int64(7), "https://cdn/old").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectCommit()
	mock.ExpectBegin()
	mock.ExpectQuery(q).WithArgs("simg_1", int64(7), "https://cdn/new").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO afv_asset")).WillReturnResult(sqlmock.NewResult(12, 1))
	mock.ExpectCommit()
	w := NewWriter(sqlx.NewSqlConnFromDB(db))
	if wrote, err := w.InsertOnceByURL(context.Background(), Asset{UserID: 7, SourceTaskID: "simg_1", Type: "image", ResultURL: "https://cdn/old"}); err != nil || wrote {
		t.Fatalf("同地址已有行不应再写: wrote=%v err=%v", wrote, err)
	}
	if wrote, err := w.InsertOnceByURL(context.Background(), Asset{UserID: 7, SourceTaskID: "simg_1", Type: "image", ResultURL: "https://cdn/new"}); err != nil || !wrote {
		t.Fatalf("新地址应写入: wrote=%v err=%v", wrote, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

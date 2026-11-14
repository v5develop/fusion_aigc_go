package asset

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

// 来源是水印图（免费档：用户看到的是 watermark_url）→ 叠字版也要把 watermark_url 写成叠字版地址，可见性与无字版一致。
func TestSaveTextOverlayVariantViaWatermark(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset WHERE user_id = ? AND (result_url = ? OR watermark_url = ?)")).
		WithArgs(int64(7), "https://cdn/wm.png", "https://cdn/wm.png").
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_task_id", "type", "entry_code", "name", "aspect_ratio", "ai_prompt", "sys_tags", "result_url", "watermark_url"}).
			AddRow(5, "t1", "image", "e1", "图", "1:1", nil, nil, "https://cdn/orig.png", "https://cdn/wm.png"))
	mock.ExpectExec("UPDATE afv_asset SET properties").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(1) FROM afv_asset WHERE user_id = ? AND result_url = ?")).WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO afv_asset")).WillReturnResult(sqlmock.NewResult(12, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE afv_asset SET watermark_url = ? WHERE id = ?")).WithArgs("https://cdn/ov.png", int64(12)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	saved, err := NewWriter(sqlx.NewSqlConnFromDB(db)).SaveTextOverlayVariant(context.Background(), 7, "https://cdn/wm.png", "https://cdn/ov.png", "")
	if err != nil || !saved {
		t.Fatalf("saved=%v err=%v", saved, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

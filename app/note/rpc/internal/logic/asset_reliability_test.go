package logic

// 付费产物入库可靠性（I1 置完成与入库同事务 + 对账补入 + 编辑整组重写同事务）。

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/config"
	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/pkg/asset"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func noteSvc(t *testing.T) (*svc.ServiceContext, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn := sqlx.NewSqlConnFromDB(db)
	var c config.Config
	c.Asset = asset.Conf{Enabled: true, Type: "image", EntryCode: "imageTextNote"}
	c.AssetReconcile = asset.ReconcileConf{Enabled: true}
	return &svc.ServiceContext{Config: c, Note: model.NewAfvImageTextNoteModel(conn), Asset: asset.NewWriter(conn)}, mock
}

var (
	qFinishNote = regexp.QuoteMeta("UPDATE `afv_image_text_note` SET `status` = ?, `image_urls` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?")
	qSeqLock    = regexp.QuoteMeta("SELECT properties FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE")
)

func doneNote() *model.AfvImageTextNote {
	return &model.AfvImageTextNote{Id: 3, UserId: 7, Title: "探店", NoteType: "explore", GenTaskId: sql.NullString{String: "n_g", Valid: true},
		ImageUrls: sql.NullString{String: `["https://cdn/a","https://cdn/b"]`, Valid: true}}
}

func TestNoteFinishWithAssetsSameTxAndFallback(t *testing.T) {
	s, mock := noteSvc(t)
	n := doneNote()
	mock.ExpectBegin()
	mock.ExpectExec(qFinishNote).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeqLock).WithArgs("n_g", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO afv_asset").WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()
	if ok, err := finishWithAssets(context.Background(), s, n, []string{"https://cdn/a", "https://cdn/b"}); err != nil || !ok {
		t.Fatalf("应置完成并入库: %v %v", ok, err)
	}
	// 入库失败：回滚后只置完成
	mock.ExpectBegin()
	mock.ExpectExec(qFinishNote).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeqLock).WillReturnError(errors.New("Error 1205: Lock wait timeout"))
	mock.ExpectRollback()
	mock.ExpectExec(qFinishNote).WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := finishWithAssets(context.Background(), s, n, []string{"https://cdn/a"}); err != nil || !ok {
		t.Fatalf("入库失败也应置完成: %v %v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 对账补入（Fix）：按笔记当前 image_urls 补一组；已有行（含崩溃前已写的）不再写。
func TestNoteReconcileFix(t *testing.T) {
	s, mock := noteSvc(t)
	rec := newAssetReconciler(s)
	n := doneNote()
	mock.ExpectBegin()
	mock.ExpectQuery(qSeqLock).WithArgs("n_g", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}).AddRow(`{"outputSeq":0}`))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset").WithArgs(int64(7), int64(3), "n_g", "image", "imageTextNote", "探店", "https://cdn/b", "https://cdn/b",
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()
	if wrote, err := rec.Fix(context.Background(), asset.ReconcileItem{ID: n.Id, UserID: 7, SourceTaskID: "n_g", Ref: n}); err != nil || !wrote {
		t.Fatalf("应只补序号 1: %v %v", wrote, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

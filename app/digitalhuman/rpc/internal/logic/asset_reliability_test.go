package logic

// 付费产物入库可靠性（I1 置完成与入库同事务 + 对账补入）。

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/config"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/pkg/asset"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func dhSvc(t *testing.T) (*svc.ServiceContext, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn := sqlx.NewSqlConnFromDB(db)
	var c config.Config
	c.Asset.Enabled = true
	c.Asset.Video.Type, c.Asset.Video.EntryCode = "video", "digitalHuman"
	c.Asset.Portrait.Type, c.Asset.Portrait.EntryCode = "customizedModel", "digitalHumanPortrait"
	c.AssetReconcile = asset.ReconcileConf{Enabled: true}
	return &svc.ServiceContext{Config: c, Human: model.NewAfvDigitalHumanModel(conn), Portrait: model.NewAfvDigitalHumanPortraitModel(conn),
		Asset: asset.NewWriter(conn)}, mock
}

var (
	qFinishDH = regexp.QuoteMeta("UPDATE `afv_digital_human` SET `status` = ?, `video_url` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?")
	qFinishPT = regexp.QuoteMeta("UPDATE `afv_digital_human_portrait` SET `status` = ?, `image_urls` = ?, `fail_reason` = ? WHERE `id` = ? AND `status` = ?")
	qSeqLock  = regexp.QuoteMeta("SELECT properties FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE")
)

func human() *model.AfvDigitalHuman {
	return &model.AfvDigitalHuman{Id: 4, UserId: 7, Name: "口播", VideoTaskId: sql.NullString{String: "dh_v", Valid: true},
		VideoUrl: sql.NullString{String: "https://cdn/v.mp4", Valid: true}}
}

func TestDigitalHumanVideoSameTxAndFallback(t *testing.T) {
	s, mock := dhSvc(t)
	h := human()
	mock.ExpectBegin()
	mock.ExpectExec(qFinishDH).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeqLock).WithArgs("dh_v", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset").WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if err := finishVideoWithAsset(context.Background(), s, h, "https://cdn/v.mp4"); err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(qFinishDH).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeqLock).WillReturnError(errors.New("boom"))
	mock.ExpectRollback()
	mock.ExpectExec(qFinishDH).WillReturnResult(sqlmock.NewResult(0, 1))
	if err := finishVideoWithAsset(context.Background(), s, h, "https://cdn/v.mp4"); err != nil {
		t.Fatalf("入库失败不应影响置完成: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 形象（不扣费）：主资产 + 子资产与置完成同事务；同 source_task_id 已有行不再写。
func TestDigitalHumanPortraitSameTxOnce(t *testing.T) {
	s, mock := dhSvc(t)
	p := &model.AfvDigitalHumanPortrait{Id: 5, UserId: 7, TaskId: sql.NullString{String: "pt_1", Valid: true}}
	mock.ExpectBegin()
	mock.ExpectExec(qFinishPT).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE")).WithArgs("pt_1", int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset ").WillReturnResult(sqlmock.NewResult(10, 1))
	mock.ExpectExec("INSERT INTO afv_asset_item").WithArgs(int64(10), "variant", "形象 1", "https://cdn/p0", "image", int64(0), nil).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO afv_asset_item").WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()
	finishPortraitWithAsset(context.Background(), s, p, []string{"https://cdn/p0", "https://cdn/p1"})
	mock.ExpectBegin()
	mock.ExpectExec(qFinishPT).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id FROM afv_asset WHERE source_task_id = ?")).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(10))
	mock.ExpectCommit()
	finishPortraitWithAsset(context.Background(), s, p, []string{"https://cdn/p0"})
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 对账补入（Fix）：口播视频按 video_url 补一行；已有带序号的行不再写。
func TestDigitalHumanReconcileFix(t *testing.T) {
	s, mock := dhSvc(t)
	rec := newAssetReconciler(s)
	h := human()
	mock.ExpectBegin()
	mock.ExpectQuery(qSeqLock).WithArgs("dh_v", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset").WithArgs(int64(7), int64(3), "dh_v", "video", "digitalHuman", "口播", sqlmock.AnyArg(), "https://cdn/v.mp4",
		sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	if wrote, err := rec.Fix(context.Background(), asset.ReconcileItem{ID: h.Id, UserID: 7, SourceTaskID: "dh_v", Ref: h}); err != nil || !wrote {
		t.Fatalf("应补入: %v %v", wrote, err)
	}
	mock.ExpectBegin()
	mock.ExpectQuery(qSeqLock).WillReturnRows(sqlmock.NewRows([]string{"properties"}).AddRow(`{"outputSeq":0}`))
	mock.ExpectCommit()
	if wrote, err := rec.Fix(context.Background(), asset.ReconcileItem{ID: h.Id, UserID: 7, SourceTaskID: "dh_v", Ref: h}); err != nil || wrote {
		t.Fatalf("已入过不应再写: %v %v", wrote, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

package logic

// 付费产物入库可靠性（I1 置完成与入库同事务 + 对账补入）。

import (
	"context"
	"database/sql/driver"
	"errors"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/pkg/asset"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func assetSvc(t *testing.T) (*svc.ServiceContext, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	conn := sqlx.NewSqlConnFromDB(db)
	var c config.Config
	c.Asset.Enabled = true
	c.Poster.AssetType, c.Poster.AssetEntryCode = "image", "poster"
	c.AssetReconcile = asset.ReconcileConf{Enabled: true, WindowHours: 168, GraceSeconds: 600, Batch: 10}
	return &svc.ServiceContext{Config: c, Creation: model.NewLocalLifeCreationModel(conn), Video: model.NewAfvVideoCreationModel(conn),
		Asset: asset.NewWriter(conn)}, mock
}

var (
	qFinishLL = regexp.QuoteMeta("UPDATE `afv_local_life_creation` SET `tasks` = ?, `status` = ?")
	qSeqLock  = regexp.QuoteMeta("SELECT properties FROM afv_asset WHERE source_task_id = ? AND user_id = ? FOR UPDATE")
)

func poster() *model.LocalLifeCreation {
	return &model.LocalLifeCreation{Id: 1, UserId: 7, Kind: model.KindPoster, Content: "夏日", Size: "3:4"}
}

// 置完成与入库同一事务：UPDATE（CAS）→ 锁定读查重 → 两张图各一行（带产物序号）→ 提交。
func TestFinishWithAssetsSameTx(t *testing.T) {
	s, mock := assetSvc(t)
	mock.ExpectBegin()
	mock.ExpectExec(qFinishLL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeqLock).WithArgs("g1", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset").WithArgs(int64(7), int64(3), "g1", "image", "poster", sqlmock.AnyArg(), "https://cdn/a", "https://cdn/a",
		"3:4", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO afv_asset").WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()
	c := poster()
	ok, err := finishWithAssets(context.Background(), s, c.Id, c.UserId, assetsOf(s, c, "g1", []string{"https://cdn/a", "https://cdn/b"}),
		func(ctx context.Context, sess sqlx.Session) (bool, error) {
			return s.Creation.WithSession(sess).UpdateTasks(ctx, c.Id, "[]", model.StatusDone, `["a"]`, "")
		})
	if err != nil || !ok {
		t.Fatalf("应置完成: %v %v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 入库失败：事务回滚后不带入库再置完成一次（照常交付、不置失败），留给对账补入。
func TestFinishWithAssetsFallbackOnAssetError(t *testing.T) {
	s, mock := assetSvc(t)
	mock.ExpectBegin()
	mock.ExpectExec(qFinishLL).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(qSeqLock).WillReturnError(errors.New("Error 1213: Deadlock found"))
	mock.ExpectRollback()
	mock.ExpectExec(qFinishLL).WillReturnResult(sqlmock.NewResult(0, 1))
	c := poster()
	ok, err := finishWithAssets(context.Background(), s, c.Id, c.UserId, assetsOf(s, c, "g1", []string{"https://cdn/a"}),
		func(ctx context.Context, sess sqlx.Session) (bool, error) {
			return s.Creation.WithSession(sess).UpdateTasks(ctx, c.Id, "[]", model.StatusDone, `["a"]`, "")
		})
	if err != nil || !ok {
		t.Fatalf("入库失败也应置完成: %v %v", ok, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 对账：已完成、素材库缺行的海报补入（source_task_id = 第一个任务号，产物取 image_urls）；已有行的跳过。
func TestImagegenReconcile(t *testing.T) {
	s, mock := assetSvc(t)
	recs := newAssetReconcilers(s)
	now := time.Now()
	recs[0].Now = func() time.Time { return now }
	cols := []string{"id", "user_id", "kind", "content", "copywriting_id", "material_urls", "template_asset_id", "template_url", "input_image_url",
		"input_video_url", "input_bg_url", "poster_type", "poster_style", "size", "duration", "model", "image_count", "tasks", "image_urls",
		"request_id", "status", "fail_reason", "deleted", "create_time", "update_time"}
	row := func(id int64, content, tasks, urls string) []driverValue {
		return []driverValue{id, 7, "poster", content, nil, nil, nil, nil, nil, nil, nil, nil, nil, "3:4", 0, "", 1, tasks, urls, nil, 1, nil, 0, now, now.Add(-time.Hour)}
	}
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_local_life_creation` WHERE `status` = ? AND `deleted` = 0 AND `update_time` >= ? AND `update_time` < ? ORDER BY `update_time`, `id` LIMIT ?")).
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow(row(1, "夏日", `[{"task_id":"g1"},{"task_id":"g1b"}]`, `["https://cdn/a","https://cdn/b"]`)...).
			AddRow(row(2, "秋", `[{"task_id":"g2"}]`, `["https://cdn/c"]`)...))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT DISTINCT source_task_id FROM afv_asset WHERE source_task_id IN (?,?)")).WithArgs("g1", "g2").
		WillReturnRows(sqlmock.NewRows([]string{"source_task_id"}).AddRow("g2"))
	mock.ExpectBegin()
	mock.ExpectQuery(qSeqLock).WithArgs("g1", int64(7)).WillReturnRows(sqlmock.NewRows([]string{"properties"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	mock.ExpectExec("INSERT INTO afv_asset").WithArgs(int64(7), int64(3), "g1", "image", "poster", sqlmock.AnyArg(), "https://cdn/a", "https://cdn/a",
		"3:4", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec("INSERT INTO afv_asset").WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()
	st := recs[0].RunOnce(context.Background())
	if st.Scanned != 2 || st.Missing != 1 || st.Inserted != 1 {
		t.Fatalf("对账计数不符: %+v", st)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type driverValue = driver.Value

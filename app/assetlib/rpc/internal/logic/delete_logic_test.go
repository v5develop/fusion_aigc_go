package logic

import (
	"context"
	"regexp"
	"testing"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/app/assetlib/rpc/internal/svc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

func newDeleteSvc(t *testing.T) (*svc.ServiceContext, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	s := &svc.ServiceContext{Repo: repo.New(bun.NewDB(sdb, mysqldialect.New()), nil)}
	s.Config.Limits.BatchAssets = 100
	return s, mock
}

const ownedSourcesSQL = "SELECT `a`.`id`, `a`.`source_type` FROM afv_asset AS a WHERE"

func TestDeleteBatchSkipsOfficialAndMissing(t *testing.T) {
	s, mock := newDeleteSvc(t)
	// 1 上传、2 生成、3 官方（source_type NULL）、4 不属于本人（没查出来）
	mock.ExpectQuery(regexp.QuoteMeta(ownedSourcesSQL)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "source_type"}).AddRow(1, 1).AddRow(2, 2).AddRow(3, nil))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_asset` SET deleted = 1 WHERE (id IN (1, 2)) AND (user_id = 7)")).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM `afv_asset_tag_link` WHERE (asset_id IN (1, 2))")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	out, err := NewDeleteLogic(context.Background(), s).Delete(&assetlib.DeleteReq{UserId: 7, AssetIds: []int64{1, 2, 3, 4, 2}})
	if err != nil {
		t.Fatal(err)
	}
	if out.Deleted != 2 || out.OfficialSkipped != 1 || out.Missing != 1 || len(out.DeletedIds) != 2 {
		t.Fatalf("out=%+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 全是官方：不开事务、不写库，返回计数。
func TestDeleteBatchAllOfficialNoWrite(t *testing.T) {
	s, mock := newDeleteSvc(t)
	mock.ExpectQuery(regexp.QuoteMeta(ownedSourcesSQL)).WillReturnRows(sqlmock.NewRows([]string{"id", "source_type"}).AddRow(3, 9))
	out, err := NewDeleteLogic(context.Background(), s).Delete(&assetlib.DeleteReq{UserId: 7, AssetIds: []int64{3}})
	if err != nil || out.Deleted != 0 || out.OfficialSkipped != 1 || out.DeletedIds == nil {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteStrict(t *testing.T) {
	t.Run("官方素材 2005", func(t *testing.T) {
		s, mock := newDeleteSvc(t)
		mock.ExpectQuery(regexp.QuoteMeta(ownedSourcesSQL)).WillReturnRows(sqlmock.NewRows([]string{"id", "source_type"}).AddRow(3, nil))
		_, err := NewDeleteLogic(context.Background(), s).Delete(&assetlib.DeleteReq{UserId: 7, AssetIds: []int64{3}, Strict: true})
		if code, msg, _ := xerr.Parse(err); code != xerr.Forbidden || msg != "官方素材不可删除" {
			t.Fatalf("code=%d msg=%s", code, msg)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("别人的 / 不存在 4001", func(t *testing.T) {
		s, mock := newDeleteSvc(t)
		mock.ExpectQuery(regexp.QuoteMeta(ownedSourcesSQL)).WillReturnRows(sqlmock.NewRows([]string{"id", "source_type"}))
		_, err := NewDeleteLogic(context.Background(), s).Delete(&assetlib.DeleteReq{UserId: 7, AssetIds: []int64{8}, Strict: true})
		if code, _, _ := xerr.Parse(err); code != xerr.NotFound {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
}

func TestDeleteParamChecks(t *testing.T) {
	s, _ := newDeleteSvc(t)
	l := NewDeleteLogic(context.Background(), s)
	if _, err := l.Delete(&assetlib.DeleteReq{UserId: 0, AssetIds: []int64{1}}); err == nil {
		t.Fatal("未登录应拒绝")
	}
	if _, err := l.Delete(&assetlib.DeleteReq{UserId: 7}); err == nil {
		t.Fatal("空 ids 应拒绝")
	}
	if _, err := l.Delete(&assetlib.DeleteReq{UserId: 7, AssetIds: []int64{1, 2}, Strict: true}); err == nil {
		t.Fatal("单删多个 id 应拒绝")
	}
	many := make([]int64, 101)
	for i := range many {
		many[i] = int64(i + 1)
	}
	if code, _, _ := xerr.Parse(func() error { _, err := l.Delete(&assetlib.DeleteReq{UserId: 7, AssetIds: many}); return err }()); code != xerr.ParamError {
		t.Fatalf("超上限应参数错误 code=%d", code)
	}
}

// untagged 与 tag 互斥，校验在查库之前。
func TestListRejectsUntaggedWithTag(t *testing.T) {
	_, err := NewListLogic(context.Background(), nil).List(&assetlib.ListReq{UserId: 7, Untagged: true, Tag: "红"})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("应返回参数错误，got %d (%v)", code, err)
	}
}

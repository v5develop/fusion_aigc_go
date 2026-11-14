package logic

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

func limitsSvc(t *testing.T) (*BatchTagLogic, sqlmock.Sqlmock) {
	s, mock := newDeleteSvc(t)
	s.Config.Limits.BatchAssets, s.Config.Limits.TagsPerAsset, s.Config.Limits.TagNameLen = 200, 20, 32
	return NewBatchTagLogic(context.Background(), s), mock
}

// 批量打 / 移除标签的入参校验：两者都没给、资产超 200、标签超 20。
func TestBatchTagValidation(t *testing.T) {
	l, _ := limitsSvc(t)
	if _, err := l.BatchTag(&assetlib.BatchTagReq{UserId: 7, AssetIds: []int64{1}}); err == nil {
		t.Fatal("tag_ids 与 remove_tag_ids 都空应被拒")
	} else if code, _, _ := xerr.Parse(err); code != xerr.ParamMissing {
		t.Fatalf("应 1002，got %d", code)
	}
	many := make([]int64, 201)
	for i := range many {
		many[i] = int64(i + 1)
	}
	if _, err := l.BatchTag(&assetlib.BatchTagReq{UserId: 7, AssetIds: many, RemoveTagIds: []int64{1}}); err == nil {
		t.Fatal("超过 200 个资产应被拒")
	}
	if _, err := l.BatchTag(&assetlib.BatchTagReq{UserId: 7, AssetIds: []int64{1}, RemoveTagIds: many[:21]}); err == nil {
		t.Fatal("超过 20 个标签应被拒")
	}
}

// 只移除：校验归属后一个事务里删关联，不走追加。
func TestBatchTagRemoveOnly(t *testing.T) {
	l, mock := limitsSvc(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `a`.`id` FROM afv_asset AS a WHERE (a.id IN (1, 2))")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta("FROM `afv_asset_tag` AS `t` WHERE (t.user_id = 7) AND (t.id IN (5))")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name"}).AddRow(5, 7, "红"))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM `afv_asset_tag_link` WHERE (asset_id IN (1, 2)) AND (tag_id IN (5)) AND (tag_id IN (SELECT id FROM afv_asset_tag WHERE user_id = 7))")).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	out, err := l.BatchTag(&assetlib.BatchTagReq{UserId: 7, AssetIds: []int64{1, 2}, RemoveTagIds: []int64{5}})
	if err != nil || out.Removed != 2 || out.Added != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 列表 tags / sys_tags 解析：逗号分隔、去空白去重、上限与字数。
func TestParseNameList(t *testing.T) {
	got, err := parseNameList("tags", " 红, ,夏,红 ,", 32)
	if err != nil || strings.Join(got, "|") != "红|夏" {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got, _ := parseNameList("tags", " , ", 32); got != nil {
		t.Fatal("全空应为 nil")
	}
	if _, err := parseNameList("tags", strings.Repeat("长", 33), 32); err == nil {
		t.Fatal("超 32 字应被拒")
	}
	var parts []string
	for i := 0; i < 21; i++ {
		parts = append(parts, string(rune('a'+i)))
	}
	if _, err := parseNameList("sys_tags", strings.Join(parts, ","), 32); err == nil {
		t.Fatal("超 20 个应被拒")
	}
	if _, err := NewListTagsLogic(context.Background(), nil).ListTags(&assetlib.ListTagsReq{UserId: 7, Sort: "x"}); err == nil {
		t.Fatal("非法 sort 应被拒")
	}
}

// 新建 / 改名禁止逗号（1003）；单个 tag 整串精确匹配、不按逗号拆，与 tags 多选合并。
func TestTagCommaRules(t *testing.T) {
	s, mock := newDeleteSvc(t)
	s.Config.Limits.TagNameLen, s.Config.Limits.TagsPerUser = 32, 500
	for _, err := range []error{
		func() error {
			_, err := NewCreateTagLogic(context.Background(), s).CreateTag(&assetlib.CreateTagReq{UserId: 7, Name: "夏,季"})
			return err
		}(),
		func() error {
			_, err := NewRenameTagLogic(context.Background(), s).RenameTag(&assetlib.RenameTagReq{UserId: 7, Id: 1, Name: "a,b"})
			return err
		}(),
	} {
		if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
			t.Fatalf("含逗号应 1003，got %d (%v)", code, err)
		}
	}
	// 存量含逗号的标签：tag=夏,季 整串精确匹配；tags=红,蓝 拆成两个
	mock.ExpectQuery(regexp.QuoteMeta("t.name = '红'") + ".*" + regexp.QuoteMeta("t.name = '蓝'") + ".*" + regexp.QuoteMeta("t.name = '夏,季'")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	if _, err := NewListLogic(context.Background(), s).List(&assetlib.ListReq{UserId: 7, Tag: "夏,季", Tags: "红,蓝"}); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

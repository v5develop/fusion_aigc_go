package repo

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/app/assetlib/model"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

func newMock(t *testing.T, videoTypes ...string) (*Repo, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return New(bun.NewDB(sdb, mysqldialect.New()), videoTypes), mock
}

func done(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

var ctx = context.Background()

// vc 与 AssetRepo.videoCond 一致的视频判据（offer category 优先，查不到看配置类型 / 名字）。
func vc(tail string) string {
	return "(a.type = 'video' OR (a.type <> 'image' AND (" +
		"a.type IN (SELECT o.entry_code FROM afv_billing_offer AS o WHERE o.status = 'ENABLED' AND o.deleted = 0 AND o.category = 'VIDEO') OR " +
		"(a.type NOT IN (SELECT o.entry_code FROM afv_billing_offer AS o WHERE o.status = 'ENABLED' AND o.deleted = 0 AND o.category IS NOT NULL AND o.category <> '') AND " +
		tail + "))))"
}

const ownedSQL = "WHERE (a.user_id = 7) AND (a.deleted = 0) AND ((a.status IS NULL OR a.status <> 0))"

func q(s string) string { return regexp.QuoteMeta(s) }

// List 的过滤组合：每组断言 WHERE 片段（count 与 select 用同一组条件）。
func TestListFilters(t *testing.T) {
	cases := []struct {
		name  string
		f     Filter
		video []string
		where string
	}{
		{"无筛选", Filter{UserID: 7}, nil, ownedSQL},
		{"未分类+input+图片", Filter{UserID: 7, Kind: "uncategorized", Source: "input", Media: "image"}, nil,
			ownedSQL + " AND (e.kind IS NULL) AND ((a.source_type IS NULL OR a.source_type <> 2)) AND ((a.type IS NULL OR NOT " + vc("a.type LIKE '%video%'") + "))"},
		{"kind+generated+视频(含配置类型)", Filter{UserID: 7, Kind: "model", Source: "generated", Media: "video"}, []string{"lipSync"},
			ownedSQL + " AND (e.kind = 'model') AND (a.source_type = 2) AND (" + vc("(a.type LIKE '%video%' OR a.type IN ('lipSync'))") + ")"},
		{"图片大类+比例", Filter{UserID: 7, KindGroup: "image", AspectRatio: "3:4"}, nil,
			ownedSQL + " AND (e.kind IN ('model', 'scene', 'makeup', 'hair', 'accessory')) AND (a.aspect_ratio = '3:4')"},
		{"视频大类", Filter{UserID: 7, KindGroup: "video"}, nil,
			ownedSQL + " AND (e.kind IN ('viral_video', 'video_remake', 'promo_video', 'digital_human', 'digital_human_talk', " +
				"'store_visit', 'promo_drama', 'motion_transfer', 'canvas_video', 'video_other'))"},
		{"入口码+比例", Filter{UserID: 7, EntryCodes: []string{"txt2img", "img.edit_v2"}, AspectRatio: "1:1"}, nil,
			ownedSQL + " AND (a.aspect_ratio = '1:1') AND (a.entry_code IN ('txt2img', 'img.edit_v2'))"},
		{"official", Filter{UserID: 7, Source: "official"}, nil, ownedSQL + " AND ((a.source_type IS NULL OR a.source_type NOT IN (1, 2)))"},
		{"upload+收藏+标签+关键字(转义)", Filter{UserID: 7, Source: "upload", FavoriteOnly: true, Tag: "红", Q: "a_%"}, nil,
			ownedSQL + " AND (a.source_type = 1) AND (e.favorite = 1) AND (EXISTS (SELECT 1 FROM afv_asset_tag_link AS l JOIN afv_asset_tag AS t ON t.id = l.tag_id " +
				"WHERE l.asset_id = a.id AND t.user_id = 7 AND t.name = '红')) AND ((a.name LIKE '%a\\\\_\\\\%%' OR a.ai_prompt LIKE '%a\\\\_\\\\%%'))"},
		{"相关资产", Filter{UserID: 7, RelatedOf: 3}, nil,
			ownedSQL + " AND (a.id IN (SELECT related_asset_id FROM afv_asset_relation WHERE asset_id = 3 AND relation_type IN ('reference', 'generated_from') " +
				"UNION SELECT asset_id FROM afv_asset_relation WHERE related_asset_id = 3 AND relation_type = 'reference'))"},
		{"生成结果", Filter{UserID: 7, ResultsOf: 3}, nil,
			ownedSQL + " AND (a.id IN (SELECT asset_id FROM afv_asset_relation WHERE related_asset_id = 3 AND relation_type = 'generated_from'))"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, mock := newMock(t, tc.video...)
			mock.ExpectQuery(q("SELECT count(*) FROM afv_asset AS a LEFT JOIN afv_asset_ext AS e ON e.asset_id = a.id "+
				"LEFT JOIN afv_asset_version AS v ON v.id = e.current_version_id "+tc.where) + "$").
				WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
			mock.ExpectQuery(q(tc.where+" ORDER BY a.create_time DESC, a.id DESC LIMIT 15 OFFSET 15") + "$").
				WillReturnRows(sqlmock.NewRows([]string{"id", "name", "type", "source_type", "create_time", "e_kind", "e_favorite", "v_image_url", "is_video"}).
					AddRow(11, "a", "image", 2, time.Unix(1730000000, 0), "model", 1, "https://x/v2.png", 0).
					AddRow(10, "b", "productReplacement", nil, time.Unix(1720000000, 0), nil, nil, nil, 1))
			rows, total, err := r.Asset.List(ctx, tc.f, 15, 15)
			if err != nil || total != 2 || len(rows) != 2 {
				t.Fatalf("rows=%d total=%d err=%v", len(rows), total, err)
			}
			if rows[0].ExtKind.String != "model" || rows[0].CurrentImage.String != "https://x/v2.png" || rows[1].ExtKind.Valid || rows[1].SourceType.Valid || rows[0].IsVideo || !rows[1].IsVideo {
				t.Fatalf("scan: %+v / %+v", rows[0], rows[1])
			}
			done(t, mock)
		})
	}
}

func TestListEmptySkipsSelect(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery("SELECT count").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	rows, total, err := r.Asset.List(ctx, Filter{UserID: 7}, 15, 0)
	if err != nil || total != 0 || rows != nil {
		t.Fatalf("rows=%v total=%d err=%v", rows, total, err)
	}
	done(t, mock)
}

// Categories：一条 GROUP BY e.kind，NULL 行即未分类；kind 条件不参与计数。
func TestCategoriesNullIsUncategorized(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("SELECT e.kind AS kind, COUNT(*) AS n FROM afv_asset AS a LEFT JOIN afv_asset_ext AS e ON e.asset_id = a.id "+
		ownedSQL+" AND (a.source_type = 2) AND ("+vc("a.type LIKE '%video%'")+") GROUP BY e.kind") + "$").
		WillReturnRows(sqlmock.NewRows([]string{"kind", "n"}).AddRow(nil, 5).AddRow("model", 2).AddRow("hair", 1))
	got, err := r.Asset.Categories(ctx, Filter{UserID: 7, Kind: "model", Source: "generated", Media: "video"})
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%v err=%v", got, err)
	}
	if got[0].Kind.Valid || got[0].N != 5 || got[1].Kind.String != "model" || got[1].N != 2 {
		t.Fatalf("got=%+v", got)
	}
	done(t, mock)
}

// 「未分类」计数：本人、未删除、非草稿，且没挂本人字典里的任何标签。
func TestTagUntaggedCount(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("SELECT count(*) FROM afv_asset AS a "+ownedSQL+
		" AND (NOT EXISTS (SELECT 1 FROM afv_asset_tag_link AS l JOIN afv_asset_tag AS t ON t.id = l.tag_id WHERE l.asset_id = a.id AND t.user_id = 7))") + "$").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(30))
	n, err := r.Tag.UntaggedCount(ctx, 7)
	if err != nil || n != 30 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	done(t, mock)
}

// AddVersion 的版本号：MAX+1（无版本时 COALESCE 成 1）。
func TestVersionNextNo(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("SELECT COALESCE(MAX(version_no), 0) + 1 FROM afv_asset_version WHERE (asset_id = 3) AND (asset_id IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0))") + "$").
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(4))
	mock.ExpectQuery(q("SELECT count(*) FROM afv_asset WHERE (id = 3) AND (user_id = 7) AND (deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectExec(q("INSERT INTO `afv_asset_version` (`id`, `asset_id`, `version_no`, `image_rid`, `image_url`, `prompt`, `model`, `params`, `created_at`) " +
		"VALUES (DEFAULT, 3, 4, NULL, 'https://x/4.png', 'p', 'GPT', '{\"seed\":1}', DEFAULT)")).
		WillReturnResult(sqlmock.NewResult(88, 1))
	n, err := r.Version.NextNo(ctx, 3, 7)
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	v := &model.Version{AssetID: 3, VersionNo: n, ImageURL: "https://x/4.png", Prompt: sql.NullString{String: "p", Valid: true}, Model: "GPT", Params: json.RawMessage(`{"seed":1}`)}
	if err := r.Version.Insert(ctx, 7, v); err != nil || v.ID != 88 {
		t.Fatalf("id=%d err=%v", v.ID, err)
	}
	done(t, mock)
}

// Upsert ext：整行 INSERT … ON DUPLICATE KEY UPDATE，user_id / created_at 不在更新列；空 JSON 写 NULL（DEFAULT）。
func TestExtUpsert(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("SELECT count(*) FROM afv_asset WHERE (id = 3) AND (user_id = 7) AND (deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	mock.ExpectExec(q("INSERT INTO `afv_asset_ext` (`asset_id`, `user_id`, `kind`, `favorite`, `license`, `metadata`, `provenance`, `current_version_id`, `created_at`, `updated_at`) "+
		"VALUES (3, 7, 'scene', 1, '', '{\"style\":\"日系\"}', DEFAULT, 88, DEFAULT, DEFAULT) ON DUPLICATE KEY UPDATE "+
		"`kind` = VALUES(`kind`), `favorite` = VALUES(`favorite`), `license` = VALUES(`license`), `metadata` = VALUES(`metadata`), "+
		"`provenance` = VALUES(`provenance`), `current_version_id` = VALUES(`current_version_id`)") + "$").
		WillReturnResult(sqlmock.NewResult(0, 2))
	e := &model.Ext{AssetID: 3, UserID: 7, Kind: sql.NullString{String: "scene", Valid: true}, Favorite: 1,
		Metadata: json.RawMessage(`{"style":"日系"}`), CurrentVersionID: sql.NullInt64{Int64: 88, Valid: true}}
	if err := r.Ext.Upsert(ctx, 7, e); err != nil {
		t.Fatal(err)
	}
	// 读改写：事务里 FOR UPDATE
	mock.ExpectQuery(q("FROM `afv_asset_ext` AS `e` WHERE (e.asset_id = 3) AND (e.asset_id IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0)) LIMIT 1 FOR UPDATE")).
		WillReturnRows(sqlmock.NewRows([]string{"asset_id"}))
	if got, err := r.Ext.Get(ctx, 3, 7, true); got != nil || err != nil {
		t.Fatalf("没有旁表行应返回 nil：%v %v", got, err)
	}
	done(t, mock)
}

func TestRenameOnlyTouchesName(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("UPDATE `afv_asset` SET name = '新标题' WHERE (id = 1) AND (user_id = 7) AND (deleted = 0)") + "$").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.Asset.Rename(ctx, 1, 7, "新标题"); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	done(t, mock)
}

func TestRelationRemoveReferenceBothWays(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("DELETE FROM `afv_asset_relation` WHERE (relation_type = 'reference') AND (1 IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0)) AND " +
		"(((asset_id = 1 AND related_asset_id = 2) OR (asset_id = 2 AND related_asset_id = 1)))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(q("DELETE FROM `afv_asset_relation` WHERE (relation_type = 'generated_from') AND (1 IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0)) AND (asset_id = 1) AND (related_asset_id = 2)")).
		WillReturnResult(sqlmock.NewResult(0, 0))
	if n, err := r.Relation.Remove(ctx, 1, 2, 7, model.RelReference); n != 1 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if n, err := r.Relation.Remove(ctx, 1, 2, 7, model.RelGeneratedFrom); n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	done(t, mock)
}

const ownedTagLinkSQL = "tag_id IN (SELECT id FROM afv_asset_tag WHERE user_id = 7)"

func TestTagsTx(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectBegin()
	mock.ExpectQuery(q("FROM `afv_asset_tag` AS `t` WHERE (t.user_id = 7) AND (t.id IN (5, 6))")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name"}).AddRow(5, 7, "a").AddRow(6, 7, "b"))
	mock.ExpectExec(q("DELETE FROM `afv_asset_tag_link` WHERE (asset_id = 3) AND (" + ownedTagLinkSQL + ")")).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(q("INSERT INTO `afv_asset_tag_link` (`asset_id`, `tag_id`, `created_at`) VALUES (3, 5, DEFAULT), (3, 6, DEFAULT)")).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	err := r.RunInTx(ctx, func(ctx context.Context, tx *Repo) error { return tx.Tag.ReplaceAssetTags(ctx, 3, 7, []int64{5, 6}) })
	if err != nil {
		t.Fatal(err)
	}
	done(t, mock)
}

// 标签不全是本人的：不写任何关联。
func TestTagLinksRejectForeignTag(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("FROM `afv_asset_tag` AS `t` WHERE (t.user_id = 7) AND (t.id IN (5, 9))")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "name"}).AddRow(5, 7, "a"))
	_, err := r.Tag.AddLinks(ctx, 7, []*model.TagLink{{AssetID: 3, TagID: 5}, {AssetID: 3, TagID: 9}})
	if !errors.Is(err, ErrTagNotOwned) {
		t.Fatalf("err=%v", err)
	}
	done(t, mock)
}

// 删标签：关联与标签本身的 DELETE 都带 user_id。
func TestTagDeleteScopedByUser(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectExec(q("DELETE FROM `afv_asset_tag_link` WHERE (tag_id = 5) AND (" + ownedTagLinkSQL + ")")).WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectExec(q("DELETE FROM `afv_asset_tag` WHERE (id = 5) AND (user_id = 7)")).WillReturnResult(sqlmock.NewResult(0, 1))
	if ok, err := r.Tag.Delete(ctx, 5, 7); !ok || err != nil {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	done(t, mock)
}

// 越权写：资产不属于该用户时不执行 INSERT，返回 ErrAssetNotOwned。
func TestWritesRejectForeignAsset(t *testing.T) {
	r, mock := newMock(t)
	for i := 0; i < 3; i++ {
		mock.ExpectQuery(q("SELECT count(*) FROM afv_asset WHERE (id = 3) AND (user_id = 9) AND (deleted = 0)")).
			WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	}
	if err := r.Version.Insert(ctx, 9, &model.Version{AssetID: 3}); !errors.Is(err, ErrAssetNotOwned) {
		t.Fatalf("version: %v", err)
	}
	if err := r.Ext.Upsert(ctx, 9, &model.Ext{AssetID: 3, UserID: 9}); !errors.Is(err, ErrAssetNotOwned) {
		t.Fatalf("ext: %v", err)
	}
	if _, err := r.Relation.Add(ctx, 3, 4, 9, "reference"); !errors.Is(err, ErrAssetNotOwned) {
		t.Fatalf("relation: %v", err)
	}
	if err := r.Ext.Upsert(ctx, 9, &model.Ext{AssetID: 3, UserID: 7}); !errors.Is(err, ErrAssetNotOwned) {
		t.Fatalf("ext user mismatch: %v", err)
	}
	done(t, mock)
}

func TestVersionReadsCarryUserID(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("v.asset_id = 3) AND (v.asset_id IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery(q("v.asset_id = 3) AND (v.asset_id IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery(q("v.id = 5) AND (v.asset_id = 3) AND (v.asset_id IN (SELECT id FROM afv_asset WHERE user_id = 7 AND deleted = 0)")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	_, _ = r.Version.Count(ctx, 3, 7)
	_, _ = r.Version.Page(ctx, 3, 7, 6, 0)
	_, _ = r.Version.Get(ctx, 5, 3, 7)
	done(t, mock)
}

// 标签计数口径：未删除且非草稿，与列表一致。
func TestTagCountExcludesDrafts(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("a.deleted = 0 AND (a.status IS NULL OR a.status <> 0) WHERE l.tag_id = t.id")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}))
	if _, err := r.Tag.List(ctx, 7); err != nil {
		t.Fatal(err)
	}
	done(t, mock)
}

package core

import (
	"context"
	"strings"
	"testing"
)

// 上传废弃对象清理集成测试（需要 PLAZA_TEST_DSN 指向临时库，已执行 Go_V1.0.44 / 48）。
// 假存储只记录删除调用，不碰真实桶。

func cleanupKey(c byte) string {
	return "aigc/" + UploadDir + "/202611/" + strings.Repeat(string(c), 32) + ".png"
}
func cleanupURL(c byte) string { return upBase + "/" + cleanupKey(c) }

func cleanupSvc(t *testing.T, cc CleanupConfig) (*Service, *delStore) {
	t.Helper()
	s := integrationService(t)
	for _, tb := range []string{"afv_plaza_upload_hash", "afv_plaza_upload_orphan"} {
		if _, err := s.db.ExecContext(context.Background(), "DELETE FROM "+tb); err != nil {
			t.Fatal(err)
		}
	}
	ds := &delStore{fakeStore: &fakeStore{}}
	u := newTestUploader(ds.fakeStore)
	u.store = ds
	u.cfg.Cleanup = cc
	s.uploader = u
	return s, ds
}

func addHash(t *testing.T, s *Service, c byte, ageDays int, cover string) {
	t.Helper()
	rj := `{"url":"` + cleanupURL(c) + `","kind":"image"`
	if cover != "" {
		rj += `,"coverUrl":"` + cover + `"`
	}
	rj += `}`
	_, err := s.db.ExecContext(context.Background(),
		"INSERT INTO afv_plaza_upload_hash (sha256, object_key, url, result_json, size, create_time) VALUES (?, ?, ?, ?, 1, DATE_SUB(NOW(), INTERVAL ? DAY))",
		strings.Repeat(string(c), 64), cleanupKey(c), cleanupURL(c), rj, ageDays)
	if err != nil {
		t.Fatal(err)
	}
}

func addOrphan(t *testing.T, s *Service, c byte, ageHours int) {
	t.Helper()
	_, err := s.db.ExecContext(context.Background(),
		"INSERT INTO afv_plaza_upload_orphan (object_key, url, reason, create_time) VALUES (?, ?, 'REJECTED', DATE_SUB(NOW(), INTERVAL ? HOUR))",
		cleanupKey(c), cleanupURL(c), ageHours)
	if err != nil {
		t.Fatal(err)
	}
}

func count(t *testing.T, s *Service, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.NewRaw(q, args...).Scan(context.Background(), &n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCleanupUploadsDisabledDoesNothing(t *testing.T) {
	s, ds := cleanupSvc(t, CleanupConfig{})
	addHash(t, s, 'a', 30, "")
	addOrphan(t, s, 'b', 100)
	rep, err := s.CleanupUploads(context.Background())
	if err != nil || rep != (CleanupReport{}) || len(ds.deleted) != 0 {
		t.Fatalf("开关关闭不得删除: %v %+v %v", err, rep, ds.deleted)
	}
	if count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_hash") != 1 || count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_orphan") != 1 {
		t.Fatal("开关关闭不得动登记 / 队列行")
	}
}

func TestCleanupUploadsDeletesUnreferencedKeepsReferenced(t *testing.T) {
	s, ds := cleanupSvc(t, CleanupConfig{Enabled: true})
	ctx := context.Background()
	addHash(t, s, 'a', 30, "") // 过期、无引用 → 删
	addHash(t, s, 'b', 30, "") // 过期、被模板 video_url 引用 → 留
	addHash(t, s, 'c', 30, "") // 过期、被回收站模板 data_json 引用 → 留
	addHash(t, s, 'd', 30, "") // 过期、被示例素材引用 → 留
	addHash(t, s, 'e', 2, "")  // 宽限期内 → 留
	addHash(t, s, 'f', 30, "") // 过期但最近被去重命中 → 留
	addOrphan(t, s, '1', 48)   // 过期孤儿、无引用 → 删
	addOrphan(t, s, '2', 48)   // 过期孤儿、被模板 poster_url 引用 → 只移出队列
	addOrphan(t, s, '3', 2)    // 宽限期内孤儿 → 留
	addOrphan(t, s, 'a', 48)   // 孤儿同时是登记对象 → 不删，移出队列（登记对象 a 本身仍按登记规则被删，见下方断言）
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("INSERT INTO afv_compat_plaza_template (title, data_json, video_url) VALUES ('t1', '{}', ?)", cleanupURL('b')+"?x=1")
	mustExec("INSERT INTO afv_compat_plaza_template (title, data_json, deleted) VALUES ('t2', ?, 1)", `{"result":["`+cleanupURL('c')+`"]}`)
	mustExec("INSERT INTO afv_compat_plaza_template (title, data_json, poster_url) VALUES ('t3', '{}', ?)", cleanupURL('2'))
	mustExec("INSERT INTO afv_plaza_sample_asset (scope_type, url, title, deleted) VALUES ('TEMPLATE', ?, 's', 1)", cleanupURL('d'))
	mustExec("UPDATE afv_plaza_upload_hash SET last_hit_time = NOW() WHERE object_key = ?", cleanupKey('f'))

	rep, err := s.CleanupUploads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, k := range ds.deleted {
		got[k] = true
	}
	// 'a' 是登记对象且过期无引用 → 被登记阶段删除；孤儿 'a' 因同时是登记对象不在孤儿阶段删。
	for _, c := range []byte{'1', 'a'} {
		if !got[cleanupKey(c)] {
			t.Errorf("应删除 %c", c)
		}
	}
	for _, c := range []byte{'b', 'c', 'd', 'e', 'f', '2', '3'} {
		if got[cleanupKey(c)] {
			t.Errorf("不应删除 %c（被引用 / 宽限期内）", c)
		}
	}
	if len(ds.deleted) != 2 || rep.OrphansDeleted != 1 || rep.HashDeleted != 1 {
		t.Fatalf("删除数不对: %v %+v", ds.deleted, rep)
	}
	if count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_hash WHERE object_key = ?", cleanupKey('a')) != 0 {
		t.Error("已删对象的登记行应一并删除")
	}
	if count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_hash") != 5 {
		t.Error("其余登记行应保留")
	}
	if count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_orphan WHERE object_key IN (?, ?, ?)", cleanupKey('1'), cleanupKey('2'), cleanupKey('a')) != 0 ||
		count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_orphan WHERE object_key = ?", cleanupKey('3')) != 1 {
		t.Error("队列行：已处理 / 被引用的应移出，宽限期内的保留")
	}
}

func TestCleanupUploadsKeepsRowWhenDeleteFails(t *testing.T) {
	s, ds := cleanupSvc(t, CleanupConfig{Enabled: true})
	addHash(t, s, 'a', 30, "")
	addOrphan(t, s, '1', 48)
	s.uploader.store = failDelStore{ds.fakeStore}
	rep, err := s.CleanupUploads(context.Background())
	if err != nil || rep.Failed != 2 || rep.HashDeleted != 0 || rep.OrphansDeleted != 0 {
		t.Fatalf("%v %+v", err, rep)
	}
	if count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_hash") != 1 || count(t, s, "SELECT attempts FROM afv_plaza_upload_orphan") != 1 {
		t.Fatal("删对象失败必须保留登记行 / 队列行（orphan.attempts+1），下轮重试")
	}
}

func TestCleanupUploadsRefusesForeignKeys(t *testing.T) {
	s, ds := cleanupSvc(t, CleanupConfig{Enabled: true})
	ctx := context.Background()
	// 队列 / 登记表里混进了不在 plaza-template/ 下的 key：绝不删对象
	if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_upload_orphan (object_key, url, reason, create_time) VALUES ('aigc/image/1/20261101/x.png', '', 'REJECTED', DATE_SUB(NOW(), INTERVAL 99 HOUR))"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO afv_plaza_upload_hash (sha256, object_key, url, result_json, create_time) VALUES (?, 'aigc/image/1/20261101/y.png', 'https://x/aigc/image/1/20261101/y.png', '{}', DATE_SUB(NOW(), INTERVAL 99 DAY))", strings.Repeat("9", 64)); err != nil {
		t.Fatal(err)
	}
	rep, err := s.CleanupUploads(ctx)
	if err != nil || len(ds.deleted) != 0 || rep.SkippedInvalid != 2 {
		t.Fatalf("目录外 key 不得删除: %v %v %+v", err, ds.deleted, rep)
	}
}

func TestCleanupUploadsDeletesCoverOfDeletedHash(t *testing.T) {
	s, ds := cleanupSvc(t, CleanupConfig{Enabled: true})
	cover := cleanupURL('9')
	addHash(t, s, 'a', 30, cover)
	if _, err := s.CleanupUploads(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ds.deleted) != 2 || ds.deleted[1] != cleanupKey('9') {
		t.Fatalf("应连同未被引用的封面一起删: %v", ds.deleted)
	}
}

type failDelStore struct{ *fakeStore }

func (failDelStore) Delete(context.Context, string) error { return context.DeadlineExceeded }

// 中：扫描到过期登记行之后，同内容上传刚命中它（last_hit_time 刷新）→ 条件删影响 0 行，跳过、不删对象、不删行。
func TestCleanupUploadsSkipsHashHitAfterScan(t *testing.T) {
	s, ds := cleanupSvc(t, CleanupConfig{Enabled: true})
	addHash(t, s, 'a', 30, "")
	var r hashRow
	if err := s.db.NewRaw("SELECT id, sha256, object_key, url, CAST(result_json AS CHAR) AS result_json FROM afv_plaza_upload_hash LIMIT 1").Scan(context.Background(), &r); err != nil {
		t.Fatal(err)
	}
	// 去重命中刷新命中时间（发生在扫描之后、删除之前）
	if _, err := s.db.ExecContext(context.Background(), "UPDATE afv_plaza_upload_hash SET last_hit_time = NOW() WHERE id = ?", r.ID); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.deleteStaleHash(context.Background(), s.uploader, r)
	if err != nil || claimed {
		t.Fatalf("刚被命中的登记行不得清理: claimed=%v err=%v", claimed, err)
	}
	if len(ds.deleted) != 0 || count(t, s, "SELECT COUNT(*) FROM afv_plaza_upload_hash") != 1 {
		t.Fatalf("影响 0 行必须跳过：不删对象、不删行: %v", ds.deleted)
	}
}

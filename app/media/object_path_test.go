package media

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 方案 B 阶段一：新对象写成 v1/<类别>/<yyyy>/<mm>/<对象键>.<后缀>，rid → 路径靠索引；旧对象保持旧路径。

func TestDatedPathCarriesDateAndExtension(t *testing.T) {
	at := time.Date(2026, 10, 22, 17, 30, 0, 0, time.UTC) // 上海时间 10-23 01:30
	got := datedPath(classWork, at, "abcdefghijklmnopqrstuvwxyz", extForMime("image/png"))
	if got != "v1/work/2026/10/abcdefghijklmnopqrstuvwxyz.png" {
		t.Fatalf("datedPath = %q（应按上海时区取月份）", got)
	}
}

func TestExtensionComesFromTheValidatedMediaType(t *testing.T) {
	if extForMime("image/jpeg") != "jpg" || extForMime("video/quicktime") != "mov" {
		t.Fatal("规范后缀映射错误")
	}
	if extForMime("application/x-unknown") != "" {
		t.Fatal("未知类型不应猜后缀")
	}
}

func TestNewPathsAreRecognisedAndIndexIsItsOwnClass(t *testing.T) {
	key := "abcdefghijklmnopqrstuvwxyz"
	for _, p := range []string{
		"v1/work/2026/10/" + key + ".png",
		"v1/asset/2026/10/" + key,
		"v1/user/2026/10/" + key + ".mp4",
	} {
		if classOfPath(p) == "" {
			t.Fatalf("新路径应合法: %s", p)
		}
	}
	if classOfPath("v1/work/2026/13/"+key+".png") != "" {
		t.Fatal("月份 13 不应合法")
	}
	if classOfPath("idx/"+key+".json") != classIndex {
		t.Fatal("索引对象应归入 index 类别")
	}
	if classOfPath("v1/work/"+key) != "" {
		t.Fatal("扁平路径已不再支持")
	}
}

func TestPlaceObjectWritesIndexOnceAndRetryKeepsThePath(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{objects: st, id: testIdentity(t), paths: newPathCache()}
	ctx := context.Background()
	rid, err := srv.id.RID("", classWork, "idem-dated-1")
	if err != nil {
		t.Fatal(err)
	}
	key := srv.id.ObjectKey(rid)
	first := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	p1, err := srv.placeObject(ctx, rid, classWork, storePrivate, key, "image/png", first)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1/work/2026/10/" + key + ".png"; p1 != want {
		t.Fatalf("placeObject = %q, want %q", p1, want)
	}
	// 重试在另一天发生：沿用索引里的原路径，不会在新日期目录写第二份
	p2, err := srv.placeObject(ctx, rid, classWork, storePrivate, key, "image/png", first.Add(72*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p2 != p1 {
		t.Fatalf("重试路径 = %q，应与首次 %q 相同", p2, p1)
	}
	_, path, err := srv.objectPath(ctx, rid)
	if err != nil || path != p1 {
		t.Fatalf("objectPath = %q, %v，应解析到索引路径", path, err)
	}
}

// 直传目标：只带日期、不带后缀（内容尚未校验），重试沿用同一路径。
func TestUploadPathIsDatedWithoutExtensionAndStableOnRetry(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{objects: st, id: testIdentity(t), paths: newPathCache()}
	ctx := context.Background()
	rid, _ := srv.id.RID("", classUser, "upload-dated-1")
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	p1, err := srv.uploadPath(ctx, rid, "", t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1/user/2026/10/" + srv.id.ObjectKey(rid); p1 != want {
		t.Fatalf("uploadPath = %q, want %q", p1, want)
	}
	p2, err := srv.uploadPath(ctx, rid, "", t0.Add(48*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if p2 != p1 {
		t.Fatalf("重试路径 = %q，应与首次 %q 相同", p2, p1)
	}
}

// 对象被清扫物理删除后，索引对象必须一起删掉，不留残留。
func TestSweepRemovesIndexWithTheObject(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	srv := &Server{objects: st, id: testIdentity(t), paths: newPathCache(), now: func() time.Time { return t0 }}
	ctx := context.Background()
	rid, _ := srv.id.RID("", classWork, "idem-sweep-index-1")

	path, err := srv.placeObject(ctx, rid, classWork, storePrivate, srv.id.ObjectKey(rid), "image/png", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, storePrivate, path, writeFixture(t, "sweep me"), "image/png"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreatePending(ctx, storePrivate, path, t0.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}

	if _, err := srv.sweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := st.Head(ctx, storePrivate, path); !errors.Is(err, errObjectNotFound) {
		t.Fatalf("对象应已被物理删除，Head err=%v", err)
	}
	if _, found, _ := srv.readIndex(ctx, rid, classWork); found {
		t.Fatal("对象删除后索引仍然残留")
	}
}

// writeIndexFile 测试用：按服务端的规则写入 rid 的索引文件（与直传/入库时写入的内容一致）。
func writeIndexFile(t *testing.T, objects, indexKey, path string) {
	t.Helper()
	raw, err := json.Marshal(objectIndex{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(objects, storePrivate, filepath.FromSlash(indexKey))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// 孤儿索引：对象不存在且超过保留期的索引删除；对象存在或未到保留期的保留。
func TestSweepRemovesOnlyOrphanIndexesPastTheRetention(t *testing.T) {
	dir := t.TempDir()
	st, err := NewLocalStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	srv := &Server{objects: st, id: testIdentity(t), paths: newPathCache(), now: func() time.Time { return t0 }}
	ctx := context.Background()
	indexFile := func(rid string) string {
		return filepath.Join(dir, storePrivate, filepath.FromSlash(srv.id.IndexKey(rid)))
	}
	age := func(rid string, at time.Time) {
		if err := os.Chtimes(indexFile(rid), at, at); err != nil {
			t.Fatal(err)
		}
	}

	// 1) 直传未完成：索引在，对象不在，超过 24 小时 → 删除索引
	orphanRID, _ := srv.id.RID("", classUser, "orphan-upload")
	writeIndexFile(t, dir, srv.id.IndexKey(orphanRID), datedPath(classUser, t0, srv.id.ObjectKey(orphanRID), ""))
	age(orphanRID, t0.Add(-48*time.Hour))

	// 2) 对象存在：索引保留（即使很旧）
	keptRID, _ := srv.id.RID("", classWork, "kept-object")
	kp, err := srv.placeObject(ctx, keptRID, classWork, storePrivate, srv.id.ObjectKey(keptRID), "image/png", t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Put(ctx, storePrivate, kp, writeFixture(t, "kept"), "image/png"); err != nil {
		t.Fatal(err)
	}
	age(keptRID, t0.Add(-48*time.Hour))

	// 3) 未到保留期：索引保留（即使对象不存在）
	freshRID, _ := srv.id.RID("", classUser, "fresh-upload")
	writeIndexFile(t, dir, srv.id.IndexKey(freshRID), datedPath(classUser, t0, srv.id.ObjectKey(freshRID), ""))
	age(freshRID, t0.Add(-time.Hour))

	if _, err := srv.sweepOnce(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(indexFile(orphanRID)); !os.IsNotExist(err) {
		t.Fatalf("超过保留期的孤儿索引应被删除，stat err=%v", err)
	}
	if _, err := os.Stat(indexFile(keptRID)); err != nil {
		t.Fatalf("对象存在的索引必须保留: %v", err)
	}
	if _, err := os.Stat(indexFile(freshRID)); err != nil {
		t.Fatalf("未到保留期的索引必须保留: %v", err)
	}
}

// 直传声明了类型：路径带规范后缀；同一幂等键改类型则拒绝。
func TestUploadPathCarriesDeclaredExtensionAndRejectsChange(t *testing.T) {
	st, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{objects: st, id: testIdentity(t), paths: newPathCache()}
	ctx := context.Background()
	rid, _ := srv.id.RID("", classUser, "upload-ext-1")
	t0 := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

	p, err := srv.uploadPath(ctx, rid, "png", t0)
	if err != nil {
		t.Fatal(err)
	}
	if want := "v1/user/2026/10/" + srv.id.ObjectKey(rid) + ".png"; p != want {
		t.Fatalf("uploadPath = %q, want %q", p, want)
	}
	if _, err := srv.uploadPath(ctx, rid, "jpg", t0); !errors.Is(err, errUploadTypeChanged) {
		t.Fatalf("改声明类型应被拒绝，err=%v", err)
	}
}

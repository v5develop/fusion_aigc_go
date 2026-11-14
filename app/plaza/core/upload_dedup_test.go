package core

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type memDedup struct{ m map[string]*ConfirmResult }

func (d *memDedup) Find(_ context.Context, sha string) (*ConfirmResult, error) { return d.m[sha], nil }
func (d *memDedup) Register(_ context.Context, sha, _ string, r *ConfirmResult) (*ConfirmResult, error) {
	if e, ok := d.m[sha]; ok {
		return e, nil
	}
	c := *r
	d.m[sha] = &c
	return nil, nil
}

type delStore struct {
	*fakeStore
	deleted []string
}

func (d *delStore) Delete(_ context.Context, key string) error {
	d.deleted = append(d.deleted, key)
	return nil
}

func dedupUploader(f *fakeStore, files map[string][]byte) *Uploader {
	u := newTestUploader(f)
	u.fetch = func(_ context.Context, raw string, _ int64) ([]byte, error) { return files[raw], nil }
	u.open = func(_ context.Context, raw string, _ int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(files[raw])), nil
	}
	u.dedup = &memDedup{m: map[string]*ConfirmResult{}}
	return u
}

func TestConfirmDedupSameContentReusesFirstURL(t *testing.T) {
	ctx := context.Background()
	img := pngBytes(600, 700)
	f := &fakeStore{statSize: int64(len(img)), statCT: "image/png"}
	a, b := objURL("a.png"), upBase+"/aigc/"+UploadDir+"/202611/"+strings.Repeat("b", 32)+".png"
	other := upBase + "/aigc/" + UploadDir + "/202611/" + strings.Repeat("c", 32) + ".png"
	u := dedupUploader(f, map[string][]byte{a: img, b: img, other: pngBytes(601, 700)})

	r1, err := u.Confirm(ctx, ConfirmInput{URL: a})
	if err != nil || r1.URL != a {
		t.Fatalf("首次上传应原样返回: %v %+v", err, r1)
	}
	r2, err := u.Confirm(ctx, ConfirmInput{URL: b})
	if err != nil || r2.URL != a || r2.Width != 600 {
		t.Fatalf("同内容应返回首次登记的 URL: %v %+v", err, r2)
	}
	// 同一对象重复 confirm：幂等，仍是自己
	if r, err := u.Confirm(ctx, ConfirmInput{URL: a}); err != nil || r.URL != a {
		t.Fatalf("重复 confirm 同一对象: %v %+v", err, r)
	}
	// 内容不同：不去重
	f.statSize = int64(len(pngBytes(601, 700)))
	if r, err := u.Confirm(ctx, ConfirmInput{URL: other}); err != nil || r.URL != other {
		t.Fatalf("不同内容不应去重: %v %+v", err, r)
	}
}

func TestConfirmDedupDeletesDuplicateWhenStoreCanDelete(t *testing.T) {
	ctx := context.Background()
	img := pngBytes(600, 700)
	f := &fakeStore{statSize: int64(len(img)), statCT: "image/png"}
	ds := &delStore{fakeStore: f}
	a, b := objURL("a.png"), upBase+"/aigc/"+UploadDir+"/202611/"+strings.Repeat("b", 32)+".png"
	u := dedupUploader(f, map[string][]byte{a: img, b: img})
	u.store = ds
	u.cfg.Cleanup.Enabled = true
	if _, err := u.Confirm(ctx, ConfirmInput{URL: a}); err != nil {
		t.Fatal(err)
	}
	r, err := u.Confirm(ctx, ConfirmInput{URL: b})
	if err != nil || r.URL != a {
		t.Fatalf("%v %+v", err, r)
	}
	if len(ds.deleted) != 1 || !strings.HasSuffix(ds.deleted[0], strings.Repeat("b", 32)+".png") {
		t.Fatalf("应删除重复对象: %v", ds.deleted)
	}
}

func TestConfirmDedupFailsOpen(t *testing.T) {
	ctx := context.Background()
	img := pngBytes(600, 700)
	f := &fakeStore{statSize: int64(len(img)), statCT: "image/png"}
	u := dedupUploader(f, map[string][]byte{objURL("a.png"): img})
	u.open = func(context.Context, string, int64) (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }
	r, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")})
	if err != nil || r.URL != objURL("a.png") {
		t.Fatalf("算哈希失败应放行: %v %+v", err, r)
	}
}

func TestHashObjectLimit(t *testing.T) {
	u := &Uploader{open: func(context.Context, string, int64) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader("0123456789")), nil
	}}
	if _, err := u.hashObject(context.Background(), "x", 5); err == nil {
		t.Error("超过上限应拒绝去重")
	}
	h1, err := u.hashObject(context.Background(), "x", 10)
	if err != nil || len(h1) != 64 {
		t.Fatalf("%v %q", err, h1)
	}
}

type memOrphans struct{ rows []string }

func (m *memOrphans) Enqueue(_ context.Context, key, _ string, reason string) error {
	m.rows = append(m.rows, reason+":"+key)
	return nil
}

// 开关关闭（默认）：重复对象不删，只入待清理队列。
func TestConfirmDedupDuplicateEnqueuedWhenCleanupDisabled(t *testing.T) {
	ctx := context.Background()
	img := pngBytes(600, 700)
	f := &fakeStore{statSize: int64(len(img)), statCT: "image/png"}
	ds := &delStore{fakeStore: f}
	a, b := objURL("a.png"), upBase+"/aigc/"+UploadDir+"/202611/"+strings.Repeat("b", 32)+".png"
	u := dedupUploader(f, map[string][]byte{a: img, b: img})
	u.store = ds
	mo := &memOrphans{}
	u.orphans = mo
	if _, err := u.Confirm(ctx, ConfirmInput{URL: a}); err != nil {
		t.Fatal(err)
	}
	if r, err := u.Confirm(ctx, ConfirmInput{URL: b}); err != nil || r.URL != a {
		t.Fatalf("%v %+v", err, r)
	}
	if len(ds.deleted) != 0 {
		t.Fatalf("开关关闭不得删除: %v", ds.deleted)
	}
	if len(mo.rows) != 1 || !strings.HasPrefix(mo.rows[0], "DUPLICATE:") || !strings.HasSuffix(mo.rows[0], strings.Repeat("b", 32)+".png") {
		t.Fatalf("重复对象应入队: %v", mo.rows)
	}
}

// confirm 拒收且对象在桶里 → 入待清理队列（REJECTED）；对象不存在的拒收不入队。
func TestConfirmRejectedObjectEnqueued(t *testing.T) {
	ctx := context.Background()
	f := &fakeStore{statSize: 10, statCT: "application/pdf"}
	u := newTestUploader(f)
	mo := &memOrphans{}
	u.orphans = mo
	if _, err := u.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil {
		t.Fatal("类型不在白名单应拒收")
	}
	if len(mo.rows) != 1 || !strings.HasPrefix(mo.rows[0], "REJECTED:") {
		t.Fatalf("拒收对象应入队: %v", mo.rows)
	}
	f2 := &fakeStore{statErr: errors.New("404")}
	u2 := newTestUploader(f2)
	mo2 := &memOrphans{}
	u2.orphans = mo2
	if _, err := u2.Confirm(ctx, ConfirmInput{URL: objURL("a.png")}); err == nil || len(mo2.rows) != 0 {
		t.Fatalf("对象不存在不应入队: %v", mo2.rows)
	}
}

func TestDeletableKeyBoundary(t *testing.T) {
	u := newTestUploader(&fakeStore{})
	ok := "aigc/" + UploadDir + "/202611/" + strings.Repeat("a", 32) + ".png"
	if !u.deletable(ok) {
		t.Fatal("合规 key 应可删")
	}
	for _, bad := range []string{
		"", "aigc/", "aigc/image/1/20261101/" + strings.Repeat("a", 24) + ".png",
		"aigc/" + UploadDir + "/202611/../x.png", "other/" + UploadDir + "/202611/" + strings.Repeat("a", 32) + ".png",
		"aigc/x/" + UploadDir + "/202611/" + strings.Repeat("a", 32) + ".png", "aigc/" + UploadDir + "/202611/" + strings.Repeat("A", 32) + ".png",
	} {
		if u.deletable(bad) {
			t.Errorf("不应可删: %q", bad)
		}
	}
}

func TestCleanupConfigDefaults(t *testing.T) {
	var c CleanupConfig
	if c.Enabled || c.hashGraceDays() != 7 || c.orphanGraceHours() != 24 || c.batchSize() != 100 || c.maxDeletes() != 1000 {
		t.Fatalf("默认值不对: %+v", c)
	}
}

// touchDedup 可控 Touch 结果的登记表：gone=true 模拟登记行在 Find 之后被清理任务删掉（Touch 影响 0 行）。
type touchDedup struct {
	*memDedup
	gone     bool
	touchErr error
	touched  int
}

func (d *touchDedup) Touch(_ context.Context, sha string) (bool, error) {
	d.touched++
	if d.touchErr != nil {
		return false, d.touchErr
	}
	if d.gone {
		delete(d.m, sha) // 清理任务已删登记行
		return false, nil
	}
	return true, nil
}

// 中：去重命中后 Touch 影响 0 行（登记对象已被清理）→ 视为未命中，保留本次新对象并重新登记，不删它。
func TestConfirmDedupTouchMissKeepsNewObject(t *testing.T) {
	ctx := context.Background()
	img := pngBytes(600, 700)
	f := &fakeStore{statSize: int64(len(img)), statCT: "image/png"}
	ds := &delStore{fakeStore: f}
	a, b := objURL("a.png"), upBase+"/aigc/"+UploadDir+"/202611/"+strings.Repeat("b", 32)+".png"
	u := dedupUploader(f, map[string][]byte{a: img, b: img})
	u.store = ds
	u.cfg.Cleanup.Enabled = true
	td := &touchDedup{memDedup: u.dedup.(*memDedup)}
	u.dedup = td
	if _, err := u.Confirm(ctx, ConfirmInput{URL: a}); err != nil {
		t.Fatal(err)
	}
	td.gone = true
	r, err := u.Confirm(ctx, ConfirmInput{URL: b})
	if err != nil || r.URL != b {
		t.Fatalf("Touch 影响 0 行应视为未命中、返回本次对象: %v %+v", err, r)
	}
	if len(ds.deleted) != 0 {
		t.Fatalf("未命中不得删除本次新对象: %v", ds.deleted)
	}
	var sha string
	for k := range td.m {
		sha = k
	}
	if len(td.m) != 1 || td.m[sha].URL != b {
		t.Fatalf("本次对象应重新登记: %+v", td.m)
	}
	// Touch 报错：判不出登记对象是否还在，同样保留本次对象
	td.gone, td.touchErr = false, errors.New("db down")
	c := upBase + "/aigc/" + UploadDir + "/202611/" + strings.Repeat("c", 32) + ".png"
	u.open = func(_ context.Context, raw string, _ int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(img)), nil
	}
	u.fetch = func(context.Context, string, int64) ([]byte, error) { return img, nil }
	if r, err := u.Confirm(ctx, ConfirmInput{URL: c}); err != nil || r.URL != c || len(ds.deleted) != 0 {
		t.Fatalf("Touch 失败应保留本次对象: %v %+v %v", err, r, ds.deleted)
	}
	// Touch 正常：照常复用并删重复对象
	td.touchErr = nil
	d := upBase + "/aigc/" + UploadDir + "/202611/" + strings.Repeat("d", 32) + ".png"
	if r, err := u.Confirm(ctx, ConfirmInput{URL: d}); err != nil || r.URL != b || len(ds.deleted) != 1 {
		t.Fatalf("命中仍应复用登记对象并删重复: %v %+v %v", err, r, ds.deleted)
	}
}

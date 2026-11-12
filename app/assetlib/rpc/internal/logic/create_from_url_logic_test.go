package logic

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/config"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

const (
	testBase   = "https://cdn.example.com"
	testPrefix = "aigc-local-life"
	libURL     = "https://lib.example.com/asset/abc.png"
	sha        = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func testOwns(u string) bool { return strings.HasPrefix(u, testBase+"/"+testPrefix+"/") }

func TestOwnUploadURL(t *testing.T) {
	ok := testBase + "/" + testPrefix + "/image/7/20261112/aa11.png"
	if got, good := ownUploadURL(testOwns, testPrefix, 7, ok+"?x=1#f"); !good || got != ok {
		t.Fatalf("本人上传应放行并去掉 query：%q %v", got, good)
	}
	bad := map[string]string{
		"别人的上传":     testBase + "/" + testPrefix + "/image/8/20261112/aa11.png",
		"视频目录":      testBase + "/" + testPrefix + "/video/7/20261112/aa11.png",
		"用户段是前缀":    testBase + "/" + testPrefix + "/image/77/20261112/aa11.png",
		"外链":        "https://evil.example.org/" + testPrefix + "/image/7/20261112/aa11.png",
		"相似域名":      "https://cdn.example.com.evil.io/" + testPrefix + "/image/7/a/b.png",
		"路径穿越":      testBase + "/" + testPrefix + "/image/7/../8/aa11.png",
		"编码穿越":      testBase + "/" + testPrefix + "/image/7/%2e%2e/8/aa11.png",
		"带账号":       "https://u:p@cdn.example.com/" + testPrefix + "/image/7/20261112/aa11.png",
		"层级不足":      testBase + "/" + testPrefix + "/image/7/aa11.png",
		"空段":        testBase + "/" + testPrefix + "/image/7//aa11.png",
		"mr 引用":     "mr://rid.png",
		"data":      "data:image/png;base64,AAAA",
		"非 http":    "ftp://cdn.example.com/" + testPrefix + "/image/7/a/b.png",
		"反斜杠":       testBase + "/" + testPrefix + "/image/7\\..\\8/a/b.png",
		"路径里嵌了他人前缀": testBase + "/" + testPrefix + "/image/8/" + testPrefix + "/image/7/a.png",
	}
	for name, u := range bad {
		if got, good := ownUploadURL(testOwns, testPrefix, 7, u); good {
			t.Errorf("%s 应被拒：%q", name, got)
		}
	}
	if _, good := ownUploadURL(nil, testPrefix, 7, ok); good {
		t.Error("owns 为 nil 一律拒")
	}
	if _, good := ownUploadURL(testOwns, testPrefix, 0, ok); good {
		t.Error("uid<=0 一律拒")
	}
}

func TestAspectRatioOf(t *testing.T) {
	for _, c := range []struct {
		w, h int
		want string
	}{{1024, 1024, "1:1"}, {1080, 1440, "3:4"}, {1920, 1080, "16:9"}, {800, 1000, "4:5"}, {1000, 1200, ""}, {0, 5, ""}} {
		if got := aspectRatioOf(c.w, c.h); got != c.want {
			t.Errorf("%dx%d = %q want %q", c.w, c.h, got, c.want)
		}
	}
}

// ---- 假实现 ----

type fakeStore struct {
	urlID      int64 // IDByURL 命中的资产 id（按用户传入的地址）
	libRefID   int64 // 已有资产引用了素材库对象地址 libURL 时的 id
	onceID     int64
	onceNew    bool
	onceErr    error
	onceCalled bool
	gotKey     string
}

func (f *fakeStore) IDByURL(_ context.Context, _ int64, u string) (int64, error) {
	if u == libURL {
		return f.libRefID, nil
	}
	return f.urlID, nil
}
func (f *fakeStore) CreateOnce(_ context.Context, _ int64, key string, _ func(context.Context, *repo.Repo) (int64, error)) (int64, bool, error) {
	f.onceCalled, f.gotKey = true, key
	return f.onceID, f.onceNew, f.onceErr
}

type fakeEnv struct {
	store     *fakeStore
	ingested  []string
	removed   []string
	removeErr error
	res       mediasvc.Resource
	ingErr    error
	d         *fromURLDeps
}

func newFake() *fakeEnv {
	e := &fakeEnv{store: &fakeStore{onceID: 55, onceNew: true},
		res: mediasvc.Resource{RID: "r1", SHA256: sha, Mime: "image/png", Bytes: 2048, Width: 800, Height: 1000, URL: libURL}}
	e.d = &fromURLDeps{
		store: e.store,
		ownsUpload: func(uid int64, u string) (string, bool) {
			return ownUploadURL(testOwns, testPrefix, uid, u)
		},
		ingest: func(_ context.Context, src, _ string) (mediasvc.Resource, error) {
			e.ingested = append(e.ingested, src)
			return e.res, e.ingErr
		},
		remove: func(_ context.Context, rid string) error {
			e.removed = append(e.removed, rid)
			return e.removeErr
		},
		load: func(_ context.Context, _, id int64) (*assetlib.Asset, error) {
			return &assetlib.Asset{Id: id, Image: libURL}, nil
		},
		now: func() time.Time { return time.UnixMilli(1762900000000) },
	}
	e.d.cfg.Limits.TitleLen, e.d.cfg.Limits.TagNameLen, e.d.cfg.Limits.TagsPerAsset = 100, 20, 10
	return e
}

func ownURL(uid string) string {
	return testBase + "/" + testPrefix + "/image/" + uid + "/20261112/aa11.png"
}

func wantCode(t *testing.T, err error, code int, sub string) {
	t.Helper()
	c, msg, ok := xerr.Parse(err)
	if !ok || c != code || !strings.Contains(msg, sub) {
		t.Fatalf("want code=%d msg~%q, got code=%d msg=%q err=%v", code, sub, c, msg, err)
	}
}

func TestCreateFromURLHappyPath(t *testing.T) {
	e := newFake()
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7") + "?v=1", Source: "canvas", CanvasId: "c1", NodeId: "n1", Tags: []string{"画布"}})
	if err != nil || out.Existing || out.Asset.Id != 55 || out.Asset.Image != libURL {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	// 媒体服务拿到的是去掉 query 的本人上传地址
	if len(e.ingested) != 1 || e.ingested[0] != ownURL("7") {
		t.Fatalf("ingested=%v", e.ingested)
	}
	// 去重键 = 内容哈希前 32 位
	if e.store.gotKey != "cu:"+sha[:32] {
		t.Fatalf("key=%q", e.store.gotKey)
	}
}

func TestCreateFromURLRejectsForeignAndExternal(t *testing.T) {
	for name, u := range map[string]string{
		"别人的上传": ownURL("8"),
		"外链":    "https://evil.example.org/a.png",
		"私有引用":  "mr://rid.png",
		"别人素材库": libURL, // 白名单 / 公共地址不是「本人」依据；IDByURL 没命中
	} {
		e := newFake()
		_, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: u})
		if err == nil {
			t.Fatalf("%s 应被拒", name)
		}
		wantCode(t, err, xerr.ParamError, "来源无法确认")
		if len(e.ingested) != 0 || e.store.onceCalled {
			t.Fatalf("%s：拒收后不能碰媒体服务 / 库", name)
		}
	}
}

func TestCreateFromURLAlreadyInLibrary(t *testing.T) {
	e := newFake()
	e.store.urlID, e.store.libRefID = 9, 9
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: libURL})
	if err != nil || !out.Existing || out.Asset.Id != 9 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(e.ingested) != 0 || e.store.onceCalled {
		t.Fatal("已在本人资产库：不复制、不写库")
	}
}

func TestCreateFromURLDedupeByContent(t *testing.T) {
	e := newFake()
	e.store.onceID, e.store.onceNew = 31, false // 库里已有同内容资产
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if err != nil || !out.Existing || out.Asset.Id != 31 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestCreateFromURLValidation(t *testing.T) {
	ctx := context.Background()
	e := newFake()
	run := func(in *assetlib.CreateFromUrlReq) error { _, err := e.d.run(ctx, in); return err }
	if err := run(&assetlib.CreateFromUrlReq{Url: ownURL("7")}); err == nil {
		t.Fatal("未登录应拒")
	} else {
		wantCode(t, err, xerr.Unauthorized, "")
	}
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7}), xerr.ParamMissing, "缺少图片地址")
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7, Url: "blob:http://x/1"}), xerr.ParamError, "临时地址")
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7, Url: "data:image/png;base64,AA"}), xerr.ParamError, "临时地址")
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7"), Source: "upload"}), xerr.ParamError, "source")
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7"), Title: strings.Repeat("字", 101)}), xerr.ParamError, "标题最多")
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7"), Tags: []string{strings.Repeat("a", 21)}}), xerr.ParamError, "标签名")
	wantCode(t, run(&assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7"), CanvasId: strings.Repeat("c", 65)}), xerr.ParamError, "canvas_id")
	if len(e.ingested) != 0 {
		t.Fatal("参数不合法时不能碰媒体服务")
	}
}

func TestCreateFromURLTypeSizeAndMedia(t *testing.T) {
	ctx := context.Background()
	in := &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")}

	e := newFake()
	e.res.Mime = "video/mp4"
	_, err := e.d.run(ctx, in)
	wantCode(t, err, xerr.ParamError, "只能存图片")
	if e.store.onceCalled {
		t.Fatal("非图片不能入库")
	}

	e = newFake()
	e.res.Bytes = 11 << 20
	_, err = e.d.run(ctx, in)
	wantCode(t, err, xerr.ParamError, "10 MB")

	e = newFake()
	e.d.cfg.Limits.FromURLBytes = 1024
	_, err = e.d.run(ctx, in)
	wantCode(t, err, xerr.ParamError, "0 MB") // 配置上限生效（1024 字节 >>20 = 0）

	e = newFake()
	e.ingErr = &mediasvc.Error{Status: 422, Code: "decode"}
	_, err = e.d.run(ctx, in)
	wantCode(t, err, xerr.ParamError, "不是有效的图片")

	e = newFake()
	e.ingErr = errors.New("connection refused")
	_, err = e.d.run(ctx, in)
	wantCode(t, err, xerr.ServerError, "稍后再试")

	for name, res := range map[string]string{"空地址": "", "mr 引用": "mr://r1.png"} {
		e = newFake()
		e.res.URL = res
		_, err = e.d.run(ctx, in)
		if err == nil || e.store.onceCalled {
			t.Fatalf("%s：afv_asset 只写公开地址，不能入库", name)
		}
	}

	e = newFake()
	e.d.ingest = nil
	_, err = e.d.run(ctx, in)
	wantCode(t, err, xerr.NotReady, "媒体服务")

	e = newFake()
	e.store.onceErr = repo.ErrLockBusy
	_, err = e.d.run(ctx, in)
	wantCode(t, err, xerr.Conflict, "正在保存")
}

// ---- 真实 repo + sqlmock：命名锁、查重、建行、旁表 ----

func newOnceDeps(t *testing.T) (*fakeEnv, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	e := newFake()
	e.d.store = nil
	e.d.repo = repo.New(bun.NewDB(sdb, mysqldialect.New()), nil)
	e.d.cfg = config.Config{FromURLEntryCode: ""}
	e.d.cfg.Limits.TitleLen, e.d.cfg.Limits.TagNameLen, e.d.cfg.Limits.TagsPerAsset = 100, 20, 10
	return e, mock
}

func TestCreateFromURLInsertsUploadRow(t *testing.T) {
	e, mock := newOnceDeps(t)
	key := "cu:" + sha[:32]
	mock.ExpectQuery("a.result_url = ").WillReturnRows(sqlmock.NewRows([]string{"id"})) // 不在本人资产库里
	mock.ExpectQuery(`SELECT GET_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow(1))
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT `a`.`id` FROM afv_asset AS a WHERE \\(a.source_task_id = '" + key + "'\\)").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	mock.ExpectQuery("FROM `afv_workspace`").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(3))
	// source_type = 1（用户上传）、status = 1、cover / result 都是素材库公开地址、比例 4:5
	mock.ExpectExec("INSERT INTO afv_asset .*VALUES \\(7, 3, '" + key + "', 'image', '', '画布图片', '" + regexp.QuoteMeta(libURL) + "', '" + regexp.QuoteMeta(libURL) + "', 800, 1000, '4:5', '.*', 1, 1\\)").
		WillReturnResult(sqlmock.NewResult(55, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT RELEASE_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if err != nil || out.Existing || out.Asset.Id != 55 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 库里已有同内容资产（重复点击）：持锁查到就返回，不建第二行，锁照常释放。
func TestCreateFromURLSecondClickReturnsExisting(t *testing.T) {
	e, mock := newOnceDeps(t)
	key := "cu:" + sha[:32]
	mock.ExpectQuery("a.result_url = ").WillReturnRows(sqlmock.NewRows([]string{"id"})) // 不在本人资产库里
	mock.ExpectQuery(`SELECT GET_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow(1))
	mock.ExpectBegin()
	mock.ExpectQuery("a.source_task_id = '" + key + "'").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(41))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT RELEASE_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
	mock.ExpectQuery("a.result_url = ").WillReturnRows(sqlmock.NewRows([]string{"id"})) // 清理前确认新对象没被资产引用
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if err != nil || !out.Existing || out.Asset.Id != 41 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(e.removed) != 1 || e.removed[0] != "r1" {
		t.Fatalf("重复对象应被清理：%v", e.removed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 抢不到命名锁：不开事务、不写库，报「正在保存」。
func TestCreateFromURLLockBusy(t *testing.T) {
	e, mock := newOnceDeps(t)
	mock.ExpectQuery("a.result_url = ").WillReturnRows(sqlmock.NewRows([]string{"id"})) // 不在本人资产库里
	mock.ExpectQuery(`SELECT GET_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow(0))
	_, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	wantCode(t, err, xerr.Conflict, "正在保存")
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ---- 孤儿对象清理 ----

// 每个「已落桶但不入库」的出口都要删掉刚落的对象。
func TestCreateFromURLCleansIngestedOnEveryFailurePath(t *testing.T) {
	ctx := context.Background()
	in := &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")}
	cases := map[string]func(e *fakeEnv){
		"非图片":    func(e *fakeEnv) { e.res.Mime = "video/mp4" },
		"超大":     func(e *fakeEnv) { e.res.Bytes = 11 << 20 },
		"配置上限":   func(e *fakeEnv) { e.d.cfg.Limits.FromURLBytes = 1024 },
		"没有公开地址": func(e *fakeEnv) { e.res.URL = "" },
		"mr 引用":  func(e *fakeEnv) { e.res.URL = "mr://r1.png" },
		"哈希缺失":   func(e *fakeEnv) { e.res.SHA256 = "abc" },
		"入库失败":   func(e *fakeEnv) { e.store.onceErr = errors.New("insert boom") },
		"内容重复":   func(e *fakeEnv) { e.store.onceID, e.store.onceNew = 31, false },
	}
	for name, mut := range cases {
		e := newFake()
		mut(e)
		_, err := e.d.run(ctx, in)
		if name == "内容重复" {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		} else if err == nil {
			t.Fatalf("%s 应失败", name)
		}
		if len(e.removed) != 1 || e.removed[0] != "r1" {
			t.Fatalf("%s：应删除刚落桶的对象 r1，removed=%v", name, e.removed)
		}
	}
}

// 拒收在落桶之前（归属 / 参数）：没有对象，更不能调删除。
func TestCreateFromURLNoCleanupBeforeIngest(t *testing.T) {
	e := newFake()
	_, _ = e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("8")})
	e.ingErr = errors.New("down")
	_, _ = e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if len(e.removed) != 0 {
		t.Fatalf("没有落桶不应删除：%v", e.removed)
	}
}

// 抢锁失败：持锁方可能正拿着同一个对象建行，不能删。
func TestCreateFromURLLockBusyKeepsObject(t *testing.T) {
	e := newFake()
	e.store.onceErr = repo.ErrLockBusy
	_, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	wantCode(t, err, xerr.Conflict, "正在保存")
	if len(e.removed) != 0 {
		t.Fatalf("removed=%v", e.removed)
	}
}

// 同一地址重试：对象与已有资产是同一个（资产引用了该地址），去重命中也不能删，否则已有资产变死链。
func TestCreateFromURLDedupeSameObjectNotDeleted(t *testing.T) {
	e := newFake()
	e.store.onceID, e.store.onceNew, e.store.libRefID = 31, false, 31
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if err != nil || !out.Existing || out.Asset.Id != 31 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(e.removed) != 0 {
		t.Fatalf("被已有资产使用的对象不能删：%v", e.removed)
	}
}

// 同内容不同地址：新落的是重复对象 → 复用已有资产并删掉重复对象；删除失败只记日志，不影响结果。
func TestCreateFromURLDuplicateContentReusesAssetAndDeletesDuplicate(t *testing.T) {
	e := newFake()
	e.store.onceID, e.store.onceNew = 31, false
	e.removeErr = errors.New("media down")
	out, err := e.d.run(context.Background(), &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if err != nil || !out.Existing || out.Asset.Id != 31 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(e.removed) != 1 || e.removed[0] != "r1" {
		t.Fatalf("removed=%v", e.removed)
	}
}

// ---- 命名锁 / 单连接 ----

func TestLockNameLengthAndStability(t *testing.T) {
	for _, uid := range []int64{1, 9223372036854775807} {
		for _, key := range []string{"cu:" + sha[:32], strings.Repeat("k", 500)} {
			n := repo.LockName(uid, key)
			if len(n) > 64 || !strings.HasPrefix(n, "afv_asset_once:") || n != repo.LockName(uid, key) {
				t.Fatalf("lock name %q (%d)", n, len(n))
			}
		}
	}
	if repo.LockName(1, "ab") == repo.LockName(11, "b") || repo.LockName(1, "a") == repo.LockName(2, "a") {
		t.Fatal("不同 (uid,key) 必须得到不同锁名")
	}
}

// 连接池只有 1 条：GET_LOCK、事务、RELEASE_LOCK 必须共用这一条，否则（旧实现）会等死。
func TestCreateOnceUsesSingleConnection(t *testing.T) {
	e, mock := newOnceDeps(t)
	e.d.repo = nil
	sdb, m2, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	_ = mock
	sdb.SetMaxOpenConns(1)
	e.d.repo = repo.New(bun.NewDB(sdb, mysqldialect.New()), nil)
	key := "cu:" + sha[:32]
	m2.ExpectQuery("a.result_url = ").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	m2.ExpectQuery(`SELECT GET_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow(1))
	m2.ExpectBegin()
	m2.ExpectQuery("a.source_task_id = '" + key + "'").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(41))
	m2.ExpectCommit()
	m2.ExpectQuery(`SELECT RELEASE_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"r"}).AddRow(1))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := e.d.run(ctx, &assetlib.CreateFromUrlReq{UserId: 7, Url: ownURL("7")})
	if err != nil || out.Asset.Id != 41 {
		t.Fatalf("单连接池下应能完成（旧实现会等死）：out=%+v err=%v", out, err)
	}
	if err := m2.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if st := sdb.Stats(); st.OpenConnections > 1 {
		t.Fatalf("连接数=%d", st.OpenConnections)
	}
}

// RELEASE_LOCK 失败：连接被丢弃（不回池），池里随后能拿到新连接。
func TestCreateOnceDiscardsConnWhenReleaseFails(t *testing.T) {
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	sdb.SetMaxOpenConns(1)
	r := repo.New(bun.NewDB(sdb, mysqldialect.New()), nil)
	mock.ExpectQuery(`SELECT GET_LOCK`).WillReturnRows(sqlmock.NewRows([]string{"l"}).AddRow(1))
	mock.ExpectBegin()
	mock.ExpectQuery("a.source_task_id").WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(5))
	mock.ExpectCommit()
	mock.ExpectQuery(`SELECT RELEASE_LOCK`).WillReturnError(errors.New("conn reset"))
	id, created, err := r.CreateOnce(context.Background(), 7, "k", nil)
	if err != nil || created || id != 5 {
		t.Fatalf("id=%d created=%v err=%v", id, created, err)
	}
	if st := sdb.Stats(); st.InUse != 0 || st.OpenConnections != 0 {
		t.Fatalf("连接应被丢弃而不是带锁回池：%+v", st)
	}
}

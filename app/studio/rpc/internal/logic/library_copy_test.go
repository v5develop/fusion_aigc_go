package logic

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

const (
	libRID = "abcdef-asset-cccccccccccccccccccccccccc" // copyMedia 复制出的素材库 rid
	libURL = "https://cdn/x.png"
)

var paidRef = "mr://" + wmRID + ".png"

// errMedia 转存按次返回错误的媒体服务假实现（errs 用完后成功）。
type errMedia struct {
	copyMedia
	errs []error
}

func (m *errMedia) CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error) {
	if len(m.errs) > 0 {
		err := m.errs[0]
		m.errs = m.errs[1:]
		m.copies = append(m.copies, rid+"|"+purpose+"|"+idem)
		return mediasvc.Resource{}, err
	}
	return m.copyMedia.CopyToLibrary(ctx, rid, purpose, idem)
}

// asyncFlow 付费档 + 开启异步转存的测试 flow。
func asyncFlow(t *testing.T) (*flow, sqlmock.Sqlmock, *fakeFlowBill, *copyMedia, *[]asset.Asset) {
	t.Helper()
	bill := onBill()
	f, mock, _ := newTestFlow(t, bill)
	media := &copyMedia{}
	f.media = media
	f.tier = fakeTier{free: false}
	f.cfg.FreeWatermark = wmOn
	f.cfg.Library.AsyncCopy = true
	f.cfg.Asset.Enabled = true
	saved := &[]asset.Asset{}
	f.asset = func(_ context.Context, list []asset.Asset) error { *saved = append(*saved, list...); return nil }
	return f, mock, bill, media, saved
}

var (
	qJobInsert = regexp.QuoteMeta("INSERT IGNORE INTO `afv_studio_copy_job` (`task_pk`, `task_id`, `user_id`, `src_rid`, `ref`, `status`, `attempt`, `next_at`) VALUES (1, 'simg_1', 7, '" +
		wmRID + "', '" + paidRef + "', 0, 0, NOW(3) + INTERVAL 10 SECOND)")
	qTask1 = regexp.QuoteMeta("WHERE (t.id = 1) LIMIT 1")
)

func taskRowRef(status int8, url string, deleted int8) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_rid", "result_url", "deleted"}).
		AddRow(1, 9, 7, "imagegen", "simg_1", status, wmRID, url, deleted)
}

func pendingJob(attempt int32) *model.CopyJob {
	return &model.CopyJob{ID: 5, TaskPK: 1, TaskID: "simg_1", UserID: 7, SrcRid: wmRID, Ref: paidRef, Attempt: attempt, CreatedAt: time.Now()}
}

func qJobUpdate(set string, attempt int) string {
	return regexp.QuoteMeta("UPDATE `afv_studio_copy_job` SET "+set) + ".*" + regexp.QuoteMeta("WHERE (id = 5) AND (status = 0) AND (attempt = "+strconv.Itoa(attempt)+")")
}

// 付费档先交付：不同步转存，成品以 mr:// 进交付中并同事务落 outbox → 结算一次 → 置成功（用户立即可见），素材库此时不写。
func TestDeliverPaidAsyncVisibleBeforeCopy(t *testing.T) {
	f, mock, bill, media, saved := asyncFlow(t)
	ref := regexp.QuoteMeta("'" + paidRef + "'")
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_rid` = '" + wmRID + "'.*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectExec(qJobInsert).WillReturnResult(ok1)
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
	met(t, mock)
	if len(media.copies) != 0 {
		t.Fatalf("交付路径上不应转存: %v", media.copies)
	}
	if len(*saved) != 0 {
		t.Fatalf("素材库不能写 mr://: %+v", *saved)
	}
	if !reflect.DeepEqual(bill.settles, []string{"studio-simg_1"}) || len(bill.fails) != 0 {
		t.Fatalf("应结算一次且不退款: settles=%v fails=%v", bill.settles, bill.fails)
	}
}

// 落 outbox 失败（Go_V1.0.30 未执行）：事务回滚，本次退回同步转存（旧行为：转存 → 交付中 → 结算 → 成功）。
func TestDeliverPaidAsyncFallbackWhenOutboxMissing(t *testing.T) {
	f, mock, bill, media, _ := asyncFlow(t)
	f.cfg.Asset.Enabled = false
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("INSERT IGNORE INTO `afv_studio_copy_job`")).WillReturnError(&mysql.MySQLError{Number: 1146, Message: "Table doesn't exist"})
	mock.ExpectRollback()
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_url` = '" + libURL + "'").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = '" + libURL + "'").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
	met(t, mock)
	if !reflect.DeepEqual(media.copies, []string{wmRID + "|asset_library|"}) || len(bill.settles) != 1 {
		t.Fatalf("应退回同步转存: copies=%v settles=%v", media.copies, bill.settles)
	}
}

// 免费档不受影响：仍写 mrw://、同步入素材库、不落 outbox。
func TestDeliverFreeTierIgnoresAsyncCopy(t *testing.T) {
	f, mock, _, media, saved := asyncFlow(t)
	f.tier = fakeTier{free: true}
	ref := regexp.QuoteMeta("'mrw://" + wmRID + ".png'")
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = " + ref).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, freeCall(), time.Now())
	met(t, mock)
	if len(media.copies) != 0 || len(*saved) != 1 || (*saved)[0].ResultURL != "mrw://"+wmRID+".png" {
		t.Fatalf("免费档应照旧: copies=%v saved=%+v", media.copies, *saved)
	}
}

// 续做交付（交付中、result_url 已是 mr://）：结算 → 置成功，不入素材库、不转存。
func TestResumeDeliverPendingRef(t *testing.T) {
	f, mock, bill, media, saved := asyncFlow(t)
	f.gwOn = false
	tk := runningTask()
	tk.Status = model.TaskDelivering
	tk.ResultRid.String, tk.ResultRid.Valid = wmRID, true
	tk.ResultURL.String, tk.ResultURL.Valid = paidRef, true
	tk.UpdatedAt = time.Now()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = '" + regexp.QuoteMeta(paidRef) + "'").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.resumeDeliver(context.Background(), tk, batch9, time.Now())
	met(t, mock)
	if len(media.copies) != 0 || len(*saved) != 0 || len(bill.settles) != 1 {
		t.Fatalf("copies=%v saved=%v settles=%v", media.copies, *saved, bill.settles)
	}
}

// 后台转存成功：认领 → 复制（幂等键 studio-copy:{rid}）→ 入素材库（公开地址）→ CAS 换子任务成品 → 完成；不动账。
func TestRunCopyJobDone(t *testing.T) {
	f, mock, bill, media, saved := asyncFlow(t)
	synced := 0
	f.syncSoon = func() { synced++ }
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET result_rid = '" + libRID + "', result_url = '" + libURL +
		"' WHERE (id = 1) AND (status = 2) AND (deleted = 0) AND (result_url = '" + paidRef + "')")).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1, finished_at = NOW(3)", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if !reflect.DeepEqual(media.copies, []string{wmRID + "|asset_library|studio-copy:" + wmRID}) {
		t.Fatalf("copies=%v", media.copies)
	}
	if len(*saved) != 1 || (*saved)[0].ResultURL != libURL || (*saved)[0].CoverURL != libURL || (*saved)[0].SourceTaskID != "simg_1" {
		t.Fatalf("素材库应写公开地址: %+v", *saved)
	}
	if len(bill.settles) != 0 || len(bill.fails) != 0 || len(bill.confirmed) != 0 {
		t.Fatal("转存不应动账")
	}
	if synced != 1 {
		t.Fatal("换地址后应尽快同步任务中心")
	}
}

// 幂等：上一轮已入素材库、换地址前崩溃 → 重做时查重命中不再写第二条，照常换地址。
func TestRunCopyJobIdempotentAfterAssetWritten(t *testing.T) {
	f, mock, _, media, saved := asyncFlow(t)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 2)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 3)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("simg_1", 88))
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 3)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET result_rid")).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1", 3)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(2))
	met(t, mock)
	if len(*saved) != 0 || len(media.copies) != 1 {
		t.Fatalf("不应重复入库: saved=%v copies=%v", *saved, media.copies)
	}
}

// 子任务还在交付中（结算未完）：只推迟，不认领、不复制、不占尝试次数。
func TestRunCopyJobWaitsWhileDelivering(t *testing.T) {
	f, mock, _, media, _ := asyncFlow(t)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskDelivering, paidRef, 0))
	mock.ExpectExec(qJobUpdate("next_at = NOW(3) + INTERVAL 15 SECOND", 0)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 {
		t.Fatal("交付未完成不能复制到公有桶")
	}
}

// 子任务交付失败（成品已清）/ 已删除 / 已重新生成：作废，不复制。
func TestRunCopyJobVoid(t *testing.T) {
	cases := map[string]*sqlmock.Rows{
		"failed":      taskRowRef(model.TaskFailed, "", 0),
		"deleted":     taskRowRef(model.TaskSucceeded, paidRef, 1),
		"regenerated": taskRowRef(model.TaskSucceeded, "mr://other.png", 0),
		"gone":        sqlmock.NewRows([]string{"id"}),
	}
	for name, rows := range cases {
		t.Run(name, func(t *testing.T) {
			f, mock, _, media, saved := asyncFlow(t)
			mock.ExpectQuery(qTask1).WillReturnRows(rows)
			mock.ExpectExec(qJobUpdate("status = 3", 0)).WillReturnResult(ok1)
			f.runCopyJob(context.Background(), pendingJob(0))
			met(t, mock)
			if len(media.copies) != 0 || len(*saved) != 0 {
				t.Fatal("作废不应复制 / 入库")
			}
		})
	}
}

// 复制期间被重新生成：入库前复查发现成品已换 → 作废，不入素材库、不换地址。
func TestRunCopyJobVoidWhenChangedDuringCopy(t *testing.T) {
	f, mock, _, _, saved := asyncFlow(t)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskRunning, "", 0))
	mock.ExpectExec(qJobUpdate("status = 3", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(*saved) != 0 {
		t.Fatal("不应入库")
	}
}

// 认领没抢到（别的实例在做）：什么都不做。
func TestRunCopyJobClaimLost(t *testing.T) {
	f, mock, _, media, _ := asyncFlow(t)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok0)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 {
		t.Fatal("没认领到不应复制")
	}
}

// 复制后认领已被接手（复制太慢、超过认领时长）：旧持有者不入库、不换地址。
func TestRunCopyJobRenewLost(t *testing.T) {
	f, mock, _, _, saved := asyncFlow(t)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok0)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(*saved) != 0 {
		t.Fatal("认领已丢不应入库")
	}
}

// 暂时失败按退避重试；到上限 / 4xx 记永久失败：成品留 mr://（不换地址、不入库），不退款。
func TestRunCopyJobRetryAndPermanentFailure(t *testing.T) {
	t.Run("temporary", func(t *testing.T) {
		f, mock, bill, _, _ := asyncFlow(t)
		f.media = &errMedia{errs: []error{&mediasvc.Error{Status: 503, Code: "busy"}}}
		mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
		mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
		mock.ExpectExec(qJobUpdate("next_at = NOW(3) + INTERVAL 5 SECOND", 1)).WillReturnResult(ok1)
		f.runCopyJob(context.Background(), pendingJob(0))
		met(t, mock)
		if len(bill.fails) != 0 {
			t.Fatal("转存失败不退款")
		}
	})
	for name, c := range map[string]struct {
		err     error
		attempt int32
	}{
		"exhausted": {errors.New("timeout"), 9},
		"4xx":       {&mediasvc.Error{Status: 404, Code: "not_found"}, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f, mock, bill, _, saved := asyncFlow(t)
			f.media = &errMedia{errs: []error{c.err}}
			mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
			mock.ExpectExec(qJobUpdate("attempt = attempt + 1", int(c.attempt))).WillReturnResult(ok1)
			mock.ExpectExec(qJobUpdate("status = 2, finished_at = NOW(3)", int(c.attempt)+1)).WillReturnResult(ok1)
			mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
			f.runCopyJob(context.Background(), pendingJob(c.attempt))
			met(t, mock)
			if len(*saved) != 0 || len(bill.fails) != 0 || len(bill.confirmed) != 0 {
				t.Fatalf("永久失败：不入库、不动账 saved=%v fails=%v", *saved, bill.fails)
			}
		})
	}
}

// 素材库写失败：重试（不换地址，成品留 mr:// 可看）。
func TestRunCopyJobAssetFailureRetries(t *testing.T) {
	f, mock, _, _, _ := asyncFlow(t)
	f.asset = func(context.Context, []asset.Asset) error { return errors.New("db blip") }
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(qJobUpdate("next_at = NOW(3) + INTERVAL 5 SECOND", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
}

// saveAssetErr 拒绝 mr://（Java 侧多个素材库读口不解析私有引用）。
func TestSaveAssetRejectsPrivateRef(t *testing.T) {
	f, _, _, _, saved := asyncFlow(t)
	if err := f.saveAssetErr(context.Background(), runningTask(), batch9, paidRef); err == nil || len(*saved) != 0 {
		t.Fatal("素材库不能写 mr://")
	}
}

// library_status：成功且 mr:// → 按 outbox 判 pending / failed；公开地址 / 水印视图 / 非成功为空。
func TestLibraryStatusOf(t *testing.T) {
	ok := int32(model.TaskSucceeded)
	cases := []struct {
		status int32
		url    string
		st     int8
		has    bool
		want   string
	}{
		{ok, paidRef, model.CopyPending, true, LibraryPending},
		{ok, paidRef, 0, false, LibraryPending},
		{ok, paidRef, model.CopyFailed, true, LibraryFailed},
		{ok, paidRef, model.CopyVoid, true, LibraryFailed},
		{ok, libURL, 0, false, ""},
		{ok, "mrw://" + wmRID + ".png", 0, false, ""},
		{int32(model.TaskDelivering), paidRef, 0, false, ""},
	}
	for i, c := range cases {
		if got := libraryStatusOf(c.status, c.url, c.st, c.has); got != c.want {
			t.Errorf("#%d got %q want %q", i, got, c.want)
		}
	}
}

// attachAssetIDs 一并填 library_status（只查成品仍是 mr:// 的成功子任务，按引用匹配）。
func TestAttachLibraryStatus(t *testing.T) {
	r, mock := newMockRepo(t)
	tasks := []*studio.TaskInfo{
		{Id: 1, TaskId: "simg_1", Status: int32(model.TaskSucceeded), ResultUrl: paidRef},
		{Id: 2, TaskId: "simg_2", Status: int32(model.TaskSucceeded), ResultUrl: "mr://" + wmRID2 + ".png"},
		{Id: 3, TaskId: "simg_3", Status: int32(model.TaskSucceeded), ResultUrl: libURL},
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT `task_pk`, `ref`, `status` FROM `afv_studio_copy_job` WHERE (task_pk IN (")).
		WillReturnRows(sqlmock.NewRows([]string{"task_pk", "ref", "status"}).
			AddRow(1, paidRef, model.CopyPending).AddRow(2, "mr://"+wmRID2+".png", model.CopyFailed).AddRow(2, "mr://old.png", model.CopyDone))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("simg_3", 90))
	attachAssetIDs(context.Background(), r, 7, tasks)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if tasks[0].LibraryStatus != LibraryPending || tasks[0].AssetId != 0 ||
		tasks[1].LibraryStatus != LibraryFailed || tasks[2].LibraryStatus != "" || tasks[2].AssetId != 90 {
		t.Fatalf("got %+v %+v %+v", tasks[0], tasks[1], tasks[2])
	}
}

// fakeBothSigner 普通签名 + 带处理参数签名。
type fakeBothSigner struct {
	fakeSigner
	plain []string
	bad   map[string]bool
}

func (s *fakeBothSigner) ResourceURLs(_ context.Context, rids []string, _ time.Duration) (map[string]string, error) {
	out := map[string]string{}
	for _, r := range rids {
		if s.bad[r] {
			return nil, &mediasvc.Error{Status: 404, Code: "not_found"}
		}
		s.plain = append(s.plain, r)
		out[r] = "https://private.oss/" + r + ".png?Signature=clean"
	}
	return out, nil
}

// 读侧：mr:// 签普通地址（并登记反查，能当输入）、mrw:// 只签水印地址、签不出置空、公开地址不动。
func TestResolveViewsSignsPrivateRefs(t *testing.T) {
	resetSignedCache()
	s := &fakeBothSigner{bad: map[string]bool{"abcdef-work-dddddddddddddddddddddddddd": true}}
	a, b, c, d := paidRef, "mrw://"+wmRID2+".png", libURL, "mr://abcdef-work-dddddddddddddddddddddddddd.png"
	resolveWatermarkViews(context.Background(), s, wmOn, []*string{&a, &b, &c, &d})
	if a != "https://private.oss/"+wmRID+".png?Signature=clean" {
		t.Fatalf("mr:// 应签普通地址: %s", a)
	}
	if b != "https://private.oss/"+wmRID2+".png?x-oss-process="+wmProcess+"&Signature=s" {
		t.Fatalf("mrw:// 只能签水印地址: %s", b)
	}
	if c != libURL || d != "" {
		t.Fatalf("公开地址不动、签不出置空: c=%s d=%s", c, d)
	}
	if ridOfSignedURL(a, time.Now()) != wmRID {
		t.Fatal("普通签名地址应能反查 rid（用户拿成品当输入）")
	}
	// 签名器不支持普通签名：mr:// 置空（失败关闭）
	e := paidRef
	resetSignedCache()
	resolveWatermarkViews(context.Background(), &fakeSigner{}, wmOn, []*string{&e})
	if e != "" {
		t.Fatal(e)
	}
}

// 永久失败后补蒙版的源地址：mr:// 签干净短期地址只给网关。
func TestMaskSourceURLPrivateRef(t *testing.T) {
	f, _, _, media, _ := asyncFlow(t)
	if got := f.maskSourceURL(context.Background(), paidRef); got != "https://private.oss/"+wmRID+".png?Signature=clean" {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(media.signed, []string{wmRID}) {
		t.Fatal(media.signed)
	}
}

// 转存池：同一子任务并发 Kick 只跑一个；名额满时不排队（留给扫描）；关闭后不接新的。
func TestCopierDedupAndClose(t *testing.T) {
	f := &flow{}
	f.cfg.Library.Workers = 1
	p := newCopier(context.Background(), f)
	release := make(chan struct{})
	var runs atomic.Int32
	var wg sync.WaitGroup
	start := func(pk int64) bool {
		return p.spawn(pk, func(context.Context) { runs.Add(1); <-release })
	}
	if !start(1) {
		t.Fatal("第一次应起协程")
	}
	wg.Add(8)
	var dup atomic.Int32
	for i := 0; i < 8; i++ {
		go func() {
			defer wg.Done()
			if start(1) {
				dup.Add(1)
			}
		}()
	}
	wg.Wait()
	if dup.Load() != 0 {
		t.Fatal("同一子任务不应并发跑两份")
	}
	if start(2) {
		t.Fatal("名额满不应再起")
	}
	close(release)
	p.close()
	if runs.Load() != 1 {
		t.Fatalf("runs=%d", runs.Load())
	}
	if start(3) {
		t.Fatal("关闭后不应再起")
	}
}

// 同一行被两个执行方并发处理：attempt CAS 只让一个认领成功，只复制一次。
func TestRunCopyJobConcurrentClaim(t *testing.T) {
	f, mock, _, _, _ := asyncFlow(t)
	mock.MatchExpectationsInOrder(false)
	var copies atomic.Int32
	f.media = &countMedia{n: &copies}
	f.cfg.Asset.Enabled = false
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	}
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok0)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET result_rid")).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1", 1)).WillReturnResult(ok1)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); f.runCopyJob(context.Background(), pendingJob(0)) }()
	}
	wg.Wait()
	met(t, mock)
	if copies.Load() != 1 {
		t.Fatalf("copies=%d", copies.Load())
	}
}

// countMedia 并发安全地计数转存次数。
type countMedia struct {
	fakeMedia
	n *atomic.Int32
}

func (m *countMedia) CopyToLibrary(context.Context, string, string, string) (mediasvc.Resource, error) {
	m.n.Add(1)
	return mediasvc.Resource{RID: libRID, URL: libURL}, nil
}

// 成品还在后台入素材库（mr:// + 待转存行）时拒绝重新生成；入库永久失败（没有待转存行）时照常放行到后续检查。
func TestUserRegenerateWaitsForLibraryCopy(t *testing.T) {
	qPending := regexp.QuoteMeta("FROM `afv_studio_copy_job` AS `j` WHERE (j.task_pk = 1) AND (j.status = 0) ORDER BY j.id DESC LIMIT 1")
	pendingTask := func() *model.Task {
		tk := poseTask(model.TaskSucceeded)
		tk.ResultURL.String = paidRef
		return tk
	}
	t.Run("pending", func(t *testing.T) {
		inner := onBill()
		f, mock, _ := newTestFlow(t, inner)
		f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-pse_1": resvConfirmed}, def: resvHeld}
		met := mock.ExpectationsWereMet
		mock.ExpectQuery(qPending).WillReturnRows(sqlmock.NewRows([]string{"id", "task_pk", "status"}).AddRow(5, 1, 0))
		err := f.userRegenerate(context.Background(), pendingTask(), poseBatch(model.BatchDone), newPoseText)
		code, msg, _ := xerr.Parse(err)
		if code != xerr.Conflict || !strings.Contains(msg, "存入素材库") || len(inner.holds) != 0 {
			t.Fatalf("err=%v holds=%v", err, inner.holds)
		}
		if err := met(); err != nil {
			t.Fatal(err)
		}
	})
}

// 上一轮已换成素材库地址、写终态前中断：不作废，按完成收尾（补蒙版），不再复制 / 入库。
func TestRunCopyJobAlreadySwappedFinishesDone(t *testing.T) {
	f, mock, _, media, saved := asyncFlow(t)
	mock.ExpectQuery(qTask1).WillReturnRows(sqlmock.NewRows([]string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_rid", "result_url", "deleted"}).
		AddRow(1, 9, 7, "imagegen", "simg_1", model.TaskSucceeded, libRID, libURL, 0))
	mock.ExpectExec(qJobUpdate("status = 1, finished_at = NOW(3)", 0)).WillReturnResult(ok1)
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 || len(*saved) != 0 {
		t.Fatalf("已换地址不应再复制 / 入库: copies=%v saved=%v", media.copies, *saved)
	}
}

// cancelMedia 复制时进程停止（取消根 ctx 后返回取消错误）。
type cancelMedia struct {
	copyMedia
	cancel context.CancelFunc
}

func (m *cancelMedia) CopyToLibrary(ctx context.Context, _, _, _ string) (mediasvc.Resource, error) {
	m.cancel()
	return mediasvc.Resource{}, ctx.Err()
}

// 进程停止打断复制：即使已到最后一轮也不记永久失败，1 秒后由扫描重做。
func TestRunCopyJobShutdownIsNotPermanent(t *testing.T) {
	f, mock, _, _, _ := asyncFlow(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.media = &cancelMedia{cancel: cancel}
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 9)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = NOW(3) + INTERVAL 1 SECOND", 10)).WillReturnResult(ok1)
	f.runCopyJob(ctx, pendingJob(9))
	met(t, mock)
}

// 本实例没配媒体服务：不认领、不碰库（留给配了的实例），扫描也不接手。
func TestRunCopyJobSkipsWithoutMedia(t *testing.T) {
	f, mock, _, _, _ := asyncFlow(t)
	f.media = nil
	f.runCopyJob(context.Background(), pendingJob(0))
	newCopier(context.Background(), f).sweep(context.Background())
	met(t, mock)
}

// 网关产物 rid 不合法（拼不成可签的 mr:// 引用）：这一张走同步转存，不落 outbox、不写引用。
func TestDeliverPaidAsyncInvalidRIDCopiesNow(t *testing.T) {
	f, mock, bill, media, _ := asyncFlow(t)
	f.cfg.Asset.Enabled = false
	mock.ExpectExec(regexp.QuoteMeta("SET `status` = 5") + ".*`result_url` = '" + libURL + "'").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2") + ".*`result_url` = '" + libURL + "'").WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
	f.deliver(context.Background(), runningTask(), batch9, succeededCall(), time.Now())
	met(t, mock)
	if !reflect.DeepEqual(media.copies, []string{"out_1|asset_library|"}) || len(bill.settles) != 1 {
		t.Fatalf("copies=%v settles=%v", media.copies, bill.settles)
	}
}

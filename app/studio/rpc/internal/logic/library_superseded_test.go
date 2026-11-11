package logic

import (
	"context"
	"encoding/json"
	"reflect"
	"regexp"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/billing"

	"github.com/DATA-DOG/go-sqlmock"
)

// 成品已换（重新生成后子任务到了新一轮，计费请求号 studio-simg_1-r1）时，旧成品（本行，预留单 studio-simg_1）的处理。

const newRef = "mr://abcdef-work-nnnnnnnnnnnnnnnnnnnnnnnnnn.png" // 新一轮成品

// taskRowBill 子任务行（带计费请求号与 prompt_meta）。
func taskRowBill(status int8, url, bill, meta string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_rid", "result_url", "billing_request_id", "prompt_meta", "deleted"}).
		AddRow(1, 9, 7, "imagegen", "simg_1", status, "abcdef-work-nnnnnnnnnnnnnnnnnnnnnnnnnn", url, bill, meta, 0)
}

// supersedeFlow asyncFlow + 按预留单号回状态的计费 + 记录 assetOnce 的假素材库（按地址查重，同 asset.InsertOnceByURL）。
func supersedeFlow(t *testing.T, oldStatus string) (*flow, sqlmock.Sqlmock, *copyMedia, *[]asset.Asset, *[]asset.Asset) {
	f, mock, inner, media, saved := asyncFlow(t)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-simg_1": oldStatus}, def: resvHeld}
	once := &[]asset.Asset{}
	f.assetOnce = func(_ context.Context, a asset.Asset) (bool, error) {
		for _, x := range *once {
			if x.SourceTaskID == a.SourceTaskID && x.UserID == a.UserID && x.ResultURL == a.ResultURL {
				return false, nil
			}
		}
		*once = append(*once, a)
		return true, nil
	}
	return f, mock, media, saved, once
}

var qMarkSuperseded = regexp.QuoteMeta("UPDATE afv_asset SET properties = JSON_SET(COALESCE(properties, JSON_OBJECT()), '$.supersededSrcRid', '" + wmRID +
	"') WHERE (source_task_id = 'simg_1') AND (user_id = 7) AND (result_url = '" + libURL + "')")

// 旧成品已确认扣费：照常复制并入素材库（带 supersededSrcRid 标记、不写新一轮的提示词），不换子任务成品地址、不动账。
func TestRunCopyJobSupersededPaidEntersLibrary(t *testing.T) {
	f, mock, media, saved, once := supersedeFlow(t, resvConfirmed)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskSucceeded, newRef, "studio-simg_1-r1", `{"regen_asset_floor":0}`))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskSucceeded, newRef, "studio-simg_1-r1", `{"regen_asset_floor":0}`))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok0) // 新写的行已带标记，没有要补的
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1, finished_at = NOW(3)", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock) // 没有 SwapLibraryRef：子任务成品是新一轮的
	if !reflect.DeepEqual(media.copies, []string{wmRID + "|asset_library|studio-copy:" + wmRID}) {
		t.Fatalf("copies=%v", media.copies)
	}
	if len(*saved) != 0 || len(*once) != 1 {
		t.Fatalf("旧成品应按地址查重写一次: saved=%v once=%v", *saved, *once)
	}
	a := (*once)[0]
	var props map[string]any
	_ = json.Unmarshal([]byte(a.Properties), &props)
	if a.ResultURL != libURL || a.SourceTaskID != "simg_1" || a.UserID != 7 || props["supersededSrcRid"] != wmRID || a.AIPrompt != "" {
		t.Fatalf("旧成品素材行应带标记、source_task_id 照旧: %+v props=%v", a, props)
	}
}

// 旧成品没扣费（预留已退回）：作废，不认领、不复制。
func TestRunCopyJobSupersededUnpaidVoids(t *testing.T) {
	f, mock, media, saved, once := supersedeFlow(t, resvRefunded)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskRunning, "", "studio-simg_1-r1", ""))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectExec(qJobUpdate("status = 3", 0)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 || len(*saved) != 0 || len(*once) != 0 {
		t.Fatal("没付费的旧成品不应复制 / 入库")
	}
}

// 旧成品的预留单仍冻结：推迟再看（不认领、不占尝试次数）。
func TestRunCopyJobSupersededHeldWaits(t *testing.T) {
	f, mock, media, _, _ := supersedeFlow(t, resvHeld)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskRunning, "", "studio-simg_1-r1", ""))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectExec(qJobUpdate("next_at = NOW(3) + INTERVAL 15 SECOND", 0)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 {
		t.Fatal("未定前不应复制")
	}
}

// 同一轮（计费请求号没变）：不是成品已换，走原逻辑——交付失败清了成品 → 作废。
func TestRunCopyJobSameRoundNotSuperseded(t *testing.T) {
	f, mock, media, _, once := supersedeFlow(t, resvConfirmed)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskFailed, "", "studio-simg_1", ""))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectExec(qJobUpdate("status = 3", 0)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 || len(*once) != 0 {
		t.Fatal("同一轮的作废不应入库")
	}
}

// 新一轮的成品已转存成素材库地址：旧行不能被 alreadySwapped 误判成「本行已换完」直接完成（旧成品会漏掉），
// 应按成品已换处理：付过费 → 复制并入素材库。
func TestRunCopyJobSupersededNotMistakenForAlreadySwapped(t *testing.T) {
	f, mock, media, _, once := supersedeFlow(t, resvConfirmed)
	newLib := sqlmock.NewRows([]string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_rid", "result_url", "billing_request_id", "deleted"}).
		AddRow(1, 9, 7, "imagegen", "simg_1", model.TaskSucceeded, "abcdef-asset-nnnnnnnnnnnnnnnnnnnnnnnnnn", "https://cdn/new.png", "studio-simg_1-r1", 0)
	mock.ExpectQuery(qTask1).WillReturnRows(newLib)
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskSucceeded, "https://cdn/new.png", "studio-simg_1-r1", ""))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok0)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 1 || len(*once) != 1 || (*once)[0].ResultURL != libURL {
		t.Fatalf("旧成品应入素材库: copies=%v once=%v", media.copies, *once)
	}
}

// 入库后、换地址前子任务被重新生成（SwapLibraryRef 没改到）：刚按当前成品写的那行属于付过费的旧成品 → 补 supersededSrcRid 标记、
// 按完成收尾；不作废、不写第二条。
func TestRunCopyJobSwapLostMarksSuperseded(t *testing.T) {
	f, mock, _, saved, once := supersedeFlow(t, resvConfirmed)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowRef(model.TaskSucceeded, paidRef, 0))
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}))
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET result_rid")).WillReturnResult(ok0) // 换地址时成品已换
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskQueued, "", "studio-simg_1-r1", `{"regen_asset_floor":0}`))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok1) // 上一步按当前成品写的行补上标记
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1", 1)).WillReturnResult(ok1)
	*once = append(*once, asset.Asset{UserID: 7, SourceTaskID: "simg_1", ResultURL: libURL}) // 视为 saveAssetErr 刚写的那行
	f.asset = func(_ context.Context, list []asset.Asset) error { *saved = append(*saved, list...); return nil }
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(*saved) != 1 || len(*once) != 1 {
		t.Fatalf("旧成品只应有一行: saved=%v once=%v", *saved, *once)
	}
}

// 两张成品各入素材库恰好一次：新成品先交付入库、旧成品随后（重新生成之后）才补入，再各自重放一遍。
// 新成品按地址查重（regen_asset_floor 键存在），旧成品的行不会让新成品被判「已入库」，重放也不写第二条。
func TestRegeneratedTaskBothOutputsOnce(t *testing.T) {
	f, mock, _, saved, once := supersedeFlow(t, resvConfirmed)
	tk := runningTask()
	tk.PromptMeta = json.RawMessage(`{"regen_asset_floor":0}`) // 重做时旧成品还没入库：floor=0
	const newURL = "https://cdn/new.png"
	qByURL := func(url string) string {
		return regexp.QuoteMeta("SELECT MAX(a.id) FROM afv_asset AS a WHERE (a.source_task_id = 'simg_1') AND (a.user_id = 7) AND (a.result_url = '" + url + "')")
	}
	// 1) 新成品交付入库
	mock.ExpectQuery(qByURL(newURL)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(nil))
	if err := f.saveAssetErr(context.Background(), tk, batch9, newURL); err != nil {
		t.Fatal(err)
	}
	// 2) 旧成品补入（带标记，按地址查重）
	tk.Status = model.TaskSucceeded
	j := pendingJob(1)
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok0)
	if err := f.saveSupersededAsset(context.Background(), tk, batch9, j, libURL); err != nil {
		t.Fatal(err)
	}
	// 3) 新成品重放（置成功失败后续做）：同地址已有行，不再写
	mock.ExpectQuery(qByURL(newURL)).WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(101))
	if err := f.saveAssetErr(context.Background(), tk, batch9, newURL); err != nil {
		t.Fatal(err)
	}
	// 4) 旧成品重放（写终态前中断）：同地址已有行，不再写
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok0)
	if err := f.saveSupersededAsset(context.Background(), tk, batch9, j, libURL); err != nil {
		t.Fatal(err)
	}
	met(t, mock)
	if len(*saved) != 1 || (*saved)[0].ResultURL != newURL || len(*once) != 1 || (*once)[0].ResultURL != libURL {
		t.Fatalf("两张成品应各入一次: new=%+v old=%+v", *saved, *once)
	}
}

// 没重新生成过的子任务仍按「该任务已有任一资产行」查重（旧行为不变）。
func TestSaveAssetNotRegeneratedKeepsTaskDedupe(t *testing.T) {
	f, mock, _, saved, _ := supersedeFlow(t, resvConfirmed)
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("simg_1", 88))
	if err := f.saveAssetErr(context.Background(), runningTask(), batch9, libURL); err != nil {
		t.Fatal(err)
	}
	met(t, mock)
	if len(*saved) != 0 {
		t.Fatal("已有资产行不应再写")
	}
	if regenFloorSet(runningTask()) {
		t.Fatal("没有 regen_asset_floor 键")
	}
	tk := runningTask()
	tk.PromptMeta = json.RawMessage(`{"regen_asset_floor":0,"edited":true}`)
	if !regenFloorSet(tk) {
		t.Fatal("floor=0 也算重新生成过")
	}
}

// taskRowAttempt 子任务行带 attempt（旧行按结构判定「成品已换」要看重做轮次）。
func taskRowAttempt(status int8, url, bill string, attempt int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_rid", "result_url", "billing_request_id", "attempt", "deleted"}).
		AddRow(1, 9, 7, "imagegen", "simg_1", status, "abcdef-work-nnnnnnnnnnnnnnnnnnnnnnnnnn", url, bill, attempt, 0)
}

var qNewerJob = regexp.QuoteMeta("FROM `afv_studio_copy_job` WHERE (task_pk = 1) AND (id > 5)")

func expectNewerJob(mock sqlmock.Sqlmock, n int) {
	mock.ExpectQuery(qNewerJob).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
}

// 计费未开启（off）：成品已换时没有扣费可核对，交付过的旧成品照常入素材库（带标记），不查预留单。
func TestRunCopyJobSupersededBillingOffEntersLibrary(t *testing.T) {
	f, mock, media, _, once := supersedeFlow(t, resvRefunded) // 预留单状态不该被查
	f.bill.(*reqStatusBill).fakeFlowBill.mode = billing.ModeOff
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskSucceeded, newRef, "studio-simg_1-r1", `{"regen_asset_floor":0}`))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowBill(model.TaskSucceeded, newRef, "studio-simg_1-r1", `{"regen_asset_floor":0}`))
	expectJobBill(mock, "studio-simg_1")
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok0)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1, finished_at = NOW(3)", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 1 || len(*once) != 1 {
		t.Fatalf("计费未开启时旧成品应入库: copies=%v once=%v", media.copies, *once)
	}
}

// 旧行（没记计费请求号）、子任务重做到第 2 轮且当前成品是另一个私有引用：旧成品属于第 0 / 1 轮；两轮预留单都已扣费 → 入库。
func TestRunCopyJobPreMigrationSupersededAllPaid(t *testing.T) {
	f, mock, media, _, once := supersedeFlow(t, resvConfirmed)
	f.bill.(*reqStatusBill).byReq["studio-simg_1-r1"] = resvConfirmed
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowAttempt(model.TaskSucceeded, newRef, "studio-simg_1-r2", 2))
	expectJobBill(mock, nil)
	expectNewerJob(mock, 0)
	mock.ExpectExec(qJobUpdate("attempt = attempt + 1", 0)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowAttempt(model.TaskSucceeded, newRef, "studio-simg_1-r2", 2))
	expectJobBill(mock, nil)
	expectNewerJob(mock, 0)
	mock.ExpectQuery(qB9).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "status"}).AddRow(9, 7, model.BatchDone))
	mock.ExpectExec(qMarkSuperseded).WillReturnResult(ok0)
	mock.ExpectExec(qJobUpdate("next_at = GREATEST", 1)).WillReturnResult(ok1)
	mock.ExpectExec(qJobUpdate("status = 1, finished_at = NOW(3)", 1)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 1 || len(*once) != 1 {
		t.Fatalf("候选轮次全部扣费，旧成品应入库: copies=%v once=%v", media.copies, *once)
	}
}

// 旧行、有更晚的转存行（子任务新一轮已失败）：候选轮次都没扣费 → 作废（不告警）；有扣费的也有没扣费的 → 判定不了，作废并告警。
func TestRunCopyJobPreMigrationSupersededVerdicts(t *testing.T) {
	for _, tc := range []struct {
		name string
		r0   string
		r1   string
	}{
		{"全部未扣费", resvRefunded, resvExpired},
		{"付费不一", resvConfirmed, resvRefunded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, mock, media, _, once := supersedeFlow(t, tc.r0)
			f.bill.(*reqStatusBill).byReq["studio-simg_1-r1"] = tc.r1
			mock.ExpectQuery(qTask1).WillReturnRows(taskRowAttempt(model.TaskFailed, "", "studio-simg_1-r2", 2))
			expectJobBill(mock, nil)
			expectNewerJob(mock, 1)
			mock.ExpectExec(qJobUpdate("status = 3", 0)).WillReturnResult(ok1)
			f.runCopyJob(context.Background(), pendingJob(0))
			met(t, mock)
			if len(media.copies) != 0 || len(*once) != 0 {
				t.Fatal("不应复制 / 入库")
			}
		})
	}
}

// 旧行、子任务没重做过（attempt=0）：只有一轮，不按成品已换处理，原逻辑（作废），不查更晚的行、不查预留单。
func TestRunCopyJobPreMigrationSingleRoundOriginalLogic(t *testing.T) {
	f, mock, media, _, _ := supersedeFlow(t, resvConfirmed)
	mock.ExpectQuery(qTask1).WillReturnRows(taskRowAttempt(model.TaskFailed, "", "studio-simg_1", 0))
	expectJobBill(mock, nil)
	mock.ExpectExec(qJobUpdate("status = 3", 0)).WillReturnResult(ok1)
	f.runCopyJob(context.Background(), pendingJob(0))
	met(t, mock)
	if len(media.copies) != 0 {
		t.Fatal("不应复制")
	}
}

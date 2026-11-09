package logic

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/mysqldialect"
)

func newMockRepo(t *testing.T) (*repo.Repo, sqlmock.Sqlmock) {
	t.Helper()
	sdb, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sdb.Close() })
	return repo.New(bun.NewDB(sdb, mysqldialect.New())), mock
}

// 范围与前端 COLORFIX_RANGES 一致：边界值通过，越界报第一个越界的参数。
func TestValidateColorFix(t *testing.T) {
	ok := []*studio.ColorFixParams{
		{},
		{Hue: 3, Saturation: -16, Value: -6, Brightness: -17, Contrast: 10}, // 前端默认校色
		{Hue: -30, Saturation: -60, Value: -40, Brightness: -80, Contrast: -50},
		{Hue: 30, Saturation: 70, Value: 40, Brightness: 80, Contrast: 50},
	}
	for _, p := range ok {
		if err := ValidateColorFix(p); err != nil {
			t.Fatalf("%+v 应通过：%v", p, err)
		}
	}
	bad := []struct {
		p   *studio.ColorFixParams
		key string
	}{
		{&studio.ColorFixParams{Hue: 31}, "hue"},
		{&studio.ColorFixParams{Saturation: 71}, "saturation"},
		{&studio.ColorFixParams{Saturation: -61}, "saturation"},
		{&studio.ColorFixParams{Value: -41}, "value"},
		{&studio.ColorFixParams{Brightness: 81}, "brightness"},
		{&studio.ColorFixParams{Contrast: -51, Hue: 99}, "hue"}, // 先报前面的
	}
	for _, c := range bad {
		code, msg, _ := xerr.Parse(ValidateColorFix(c.p))
		if code != xerr.ParamError || !strings.Contains(msg, c.key) {
			t.Fatalf("%+v：code=%d msg=%s", c.p, code, msg)
		}
	}
	if code, _, _ := xerr.Parse(ValidateColorFix(nil)); code != xerr.ParamMissing {
		t.Fatalf("缺参数应为 ParamMissing，got %d", code)
	}
}

func TestParseColorFix(t *testing.T) {
	if parseColorFix(nil) != nil || parseColorFix(json.RawMessage("null")) != nil || parseColorFix(json.RawMessage("{bad")) != nil {
		t.Fatal("NULL / 坏 JSON 应为 nil")
	}
	p := parseColorFix(json.RawMessage(`{"hue":3,"saturation":-16,"value":-6,"brightness":-17,"contrast":10}`))
	if p == nil || p.Hue != 3 || p.Saturation != -16 || p.Value != -6 || p.Brightness != -17 || p.Contrast != 10 {
		t.Fatalf("p=%+v", p)
	}
}

var cfTaskCols = []string{"id", "batch_id", "user_id", "kind", "task_id", "status", "result_url", "deleted", "created_at", "color_fix_params"}

func taskRow(status int8, userID int64, deleted int8) *sqlmock.Rows {
	return sqlmock.NewRows(cfTaskCols).AddRow(11, 9, userID, "tryon", "tyo_1", status, "https://cdn/r.png", deleted, time.Now(), nil)
}

var qTaskByID = regexp.QuoteMeta("WHERE (t.task_id = 'tyo_1') LIMIT 1")

// 保存成功：写 JSON 列（只改本人、未删、成功的那条）→ 回任务带参数与素材库 asset_id。
func TestUpdateColorFix(t *testing.T) {
	r, mock := newMockRepo(t)
	mock.ExpectQuery(qTaskByID).WillReturnRows(taskRow(model.TaskSucceeded, 7, 0))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET color_fix_params = '{\"hue\":3,\"saturation\":-16,\"value\":-6,\"brightness\":-17,\"contrast\":10}' " +
		"WHERE (id = 11) AND (user_id = 7) AND (deleted = 0) AND (status = 2)")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT a.source_task_id, MAX(a.id) AS id FROM afv_asset AS a WHERE (a.source_task_id IN ('tyo_1')) AND (a.user_id = 7)")).
		WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("tyo_1", 501))
	out, err := updateColorFix(context.Background(), r, &studio.UpdateTaskColorFixReq{
		TaskId: " tyo_1 ", UserId: 7, Params: &studio.ColorFixParams{Hue: 3, Saturation: -16, Value: -6, Brightness: -17, Contrast: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.AssetId != 501 || out.ColorFixParams == nil || out.ColorFixParams.Brightness != -17 || out.Progress != 100 {
		t.Fatalf("out=%+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 拒绝：越界不碰库；别人的 / 已删除 → 4001；未成功 → 4002。
func TestUpdateColorFixRejects(t *testing.T) {
	r, mock := newMockRepo(t)
	_, err := updateColorFix(context.Background(), r, &studio.UpdateTaskColorFixReq{TaskId: "tyo_1", UserId: 7, Params: &studio.ColorFixParams{Hue: 40}})
	if code, _, _ := xerr.Parse(err); code != xerr.ParamError {
		t.Fatalf("越界：%v", err)
	}
	cases := []struct {
		name    string
		status  int8
		user    int64
		deleted int8
		code    int
	}{
		{"别人的", model.TaskSucceeded, 8, 0, xerr.NotFound},
		{"已删除", model.TaskSucceeded, 7, 1, xerr.NotFound},
		{"失败的", model.TaskFailed, 7, 0, xerr.Conflict},
		{"生成中", model.TaskRunning, 7, 0, xerr.Conflict},
	}
	for _, c := range cases {
		mock.ExpectQuery(qTaskByID).WillReturnRows(taskRow(c.status, c.user, c.deleted))
		_, err := updateColorFix(context.Background(), r, &studio.UpdateTaskColorFixReq{TaskId: "tyo_1", UserId: 7, Params: &studio.ColorFixParams{}})
		if code, _, _ := xerr.Parse(err); code != c.code {
			t.Fatalf("%s：%v", c.name, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// asset_id：只查成功的任务、一次 IN 查齐；没入库的为 0；查询失败不报错。
func TestAttachAssetIDs(t *testing.T) {
	r, mock := newMockRepo(t)
	d1 := &studio.BatchDetail{Tasks: []*studio.TaskInfo{
		{TaskId: "tyo_1", Status: int32(model.TaskSucceeded)},
		{TaskId: "tyo_2", Status: int32(model.TaskFailed)},
	}}
	d2 := &studio.BatchDetail{Tasks: []*studio.TaskInfo{{TaskId: "tyo_3", Status: int32(model.TaskSucceeded)}}}
	mock.ExpectQuery(regexp.QuoteMeta("WHERE (a.source_task_id IN ('tyo_1', 'tyo_3')) AND (a.user_id = 7) AND (a.deleted = 0) AND ((a.status IS NULL OR a.status <> 0)) GROUP BY a.source_task_id")).
		WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("tyo_1", 501))
	attachAssetIDsToDetails(context.Background(), r, 7, d1, d2)
	if d1.Tasks[0].AssetId != 501 || d1.Tasks[1].AssetId != 0 || d2.Tasks[0].AssetId != 0 {
		t.Fatalf("d1=%+v d2=%+v", d1.Tasks, d2.Tasks)
	}
	// 没有成功的任务：不查库
	attachAssetIDsToDetails(context.Background(), r, 7, &studio.BatchDetail{Tasks: []*studio.TaskInfo{{TaskId: "tyo_2", Status: int32(model.TaskFailed)}}})
	// 查询失败：asset_id 留 0，不报错
	mock.ExpectQuery("FROM afv_asset").WillReturnError(context.DeadlineExceeded)
	d3 := &studio.BatchDetail{Tasks: []*studio.TaskInfo{{TaskId: "tyo_1", Status: int32(model.TaskSucceeded)}}}
	attachAssetIDsToDetails(context.Background(), r, 7, d3)
	if d3.Tasks[0].AssetId != 0 {
		t.Fatal("查询失败应留 0")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// 「默认校色」只在开启时进 payload：不开时 payload 与旧版逐字一致（幂等哈希不变）。
func TestSubmitColorFixPayload(t *testing.T) {
	treq := func(on bool) *studio.SubmitTryonReq {
		return &studio.SubmitTryonReq{
			UserId: 7, RequestId: "r", Items: []string{"top"}, CountPerModel: 1, ColorFix: on,
			ModelImageUrls: []string{"https://a/m1.png"}, GarmentImages: []*studio.GarmentImage{{Url: "https://a/g.png", Role: "上衣"}},
		}
	}
	for _, on := range []bool{false, true} {
		in := treq(on)
		v, err := ValidateTryon(in, nil)
		if err != nil {
			t.Fatal(err)
		}
		raw, _, _ := payloadOf(tryonSpec(in, v).Payload)
		if got := strings.Contains(string(raw), `"color_fix":true`); got != on {
			t.Fatalf("tryon on=%v payload=%s", on, raw)
		}
		if !on && strings.Contains(string(raw), "color_fix") {
			t.Fatalf("关闭时不应写键：%s", raw)
		}
		pin := &studio.SubmitPoseReq{UserId: 7, RequestId: "r", SourceImageUrl: "https://a/s.png", PoseImageUrls: []string{"https://a/p.png"}, ColorFix: on}
		pv, err := ValidatePose(pin, nil)
		if err != nil {
			t.Fatal(err)
		}
		praw, _, _ := payloadOf(poseSpec(pin, pv).Payload)
		if got := strings.Contains(string(praw), `"color_fix":true`); got != on {
			t.Fatalf("pose on=%v payload=%s", on, praw)
		}
	}
}

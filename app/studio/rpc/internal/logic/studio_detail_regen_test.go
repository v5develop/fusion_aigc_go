package logic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/reverse"
	"aigc-local-life/pkg/xerr"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------- 精细读图 / 用户细节框：提交 → payload → 编排

func detailTryonReq() *studio.SubmitTryonReq {
	return &studio.SubmitTryonReq{
		UserId: 7, RequestId: "r1", Items: []string{"top"}, CountPerModel: 1, Model: "Seedream",
		ModelImageUrls: []string{"https://own/m.jpg"},
		GarmentImages: []*studio.GarmentImage{
			{Url: "https://own/g1.jpg", Role: "上衣", Angle: "front"},
			{Url: "https://own/g2.jpg", Role: "上衣", Angle: "back"},
		},
	}
}

// fine_read / detail_crops 写进批次 payload（只在用到时写，不用的提交 payload 与旧版逐字一致），编排解析回来原样可用。
func TestTryonFineReadAndCropsPayload(t *testing.T) {
	in := detailTryonReq()
	v, err := ValidateTryon(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp := tryonSpec(in, v)
	if _, ok := sp.Payload["fine_read"]; ok {
		t.Fatal("没勾精细读图不应写 fine_read")
	}
	if _, ok := sp.Payload["detail_crops_manual"]; ok {
		t.Fatal("没确认细节框不应写 detail_crops")
	}

	in.FineRead = true
	in.DetailCropsManual = true
	in.DetailCrops = []*studio.DetailCrop{
		{GarmentIndex: 1, Label: "上衣·后领", Box: []float64{0.3, 0.05, 0.4, 0.15}, Confidence: 0.9},
		{GarmentIndex: 0, Label: "上衣·袖口", Box: []float64{0, 0.5, 0.2, 0.2}},
	}
	v, err = ValidateTryon(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(tryonSpec(in, v).Payload)
	p, err := parsePayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !p.FineRead || !p.DetailCropsManual || len(p.DetailCrops) != 2 || p.DetailCrops[0].GarmentIndex != 1 || p.DetailCrops[0].Label != "上衣·后领" {
		t.Fatalf("payload 回读不对：%+v", p)
	}
	// 编排：精细读图 → analyze-batch 带 fine=true（sidecar 精细模式）
	spec := pipeline.TryonSpec{Items: p.Items, Fine: p.FineRead}
	breq := pipeline.BuildTryonAnalyzeBatchReq(spec, pipeline.Selection{}, nil, 3)
	body, _ := json.Marshal(breq)
	if !breq.Fine || !breq.Deep || !strings.Contains(string(body), `"fine":true`) {
		t.Fatalf("analyze-batch 请求缺 fine：%s", body)
	}
	spec.Fine = false
	body, _ = json.Marshal(pipeline.BuildTryonAnalyzeReq(spec, pipeline.Selection{}))
	if strings.Contains(string(body), `"fine"`) {
		t.Fatalf("没勾精细读图不应带 fine：%s", body)
	}

	// 用户删光了细节框：manual + 空数组，payload 写 []（编排据此不用细节图）
	in.DetailCrops = nil
	v, _ = ValidateTryon(in, nil)
	raw, _ = json.Marshal(tryonSpec(in, v).Payload)
	if !strings.Contains(string(raw), `"detail_crops":[]`) || !strings.Contains(string(raw), `"detail_crops_manual":true`) {
		t.Fatalf("删光细节框应写空数组：%s", raw)
	}
}

func TestCheckDetailCrops(t *testing.T) {
	box := []float64{0.1, 0.1, 0.2, 0.2}
	many := func(n, gi int) []*studio.DetailCrop {
		out := make([]*studio.DetailCrop, n)
		for i := range out {
			out[i] = &studio.DetailCrop{GarmentIndex: int32(gi), Label: "上衣·领口", Box: box}
		}
		return out
	}
	if got, err := CheckDetailCrops(many(3, 0), 1, false); got != nil || err != nil {
		t.Fatalf("manual=false 应忽略：%v %v", got, err)
	}
	cases := []struct {
		name   string
		crops  []*studio.DetailCrop
		substr string
	}{
		{"总数超限", append(many(12, 0), many(13, 1)...), "最多 24"},
		{"单张超限", many(13, 0), "最多 12"},
		{"服装图越界", []*studio.DetailCrop{{GarmentIndex: 2, Label: "a", Box: box}}, "不存在"},
		{"框越界", []*studio.DetailCrop{{GarmentIndex: 0, Label: "a", Box: []float64{0.9, 0.1, 0.3, 0.2}}}, "裁切框无效"},
		{"框缺位", []*studio.DetailCrop{{GarmentIndex: 0, Label: "a", Box: []float64{0.1, 0.1}}}, "裁切框无效"},
		{"部位名为空", []*studio.DetailCrop{{GarmentIndex: 0, Label: " ", Box: box}}, "部位名"},
		{"部位名过长", []*studio.DetailCrop{{GarmentIndex: 0, Label: strings.Repeat("领", 25), Box: box}}, "部位名"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := CheckDetailCrops(c.crops, 2, true)
			if code, msg, _ := xerr.Parse(err); code != xerr.ParamError || !strings.Contains(msg, c.substr) {
				t.Fatalf("err=%v", err)
			}
		})
	}
	got, err := CheckDetailCrops([]*studio.DetailCrop{{GarmentIndex: 1, Label: " 上衣·领口 ", Box: box, Confidence: 3}}, 2, true)
	if err != nil || len(got) != 1 || got[0].Label != "上衣·领口" || got[0].Confidence != 0 || got[0].GarmentIndex != 1 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

// 用户细节框多于模型参考图名额：提交预警按实际框数说明会少用几张；切图 → 组装后细节图恒 ≤ 名额（RefCap/CropBudget）。
func TestManualCropsRespectRefCap(t *testing.T) {
	// Seedream 5.5 上限 10（兜底表）：2 张服装图 + 模特 1 → 细节名额 7
	in := detailTryonReq()
	in.DetailCropsManual = true
	for i := 0; i < 9; i++ {
		in.DetailCrops = append(in.DetailCrops, &studio.DetailCrop{GarmentIndex: int32(i % 2), Label: "上衣·细节" + string(rune('A'+i)), Box: []float64{0.05 * float64(i), 0.1, 0.1, 0.1}})
	}
	v, err := ValidateTryonCap(in, nil, func(string) int { return 10 })
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(v.Warnings, "|")
	if !strings.Contains(joined, "最多还能放 7 张细节图，已选的 9 张中将保留 7 张") || strings.Contains(joined, "约") {
		t.Fatalf("warnings=%q", v.Warnings)
	}

	gs := []pipeline.Image{
		{Key: "g-top-front-1", Kind: pipeline.ImageGarment, Item: "top", Source: 0, RID: "g0", Width: 1000, Height: 1000},
		{Key: "g-top-back-1", Kind: pipeline.ImageGarment, Item: "top", Source: 1, RID: "g1", Width: 1000, Height: 1000},
	}
	plans, warns := pipeline.UserCropPlans(gs, v.DetailCrops)
	jobs, bw := pipeline.BuildCropJobs(gs, plans, v.Items)
	if len(warns)+len(bw) != 0 || len(jobs) != 2 {
		t.Fatalf("jobs=%d warns=%v %v", len(jobs), warns, bw)
	}
	var details []pipeline.Image
	for _, j := range jobs {
		res := make([]mediasvc.Resource, len(j.Boxes))
		for i := range res {
			res[i] = mediasvc.Resource{RID: j.Source.RID + "-c" + string(rune('1'+i))}
		}
		details = append(details, pipeline.DetailImages(j, res)...)
	}
	if len(details) != 9 {
		t.Fatalf("details=%d", len(details))
	}
	model := pipeline.Image{Key: "m-1", Kind: pipeline.ImageModel, RID: "m"}
	sel := pipeline.SelectImages("top", gs, details, &model)
	gen := pipeline.GenerateRefs(sel, model, pipeline.RefCap("img.seedream-5.5", pipeline.KindTryon, 10))
	if len(gen.Images) > 10 || gen.Details != 7 || gen.DroppedDetails != 2 || gen.Mains != 2 {
		t.Fatalf("gen=%+v", gen)
	}
}

// ---------------------------------------------------------------- 提示词 / 提示透出

func TestTaskInfoExposesPromptAndWarnings(t *testing.T) {
	pose := &model.Task{ID: 1, Kind: model.KindPose, Status: model.TaskSucceeded,
		InputImages: json.RawMessage(`[{"id":"m-1"},{"id":"p-1"}]`),
		PromptMeta:  json.RawMessage(`{"warnings":["配对被反推服务校正：提交 [0 0]，实际 [0 1]"],"user_warnings":["所选人物配对无效，已按默认配对生成"],"edited":true}`)}
	pose.PromptText.String, pose.PromptText.Valid = "图序：图1 是原图，图2 是姿势参考图。\n【任务】把图1人物摆成图2姿势", true
	info := toTaskInfo(pose, model.BatchDone)
	if info.PromptText != pose.PromptText.String || !info.PromptEdited ||
		!reflect.DeepEqual(info.Warnings, []string{"所选人物配对无效，已按默认配对生成"}) {
		t.Fatalf("info=%+v", info)
	}
	if strings.HasPrefix(info.PromptSummary, "图序") {
		t.Fatalf("摘要不应含图序行：%q", info.PromptSummary)
	}

	tryon := &model.Task{ID: 2, Kind: model.KindTryon, Status: model.TaskFailed,
		PromptMeta: json.RawMessage(`{"warnings":["读图返回 FAILED/NO_CAPACITY，已改用文字信息生成"],"user_warnings":["读图失败，已改用文字信息生成","所选模型参考图数量有限，细节图已减少为 3 张"]}`)}
	tryon.PromptText.String, tryon.PromptText.Valid = "图序：图1 是模特。\n正文", true
	img := &model.Task{ID: 3, Kind: model.KindImageGen, PromptMeta: json.RawMessage(`{"user_warnings":["读图失败，已改用文字信息生成"]}`)}
	img.PromptText.String, img.PromptText.Valid = "用户自己写的", true
	if toTaskInfo(img, model.BatchDone).PromptText != "" {
		t.Fatal("图片生成不重复下发提示词")
	}
	b := &model.Batch{ID: 9, Kind: model.KindTryon}
	bi := toBatchInfo(b, []*model.Task{tryon, img, {ID: 4, Kind: model.KindTryon}})
	want := []string{"读图失败，已改用文字信息生成", "所选模型参考图数量有限，细节图已减少为 3 张"}
	if !reflect.DeepEqual(bi.Warnings, want) {
		t.Fatalf("batch warnings=%q", bi.Warnings)
	}
	for _, w := range append(bi.Warnings, toTaskInfo(tryon, 0).Warnings...) {
		if strings.Contains(w, "FAILED") || strings.Contains(w, "NO_CAPACITY") {
			t.Fatalf("透出了网关原文：%q", w)
		}
	}
	if toBatchInfo(b, []*model.Task{{ID: 5}}).Warnings != nil {
		t.Fatal("没有提示时应为空")
	}
}

// sidecar 422 文案：产品中文原样给用户；带网关失败类别 / HTTP 码的原文换成通用文案。
func TestClassifyRejectedHidesVendorText(t *testing.T) {
	err := classify("crop_plan", &reverse.Error{Path: "/api/reverse/crop-plan", Status: 422, Kind: reverse.KindInvalid, Message: "没有可识别的服装图"})
	var se *stepError
	if !errors.As(err, &se) || se.User != "没有可识别的服装图，点数已退回" {
		t.Fatalf("err=%v", err)
	}
	err = classify("crop_plan", &reverse.Error{Path: "/api/reverse/crop-plan", Status: 422, Kind: reverse.KindInvalid, Message: "读图返回 FAILED/CONTENT_POLICY，请稍后重试"})
	if !errors.As(err, &se) || se.User != "图片无法识别，点数已退回" {
		t.Fatalf("err=%v", err)
	}
}

// ---------------------------------------------------------------- AI 找细节

type detectMedia struct{ pipeline.Media }

func (detectMedia) IngestURL(_ context.Context, u, _, _ string) (mediasvc.Resource, error) {
	return mediasvc.Resource{RID: "rid:" + u, Width: 1000, Height: 800}, nil
}
func (detectMedia) Conform(_ context.Context, rid string, _ mediasvc.ConformParams, _ string) (mediasvc.Resource, error) {
	return mediasvc.Resource{RID: rid, Width: 1000, Height: 800}, nil
}
func (detectMedia) FetchBytes(_ context.Context, rid string) ([]byte, string, error) {
	return []byte(rid), "image/jpeg", nil
}

type detectRev struct {
	pipeline.Reverse
	mu    sync.Mutex
	reqs  []reverse.CropPlanReq
	plans []reverse.CropPlan
	err   error
	block chan struct{}
}

func (r *detectRev) CropPlan(_ context.Context, req reverse.CropPlanReq) (*reverse.CropPlanResp, error) {
	if r.block != nil {
		<-r.block
	}
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	return &reverse.CropPlanResp{Plans: r.plans}, nil
}

func detectReq(uid int64) *studio.DetectDetailsReq {
	return &studio.DetectDetailsReq{UserId: uid, Items: []string{"top", "shoes"}, SetMode: "two", GarmentImages: []*studio.GarmentImage{
		{Url: "https://own/shoes.jpg", Role: "鞋子"},
		{Url: "https://own/top.jpg", Role: "上衣", Angle: "front"},
	}}
}

func TestDetectDetails(t *testing.T) {
	rev := &detectRev{plans: []reverse.CropPlan{
		// gs 按单品排序：上衣（提交下标 1）在前、鞋子（下标 0）在后
		{Index: 0, Angle: "front", Crops: []reverse.Crop{
			{Label: "上衣·领口", Box: []float64{0.3, 0.1, 0.4, 0.2}, Confidence: 0.86},
			{Label: "上衣·领口", Box: []float64{0.3, 0.1, 0.4, 0.2}},  // 同图同名去重
			{Label: "下装·腰头", Box: []float64{0.2, 0.6, 0.5, 0.1}},  // 串件丢弃
			{Label: "袖口", Box: []float64{0.0, 0.5, 0.2, 0.2}},     // 补单品前缀
			{Label: "下摆", Box: []float64{0.9, 0.9, 0.3, 0.3}},     // 越界丢弃
			{Label: "门襟", Box: []float64{0.6, 0.2, 0.40004, 0.3}}, // 舍入超 1 收回
		}},
	}}
	d := &detailDetector{media: detectMedia{}, rev: rev, owns: func(u string) bool { return strings.HasPrefix(u, "https://own/") }}
	resp, err := d.detect(context.Background(), detectReq(7))
	if err != nil {
		t.Fatal(err)
	}
	if len(rev.reqs) != 1 || rev.reqs[0].Category != "compose" || len(rev.reqs[0].Images) != 2 || rev.reqs[0].Images[0].Role != "上衣" {
		t.Fatalf("crop-plan 请求不对：%+v", rev.reqs)
	}
	if len(resp.Plans) != 2 || resp.Total != 3 {
		t.Fatalf("resp=%+v", resp)
	}
	top := resp.Plans[1]
	if top.GarmentIndex != 1 || top.Angle != "front" || len(top.Crops) != 3 {
		t.Fatalf("top=%+v", top)
	}
	labels := []string{top.Crops[0].Label, top.Crops[1].Label, top.Crops[2].Label}
	if !reflect.DeepEqual(labels, []string{"上衣·领口", "上衣·袖口", "上衣·门襟"}) || top.Crops[0].GarmentIndex != 1 || top.Crops[0].Confidence != 0.86 {
		t.Fatalf("crops=%+v", top.Crops)
	}
	if b := top.Crops[2].Box; b[0]+b[2] > 1 {
		t.Fatalf("框应收回图内：%v", b)
	}
	if len(resp.Plans[0].Crops) != 0 || len(resp.Warnings) != 1 || !strings.Contains(resp.Warnings[0], "第 1 张服装图没有找到细节") {
		t.Fatalf("没找到细节的图要提示：%+v", resp)
	}
	// 回传的框原样作为提交的 detail_crops 必须能通过校验
	var back []*studio.DetailCrop
	for _, p := range resp.Plans {
		back = append(back, p.Crops...)
	}
	if _, err := CheckDetailCrops(back, 2, true); err != nil {
		t.Fatalf("回传的框应能提交：%v", err)
	}
}

func TestDetectDetailsGuards(t *testing.T) {
	owns := func(u string) bool { return strings.HasPrefix(u, "https://own/") }
	// 非本服务地址
	d := &detailDetector{media: detectMedia{}, rev: &detectRev{}, owns: owns}
	req := detectReq(8)
	req.GarmentImages[0].Url = "https://evil/x.jpg"
	if _, err := d.detect(context.Background(), req); codeOf(err) != xerr.ParamError {
		t.Fatalf("err=%v", err)
	}
	// 未开放
	if _, err := (&detailDetector{owns: owns}).detect(context.Background(), detectReq(8)); codeOf(err) != xerr.NotReady {
		t.Fatalf("err=%v", err)
	}
	// 同一用户单飞：上一次还没结束 → 4002
	rev := &detectRev{block: make(chan struct{})}
	d = &detailDetector{media: detectMedia{}, rev: rev, owns: owns, slots: make(chan struct{}, 1)}
	done := make(chan error, 1)
	go func() { _, err := d.detect(context.Background(), detectReq(9)); done <- err }()
	for {
		if _, busy := detectInflight.Load(int64(9)); busy {
			break
		}
	}
	_, err := d.detect(context.Background(), detectReq(9))
	if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || !strings.Contains(msg, "还在进行中") {
		t.Fatalf("err=%v", err)
	}
	close(rev.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, busy := detectInflight.Load(int64(9)); busy {
		t.Fatal("结束后应释放")
	}
	// sidecar 原文不透出
	d = &detailDetector{media: detectMedia{}, owns: owns, rev: &detectRev{err: &reverse.Error{Status: 422, Kind: reverse.KindInvalid, Message: "读图返回 FAILED/CONTENT_POLICY"}}}
	_, err = d.detect(context.Background(), detectReq(10))
	if code, msg, _ := xerr.Parse(err); code != xerr.ParamError || strings.Contains(msg, "FAILED") {
		t.Fatalf("err=%v", err)
	}
	d.rev = &detectRev{err: &reverse.Error{Status: 503, Kind: reverse.KindUpstream, Message: "读图返回 503"}}
	if _, err = d.detect(context.Background(), detectReq(10)); codeOf(err) != xerr.ThirdPartyError {
		t.Fatalf("err=%v", err)
	}
}

// ---------------------------------------------------------------- 编辑提示词后重新生成

const newPoseBody = "【任务】把图1人物摆成图2姿势，右手叉腰"

var newPoseText = pipeline.WithImageOrderLine(newPoseBody, pipeline.KindPose, 2)

func poseTask(status int8) *model.Task {
	tk := &model.Task{ID: 1, BatchID: 9, UserID: 7, Kind: model.KindPose, Status: status, BillingStatus: model.BillingRefunded,
		InputImages: json.RawMessage(`[{"id":"m-1","url":"mr://a"},{"id":"p-1","url":"mr://b"}]`),
		PromptMeta:  json.RawMessage(`{"gen":{"aspect_ratio":"3:4","resolution":"1k"},"user_warnings":["人物数量与预览时不一致，已按默认配对生成"]}`)}
	tk.TaskID.String, tk.TaskID.Valid = "pse_1", true
	tk.BillingRequestID.String, tk.BillingRequestID.Valid = "studio-pse_1", true
	tk.PromptText.String, tk.PromptText.Valid = "图序：图1 是原图，图2 是姿势参考图。\n旧提示词", true
	if status == model.TaskSucceeded {
		tk.BillingStatus = model.BillingSettled
		tk.ResultURL.String, tk.ResultURL.Valid = "https://cdn/old.png", true
		tk.ResultRid.String, tk.ResultRid.Valid = "out_old", true
	}
	return tk
}

func poseBatch(status int8) *model.Batch {
	return &model.Batch{ID: 9, UserID: 7, Kind: model.KindPose, Status: status, Payload: []byte(`{"resolution":"1k","aspect_ratio":"3:4"}`)}
}

func qClaimFrom(n int) string {
	return regexp.QuoteMeta(fmt.Sprintf("UPDATE `afv_studio_task` SET attempt = attempt + 1 WHERE (id = 1) AND (attempt = %d) AND (status IN (3, 4, 2)) AND (admin_locked = 0)", n))
}

// 失败的姿态任务：换提示词重跑，计费同用户重试（新号 -r1 冻结 1 张、原单不动），提示词写库（含图序行），meta 记 edited 并清掉旧提示。
func TestUserRegenerateFromFailed(t *testing.T) {
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-pse_1": resvRefunded}, def: resvHeld}
	mock.ExpectExec(qClaimFrom(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 0") +
		".*`billing_request_id` = 'studio-pse_1-r1'.*" + regexp.QuoteMeta("`prompt_text` = '"+newPoseText+"'") +
		".*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 3)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 0, task_failed = task_failed + -1")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("(`status` = 6)")).WillReturnResult(ok1)
	mock.ExpectExec("user_regenerate").WillReturnResult(ok1)
	tk := poseTask(model.TaskFailed)
	if err := f.userRegenerate(context.Background(), tk, poseBatch(model.BatchPartial), newPoseText); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inner.holds, []string{"studio-pse_1-r1"}) || len(inner.fails) != 0 || len(inner.enoughs) != 1 {
		t.Fatalf("holds=%v fails=%v enoughs=%d", inner.holds, inner.fails, len(inner.enoughs))
	}
	met(t, mock)
	meta, _ := f.regenMeta(context.Background(), tk, poseBatch(model.BatchPartial))
	var m map[string]any
	_ = json.Unmarshal(meta, &m)
	if m["edited"] != true || m[metaUserWarnings] != nil || m["gen"] == nil {
		t.Fatalf("meta=%s", meta)
	}
}

// 成功的姿态任务也能改提示词重新生成：原单已扣费（那张图的钱）不动，新号冻结 1 张；成品字段清空、成功计数 -1、批次拉回生图中；
// meta 记下当前素材库最新资产 id，新成品交付时照常入库。
func TestUserRegenerateFromSucceeded(t *testing.T) {
	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	f.bill = &reqStatusBill{fakeFlowBill: inner, byReq: map[string]string{"studio-pse_1": resvConfirmed}, def: resvHeld}
	mock.ExpectQuery(regexp.QuoteMeta("FROM afv_asset AS a")).WillReturnRows(sqlmock.NewRows([]string{"source_task_id", "id"}).AddRow("pse_1", 55))
	mock.ExpectExec(qClaimFrom(0)).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 0") +
		".*`billing_request_id` = 'studio-pse_1-r1'.*" + regexp.QuoteMeta(`"regen_asset_floor":55`) +
		".*" + regexp.QuoteMeta("`result_url` = NULL") + ".*" + regexp.QuoteMeta("WHERE (`id` = 1) AND (`status` = 2)")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + -1, task_failed = task_failed + 0")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_batch` SET `status` = 3") + ".*" + regexp.QuoteMeta("(`status` = 4)")).WillReturnResult(ok1)
	mock.ExpectExec("user_regenerate").WillReturnResult(ok1)
	if err := f.userRegenerate(context.Background(), poseTask(model.TaskSucceeded), poseBatch(model.BatchDone), newPoseText); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inner.holds, []string{"studio-pse_1-r1"}) || len(inner.fails) != 0 || len(inner.confirmed) != 0 {
		t.Fatalf("holds=%v fails=%v confirmed=%v", inner.holds, inner.fails, inner.confirmed)
	}
	met(t, mock)
	// 交付时的入库查重：素材库最新资产仍是 55（= floor）→ 照常入库；已有更新的资产 → 跳过（重放不写两条）
	tk := poseTask(model.TaskRunning)
	tk.PromptMeta = json.RawMessage(`{"regen_asset_floor":55}`)
	if regenAssetFloor(tk) != 55 || regenAssetFloor(poseTask(model.TaskRunning)) != 0 {
		t.Fatal("regen_asset_floor 解析不对")
	}
}

func TestUserRegenerateRejects(t *testing.T) {
	cases := []struct {
		name   string
		task   func() *model.Task
		batch  int8
		old    string
		code   int
		substr string
	}{
		{"在途", func() *model.Task { return poseTask(model.TaskRunning) }, model.BatchGenerating, resvRefunded, xerr.Conflict, "正在生成中"},
		{"已取消", func() *model.Task { return poseTask(model.TaskCanceled) }, model.BatchPartial, resvRefunded, xerr.Conflict, "已取消"},
		{"批次编排中", func() *model.Task { return poseTask(model.TaskFailed) }, model.BatchPrompting, resvRefunded, xerr.Conflict, "生成中"},
		{"轮次用完", func() *model.Task { tk := poseTask(model.TaskFailed); tk.Attempt = regenMaxAttempts; return tk }, model.BatchFailed, resvRefunded, xerr.Conflict, "多次"},
		{"原单仍 HELD", func() *model.Task { return poseTask(model.TaskFailed) }, model.BatchFailed, resvHeld, xerr.Conflict, "结算中"},
		{"失败任务原单已扣费", func() *model.Task { return poseTask(model.TaskFailed) }, model.BatchFailed, resvConfirmed, xerr.Conflict, "结算中"},
		{"没有提示词", func() *model.Task { tk := poseTask(model.TaskFailed); tk.PromptText.Valid = false; return tk }, model.BatchFailed, resvRefunded, xerr.Conflict, "提示词"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, inner, met := newRetryFlow(t, c.old)
			f.bill.(*reqStatusBill).byReq = map[string]string{"studio-pse_1": c.old}
			err := f.userRegenerate(context.Background(), c.task(), poseBatch(c.batch), newPoseText)
			code, msg, _ := xerr.Parse(err)
			if code != c.code || !strings.Contains(msg, c.substr) {
				t.Fatalf("err=%v", err)
			}
			if len(inner.holds) != 0 {
				t.Fatalf("不应冻结：%v", inner.holds)
			}
			if err := met(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// regenerate 入口：同一提示词重复提交（任务已按它在重做中）幂等，不再冻结；提示词不同 → 冲突；非姿态 → 参数错误；别人的任务 → 不存在。
func TestRegenerateIdempotentAndOwnership(t *testing.T) {
	taskCols := []string{"id", "batch_id", "user_id", "kind", "status", "task_id", "billing_request_id", "prompt_text", "deleted"}
	qTask := regexp.QuoteMeta("WHERE (t.task_id = 'pse_1') LIMIT 1")
	batchRow := func(m sqlmock.Sqlmock) {
		m.ExpectQuery(regexp.QuoteMeta("WHERE (b.id = 9) AND (b.user_id = 7)")).WillReturnRows(sqlmock.NewRows([]string{"id", "user_id", "kind", "status"}).AddRow(9, 7, "pose", model.BatchGenerating))
	}
	render := func(b *model.Batch, ts []*model.Task) *studio.BatchDetail { return toBatchDetail(b, ts) }

	inner := onBill()
	f, mock, _ := newTestFlow(t, inner)
	mock.ExpectQuery(qTask).WillReturnRows(sqlmock.NewRows(taskCols).AddRow(1, 9, 7, "pose", model.TaskQueued, "pse_1", "studio-pse_1-r1", newPoseText, 0))
	batchRow(mock)
	batchRow(mock)
	mock.ExpectQuery(qListBatch9).WillReturnRows(sqlmock.NewRows(taskCols).AddRow(1, 9, 7, "pose", model.TaskQueued, "pse_1", "studio-pse_1-r1", newPoseText, 0))
	out, err := regenerate(context.Background(), f, nil, 7, "pse_1", newPoseBody, render)
	if err != nil || len(out.Tasks) != 1 || out.Tasks[0].PromptText != newPoseText {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	if len(inner.holds) != 0 {
		t.Fatalf("幂等重放不应冻结：%v", inner.holds)
	}
	met(t, mock)

	// 正在按另一段提示词重做：冲突
	f, mock, _ = newTestFlow(t, onBill())
	for i := 0; i < 2; i++ {
		mock.ExpectQuery(qTask).WillReturnRows(sqlmock.NewRows(taskCols).AddRow(1, 9, 7, "pose", model.TaskRunning, "pse_1", "studio-pse_1-r1", "图序：图1 是原图，图2 是姿势参考图。\n别的", 0))
		batchRow(mock)
	}
	_, err = regenerate(context.Background(), f, nil, 7, "pse_1", newPoseBody, render)
	if code, msg, _ := xerr.Parse(err); code != xerr.Conflict || !strings.Contains(msg, "正在生成中") {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)

	// 商拍任务：只读，不支持改提示词重新生成
	f, mock, _ = newTestFlow(t, onBill())
	mock.ExpectQuery(qTask).WillReturnRows(sqlmock.NewRows(taskCols).AddRow(1, 9, 7, "tryon", model.TaskFailed, "pse_1", "studio-pse_1", "p", 0))
	batchRow(mock)
	if _, err = regenerate(context.Background(), f, nil, 7, "pse_1", newPoseBody, render); codeOf(err) != xerr.ParamError {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)

	// 别人的任务
	f, mock, _ = newTestFlow(t, onBill())
	mock.ExpectQuery(qTask).WillReturnRows(sqlmock.NewRows(taskCols).AddRow(1, 9, 8, "pose", model.TaskFailed, "pse_1", "studio-pse_1", "p", 0))
	if _, err = regenerate(context.Background(), f, nil, 7, "pse_1", newPoseBody, render); codeOf(err) != xerr.NotFound {
		t.Fatalf("err=%v", err)
	}
	met(t, mock)
}

// 自选细节框的部位名会进生图提示词，必须和提示词 / 面料一样过文本审核。
func TestTryonDetailCropLabelsModerated(t *testing.T) {
	in := detailTryonReq()
	in.DetailCropsManual = true
	in.DetailCrops = []*studio.DetailCrop{{GarmentIndex: 1, Label: "上衣·后领", Box: []float64{0.3, 0.05, 0.4, 0.15}}}
	v, err := ValidateTryon(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, tc := range tryonSpec(in, v).Texts {
		if tc.text == v.DetailCrops[0].Label {
			found = true
		}
	}
	if !found {
		t.Fatalf("细节框部位名 %q 没进审核列表", v.DetailCrops[0].Label)
	}
}

// 免费档用户拿自己成品的签名地址当服装图：AI 找细节与提交同口径放行（owns || ownsSigned），且按用户校验。
func TestDetectDetailsAcceptsOwnSignedURL(t *testing.T) {
	owns := func(u string) bool { return strings.HasPrefix(u, "https://own/") }
	signed := func(_ context.Context, uid int64, u string) bool {
		return uid == 11 && strings.HasPrefix(u, "https://signed/")
	}
	req := detectReq(11)
	req.GarmentImages[0].Url = "https://signed/r.jpg"
	d := &detailDetector{media: detectMedia{}, rev: &detectRev{}, owns: owns, ownsSigned: signed}
	if _, err := d.detect(context.Background(), req); codeOf(err) == xerr.ParamError {
		t.Fatalf("本人成品签名地址应放行：%v", err)
	}
	other := detectReq(12)
	other.GarmentImages[0].Url = "https://signed/r.jpg"
	if _, err := d.detect(context.Background(), other); codeOf(err) != xerr.ParamError {
		t.Fatalf("别人的签名地址应拒收：%v", err)
	}
}

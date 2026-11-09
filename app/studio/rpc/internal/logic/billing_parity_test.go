package logic

import (
	"context"
	"fmt"
	"testing"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/config"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/billing"
)

// groupPriceQuoter 按 (模型组, 分辨率) 出单价的假报价：模拟 Java 后台按模型分档位 + 分辨率单价行的配置。
type groupPriceQuoter struct{ got []billing.Charge }

func (*groupPriceQuoter) Mode() string { return billing.ModeOn }

func (g *groupPriceQuoter) Quote(_ context.Context, c billing.Charge) (*billing.Quote, error) {
	g.got = append(g.got, c)
	return &billing.Quote{PricePoints: unitPrice(c), PriceUnit: "PER_IMAGE"}, nil
}

// unitPrice 不同模型 / 分辨率给不同价，用于断言报价确实按模型组取价。
func unitPrice(c billing.Charge) int64 {
	base := map[string]int64{"img.gpt-image-2.6-sunburst": 100, "img.nano-banana-2.1-dev": 30, "img.seedream-5.5": 20,
		"img.gpt-image-2.5-sunburst-developer": 17}[c.ModelGroup]
	return base * map[string]int64{"1K": 1, "2K": 2, "4K": 3}[c.Resolution]
}

// 报价与提交冻结同价：三种 kind × 四个模型档 × 三档分辨率，报价发出的计费请求与提交逐张冻结的计费请求
// （除预留单号外）逐字段相同，且带上了该模型档实际出图的模型组。
func TestQuoteHoldParityPerModelAndResolution(t *testing.T) {
	groups := map[string]string{
		"通用": "img.gpt-image-2.6-sunburst", "GPT": "img.gpt-image-2.6-sunburst",
		"Banana": "img.nano-banana-2.1-dev", "Seedream": "img.seedream-5.5",
		"GPT25Dev": "img.gpt-image-2.5-sunburst-developer",
	}
	d, _ := newTestDeps(t, &fakeBill{mode: billing.ModeOn})
	for _, kind := range []model.Kind{model.KindTryon, model.KindPose, model.KindImageGen} {
		for name, group := range groups {
			for _, res := range []string{"1k", "2k", "4k"} {
				t.Run(fmt.Sprintf("%s/%s/%s", kind, name, res), func(t *testing.T) {
					sp := specFor(t, kind, name, res, groups)
					if sp.ModelGroup != group {
						t.Fatalf("提交模型组=%s，期望 %s", sp.ModelGroup, group)
					}
					q := &groupPriceQuoter{}
					r := quoteStudio(context.Background(), q, d.entry(kind), kind,
						&studio.QuoteReq{Kind: string(kind), UserId: 7, Model: name, Resolution: res, TaskTotal: 2}, groups)
					if !r.Available || len(q.got) != 1 {
						t.Fatalf("报价不可用：%+v", r)
					}
					task := &model.Task{ModelGroup: sp.ModelGroup}
					task.TaskID.String = "x_1"
					hold := d.charge(sp, task)
					quote := q.got[0]
					if hold.RequestID != "studio-x_1" {
						t.Fatalf("冻结请求号=%s", hold.RequestID)
					}
					hold.RequestID = ""
					if quote != hold {
						t.Fatalf("报价与冻结的计费请求不同：\n报价 %+v\n冻结 %+v", quote, hold)
					}
					if quote.ModelGroup != group {
						t.Fatalf("计费请求未带模型组：%+v", quote)
					}
					if want := unitPrice(hold); r.UnitPoints != want || r.Points != want*2 {
						t.Fatalf("单价=%d 合计=%d，期望 %d / %d", r.UnitPoints, r.Points, want, want*2)
					}
				})
			}
		}
	}
}

// 不同模型同分辨率价格不同：GPT 比 Seedream 贵（报价按模型组走，不是一个入口一个价）。
func TestQuoteDiffersByModel(t *testing.T) {
	groups := map[string]string{"GPT": "img.gpt-image-2.6-sunburst", "Seedream": "img.seedream-5.5"}
	unit := func(m string) int64 {
		r := quoteStudio(context.Background(), &groupPriceQuoter{}, "studio_tryon", model.KindTryon,
			&studio.QuoteReq{Model: m, Resolution: "2k", TaskTotal: 1}, groups)
		return r.UnitPoints
	}
	if gpt, sd := unit("GPT"), unit("Seedream"); gpt <= sd {
		t.Fatalf("GPT %d 应高于 Seedream %d", gpt, sd)
	}
}

// 重做按原任务的模型组计价：前端模型档映射后来变了（如「通用」换组），重做仍取子任务上落库的组；
// 且重做的计费请求与「同组同分辨率」的提交报价逐字段相同。
func TestRetryChargeUsesTaskModelGroup(t *testing.T) {
	f, _, _ := newTestFlow(t, onBill())
	f.cfg = config.Config{}
	f.cfg.EntryCodes.Tryon = "studio_tryon"
	tk := failedTask(0)
	tk.Kind, tk.ModelGroup = model.KindTryon, "img.nano-banana-2.1-dev"
	b := &model.Batch{ID: 9, UserID: 7, Kind: model.KindTryon, Payload: []byte(`{"model":"通用","resolution":"2k","aspect_ratio":"3:4"}`)}
	c, err := f.retryCharge(tk, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.ModelGroup != "img.nano-banana-2.1-dev" || c.Resolution != "2K" || c.EntryCode != "studio_tryon" || c.Count != 1 || c.UserID != 7 {
		t.Fatalf("重做计费请求：%+v", c)
	}
	if want := quoteCharge("studio_tryon", 7, "img.nano-banana-2.1-dev", "2k"); c != want {
		t.Fatalf("重做与提交报价口径不同：\n重做 %+v\n报价 %+v", c, want)
	}
}

func specFor(t *testing.T, kind model.Kind, name, res string, groups map[string]string) *submitSpec {
	t.Helper()
	switch kind {
	case model.KindTryon:
		in := &studio.SubmitTryonReq{
			UserId: 7, RequestId: "r", Items: []string{"top"}, CountPerModel: 1, Model: name, Resolution: res, AspectRatio: "1:1",
			ModelImageUrls: []string{"https://a/m1.png"},
			GarmentImages:  []*studio.GarmentImage{{Url: "https://a/g.png", Role: "上衣"}},
		}
		v, err := ValidateTryon(in, groups)
		if err != nil {
			t.Fatal(err)
		}
		return tryonSpec(in, v)
	case model.KindPose:
		in := &studio.SubmitPoseReq{UserId: 7, RequestId: "r", SourceImageUrl: "https://a/s.png",
			PoseImageUrls: []string{"https://a/p.png"}, Model: name, Resolution: res, AspectRatio: "1:1"}
		v, err := ValidatePose(in, groups)
		if err != nil {
			t.Fatal(err)
		}
		return poseSpec(in, v)
	}
	in := &studio.SubmitImageGenReq{UserId: 7, RequestId: "r", Prompt: "一只猫", Count: 1, Model: name, Resolution: res, AspectRatio: "1:1"}
	v, err := ValidateImageGen(in, groups)
	if err != nil {
		t.Fatal(err)
	}
	return imageGenSpec(in, v)
}

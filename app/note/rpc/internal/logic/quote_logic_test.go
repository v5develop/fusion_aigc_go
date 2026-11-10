package logic

import (
	"context"
	"database/sql"
	"testing"

	"aigc-local-life/app/note/model"
	"aigc-local-life/app/note/rpc/internal/svc"
	"aigc-local-life/app/note/rpc/pb"
	"aigc-local-life/pkg/billing"
)

func TestNoteImageCount(t *testing.T) {
	if n, err := noteImageCount(4, 0); err != nil || n != 4 {
		t.Fatalf("不传用默认: %d %v", n, err)
	}
	if n, err := noteImageCount(4, 9); err != nil || n != 9 {
		t.Fatalf("不设上限: %d %v", n, err)
	}
	if _, err := noteImageCount(4, -1); err == nil {
		t.Fatal("至少 1 张")
	}
}

func TestNoteQuote(t *testing.T) {
	s := &svc.ServiceContext{}
	s.Config.Limits.DefaultImages = 4
	l := NewQuoteLogic(context.Background(), s)
	out, err := l.Quote(&pb.QuoteReq{UserId: 1, ImageCount: 3})
	if err != nil {
		t.Fatal(err)
	}
	// 没配计费等同 off：0 点、不扣；冻结在出图那一步
	if !out.Available || out.Charged || out.Points != 0 || out.BillingMode != billing.ModeOff || out.Stage != "generate" {
		t.Fatalf("got %+v", out)
	}
	out, _ = l.Quote(&pb.QuoteReq{UserId: 1, ImageCount: -2})
	if out.Available || out.Reason != "套图数量至少 1 张" {
		t.Fatalf("got %+v", out)
	}
	// 报价请求与出图冻结同形（只差请求号）
	if c := noteCharge(1, "", 3); c.EntryCode != "imageTextNote" || c.Count != 3 || c.Kind != billing.KindQuick {
		t.Fatalf("charge %+v", c)
	}
}

// fakeNoteModel 只实现 FindOwned（报价带 id 时只读这一处）。
type fakeNoteModel struct {
	model.AfvImageTextNoteModel
	n *model.AfvImageTextNote
}

func (f *fakeNoteModel) FindOwned(_ context.Context, id, userID int64) (*model.AfvImageTextNote, error) {
	if f.n == nil || f.n.Id != id || f.n.UserId != userID {
		return nil, model.ErrNotFound
	}
	return f.n, nil
}

func TestNoteQuoteWithIDUsesPlanAndGenerateRule(t *testing.T) {
	script := `{"note":{"title":"t","content":"c"},"images":[{"index":1},{"index":2},{"index":3}]}`
	n := &model.AfvImageTextNote{Id: 9, UserId: 1, Status: model.StatusPlanned, ImageCount: 3,
		PlanScript: sql.NullString{String: script, Valid: true}}
	s := &svc.ServiceContext{Note: &fakeNoteModel{n: n}}
	s.Config.Limits.DefaultImages = 4
	l := NewQuoteLogic(context.Background(), s)

	// 沿用构思张数（3），不用表单默认（4）
	if got, err := l.count(&pb.QuoteReq{UserId: 1, Id: 9}); err != nil || got != 3 {
		t.Fatalf("沿用记录张数: %d %v", got, err)
	}
	// 改少：与出图一样取前几张
	if got, err := l.count(&pb.QuoteReq{UserId: 1, Id: 9, ImageCount: 2}); err != nil || got != 2 {
		t.Fatalf("改少: %d %v", got, err)
	}
	// 改多：与出图同一句拒绝
	out, err := l.Quote(&pb.QuoteReq{UserId: 1, Id: 9, ImageCount: 5})
	if err != nil || out.Available || out.Reason != "当前方案只有 3 张图，增加数量请点「重新生成」重新构思" {
		t.Fatalf("改多应拒: %+v %v", out, err)
	}
	if _, gerr := generateImageCount(3, 5, 3); gerr == nil {
		t.Fatal("出图同口径应拒")
	}
	// 别人的笔记
	out, err = l.Quote(&pb.QuoteReq{UserId: 2, Id: 9})
	if err != nil || out.Available || out.Reason == "" {
		t.Fatalf("不属于本人: %+v %v", out, err)
	}
	// 方案损坏：与出图同一句
	n.PlanScript = sql.NullString{String: "{", Valid: true}
	out, _ = l.Quote(&pb.QuoteReq{UserId: 1, Id: 9})
	if out.Available || out.Reason != "构思结果异常，请重新构思" {
		t.Fatalf("坏方案: %+v", out)
	}
	// 还在构思：按记录张数
	n.Status = model.StatusPlanning
	if got, err := l.count(&pb.QuoteReq{UserId: 1, Id: 9}); err != nil || got != 3 {
		t.Fatalf("构思中: %d %v", got, err)
	}
}

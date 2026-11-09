package logic

import (
	"context"
	"database/sql"
	"net/http"
	"testing"
	"time"

	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/pkg/mediasvc"
)

const (
	ridA = "abc123-asset-aaaaaaaaaaaaaaaaaaaaaaaaaa"
	ridB = "abc123-work-bbbbbbbbbbbbbbbbbbbbbbbbbb"
)

type fakeSigner struct {
	calls [][]string
	ttl   time.Duration
	bad   map[string]bool // 这些 rid 返回 4xx
	down  bool            // 整体 5xx
}

func (f *fakeSigner) ResourceURLs(_ context.Context, rids []string, ttl time.Duration) (map[string]string, error) {
	f.calls = append(f.calls, append([]string(nil), rids...))
	f.ttl = ttl
	if f.down {
		return nil, &mediasvc.Error{Status: http.StatusBadGateway, Code: "upstream", Message: "down"}
	}
	out := map[string]string{}
	for _, r := range rids {
		if f.bad[r] {
			return nil, &mediasvc.Error{Status: http.StatusNotFound, Code: "not_found", Message: "x"}
		}
		out[r] = "https://signed/" + r
	}
	return out, nil
}

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func row(id int64, cover string) *repo.AssetRow {
	r := &repo.AssetRow{}
	r.ID = id
	r.CoverURL = ns(cover)
	return r
}

func TestRidOf(t *testing.T) {
	cases := map[string]string{"mr://" + ridA: ridA, "mr://" + ridA + ".jpg": ridA, "https://x/a.jpg": "", "": ""}
	for in, want := range cases {
		if got := ridOf(in); got != want {
			t.Errorf("ridOf(%q)=%q want %q", in, got, want)
		}
	}
}

func TestResolveRefsReplacesAndBatches(t *testing.T) {
	f := &fakeSigner{}
	rows := []*repo.AssetRow{row(1, "mr://"+ridA), row(2, "https://cdn/x.jpg"), row(3, "mr://"+ridB+".png"), row(4, "mr://"+ridA)}
	rows[1].ResultURL = ns("mr://" + ridB)
	resolveRefs(context.Background(), f, rows)
	if len(f.calls) != 1 || len(f.calls[0]) != 2 {
		t.Fatalf("应整页一次调用且去重：%v", f.calls)
	}
	if f.ttl != refURLTTL {
		t.Fatalf("ttl=%v", f.ttl)
	}
	if rows[0].CoverURL.String != "https://signed/"+ridA || rows[3].CoverURL.String != "https://signed/"+ridA {
		t.Fatalf("mr:// 未替换：%v %v", rows[0].CoverURL, rows[3].CoverURL)
	}
	if rows[1].CoverURL.String != "https://cdn/x.jpg" {
		t.Fatalf("普通地址被改：%v", rows[1].CoverURL)
	}
	if rows[1].ResultURL.String != "https://signed/"+ridB || rows[2].CoverURL.String != "https://signed/"+ridB {
		t.Fatalf("带扩展名 / result_url 未替换：%v %v", rows[1].ResultURL, rows[2].CoverURL)
	}
}

func TestResolveRefsNoRefsNoCall(t *testing.T) {
	f := &fakeSigner{}
	rows := []*repo.AssetRow{row(1, "https://cdn/x.jpg"), row(2, "")}
	resolveRefs(context.Background(), f, rows)
	if len(f.calls) != 0 || rows[0].CoverURL.String != "https://cdn/x.jpg" {
		t.Fatalf("calls=%v cover=%v", f.calls, rows[0].CoverURL)
	}
}

func TestResolveRefsFailureGivesEmpty(t *testing.T) {
	// 整体暂时性失败：全部置空，不逐个重试
	f := &fakeSigner{down: true}
	rows := []*repo.AssetRow{row(1, "mr://"+ridA), row(2, "mr://"+ridB), row(3, "https://cdn/x.jpg")}
	resolveRefs(context.Background(), f, rows)
	if len(f.calls) != 1 {
		t.Fatalf("暂时性失败不应逐个重试：%v", f.calls)
	}
	if rows[0].CoverURL.Valid || rows[0].CoverURL.String != "" || rows[1].CoverURL.String != "" || rows[2].CoverURL.String != "https://cdn/x.jpg" {
		t.Fatalf("%v %v %v", rows[0].CoverURL, rows[1].CoverURL, rows[2].CoverURL)
	}

	// 个别 rid 永久失败：逐个重签，只有坏的那个为空
	f = &fakeSigner{bad: map[string]bool{ridB: true}}
	rows = []*repo.AssetRow{row(1, "mr://"+ridA), row(2, "mr://"+ridB)}
	resolveRefs(context.Background(), f, rows)
	if rows[0].CoverURL.String != "https://signed/"+ridA || rows[1].CoverURL.String != "" {
		t.Fatalf("%v %v", rows[0].CoverURL, rows[1].CoverURL)
	}

	// 不合法 rid 不送签，直接置空；未配置媒体服务也置空
	f = &fakeSigner{}
	rows = []*repo.AssetRow{row(1, "mr://not-a-rid"), row(2, "mr://"+ridA)}
	resolveRefs(context.Background(), f, rows)
	if rows[0].CoverURL.String != "" || rows[1].CoverURL.String != "https://signed/"+ridA || len(f.calls) != 1 || len(f.calls[0]) != 1 {
		t.Fatalf("%v %v %v", rows[0].CoverURL, rows[1].CoverURL, f.calls)
	}
	rows = []*repo.AssetRow{row(1, "mr://"+ridA)}
	resolveRefs(context.Background(), nil, rows)
	if rows[0].CoverURL.String != "" {
		t.Fatalf("%v", rows[0].CoverURL)
	}
}

func TestToAssetAfterResolveNoRawRef(t *testing.T) {
	f := &fakeSigner{down: true}
	r := row(1, "mr://"+ridA)
	r.IsVideo = true
	r.ResultURL = ns("https://x/a.mp4")
	resolveRefs(context.Background(), f, []*repo.AssetRow{r})
	a := toAsset(r, nil)
	if a.Image != "https://x/a.mp4" || a.VideoUrl != "https://x/a.mp4" {
		t.Fatalf("%+v", a)
	}
}

// processFake 支持带处理参数现签的假签名器：记下每次的处理串。
type processFake struct {
	fakeSigner
	processes []string
	failWm    bool
}

func (f *processFake) ResourceURLsWithProcess(_ context.Context, rids []string, _ time.Duration, process string) (map[string]string, error) {
	f.processes = append(f.processes, process)
	if f.failWm {
		return nil, &mediasvc.Error{Status: http.StatusBadGateway, Code: "upstream", Message: "down"}
	}
	out := map[string]string{}
	for _, r := range rids {
		out[r] = "https://signed/" + r + "?x-oss-process=" + process
	}
	return out, nil
}

const wmProcess = "image/watermark,text_WFE,t_85,g_se,x_40,y_40"

// 免费档水印视图：带处理参数现签，绝不走普通（干净）签名；同一页的 mr:// 照常签。
func TestResolveRefsSignsWatermarkViewWithProcess(t *testing.T) {
	f := &processFake{}
	r := row(1, "mrw://"+ridB+".png")
	r.ResultURL = ns("mrw://" + ridB + ".png")
	other := row(2, "mr://"+ridA+".jpg")
	resolveRefsWith(context.Background(), f, wmProcess, []*repo.AssetRow{r, other})

	want := "https://signed/" + ridB + "?x-oss-process=" + wmProcess
	if r.ResultURL.String != want || r.CoverURL.String != want {
		t.Fatalf("result=%q cover=%q want %q", r.ResultURL.String, r.CoverURL.String, want)
	}
	if other.CoverURL.String != "https://signed/"+ridA {
		t.Fatalf("普通引用 = %q", other.CoverURL.String)
	}
	for _, call := range f.calls {
		for _, rid := range call {
			if rid == ridB {
				t.Fatal("水印视图的 rid 不应进入普通（干净）签名")
			}
		}
	}
	if len(f.processes) != 1 || f.processes[0] != wmProcess {
		t.Fatalf("processes = %v", f.processes)
	}
}

// 未配置处理串 / 签名器不支持 / 现签失败：水印视图置空（失败关闭），不下发原始引用、不签干净地址。
func TestResolveRefsWithholdsWatermarkViewWhenItCannotBeWatermarked(t *testing.T) {
	cases := map[string]struct {
		signer  resourceSigner
		process string
	}{
		"no process":        {&processFake{}, ""},
		"plain signer only": {&fakeSigner{}, wmProcess},
		"sign failure":      {&processFake{failWm: true}, wmProcess},
		"no media service":  {nil, wmProcess},
	}
	for name, c := range cases {
		r := row(1, "")
		r.ResultURL = ns("mrw://" + ridB + ".png")
		resolveRefsWith(context.Background(), c.signer, c.process, []*repo.AssetRow{r})
		if r.ResultURL.Valid || r.ResultURL.String != "" {
			t.Fatalf("%s: result = %q, want empty", name, r.ResultURL.String)
		}
		if fs, ok := c.signer.(*fakeSigner); ok && len(fs.calls) != 0 {
			t.Fatalf("%s: 不应调用普通签名", name)
		}
	}
}

func TestWatermarkRidOf(t *testing.T) {
	if got := watermarkRidOf("mrw://" + ridB + ".png"); got != ridB {
		t.Fatalf("watermarkRidOf = %q", got)
	}
	if got := ridOf("mrw://" + ridB + ".png"); got != "" {
		t.Fatalf("ridOf 不应认水印视图: %q", got)
	}
}

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

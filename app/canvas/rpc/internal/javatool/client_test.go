package javatool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSubmitAndTask(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok123" {
			w.WriteHeader(401)
			_, _ = io.WriteString(w, `{"code":2001,"reason":"UNAUTHORIZED","msg":"未登录"}`)
			return
		}
		switch r.URL.Path {
		case PathInpaint:
			if r.Header.Get("Idempotency-Key") != "canvas-x" {
				t.Errorf("Idempotency-Key=%q", r.Header.Get("Idempotency-Key"))
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &gotBody)
			_, _ = io.WriteString(w, `{"code":0,"msg":"success","data":{"taskId":"T1","taskIds":["T1"]}}`)
		case PathUpscale:
			_, _ = io.WriteString(w, `{"code":3003,"reason":"INSUFFICIENT_POINTS","msg":"点数余额不足"}`)
		case "/api/v1/tasks/T1":
			if r.URL.Query().Get("original") != "true" {
				t.Error("应取下载地址 original=true")
			}
			_, _ = io.WriteString(w, `{"code":0,"data":{"taskUid":"T1","status":"SUCCEED","result":{"url":"https://j/r.png"}}}`)
		default:
			w.WriteHeader(502)
			_, _ = io.WriteString(w, `<html>bad gateway</html>`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, 0)
	ctx := context.Background()
	sub, err := c.Submit(ctx, "Bearer tok123", PathInpaint, InpaintReq{Content: "换成红色", Num: 1,
		Images: []InpaintImage{{URL: "https://x/a.png", BBox: []RatioPoint{{0, 0}, {1, 0}, {1, 1}, {0, 1}}}}}, "canvas-x")
	if err != nil || sub.TaskID != "T1" {
		t.Fatalf("%+v %v", sub, err)
	}
	imgs := gotBody["images"].([]any)
	if gotBody["content"] != "换成红色" || len(imgs[0].(map[string]any)["bbox"].([]any)) != 4 {
		t.Fatalf("请求体按 Java 字段名: %v", gotBody)
	}
	_, err = c.Submit(ctx, "tok123", PathUpscale, UpscaleReq{ImageURL: "https://x/a.png", Resolution: "2k"}, "k")
	var je *Error
	if !errors.As(err, &je) || je.Code != 3003 || je.Reason != "INSUFFICIENT_POINTS" {
		t.Fatalf("业务错误: %v", err)
	}
	task, err := c.GetTask(ctx, "tok123", "T1")
	if err != nil || task.Status != StatusSucceed || task.ResultURL != "https://j/r.png" || !task.Terminal() {
		t.Fatalf("%+v %v", task, err)
	}
	_, err = c.GetTask(ctx, "bad", "T1")
	if !errors.As(err, &je) || !je.Unauthorized() {
		t.Fatalf("401: %v", err)
	}
	_, err = c.Submit(ctx, "tok123", "/nope", map[string]any{}, "")
	if !errors.As(err, &je) || !je.Temporary() {
		t.Fatalf("502 非信封应为暂时性: %v", err)
	}
	if _, err := c.Submit(ctx, "", PathUpscale, nil, ""); !errors.Is(err, ErrNoBearer) {
		t.Fatal("没有 Bearer")
	}
	if _, err := New("", 0).GetTask(ctx, "x", "T1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatal("未配置")
	}
}

func TestQuote(t *testing.T) {
	var gotPath, gotAuth, gotIdem string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotIdem = r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Idempotency-Key")
		switch r.URL.Path {
		case PathUpscale + QuoteSuffix:
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &gotBody)
			_, _ = io.WriteString(w, `{"code":0,"data":{"points":30,"priceBasis":{"priceUnit":"PER_IMAGE","quantity":1},"freeQuotaApplied":true}}`)
		case PathOutpaint + QuoteSuffix:
			_, _ = io.WriteString(w, `{"code":3003,"reason":"INSUFFICIENT_POINTS","msg":"点数余额不足"}`)
		default: // 旧版 Java：没有这条路由
			w.WriteHeader(404)
			_, _ = io.WriteString(w, `{"timestamp":"x","status":404,"error":"Not Found"}`)
		}
	}))
	defer srv.Close()
	c := New(srv.URL, 0)

	q, err := c.Quote(context.Background(), "tok123", PathUpscale, UpscaleReq{ImageURL: "https://a/1.png", Resolution: "2k"})
	if err != nil || q.Points != 30 || q.PriceUnit != "PER_IMAGE" || q.Quantity != 1 || !q.FreeQuotaApplied {
		t.Fatalf("quote=%+v err=%v", q, err)
	}
	if gotPath != PathUpscale+"/quote" || gotAuth != "Bearer tok123" || gotIdem != "" || gotBody["imageUrl"] != "https://a/1.png" {
		t.Fatalf("path=%s auth=%s idem=%q body=%v", gotPath, gotAuth, gotIdem, gotBody)
	}
	if _, err := c.Quote(context.Background(), "tok123", PathInpaint, InpaintReq{}); !errors.Is(err, ErrQuoteUnsupported) {
		t.Fatalf("404 应回 ErrQuoteUnsupported: %v", err)
	}
	var je *Error
	if _, err := c.Quote(context.Background(), "tok123", PathOutpaint, RatioConvertReq{}); !errors.As(err, &je) || je.Reason != "INSUFFICIENT_POINTS" {
		t.Fatalf("业务拒绝应原样返回: %v", err)
	}
	if _, err := c.Quote(context.Background(), "", PathUpscale, UpscaleReq{}); !errors.Is(err, ErrNoBearer) {
		t.Fatalf("没有 Bearer: %v", err)
	}
}

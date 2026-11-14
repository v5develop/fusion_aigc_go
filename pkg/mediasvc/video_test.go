package mediasvc

import (
	"context"
	"errors"
	"testing"
)

func TestCutRangesAndCutSatisfyCutter(t *testing.T) {
	c, f := newFake(t, func(path string, body []byte) (int, any) {
		return 200, map[string]any{"segments": []any{
			merge(res(ridB, 160, 120), map[string]any{"start": 0.5, "end": 1.5}),
			merge(res(ridC, 160, 120), map[string]any{"start": 2.0, "end": 3.0}),
		}}
	})
	got, err := c.CutRanges(context.Background(), ridA, []TimeRange{{0.5, 1.5}, {2, 3}}, "")
	if err != nil || len(got) != 2 || got[0].RID != ridB || got[1].RID != ridC || got[1].Start != 2.0 || got[1].End != 3.0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	last := f.last()
	m := decode(t, last.body)
	segs := m["segments"].([]any)
	if last.path != "/v1/mediasvc/video/cut" || last.idem == "" || len(segs) != 2 ||
		m["source"].(map[string]any)["rid"] != ridA || m["purpose"] != "intermediate" {
		t.Fatalf("请求不对：%s %v", last.path, m)
	}
	// 同入参同幂等键；入参变化键变化
	_, _ = c.CutRanges(context.Background(), ridA, []TimeRange{{0.5, 1.5}, {2, 3}}, "")
	_, _ = c.CutRanges(context.Background(), ridA, []TimeRange{{0.5, 1.5}, {2, 3.5}}, "")
	if f.reqs[0].idem != f.reqs[1].idem || f.reqs[1].idem == f.reqs[2].idem {
		t.Fatal("自动幂等键应由入参决定")
	}
	// 显式幂等键原样透传
	_, _ = c.CutRanges(context.Background(), ridA, []TimeRange{{0, 1}}, "my-key")
	if f.last().idem != "my-key" {
		t.Fatalf("idem = %q", f.last().idem)
	}
	// 前置检查
	for name, rs := range map[string][]TimeRange{
		"空": nil, "7 段": make([]TimeRange, 7), "零长度": {{1, 1}}, "反向": {{2, 1}}, "负起点": {{-1, 1}},
	} {
		if len(rs) == 7 {
			for i := range rs {
				rs[i] = TimeRange{0, 1}
			}
		}
		var e *Error
		if _, err := c.CutRanges(context.Background(), ridA, rs, ""); !errors.As(err, &e) || e.Status != 400 || e.Check == "" {
			t.Fatalf("%s 应在客户端被拒: %v", name, err)
		}
	}
}

// *Client 的 Cut 满足 replicate 的 Cutter 接口形状：Cut(ctx, rid, t0, t1, idem) (rid, err)。
func TestCutReturnsRID(t *testing.T) {
	var _ interface {
		Cut(ctx context.Context, sourceRid string, t0, t1 float64, idem string) (string, error)
	} = (*Client)(nil)
	c, _ := newFake(t, func(string, []byte) (int, any) {
		return 200, map[string]any{"segments": []any{merge(res(ridB, 1, 1), map[string]any{"start": 1.0, "end": 2.0})}}
	})
	rid, err := c.Cut(context.Background(), ridA, 1, 2, "")
	if err != nil || rid != ridB {
		t.Fatalf("rid=%q err=%v", rid, err)
	}
}

func TestConcatItemsRequestAndTimeline(t *testing.T) {
	c, f := newFake(t, func(string, []byte) (int, any) {
		return 200, merge(res(ridC, 160, 120), map[string]any{"timeline": []any{
			map[string]any{"index": 0, "kind": "source", "start": 0.0, "duration": 2.0},
			map[string]any{"index": 1, "kind": "placeholder", "start": 2.0, "duration": 1.5},
		}})
	})
	out, err := c.ConcatItems(context.Background(), []ConcatItem{
		{RID: ridA}, {Placeholder: &Placeholder{Duration: 1.5, Text: "这一段还在重做", FontRid: ridB}},
	}, ConcatParams{Width: 720, Height: 1280, DropAudio: true}, "")
	if err != nil || out.RID != ridC || len(out.Timeline) != 2 || out.Timeline[1].Kind != "placeholder" || out.Timeline[1].Start != 2.0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	m := decode(t, f.last().body)
	items := m["items"].([]any)
	ph := items[1].(map[string]any)["placeholder"].(map[string]any)
	if f.last().path != "/v1/mediasvc/video/concat" || items[0].(map[string]any)["source"].(map[string]any)["rid"] != ridA ||
		ph["text"] != "这一段还在重做" || ph["font_rid"] != ridB || m["width"].(float64) != 720 || m["audio"] != "drop" {
		t.Fatalf("请求不对：%v", m)
	}
	// 不给规格 / 不去音轨时不带这些键
	_, _ = c.Concat(context.Background(), []string{ridA, ridB}, ConcatParams{}, "")
	m = decode(t, f.last().body)
	if _, ok := m["width"]; ok {
		t.Fatalf("不应带 width: %v", m)
	}
	if _, ok := m["audio"]; ok {
		t.Fatalf("不应带 audio: %v", m)
	}
	// 前置检查
	var e *Error
	if _, err := c.ConcatItems(context.Background(), nil, ConcatParams{}, ""); !errors.As(err, &e) || e.Check != "request.segments_count" {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.ConcatItems(context.Background(), []ConcatItem{{RID: ridA, Placeholder: &Placeholder{Duration: 1}}}, ConcatParams{}, ""); !errors.As(err, &e) || e.Check != "request.item_exactly_one" {
		t.Fatalf("err = %v", err)
	}
}

func TestTextOpsRequestAndErrors(t *testing.T) {
	c, f := newFake(t, func(path string, body []byte) (int, any) { return 200, res(ridB, 320, 240) })
	start, end := 1.0, 2.0
	slots := []TextSlot{{Text: "店名", Box: TextBox{X: 0.1, Y: 0.1, W: 0.5, H: 0.2}, FontRid: ridC, Size: 40, Color: "#FF0000",
		Stroke: &TextStroke{Width: 2}, Shadow: &TextShadow{}, Start: &start, End: &end}}
	if r, err := c.OverlayText(context.Background(), ridA, slots, ""); err != nil || r.RID != ridB {
		t.Fatalf("r=%+v err=%v", r, err)
	}
	if f.last().path != "/v1/mediasvc/video/overlay-text" {
		t.Fatalf("path = %s", f.last().path)
	}
	m := decode(t, f.last().body)
	sl := m["slots"].([]any)[0].(map[string]any)
	if sl["font_rid"] != ridC || sl["start"].(float64) != 1 || sl["box"].(map[string]any)["w"].(float64) != 0.5 || sl["stroke"].(map[string]any)["width"].(float64) != 2 {
		t.Fatalf("slot = %v", sl)
	}
	slots[0].Start, slots[0].End = nil, nil
	if _, err := c.ImageText(context.Background(), ridA, slots, ""); err != nil || f.last().path != "/v1/mediasvc/image/text" {
		t.Fatalf("err=%v path=%s", err, f.last().path)
	}
	if _, ok := decode(t, f.last().body)["slots"].([]any)[0].(map[string]any)["start"]; ok {
		t.Fatal("图片槽位不应带 start")
	}
	var e *Error
	if _, err := c.ImageText(context.Background(), ridA, nil, ""); !errors.As(err, &e) || e.Check != "request.slots_count" {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.OverlayText(context.Background(), ridA, make([]TextSlot, 17), ""); !errors.As(err, &e) {
		t.Fatalf("err = %v", err)
	}
}

// 服务端的错误体是 {"error": {code, check, message}}：客户端要能取到 Code 与 Check。
func TestErrorParsesServerEnvelope(t *testing.T) {
	c, _ := newFake(t, func(string, []byte) (int, any) {
		return 422, map[string]any{"error": map[string]any{"code": "source_too_short", "class": "source", "retryable": false,
			"check": "video.cut_out_of_range", "message": "参考视频比请求的时长短"}}
	})
	_, err := c.CutRanges(context.Background(), ridA, []TimeRange{{0, 99}}, "")
	var e *Error
	if !errors.As(err, &e) || e.Code != "source_too_short" || e.Check != "video.cut_out_of_range" || e.Status != 422 || !e.Permanent() {
		t.Fatalf("err = %#v", err)
	}
}

func merge(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

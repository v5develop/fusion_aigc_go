package mediasvc

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFrames(t *testing.T) {
	c, f := newFake(t, func(path string, body []byte) (int, any) {
		return 200, map[string]any{"frames": []any{
			map[string]any{"timestamp_s": 0.5, "object": map[string]any{"rid": ridB, "width": 720, "height": 1280}},
			map[string]any{"timestamp_s": 4.5, "object": map[string]any{"rid": ridC, "width": 720, "height": 1280}},
		}}
	})
	got, err := c.Frames(context.Background(), ridA, 2, "")
	if err != nil || len(got) != 2 || got[1].RID != ridC || got[1].TimestampS != 4.5 || got[0].Width != 720 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	last := f.last()
	if last.path != "/v1/mediasvc/frames" || last.idem == "" {
		t.Fatalf("请求不对：%+v", last)
	}
	var body map[string]any
	_ = json.Unmarshal(last.body, &body)
	if body["count"].(float64) != 2 || body["source"].(map[string]any)["rid"] != ridA {
		t.Fatalf("body=%v", body)
	}
	// 同入参同幂等键
	_, _ = c.Frames(context.Background(), ridA, 2, "")
	if f.reqs[0].idem != f.reqs[1].idem {
		t.Fatal("同入参应得到同一个自动幂等键")
	}
	if _, err := c.Frames(context.Background(), ridA, 33, ""); err == nil {
		t.Fatal("count>32 应拒绝")
	}
}

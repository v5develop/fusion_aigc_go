package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// polluted 修复前落库的典型快照：模型节点 data 里 runId / requestId 被补出副本，之后 camel 被新 run 更新、副本陈旧；
// 嵌套 params / module 子节点 / 数组里的对象也有；带大整数、HTML 字符、没有 camel 孪生的下划线键（必须保留）。
const polluted = `{
  "nodes": [
    {"id": "n_1", "type": "text", "position": {"x": 10, "y": 20.5},
     "data": {"text": "<b>白底</b> & 模特 > 1", "title": "提示词"}},
    {"id": "n_2", "type": "model", "parentNode": "m_1",
     "data": {"model": "Banana", "ratio": "3:4", "runId": "run_new", "run_id": "run_old",
              "requestId": "1234567890123456789", "request_id": "1234567890123456789",
              "seed": 9007199254740993, "selectedCandidate": 1,
              "params": {"aspectRatio": "1:1", "aspect_ratio": "1:1", "image_url": "https://oss/a.png?x=1&y=2"},
              "history": [{"taskId": "t2", "task_id": "t1", "score": 1.50}, {"task_id": "only_snake"}]}},
    {"id": "m_1", "type": "module", "data": {"title": "模块", "image_2k": "x", "image2k": "y", "_id": 1, "Id": 2}}
  ],
  "edges": [{"id": "e_1", "source": "n_1", "target": "n_2", "sourceHandle": "text", "targetHandle": "prompt"}],
  "viewport": {"x": 0, "y": 0, "zoom": 1}
}`

func TestCleanSnapshotRemovesOnlyTwinsWithCamel(t *testing.T) {
	out, removed, err := cleanSnapshot([]byte(polluted))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range removed {
		got[r.Path] = r.Differs
	}
	want := map[string]bool{
		"$.nodes[1].data.run_id":              false, // differs → true，下面单独断言
		"$.nodes[1].data.request_id":          false,
		"$.nodes[1].data.params.aspect_ratio": false,
		"$.nodes[1].data.history[0].task_id":  true,
	}
	want["$.nodes[1].data.run_id"] = true
	if len(got) != len(want) {
		t.Fatalf("removed = %+v, want %v", removed, want)
	}
	for p, d := range want {
		if dd, ok := got[p]; !ok || dd != d {
			t.Errorf("path %s: got (present=%v differs=%v), want differs=%v", p, ok, dd, d)
		}
	}
	// 报告按路径排序
	for i := 1; i < len(removed); i++ {
		if removed[i-1].Path > removed[i].Path {
			t.Errorf("removals not sorted: %v", removed)
		}
	}
	s := string(out)
	for _, keep := range []string{
		`"image_url":"https://oss/a.png?x=1&y=2"`, // 无 camel 孪生（imageUrl 不在）→ 保留；& 不转义
		`"task_id":"only_snake"`,                  // 无 camel 孪生 → 保留
		`"image_2k":"x"`, `"_id":1`,               // toSnake 反推不回 → 不是中间件补的
		`"seed":9007199254740993`,     // 大整数原文
		`"score":1.50`,                // 数字原文（不变成 1.5）
		`"text":"<b>白底</b> & 模特 > 1"`, // 不做 HTML 转义
		`"runId":"run_new"`, `"aspectRatio":"1:1"`,
	} {
		if !strings.Contains(s, keep) {
			t.Errorf("output lost %s:\n%s", keep, s)
		}
	}
	for _, gone := range []string{`"run_id"`, `"request_id"`, `"aspect_ratio"`, `"task_id":"t1"`} {
		if strings.Contains(s, gone) {
			t.Errorf("output still has %s", gone)
		}
	}
	// 语义：除去删掉的键外与原快照一致
	var a, b any
	dec := json.NewDecoder(strings.NewReader(polluted))
	dec.UseNumber()
	_ = dec.Decode(&a)
	dec = json.NewDecoder(bytes.NewReader(out))
	dec.UseNumber()
	_ = dec.Decode(&b)
	data := a.(map[string]any)["nodes"].([]any)[1].(map[string]any)["data"].(map[string]any)
	delete(data, "run_id")
	delete(data, "request_id")
	delete(data["params"].(map[string]any), "aspect_ratio")
	delete(data["history"].([]any)[0].(map[string]any), "task_id")
	if !sameJSON(a, b) {
		t.Errorf("semantic drift:\n%s\n%s", mustEnc(a), mustEnc(b))
	}
}

func TestCleanSnapshotIdempotentAndUntouchedBytes(t *testing.T) {
	out, removed, err := cleanSnapshot([]byte(polluted))
	if err != nil || len(removed) == 0 {
		t.Fatalf("first pass: %v %v", removed, err)
	}
	again, removed2, err := cleanSnapshot(out)
	if err != nil || len(removed2) != 0 || !bytes.Equal(again, out) {
		t.Fatalf("second pass not a no-op: %v %v", removed2, err)
	}
	// 干净快照原字节返回（含空白），不重写
	clean := []byte("{ \"nodes\": [ {\"id\":\"a\",\"data\":{\"task_id\":\"x\",\"runId\":\"r\"}} ], \"edges\": [] }")
	got, rm, err := cleanSnapshot(clean)
	if err != nil || len(rm) != 0 || !bytes.Equal(got, clean) {
		t.Fatalf("clean snapshot rewritten: %s %v %v", got, rm, err)
	}
}

func TestCleanSnapshotNestedTwinNotDoubleReported(t *testing.T) {
	// 副本本身是对象且内部也有副本：只报外层一条（整个副本删掉），camel 里的嵌套副本照常删
	raw := `{"data":{"batchId":{"taskId":"a","task_id":"a"},"batch_id":{"taskId":"a","task_id":"a"}}}`
	out, removed, err := cleanSnapshot([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, r := range removed {
		paths = append(paths, r.Path)
	}
	want := []string{"$.data.batchId.task_id", "$.data.batch_id"}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if removed[1].Differs {
		t.Errorf("batch_id should compare equal after cleaning both sides")
	}
	if string(out) != `{"data":{"batchId":{"taskId":"a"}}}` {
		t.Errorf("out = %s", out)
	}
}

func TestCleanSnapshotOddKeysAndBadJSON(t *testing.T) {
	raw := `{"we ird":{"runId":"1","run_id":"0"}}`
	_, removed, err := cleanSnapshot([]byte(raw))
	if err != nil || len(removed) != 1 || removed[0].Path != `$["we ird"].run_id` || !removed[0].Differs {
		t.Fatalf("removed = %+v err=%v", removed, err)
	}
	for _, bad := range []string{`{"a":`, `{"a":1} {"b":2}`, ``} {
		if _, _, err := cleanSnapshot([]byte(bad)); err == nil {
			t.Errorf("bad json %q accepted", bad)
		}
	}
}

func TestPollutedKeyRequiresWhitelist(t *testing.T) {
	// not_a_known_name_xyz 不在白名单：即使有 camel 孪生也不删
	obj := map[string]any{"notAKnownNameXyz": 1, "not_a_known_name_xyz": 1, "runId": 1, "run_id": 2}
	if _, ok := pollutedKey(obj, "not_a_known_name_xyz"); ok {
		t.Error("non-whitelisted key treated as polluted")
	}
	if c, ok := pollutedKey(obj, "run_id"); !ok || c != "runId" {
		t.Error("run_id with runId twin not detected")
	}
	if _, ok := pollutedKey(map[string]any{"run_id": 1}, "run_id"); ok {
		t.Error("run_id without twin treated as polluted")
	}
}

func mustEnc(v any) string { b, _ := encodeJSON(v); return string(b) }

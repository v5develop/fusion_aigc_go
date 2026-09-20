package aigc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubmitFlattensParams(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/aigc/generate" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"code":200,"message":"success","success":true,"data":{"task_id":"x_1","task_type":85,"queue_length":3}}`))
	}))
	defer srv.Close()

	c := New(Conf{Endpoint: srv.URL + "/"})
	res, err := c.Submit(context.Background(), "x_1", DigitalHuman{
		ImagePath: "https://a/face.png", AudioPath: "https://a/v.wav", AspectRatio: "9:16", Resolution: "720p", Duration: 6,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.TaskID != "x_1" || res.TaskType != 85 || res.QueueLength != 3 {
		t.Fatalf("result %+v", res)
	}
	if got["task_id"] != "x_1" || got["task_type"] != float64(85) || got["image_path"] != "https://a/face.png" || got["duration"] != float64(6) {
		t.Fatalf("body %v", got)
	}
	if _, ok := got["model"]; ok {
		t.Fatal("empty optional field should be omitted")
	}
}

func TestSubmitErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"code":400,"message":"task_type=85 缺少 image_path","success":false}`))
	}))
	defer srv.Close()
	c := New(Conf{Endpoint: srv.URL})

	if _, err := c.Submit(context.Background(), "", DigitalHuman{}); err == nil || !strings.Contains(err.Error(), "image_path") {
		t.Fatalf("local validation: %v", err)
	}
	ok := DigitalHuman{ImagePath: "a", AudioPath: "b", AspectRatio: "9:16", Resolution: "720p", Duration: 5}
	if _, err := c.Submit(context.Background(), "", ok); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("server 400: %v", err)
	}
	if _, err := New(Conf{}).Submit(context.Background(), "", ok); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("not configured: %v", err)
	}
}

func TestGetResultStates(t *testing.T) {
	bodies := map[string]string{
		"w":  `{"task_id":"w","status":"waiting","message":"任务正在队列中等待","original_task":null}`,
		"s":  `{"task_id":"s","task_type":75,"status":"success","generated_image_path":"","image_paths":["u0","u1"],"pool":"platform","cost":0}`,
		"f":  `{"task_id":"f","task_type":84,"status":"failed","error_msg":"voice not found","error_code":null,"upstream":null,"cost":0}`,
		"n":  `{"task_id":"n","status":"not_found","message":"任务不存在","original_task":null}`,
		"sc": `{"task_id":"sc","task_type":74,"status":"success","generated_image_path":null,"script":{"title":"t"}}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(bodies[strings.TrimPrefix(r.URL.Path, "/aigc/getRes/")]))
	}))
	defer srv.Close()
	c := New(Conf{Endpoint: srv.URL})
	ctx := context.Background()

	r, err := c.GetResult(ctx, "w")
	if err != nil || r.Done() {
		t.Fatalf("waiting: %+v %v", r, err)
	}
	r, _ = c.GetResult(ctx, "s")
	if !r.Done() || r.Err() != nil || len(r.Outputs()) != 2 || r.Outputs()[1] != "u1" {
		t.Fatalf("success: %+v", r)
	}
	r, _ = c.GetResult(ctx, "f")
	if !r.Done() || r.Err() == nil || !strings.Contains(r.Err().Error(), "voice not found") {
		t.Fatalf("failed: %+v", r)
	}
	r, _ = c.GetResult(ctx, "n")
	if !r.Done() || r.Err() == nil {
		t.Fatalf("not_found: %+v", r)
	}
	r, _ = c.GetResult(ctx, "sc")
	if r.Outputs() != nil || string(r.Script) != `{"title":"t"}` {
		t.Fatalf("script: %+v", r)
	}
}

func TestWait(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch n.Add(1) {
		case 1:
			w.WriteHeader(http.StatusBadGateway) // 抖动：非 JSON，继续重试
			_, _ = w.Write([]byte("bad gateway"))
		case 2:
			_, _ = w.Write([]byte(`{"task_id":"t","status":"running"}`))
		default:
			_, _ = w.Write([]byte(`{"task_id":"t","task_type":85,"status":"success","generated_image_path":"https://oss/x.mp4"}`))
		}
	}))
	defer srv.Close()

	r, err := New(Conf{Endpoint: srv.URL}).Wait(context.Background(), "t", 10*time.Millisecond)
	if err != nil || r.GeneratedImagePath != "https://oss/x.mp4" || n.Load() != 3 {
		t.Fatalf("wait: %+v %v calls=%d", r, err, n.Load())
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		task Task
		ok   bool
	}{
		{NoteImages{Images: []NoteImage{{ImagePrompt: "p", ReferenceMaterials: []int{1}}}, ImageURLs: []string{"a"}}, true},
		{NoteImages{Images: []NoteImage{{ImagePrompt: "p", ReferenceMaterials: []int{2}}}, ImageURLs: []string{"a"}}, false},
		{NoteImages{}, false},
		{VoiceClone{Text: "t"}, false},
		{VoiceClone{Text: "t", Voice: "v"}, true},
		{LongImage{UserText: "t", TemplateURL: "a", MaterialURLs: []string{"1", "2", "3", "4"}}, false},
		{Poster{Template: "t", Materials: []string{"m"}}, true},
		{Poster{Template: "t"}, true}, // 素材选填
		{Poster{Materials: []string{"m"}}, false},
		{Poster{Template: "t", Materials: []string{"1", "2", "3", "4"}}, false},
		{DigitalHuman{ImagePath: "a", AudioPath: "b", AspectRatio: "1:1", Resolution: "720p", Duration: 5}, false},
		{DigitalHuman{ImagePath: "a", AudioPath: "b", AspectRatio: "9:16", Resolution: "720p", Duration: 31}, false},
	}
	for i, c := range cases {
		if err := c.task.Validate(); (err == nil) != c.ok {
			t.Errorf("case %d %T: err=%v want ok=%v", i, c.task, err, c.ok)
		}
	}
}

// 75 的请求体以真实 74→75 交互为准：参考图字段是 images_url，images 带 index。
func TestNoteImagesBody(t *testing.T) {
	b, err := buildBody("t_1", NoteImages{
		Images:    []NoteImage{{Index: 1, ImagePrompt: "p", ReferenceMaterials: []int{1, 2}}},
		ImageURLs: []string{"u1", "u2", "u3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if _, ok := m["images_url"]; !ok || m["image_urls"] != nil || m["model"] != nil || m["size"] != nil || m["task_type"] != float64(75) {
		t.Fatalf("body %s", b)
	}
	if img := m["images"].([]any)[0].(map[string]any); img["index"] != float64(1) {
		t.Fatalf("image %v", img)
	}
}

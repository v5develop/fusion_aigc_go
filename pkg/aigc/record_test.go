package aigc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeRecorder 记下每次落库的快照，供断言。
type fakeRecorder struct {
	mu   sync.Mutex
	subs []Submission
}

func (f *fakeRecorder) RecordSubmission(_ context.Context, s Submission) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subs = append(f.subs, s)
	return nil
}

func newSubmitServer(t *testing.T, status int, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return New(Conf{Endpoint: srv.URL})
}

// 受理的提交：快照带上实际入参与路由后的类型（9 → 86），且入参不含 task_id / task_type。
func TestSubmitRecordsAcceptedSnapshot(t *testing.T) {
	c := newSubmitServer(t, http.StatusOK, `{"code":0,"message":"ok","success":true,"data":{"task_id":"T1","task_type":86}}`)
	rec := &fakeRecorder{}
	c.WithRecorder(rec)

	task := MotionDrive{InputVideo: "https://v/a.mp4", ReferenceImage: "https://i/a.png", Model: "doubao-seedance-2-0-260128"}
	if _, err := c.Submit(context.Background(), "T1", task); err != nil {
		t.Fatalf("submit: %v", err)
	}
	if len(rec.subs) != 1 {
		t.Fatalf("快照条数 = %d，want 1", len(rec.subs))
	}
	s := rec.subs[0]
	if s.TaskID != "T1" || s.TaskType != TypeMotionDrive || s.RoutedType != TypeMotionDriveV2 || s.Error != "" {
		t.Fatalf("快照 = %+v", s)
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(s.Input), &in); err != nil {
		t.Fatalf("入参不是合法 JSON: %v", err)
	}
	if in["input_video"] != "https://v/a.mp4" || in["model"] != "doubao-seedance-2-0-260128" {
		t.Fatalf("入参 = %s", s.Input)
	}
	if _, ok := in["task_id"]; ok || strings.Contains(s.Input, "task_type") {
		t.Fatalf("入参不应含 task_id / task_type: %s", s.Input)
	}
}

// 被执行面拒绝的提交也要留快照：记下发出去的入参与拒绝原因，「记的」与「发的」始终一致。
func TestSubmitRecordsRejectedSnapshot(t *testing.T) {
	c := newSubmitServer(t, http.StatusInternalServerError, `{"code":500,"message":"busy","success":false}`)
	rec := &fakeRecorder{}
	c.WithRecorder(rec)

	task := MotionDrive{InputVideo: "https://v/a.mp4", ReferenceImage: "https://i/a.png"}
	if _, err := c.Submit(context.Background(), "T2", task); err == nil {
		t.Fatal("want submit error")
	}
	if len(rec.subs) != 1 {
		t.Fatalf("快照条数 = %d，want 1", len(rec.subs))
	}
	s := rec.subs[0]
	if s.Error == "" || s.RoutedType != 0 || s.Input == "" {
		t.Fatalf("拒绝的快照 = %+v", s)
	}
}

// 没配置执行面时也记快照（入参 + 未配置原因），不会出现「有的记、有的不记」。
func TestSubmitRecordsWhenNotConfigured(t *testing.T) {
	c := New(Conf{})
	rec := &fakeRecorder{}
	c.WithRecorder(rec)

	if _, err := c.Submit(context.Background(), "T3", MotionDrive{InputVideo: "v", ReferenceImage: "r"}); err == nil {
		t.Fatal("want ErrNotConfigured")
	}
	if len(rec.subs) != 1 || rec.subs[0].Error == "" {
		t.Fatalf("未配置时快照 = %+v", rec.subs)
	}
}

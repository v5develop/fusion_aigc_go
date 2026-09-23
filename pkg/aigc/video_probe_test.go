package aigc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

func testLimits() VideoLimits {
	return VideoLimits{MaxBytes: 50 << 20, MaxSeconds: 180, Formats: []string{"mp4", "mov"}}
}

func TestProbeVideoFormat(t *testing.T) {
	l := testLimits()
	for _, u := range []string{"https://x/a.avi", "https://x/a", "https://x/a.mp3?t=1"} {
		err := l.ProbeVideo(context.Background(), http.DefaultClient, u)
		var ve *VideoError
		if err == nil {
			t.Fatalf("%s 应该被拒", u)
		} else if !asVideoError(err, &ve) || ve.Reason != "badFormat" {
			t.Fatalf("%s: %v", u, err)
		}
	}
}

func TestProbeVideoSize(t *testing.T) {
	l := testLimits()
	cases := []struct {
		name   string
		size   int64
		head   bool
		reason string // 空表示应通过
	}{
		{"HEAD 小文件通过", 10 << 20, true, ""},
		{"HEAD 超 50M 被拒", 60 << 20, true, "tooLarge"},
		{"不认 HEAD 时用 Range 探", 60 << 20, false, "tooLarge"},
		{"拿不到长度就放行（交给边下边数）", -1, true, ""},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead && !c.head {
				w.WriteHeader(http.StatusMethodNotAllowed)
				return
			}
			if c.size < 0 {
				w.WriteHeader(http.StatusOK)
				return
			}
			if r.Header.Get("Range") != "" {
				w.Header().Set("Content-Range", "bytes 0-0/"+strconv.FormatInt(c.size, 10))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write([]byte("x"))
				return
			}
			w.Header().Set("Content-Length", strconv.FormatInt(c.size, 10))
			w.WriteHeader(http.StatusOK)
		}))
		err := l.ProbeVideo(context.Background(), srv.Client(), srv.URL+"/a.mp4")
		srv.Close()
		var ve *VideoError
		switch {
		case c.reason == "" && err != nil:
			t.Errorf("%s: 不该被拒: %v", c.name, err)
		case c.reason != "" && (!asVideoError(err, &ve) || ve.Reason != c.reason):
			t.Errorf("%s: got %v, want %s", c.name, err, c.reason)
		}
	}
}

func TestLimitMsg(t *testing.T) {
	if got := testLimits().LimitMsg(); got != "仅支持 3 分钟、50M 以内的视频，请重新上传" {
		t.Fatalf("文案不对: %s", got)
	}
}

func asVideoError(err error, target **VideoError) bool {
	ve, ok := err.(*VideoError)
	if ok {
		*target = ve
	}
	return ok
}

package tts

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// wavBytes 构造百炼风格的 wav：头里长度字段是 0x7FFFFFFF 占位。
func wavBytes(samples int, placeholder bool) []byte {
	const sr, ch, bits = 24000, 1, 16
	data := samples * ch * bits / 8
	b := make([]byte, 44+data)
	copy(b[0:], "RIFF")
	copy(b[8:], "WAVE")
	copy(b[12:], "fmt ")
	binary.LittleEndian.PutUint32(b[16:], 16)
	binary.LittleEndian.PutUint16(b[20:], 1)
	binary.LittleEndian.PutUint16(b[22:], ch)
	binary.LittleEndian.PutUint32(b[24:], sr)
	binary.LittleEndian.PutUint32(b[28:], sr*ch*bits/8)
	binary.LittleEndian.PutUint16(b[32:], ch*bits/8)
	binary.LittleEndian.PutUint16(b[34:], bits)
	copy(b[36:], "data")
	size := uint32(data)
	if placeholder {
		size = 0x7FFFFFFF
		binary.LittleEndian.PutUint32(b[4:], 0x7FFFFFFF)
	}
	binary.LittleEndian.PutUint32(b[40:], size)
	return b
}

func TestWavDuration(t *testing.T) {
	for _, placeholder := range []bool{false, true} {
		d, err := WavDuration(wavBytes(24000*5+12000, placeholder))
		if err != nil {
			t.Fatal(err)
		}
		if d != 5500*time.Millisecond {
			t.Fatalf("placeholder=%v got %v", placeholder, d)
		}
	}
}

func TestWavDurationInvalid(t *testing.T) {
	if _, err := WavDuration([]byte("not a wav file at all")); err == nil {
		t.Fatal("want error")
	}
}

// 限流（429）会重试，参数错误（400）不重试。
func TestSynthesizeRetry(t *testing.T) {
	old := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryBackoff = old }()

	cases := []struct {
		name      string
		responses []int // 依次返回的状态码
		wantCalls int32
		wantOK    bool
	}{
		{"429 后成功", []int{429, 200}, 2, true},
		{"一直 429，最多 3 次", []int{429, 429, 429, 429}, 3, false},
		{"400 不重试", []int{400, 200}, 1, false},
	}
	for _, c := range cases {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code := c.responses[n.Add(1)-1]
			w.WriteHeader(code)
			if code == 200 {
				_, _ = w.Write([]byte(`{"output":{"audio":{"url":"http://oss/a.wav"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":"Throttling.RateQuota","message":"rate limit"}`))
		}))
		url, err := New().Synthesize(context.Background(), Endpoint{BaseURL: srv.URL, APIKey: "k", Model: "m"}, "你好", "v")
		srv.Close()
		if (err == nil) != c.wantOK || n.Load() != c.wantCalls {
			t.Errorf("%s: url=%q err=%v calls=%d, want ok=%v calls=%d", c.name, url, err, n.Load(), c.wantOK, c.wantCalls)
		}
	}
}

// 下载音频：网络/5xx 重试，4xx 不重试。
func TestDownloadRetry(t *testing.T) {
	old := retryBackoff
	retryBackoff = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryBackoff = old }()

	cases := []struct {
		name      string
		responses []int
		wantCalls int32
		wantOK    bool
	}{
		{"503 后成功", []int{503, 200}, 2, true},
		{"一直 503，最多 3 次", []int{503, 503, 503, 503}, 3, false},
		{"404 不重试", []int{404, 200}, 1, false},
	}
	for _, c := range cases {
		var n atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			code := c.responses[n.Add(1)-1]
			w.WriteHeader(code)
			_, _ = w.Write([]byte("audio"))
		}))
		_, err := New().Download(context.Background(), srv.URL)
		srv.Close()
		if (err == nil) != c.wantOK || n.Load() != c.wantCalls {
			t.Errorf("%s: err=%v calls=%d, want ok=%v calls=%d", c.name, err, n.Load(), c.wantOK, c.wantCalls)
		}
	}
}

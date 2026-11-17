package media

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"testing"
)

// genClipFilter 用 filter_complex 把若干 lavfi 源拼成一条 H.264 片子。
func genClipFilter(t *testing.T, path, graph string, srcs ...string) {
	t.Helper()
	args := []string{"-y", "-loglevel", "error"}
	for _, s := range srcs {
		args = append(args, "-f", "lavfi", "-i", s)
	}
	args = append(args, "-filter_complex", graph, "-pix_fmt", "yuv420p", "-c:v", "libx264", path)
	if out, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
		t.Fatalf("生成失败: %v: %s", err, out)
	}
}

// 解析：补首项、坏值按 0、无行 = nil。
func TestParseMotionScores(t *testing.T) {
	out := []byte("frame:0    pts:1       pts_time:0.25\nlavfi.signalstats.YAVG=1.5\nframe:1 pts:2 pts_time:0.5\nlavfi.signalstats.YAVG=nan\nlavfi.signalstats.YAVG=70.5201\n")
	got := parseMotionScores(out)
	want := []float64{1.5, 1.5, 0, 70.52}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if !near(got[i], want[i], 1e-9) {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	if parseMotionScores([]byte("nothing")) != nil {
		t.Fatal("没有分值应为 nil")
	}
}

func TestCheckMotionReq(t *testing.T) {
	neg, one := -1.0, 1.0
	cases := []struct {
		name string
		req  videoMotionReq
		bad  bool
	}{
		{"缺省", videoMotionReq{Source: SourceRef{Url: "https://x/v.mp4"}}, false},
		{"fps 太大", videoMotionReq{Source: SourceRef{Url: "https://x/v.mp4"}, FPS: 30}, true},
		{"fps 太小", videoMotionReq{Source: SourceRef{Url: "https://x/v.mp4"}, FPS: 0.1}, true},
		{"from 为负", videoMotionReq{Source: SourceRef{Url: "https://x/v.mp4"}, FromS: &neg}, true},
		{"to<=from", videoMotionReq{Source: SourceRef{Url: "https://x/v.mp4"}, FromS: &one, ToS: &one}, true},
		{"来源冲突", videoMotionReq{Rid: "r1", Source: SourceRef{Url: "https://x/v.mp4"}}, true},
		{"没有来源", videoMotionReq{}, true},
	}
	for _, c := range cases {
		req := c.req
		if err := checkMotionReq(&req); (err != nil) != c.bad {
			t.Fatalf("%s: err=%v", c.name, err)
		}
	}
}

func motionOf(t *testing.T, f *apiFixture, body map[string]any) VideoMotionResult {
	t.Helper()
	var out VideoMotionResult
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/motion-profile", body, testToken, "")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

// 真实 ffmpeg：前 3 秒静止、后 3 秒运动的片子，静止段分值 ≈ 0，运动段明显更大，硬切处最大；from_s / to_s 截取窗口。
func TestVideoMotionProfileStaticThenMotion(t *testing.T) {
	requireFFmpeg(t)
	requireFFmpegFilter(t, "tblend")
	requireFFmpegFilter(t, "signalstats")
	f := newAPIFixture(t)
	clip := filepath.Join(f.dir, "sm.mp4")
	genClipFilter(t, clip, "[0][1]concat=n=2:v=1:a=0",
		"color=c=gray:s=160x120:r=10:d=3", "testsrc=duration=3:size=160x120:rate=10")
	r := motionOf(t, f, map[string]any{"source": f.url("sm.mp4")})
	if r.FPS != 4 || len(r.Scores) < 20 || len(r.Scores) > 26 {
		t.Fatalf("fps=%v 采样数=%d：%+v", r.FPS, len(r.Scores), r)
	}
	maxStatic, minMotion, peak, peakAt := 0.0, 1e9, 0.0, 0
	for i, v := range r.Scores {
		ts := float64(i) / r.FPS
		switch {
		case ts < 2.8:
			maxStatic = max(maxStatic, v)
		case ts > 3.4:
			minMotion = min(minMotion, v)
		}
		if v > peak {
			peak, peakAt = v, i
		}
	}
	if maxStatic > 0.5 || minMotion < 1 || minMotion < 5*maxStatic+0.5 {
		t.Fatalf("静止段应≈0、运动段明显更大：static<=%.3f motion>=%.3f scores=%v", maxStatic, minMotion, r.Scores)
	}
	if at := float64(peakAt) / r.FPS; at < 2.9 || at > 3.3 {
		t.Fatalf("硬切（第 3 秒）应是峰值：峰值在 %.2fs", at)
	}
	// 截取后半段
	w := motionOf(t, f, map[string]any{"source": f.url("sm.mp4"), "from_s": 3.5, "to_s": 5.5, "fps": 2})
	if w.FPS != 2 || w.FromS != 3.5 || len(w.Scores) < 3 || len(w.Scores) > 6 {
		t.Fatalf("窗口：%+v", w)
	}
	for _, v := range w.Scores {
		if v < 1 {
			t.Fatalf("后半段都在运动：%v", w.Scores)
		}
	}
}

// 没有视频流（纯音频）→ 空曲线而不是错误。
func TestVideoMotionProfileNoVideoIsEmpty(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	p := filepath.Join(f.dir, "a.m4a")
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=duration=2", "-c:a", "aac", p).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if r := motionOf(t, f, map[string]any{"source": f.url("a.m4a")}); len(r.Scores) != 0 {
		t.Fatalf("%+v", r)
	}
}

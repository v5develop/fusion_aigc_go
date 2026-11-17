package media

import (
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 改画幅算子：素材全用 ffmpeg 现生成。覆盖尺寸（长边保持、偶数）、pad / crop、音轨拷贝、无声源、参数校验与幂等重放。

func decodeReframe(t *testing.T, resp io.Reader) reframeResp {
	t.Helper()
	var out reframeResp
	if err := json.NewDecoder(resp).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestReframeSize(t *testing.T) {
	cases := []struct {
		w, h   int
		ratio  string
		ow, oh int
	}{
		{1280, 720, "9:16", 720, 1280},   // 横版转竖版：长边 1280 保持
		{720, 1280, "16:9", 1280, 720},   // 竖版转横版
		{720, 1280, "1:1", 1280, 1280},   // 方形：长边为边长
		{1920, 1080, "3:4", 1440, 1920},  // 3:4：长边 1920
		{1080, 1920, "4:3", 1920, 1440},  // 4:3
		{160, 120, "9:16", 90, 160},      // 小片：短边取偶数
		{4096, 2160, "16:9", 3840, 2160}, // 长边封顶 3840
		{161, 121, "1:1", 160, 160},      // 奇数长边落到偶数
	}
	for _, c := range cases {
		ow, oh := reframeSize(c.w, c.h, reframeRatios[c.ratio])
		if ow != c.ow || oh != c.oh || ow%2 != 0 || oh%2 != 0 {
			t.Errorf("%dx%d → %s 得 %dx%d，期望 %dx%d（须偶数）", c.w, c.h, c.ratio, ow, oh, c.ow, c.oh)
		}
	}
}

func TestReframeFilterShape(t *testing.T) {
	pad := reframeFilter("pad", 720, 1280)
	for _, want := range []string{"split=2", "gblur", "force_original_aspect_ratio=increase", "force_original_aspect_ratio=decrease", "overlay=(W-w)/2:(H-h)/2", "[vout]"} {
		if !strings.Contains(pad, want) {
			t.Fatalf("pad 滤镜缺 %q: %s", want, pad)
		}
	}
	crop := reframeFilter("crop", 720, 1280)
	if !strings.Contains(crop, "crop=720:1280") || strings.Contains(crop, "gblur") {
		t.Fatalf("crop 滤镜不对: %s", crop)
	}
}

// pad 模式的底必须是模糊画面而不是黑边：直接用同一条滤镜跑一遍，取左上角像素亮度。
func TestReframePadBackgroundNotBlack(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	src, out := filepath.Join(dir, "s.mp4"), filepath.Join(dir, "o.mp4")
	makeSpecClip(t, src, 1, "320x240", 10, 0)
	for mode, wantBright := range map[string]bool{"pad": true} {
		ow, oh := reframeSize(320, 240, reframeRatios["9:16"])
		args := []string{"-y", "-loglevel", "error", "-i", src, "-filter_complex", reframeFilter(mode, ow, oh), "-map", "[vout]", "-frames:v", "1", out}
		if b, err := exec.Command("ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
		raw, err := exec.Command("ffmpeg", "-loglevel", "error", "-i", out, "-vf", "crop=8:8:0:0,format=gray", "-f", "rawvideo", "-").Output()
		if err != nil || len(raw) == 0 {
			t.Fatalf("取像素失败: %v", err)
		}
		sum := 0
		for _, v := range raw {
			sum += int(v)
		}
		if avg := sum / len(raw); (avg > 24) != wantBright {
			t.Fatalf("%s 左上角亮度 %d，底应是模糊画面而非黑边", mode, avg)
		}
	}
}

func TestVideoReframe(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "land.mp4"), 3, "160x120", 10, 44100) // 横版 4:3，有声
	makeSpecClip(t, filepath.Join(f.dir, "mute.mp4"), 2, "120x160", 10, 0)     // 竖版，无声
	land := ingestRID(t, f, "land.mp4", "intermediate")
	mute := ingestRID(t, f, "mute.mp4", "intermediate")
	rid := func(r string) map[string]string { return map[string]string{"rid": r} }

	cases := []struct {
		name      string
		src       string
		ratio     string
		mode      string
		w, h      int
		wantAudio bool
		wantDur   float64
	}{
		{"横版转 9:16 pad（缺省 mode）", land, "9:16", "", 90, 160, true, 3},
		{"横版转 9:16 crop", land, "9:16", "crop", 90, 160, true, 3},
		{"横版转 1:1 pad", land, "1:1", "pad", 160, 160, true, 3},
		{"竖版转 16:9 pad，无声源成片也无声", mute, "16:9", "pad", 160, 90, false, 2},
		{"竖版转 4:3 crop", mute, "4:3", "crop", 160, 120, false, 2},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"source": rid(tc.src), "ratio": tc.ratio, "purpose": "intermediate"}
			if tc.mode != "" {
				body["mode"] = tc.mode
			}
			resp := postJSON(t, f, "/v1/mediasvc/video/reframe", fmt.Sprintf("k-reframe-ok-%d", i), body)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			out := decodeReframe(t, resp.Body)
			o := out.Object
			if o.RID == "" || o.RID == tc.src || o.Width != tc.w || o.Height != tc.h || o.HasAudio != tc.wantAudio || !near(o.DurationS, tc.wantDur, 0.4) {
				t.Fatalf("object = %+v，期望 %dx%d 有声=%v 时长 %.1f", o, tc.w, tc.h, tc.wantAudio, tc.wantDur)
			}
			wantMode := tc.mode
			if wantMode == "" {
				wantMode = "pad"
			}
			if out.Ratio != tc.ratio || out.Mode != wantMode {
				t.Fatalf("回显不对: %+v", out)
			}
		})
	}

	errCases := []struct {
		name   string
		body   map[string]any
		status int
		code   string
		check  string
	}{
		{"不支持的比例", map[string]any{"source": rid(land), "ratio": "21:9"}, 400, "invalid_request", "request.ratio"},
		{"缺比例", map[string]any{"source": rid(land)}, 400, "invalid_request", "request.ratio"},
		{"不支持的 mode", map[string]any{"source": rid(land), "ratio": "1:1", "mode": "stretch"}, 400, "invalid_request", "request.mode"},
		{"来源只接受 rid", map[string]any{"source": map[string]string{"url": f.origin.URL + "/land.mp4"}, "ratio": "1:1"}, 400, "invalid_request", "request.source_rid_only"},
	}
	for i, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["purpose"] = "intermediate"
			assertCheck(t, postJSON(t, f, "/v1/mediasvc/video/reframe", fmt.Sprintf("k-reframe-err-%d", i), tc.body), tc.status, tc.code, tc.check)
		})
	}
	t.Run("缺 Idempotency-Key", func(t *testing.T) {
		resp := postJSON(t, f, "/v1/mediasvc/video/reframe", "", map[string]any{"source": rid(land), "ratio": "1:1", "purpose": "intermediate"})
		assertAPIError(t, resp, 400, "invalid_request")
	})
}

func TestVideoReframeIdempotentReplay(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 2, "160x120", 10, 44100)
	v := ingestRID(t, f, "v.mp4", "intermediate")
	body := map[string]any{"source": map[string]string{"rid": v}, "ratio": "9:16", "mode": "pad", "purpose": "intermediate"}
	first := decodeReframe(t, postJSON(t, f, "/v1/mediasvc/video/reframe", "k-reframe-replay", body).Body)
	resp := postJSON(t, f, "/v1/mediasvc/video/reframe", "k-reframe-replay", body)
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("同键同体应回放")
	}
	if again := decodeReframe(t, resp.Body); again.Object.RID != first.Object.RID {
		t.Fatalf("回放 rid 不一致: %s vs %s", again.Object.RID, first.Object.RID)
	}
}

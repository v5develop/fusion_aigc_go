package media

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

// 保留原声贴回算子：素材全用 ffmpeg 现生成。覆盖对齐、补静音、截断、原片无声、参数校验与幂等重放。

func decodeMux(t *testing.T, resp io.Reader) muxAudioResp {
	t.Helper()
	var out muxAudioResp
	if err := json.NewDecoder(resp).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVideoMuxAudio(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v4.mp4"), 4, "160x120", 10, 0)      // 生成视频：4 秒，无声
	makeSpecClip(t, filepath.Join(f.dir, "v2.mp4"), 2, "160x120", 10, 0)      // 更短的视频：2 秒
	makeSpecClip(t, filepath.Join(f.dir, "src.mp4"), 3, "160x120", 10, 44100) // 原片：3 秒，有声
	v4 := ingestRID(t, f, "v4.mp4", "intermediate")
	v2 := ingestRID(t, f, "v2.mp4", "intermediate")
	src := ingestRID(t, f, "src.mp4", "intermediate")
	rid := func(r string) map[string]string { return map[string]string{"rid": r} }
	rg := func(a, b float64) map[string]float64 { return map[string]float64{"start": a, "end": b} }

	cases := []struct {
		name       string
		video      string
		ranges     []map[string]float64
		wantDur    float64
		wantAudio  float64
		wantPadded float64
	}{
		{"两段区间首尾相接，成片比原片长 → 末尾补静音", v4, []map[string]float64{rg(0, 2), rg(2, 3)}, 4, 3, 1},
		{"区间超出原片时长 → 超出部分补静音，不循环", v4, []map[string]float64{rg(0, 4)}, 4, 3, 1},
		{"原片音轨比成片长 → 截到成片长度", v2, []map[string]float64{rg(0, 3)}, 2, 2, 0},
		{"区间整段落在原片之外 → 全静音但仍有音轨", v4, []map[string]float64{rg(5, 7)}, 4, 0, 4},
		{"选取原片中段", v2, []map[string]float64{rg(1, 3)}, 2, 2, 0},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, f, "/v1/mediasvc/video/mux-audio", fmt.Sprintf("k-mux-ok-%d", i),
				map[string]any{"video": rid(tc.video), "audio": rid(src), "ranges": tc.ranges, "purpose": "intermediate"})
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d", resp.StatusCode)
			}
			out := decodeMux(t, resp.Body)
			o := out.Object
			if o.RID == "" || !o.HasAudio || !near(o.DurationS, tc.wantDur, 0.35) || o.Width != 160 || o.Height != 120 {
				t.Fatalf("object = %+v, want dur %.2f 160x120 有声", o, tc.wantDur)
			}
			if !near(out.AudioSeconds, tc.wantAudio, 0.05) || !near(out.PaddedSeconds, tc.wantPadded, 0.05) {
				t.Fatalf("audio=%.3f padded=%.3f, want %.1f / %.1f", out.AudioSeconds, out.PaddedSeconds, tc.wantAudio, tc.wantPadded)
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
		{"原片没有音轨", map[string]any{"video": rid(v4), "audio": rid(v2), "ranges": []any{rg(0, 1)}}, 422, "source_rejected", "audio.no_audio_stream"},
		{"没有区间", map[string]any{"video": rid(v4), "audio": rid(src), "ranges": []any{}}, 400, "invalid_request", "request.ranges_count"},
		{"区间 end<=start", map[string]any{"video": rid(v4), "audio": rid(src), "ranges": []any{rg(2, 2)}}, 400, "invalid_request", "request.segment_range"},
		{"区间起点为负", map[string]any{"video": rid(v4), "audio": rid(src), "ranges": []any{rg(-1, 2)}}, 400, "invalid_request", "request.segment_range"},
		{"视频只接受 rid", map[string]any{"video": map[string]string{"url": f.origin.URL + "/v4.mp4"}, "audio": rid(src), "ranges": []any{rg(0, 1)}}, 400, "invalid_request", "request.source_rid_only"},
	}
	for i, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			tc.body["purpose"] = "intermediate"
			assertCheck(t, postJSON(t, f, "/v1/mediasvc/video/mux-audio", fmt.Sprintf("k-mux-err-%d", i), tc.body), tc.status, tc.code, tc.check)
		})
	}

	t.Run("缺 Idempotency-Key", func(t *testing.T) {
		resp := postJSON(t, f, "/v1/mediasvc/video/mux-audio", "", map[string]any{"video": rid(v4), "audio": rid(src), "ranges": []any{rg(0, 1)}, "purpose": "intermediate"})
		assertAPIError(t, resp, 400, "invalid_request")
	})
}

func TestVideoMuxAudioIdempotentReplay(t *testing.T) {
	requireFFmpeg(t)
	f := newCopyFixture(t)
	makeSpecClip(t, filepath.Join(f.dir, "v.mp4"), 2, "160x120", 10, 0)
	makeSpecClip(t, filepath.Join(f.dir, "s.mp4"), 2, "160x120", 10, 44100)
	v := ingestRID(t, f, "v.mp4", "intermediate")
	s := ingestRID(t, f, "s.mp4", "intermediate")
	body := map[string]any{"video": map[string]string{"rid": v}, "audio": map[string]string{"rid": s},
		"ranges": []any{map[string]float64{"start": 0, "end": 2}}, "purpose": "intermediate"}
	first := decodeMux(t, postJSON(t, f, "/v1/mediasvc/video/mux-audio", "k-mux-replay", body).Body)
	resp := postJSON(t, f, "/v1/mediasvc/video/mux-audio", "k-mux-replay", body)
	if resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatal("同键同体应回放")
	}
	if again := decodeMux(t, resp.Body); again.Object.RID != first.Object.RID {
		t.Fatalf("回放 rid 不一致: %s vs %s", again.Object.RID, first.Object.RID)
	}
}

func TestMuxFilterShape(t *testing.T) {
	fg, covered := muxFilter([]cutRange{{0, 2}, {5, 7}}, 3, 4, 44100)
	if !near(covered, 2, 1e-9) {
		t.Fatalf("covered=%v", covered)
	}
	for _, want := range []string{"atrim=start=0:end=2", "anullsrc=r=44100", "concat=n=2:v=0:a=1", "apad=whole_dur=4", "[aout]"} {
		if !strings.Contains(fg, want) {
			t.Fatalf("滤镜缺 %q: %s", want, fg)
		}
	}
}

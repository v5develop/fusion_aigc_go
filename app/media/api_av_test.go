package media

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 时间线：两段输入按起点拼成整条音轨，结果落桶并带 rid。
func TestAudioTimelineMixesSegmentsIntoOneObject(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/audio/timeline", map[string]any{
		"sources":       []map[string]string{f.url("a.mp4"), f.url("a.mp4")},
		"starts":        []float64{0, 1.5},
		"total_seconds": 4,
		"purpose":       "intermediate",
	}, testToken, "k-timeline")
	res := decodeMediaResult(t, resp)
	if res.Object.RID == "" || res.Object.Class != classWork {
		t.Fatalf("timeline result = %+v", res.Object)
	}
}

// 时间线的起点数与来源数必须一致。
func TestAudioTimelineRejectsMismatchedStarts(t *testing.T) {
	f := newAPIFixture(t)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/audio/timeline", map[string]any{
		"sources":       []map[string]string{f.url("a.mp4")},
		"starts":        []float64{0, 1},
		"total_seconds": 4,
		"purpose":       "intermediate",
	}, testToken, "k-timeline-bad")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

// 换音轨：视频流拷贝，结果落桶。
func TestReplaceAudioProducesObject(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/replace-audio", map[string]any{
		"video": f.url("a.mp4"), "audio": f.url("a.mp4"), "purpose": "intermediate"}, testToken, "k-replace"))
	if res.Object.Sha256 == "" || res.Object.Class != classWork {
		t.Fatalf("replace result = %+v", res.Object)
	}
}

// 烧字幕：空字幕直接拒绝，不进入 ffmpeg。
func TestBurnSubtitlesRejectsEmptySubtitle(t *testing.T) {
	f := newAPIFixture(t)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/burn-subtitles", map[string]any{
		"video": f.url("a.mp4"), "srt": "", "purpose": "intermediate"}, testToken, "k-burn-empty")
	assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
}

// 烧字幕：正常字幕落桶。需要 ffmpeg 带 subtitles 滤镜（libass）；镜像里有，本机没有时跳过。
func TestBurnSubtitlesProducesObject(t *testing.T) {
	requireFFmpeg(t)
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-filters").Output(); err != nil || !strings.Contains(string(out), " subtitles ") {
		t.Skip("本机 ffmpeg 没有 subtitles 滤镜（libass），跳过；容器镜像里有")
	}
	f := newAPIFixture(t)
	makeAVClip(t, filepath.Join(f.dir, "a.mp4"))
	srt := "1\n00:00:00,000 --> 00:00:01,000\n你好\n"
	res := decodeMediaResult(t, postWithKey(t, f.ts.URL, "/v1/mediasvc/video/burn-subtitles", map[string]any{
		"video": f.url("a.mp4"), "srt": srt, "purpose": "intermediate"}, testToken, "k-burn"))
	if res.Object.Sha256 == "" {
		t.Fatalf("burn result = %+v", res.Object)
	}
}

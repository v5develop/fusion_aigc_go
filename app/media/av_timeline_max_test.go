package media

import (
	"context"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 每段最长时长：不传（或为 0）时与原滤镜逐字相同（金样不变）；传了就在 adelay 之前 atrim。
func TestTimelineFilterMax(t *testing.T) {
	same, err := TimelineFilterMax([]float64{0, 1.5}, nil, 4)
	base, _ := TimelineFilter([]float64{0, 1.5}, 4)
	if err != nil || same != base {
		t.Fatalf("不传最长时长必须与 TimelineFilter 一致：%v\n%s\n%s", err, same, base)
	}
	zero, _ := TimelineFilterMax([]float64{0, 1.5}, []float64{0, 0}, 4)
	if zero != base {
		t.Fatalf("全 0 等于不截：%s", zero)
	}
	got, err := TimelineFilterMax([]float64{0, 1.5}, []float64{1.2, 0}, 4)
	if err != nil {
		t.Fatal(err)
	}
	want := "[0:a]atrim=duration=1.2,asetpts=PTS-STARTPTS,adelay=0:all=1[a0];[1:a]adelay=1500:all=1[a1];[a0][a1]amix=inputs=2:normalize=0,apad,atrim=duration=4,asetpts=N/SR/TB[aout]"
	if got != want {
		t.Fatalf("filter =\n%s\nwant\n%s", got, want)
	}
	if _, err := TimelineFilterMax([]float64{0, 1}, []float64{1}, 4); err == nil {
		t.Fatal("数量不一致必须报错")
	}
}

// 真实 ffmpeg：第一段 3 秒的音被截到 1 秒，1~3 秒之间应是静音；第二段从第 3 秒起照常有声。
func TestAudioTimelineMaxTruncatesClip(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	clip := filepath.Join(dir, "a.mp4")
	makeAVClip(t, clip)
	svc := newAVService(t)
	mp3 := filepath.Join(dir, "a.mp3")
	ctx := context.Background()
	if err := svc.ExtractAudio(ctx, clip, mp3); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "t.m4a")
	if err := svc.AudioTimelineMax(ctx, []string{mp3, mp3}, []float64{0, 3}, []float64{1, 0}, 6, 44100, 1, out); err != nil {
		t.Fatal(err)
	}
	if !near(durationOf(t, out), 6, 0.3) {
		t.Fatalf("总时长 = %v，期望 6", durationOf(t, out))
	}
	meanVolume := func(from, to string) string {
		b, err := exec.Command("ffmpeg", "-hide_banner", "-i", out, "-af", "atrim="+from+":"+to+",volumedetect", "-f", "null", "-").CombinedOutput()
		if err != nil {
			t.Fatalf("volumedetect: %v %s", err, b)
		}
		for _, ln := range strings.Split(string(b), "\n") {
			if i := strings.Index(ln, "mean_volume:"); i >= 0 {
				return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(ln[i+len("mean_volume:"):]), "dB"))
			}
		}
		return "none"
	}
	if v := meanVolume("1.3", "2.7"); v != "-91.0" && v != "-inf" && v != "none" {
		t.Fatalf("被截掉的区间应静音，实际 mean_volume=%s dB", v)
	}
	if v := meanVolume("3.2", "3.9"); v == "-91.0" || v == "-inf" || v == "none" {
		t.Fatalf("第二段应有声，实际 mean_volume=%s dB", v)
	}
}

// 接口：max_seconds 数量必须与 sources 一致，且不能为负。
func TestAudioTimelineMaxSecondsValidation(t *testing.T) {
	f := newAPIFixture(t)
	for i, max := range [][]float64{{1, 2}, {-1}} {
		resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/audio/timeline", map[string]any{
			"sources": []map[string]string{f.url("a.mp4")}, "starts": []float64{0}, "max_seconds": max,
			"total_seconds": 4, "purpose": "intermediate",
		}, testToken, "k-timeline-max-bad-"+string(rune('a'+i)))
		assertAPIError(t, resp, http.StatusBadRequest, "invalid_request")
	}
}

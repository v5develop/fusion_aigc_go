package media

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 解析：黑屏行、定格 start/end、片尾未结束的定格（只有 start）。
func TestParseInspectOutput(t *testing.T) {
	raw := []byte("[blackdetect @ 0x1] black_start:0 black_end:2.5 black_duration:2.5\n" +
		"[freezedetect @ 0x2] lavfi.freezedetect.freeze_start: 1\n" +
		"[freezedetect @ 0x2] lavfi.freezedetect.freeze_duration: 1.5\n" +
		"[freezedetect @ 0x2] lavfi.freezedetect.freeze_end: 2.5\n" +
		"[freezedetect @ 0x2] lavfi.freezedetect.freeze_start: 4\n")
	black := parseBlackSpans(raw)
	freeze := parseFreezeSpans(raw, 5)
	if len(black) != 1 || len(freeze) != 2 || freeze[1].end != 5 {
		t.Fatalf("black=%v freeze=%v", black, freeze)
	}
	res := buildInspectResult(5, black, freeze)
	if !near(res.BlackRatio, 0.5, 1e-9) || !near(res.FreezeRatio, 0.5, 1e-9) { // 1~2.5 与 4~5
		t.Fatalf("res=%+v", res)
	}
}

// 重叠区间合并、超出时长裁剪、时长未知时以区间末端兜底。
func TestCoveredSecondsMergesAndClamps(t *testing.T) {
	if got := coveredSeconds([]span{{0, 2}, {1, 3}, {5, 9}}, 6); !near(got, 4, 1e-9) {
		t.Fatalf("got %v", got)
	}
	res := buildInspectResult(0, []span{{0, 2}}, nil)
	if res.DurationS != 2 || res.BlackRatio != 1 {
		t.Fatalf("res=%+v", res)
	}
}

func inspectOf(t *testing.T, f *apiFixture, name, key string) VideoInspectResult {
	t.Helper()
	var out VideoInspectResult
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/inspect", map[string]any{"source": f.url(name)}, testToken, key)
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func genClip(t *testing.T, path, src string) {
	t.Helper()
	if out, err := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", src,
		"-pix_fmt", "yuv420p", "-c:v", "libx264", path).CombinedOutput(); err != nil {
		t.Fatalf("生成失败: %v: %s", err, out)
	}
}

// 真实 ffmpeg：正常片、全黑片、定格片（静态纯色）、损坏文件各自的判定。
func TestVideoInspectVerdicts(t *testing.T) {
	requireFFmpeg(t)
	f := newAPIFixture(t)
	genClip(t, filepath.Join(f.dir, "ok.mp4"), "testsrc=duration=3:size=160x120:rate=10")
	genClip(t, filepath.Join(f.dir, "black.mp4"), "color=c=black:s=160x120:r=10:d=3")
	genClip(t, filepath.Join(f.dir, "frozen.mp4"), "color=c=red:s=160x120:r=10:d=3")
	if err := os.WriteFile(filepath.Join(f.dir, "bad.mp4"), []byte("this is not a video at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := inspectOf(t, f, "ok.mp4", "i1"); !r.Decodable || r.BlackRatio > 0.05 || r.FreezeRatio > 0.05 {
		t.Fatalf("正常片被误判: %+v", r)
	}
	if r := inspectOf(t, f, "black.mp4", "i2"); !r.Decodable || r.BlackRatio < 0.9 {
		t.Fatalf("黑屏未识别: %+v", r)
	}
	if r := inspectOf(t, f, "frozen.mp4", "i3"); !r.Decodable || r.FreezeRatio < 0.8 || r.BlackRatio > 0.05 {
		t.Fatalf("定格未识别: %+v", r)
	}
	if r := inspectOf(t, f, "bad.mp4", "i4"); r.Decodable {
		t.Fatalf("损坏文件应 decodable=false: %+v", r)
	}
}

func TestVideoInspectRejectsBadSource(t *testing.T) {
	f := newAPIFixture(t)
	resp := postWithKey(t, f.ts.URL, "/v1/mediasvc/video/inspect", map[string]any{"source": map[string]string{}}, testToken, "i5")
	assertAPIError(t, resp, 400, "invalid_request")
}

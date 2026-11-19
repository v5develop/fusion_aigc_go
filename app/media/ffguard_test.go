package media

import (
	"reflect"
	"testing"
)

func TestGuardInputsPrecedesEveryInput(t *testing.T) {
	g := []string{"-protocol_whitelist", "file,pipe"}
	got := guardInputs("/usr/bin/ffmpeg", []string{"-y", "-i", "a.mp4", "-i", "b.png", "out.mp4"})
	want := append(append(append(append([]string{"-y"}, g...), "-i", "a.mp4"), g...), "-i", "b.png", "out.mp4")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	got = guardInputs("ffprobe", []string{"-v", "quiet", "a.mp4"})
	if !reflect.DeepEqual(got, append(g, "-v", "quiet", "a.mp4")) {
		t.Fatalf("ffprobe: %v", got)
	}
	// concat：-f concat 在白名单之前，白名单仍紧贴 -i
	got = guardInputs("ffmpeg", []string{"-f", "concat", "-safe", "0", "-i", "l.txt", "o"})
	if got[4] != "-protocol_whitelist" || got[6] != "-i" {
		t.Fatalf("concat: %v", got)
	}
}

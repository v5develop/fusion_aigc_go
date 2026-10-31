package svc

import (
	"strings"
	"testing"

	"aigc-local-life/app/studio/rpc/internal/config"
)

// 任务中心坐标 = md5("<前缀>:<id>")、origin_ref = "<前缀>:<id>"：前缀与现有来源撞了会互相覆盖任务中心的行，
// admin 的 parseOriginRef 也无法分流。现有：img（海报/长图）、vid、note、dh、cw。
func TestTaskSourcesPrefixes(t *testing.T) {
	var c config.Config
	c.TaskIDPrefix.Tryon, c.TaskIDPrefix.Pose, c.TaskIDPrefix.ImageGen = "tyo", "pse", "simg"
	c.EntryCodes.Tryon, c.EntryCodes.Pose, c.EntryCodes.ImageGen = "studio_tryon", "studio_pose", "studio_imagegen"
	taken := map[string]bool{"img": true, "vid": true, "note": true, "dh": true, "cw": true}
	seen := map[string]bool{}
	for _, s := range TaskSources(c) {
		if s.Prefix == "" || strings.Contains(s.Prefix, "_") || taken[s.Prefix] || seen[s.Prefix] {
			t.Fatalf("前缀不可用：%q", s.Prefix)
		}
		seen[s.Prefix] = true
		if s.Table != "afv_studio_task" || s.Kind == "" || s.EntryCode == "" || s.UpdateTimeCol != "updated_at" {
			t.Fatalf("源配置不全：%+v", s)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("应有 3 个源，得到 %d", len(seen))
	}
}

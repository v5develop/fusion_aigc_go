package analysisfix

import (
	"strings"
	"testing"
)

func rules(cs []Correction) map[string]Correction {
	m := map[string]Correction{}
	for _, c := range cs {
		m[c.Rule+"/"+c.Field] = c
	}
	return m
}

func TestTimeSwapped(t *testing.T) {
	d := &Doc{Sections: []Section{{T0: 3, T1: 1}, {T0: 3, T1: 6}}, Lines: []Line{{T0: 2, T1: 1, Text: "a"}}, Others: []Span{{T0: 5, T1: 4}}}
	cs := rules(Apply(d, Config{Disabled: []string{RuleDurationScale}}))
	if d.Sections[0].T0 != 1 || d.Sections[0].T1 != 3 || d.Lines[0].T0 != 1 || d.Others[0].T1 != 5 {
		t.Fatalf("应交换：%+v", d)
	}
	for _, k := range []string{"TIME_SWAPPED/sections", "TIME_SWAPPED/lines", "TIME_SWAPPED/timeline"} {
		if c, ok := cs[k]; !ok || c.Count != 1 || c.Stage != StageUnderstand {
			t.Fatalf("缺 %s：%+v", k, cs)
		}
	}
}

func TestDurationScaleCompressedTimeline(t *testing.T) {
	// 原片 20 秒，画面理解以为 10 秒：全部时间 ×2；台词取自转写，不动
	d := &Doc{DurationS: 20, Sections: []Section{{T0: 0, T1: 4}, {T0: 4, T1: 10}}, Others: []Span{{T0: 1, T1: 2}},
		Lines: []Line{{T0: 1, T1: 3, Text: "你好"}}, LinesFromTranscript: true}
	cs := rules(Apply(d, Config{}))
	c, ok := cs["DURATION_SCALE/timeline"]
	if !ok || c.Before != "10.0 秒" || c.After != "20.0 秒" {
		t.Fatalf("应缩放：%+v", cs)
	}
	if d.Sections[1].T1 != 20 || d.Sections[0].T1 != 8 || d.Others[0].T0 != 2 || d.Lines[0].T1 != 3 {
		t.Fatalf("缩放结果：%+v", d)
	}
}

func TestDurationScaleWithinTolerance(t *testing.T) {
	d := &Doc{DurationS: 20, Sections: []Section{{T0: 0, T1: 19.2}}}
	if cs := Apply(d, Config{}); len(cs) != 0 || d.Sections[0].T1 != 19.2 {
		t.Fatalf("误差在容差内不缩放：%+v", cs)
	}
	// 相对误差超了但绝对误差小于下限（原片很短）也不缩放
	d = &Doc{DurationS: 3, Sections: []Section{{T0: 0, T1: 2.4}}}
	if cs := Apply(d, Config{}); len(cs) != 0 {
		t.Fatalf("绝对误差 < 1 秒不缩放：%+v", cs)
	}
	// 只是最后一个镜头的结束写超了（开始都在原片内）：交给夹紧，不缩放
	d = &Doc{DurationS: 12, Sections: []Section{{T0: 0, T1: 4}, {T0: 4, T1: 8}, {T0: 8, T1: 120}}}
	if cs := Apply(d, Config{}); len(cs) != 0 || d.Sections[1].T0 != 4 {
		t.Fatalf("末尾写超不缩放：%+v", cs)
	}
}

func TestDurationScaleStretchedTimeline(t *testing.T) {
	// 原片 10 秒，画面理解按 20 秒写：后两个镜头整段落在原片之外 → 全部 ×0.5
	d := &Doc{DurationS: 10, Sections: []Section{{T0: 0, T1: 8}, {T0: 8, T1: 12}, {T0: 12, T1: 20}}, Lines: []Line{{T0: 14, T1: 16, Text: "猜的"}}}
	cs := rules(Apply(d, Config{}))
	if _, ok := cs["DURATION_SCALE/timeline"]; !ok {
		t.Fatalf("应缩放：%+v", cs)
	}
	if d.Sections[1].T0 != 4 || d.Sections[2].T1 != 10 || d.Lines[0].T0 != 7 {
		t.Fatalf("缩放结果（画面理解的台词也缩放）：%+v", d)
	}
}

func TestShotNoKeyframeMerged(t *testing.T) {
	// 关键帧在 1、3、5、7 秒；镜头 [3.2,3.8] 里一张关键帧都没有 → 并入前一个
	d := &Doc{DurationS: 8, Frames: []float64{1, 3, 5, 7},
		Sections: []Section{{T0: 0, T1: 3.2, Desc: "a"}, {T0: 3.2, T1: 3.8, Desc: "编的"}, {T0: 3.8, T1: 8, Desc: "c"}}}
	cs := rules(Apply(d, Config{}))
	c, ok := cs["SHOT_NO_KEYFRAME/sections"]
	if !ok || c.Count != 1 || c.Stage != StageShots || c.Before != "3 个镜头" || c.After != "2 个镜头" {
		t.Fatalf("应合并：%+v", cs)
	}
	if len(d.Sections) != 2 || d.Sections[0].T1 != 3.8 || d.Sections[0].Desc != "a" {
		t.Fatalf("合并结果：%+v", d.Sections)
	}
}

func TestShotNoKeyframeFirstMergesIntoNext(t *testing.T) {
	d := &Doc{DurationS: 8, Frames: []float64{2, 6}, Sections: []Section{{T0: 0, T1: 1}, {T0: 1, T1: 4}, {T0: 4, T1: 8}}}
	Apply(d, Config{})
	if len(d.Sections) != 2 || d.Sections[0].T0 != 0 || d.Sections[0].T1 != 4 {
		t.Fatalf("第一个镜头并入后一个：%+v", d.Sections)
	}
}

func TestShotOverFrames(t *testing.T) {
	// 关键帧 2 张，镜头 4 个且每个都含关键帧边界（容差内）：只能靠数量规则合并到 2 个
	d := &Doc{DurationS: 8, Frames: []float64{2, 6},
		Sections: []Section{{T0: 0, T1: 2}, {T0: 2, T1: 2.5}, {T0: 2.5, T1: 6}, {T0: 6, T1: 8}}}
	cs := rules(Apply(d, Config{Disabled: []string{RuleShotNoKeyframe}}))
	if c, ok := cs["SHOT_OVER_FRAMES/sections"]; !ok || c.Count != 2 || len(d.Sections) != 2 {
		t.Fatalf("应合并到 2 个：%+v %+v", cs, d.Sections)
	}
	if d.Sections[0].T0 != 0 || d.Sections[len(d.Sections)-1].T1 != 8 {
		t.Fatalf("合并后应仍覆盖全片：%+v", d.Sections)
	}
}

func TestLineDuplicate(t *testing.T) {
	d := &Doc{Lines: []Line{{T0: 0, T1: 1, Text: "欢迎光临"}, {T0: 1.1, T1: 2, Text: "欢迎光临 "}, {T0: 5, T1: 6, Text: "欢迎光临"}}}
	cs := rules(Apply(d, Config{}))
	if c, ok := cs["LINE_DUPLICATE/lines"]; !ok || c.Count != 1 || len(d.Lines) != 2 || d.Lines[0].T1 != 2 {
		t.Fatalf("相接的重复句合并、隔得远的不合并：%+v %+v", cs, d.Lines)
	}
}

func TestSpeechNoLines(t *testing.T) {
	d := &Doc{HasAudio: true, TranscriptRan: true, AudioKind: "speech", LinePresent: true, LineDesc: "口播"}
	cs := rules(Apply(d, Config{}))
	if d.AudioKind != "music" || d.LinePresent || d.LineDesc != "" {
		t.Fatalf("应改为只有音乐、台词置无：%+v", d)
	}
	if cs["SPEECH_NO_LINES/audioKind"].Before != "speech" || cs["SPEECH_NO_LINES/dims.line"].After != "无" {
		t.Fatalf("纠偏记录：%+v", cs)
	}
	// 转写没跑（关闭 / 失败）：不知道有没有人声，不改声音类型
	d = &Doc{HasAudio: true, AudioKind: "mixed", LinePresent: true, LineDesc: "有人说话"}
	if cs := Apply(d, Config{}); len(cs) != 0 || d.AudioKind != "mixed" {
		t.Fatalf("没转写不改：%+v", cs)
	}
}

func TestLinePresentEmpty(t *testing.T) {
	d := &Doc{HasAudio: true, AudioKind: "music", LinePresent: true}
	cs := rules(Apply(d, Config{}))
	if _, ok := cs["LINE_PRESENT_EMPTY/dims.line"]; !ok || d.LinePresent {
		t.Fatalf("台词项置无：%+v", cs)
	}
}

func TestLinesNoSpeechMark(t *testing.T) {
	d := &Doc{HasAudio: true, TranscriptRan: true, LinesFromTranscript: true, AudioKind: "music", Lines: []Line{{T0: 0, T1: 1, Text: "买它"}}}
	cs := rules(Apply(d, Config{}))
	if d.AudioKind != "mixed" || cs["LINES_NO_SPEECH_MARK/audioKind"].Before != "music" {
		t.Fatalf("music → mixed：%+v %+v", d, cs)
	}
	d = &Doc{HasAudio: true, LinesFromTranscript: true, AudioKind: "silent", Lines: []Line{{T0: 0, T1: 1, Text: "买它"}}}
	Apply(d, Config{})
	if d.AudioKind != "speech" {
		t.Fatalf("silent → speech：%+v", d)
	}
	// 台词是画面理解猜的（不是转写）：不改
	d = &Doc{HasAudio: true, AudioKind: "music", Lines: []Line{{T0: 0, T1: 1, Text: "字幕"}}}
	Apply(d, Config{})
	if d.AudioKind != "music" {
		t.Fatalf("非转写台词不改声音类型：%+v", d)
	}
}

func TestDisabledRuleSkipped(t *testing.T) {
	d := &Doc{HasAudio: true, TranscriptRan: true, AudioKind: "speech"}
	if cs := Apply(d, Config{Disabled: []string{"speech_no_lines"}}); len(cs) != 0 || d.AudioKind != "speech" {
		t.Fatalf("关掉的规则不应用（规则码大小写不敏感）：%+v", cs)
	}
}

func TestParseConfigAndVersion(t *testing.T) {
	c, err := ParseConfig("")
	if err != nil || VersionOf(c) != Version {
		t.Fatalf("空配置 = 默认版本：%v %s", err, VersionOf(c))
	}
	if _, err := ParseConfig(`{"disabled":["NOPE"]}`); err == nil {
		t.Fatal("未知规则码应报错")
	}
	if _, err := ParseConfig(`{`); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
	c1, err := ParseConfig(`{"disabled":["LINE_DUPLICATE"],"durationTolerance":0.2}`)
	if err != nil {
		t.Fatal(err)
	}
	c2, _ := ParseConfig(`{"durationTolerance":0.2,"disabled":["line_duplicate","LINE_DUPLICATE"]}`)
	v1, v2 := VersionOf(c1), VersionOf(c2)
	if v1 == Version || !strings.HasPrefix(v1, Version+"+") || v1 != v2 {
		t.Fatalf("非默认配置带指纹，同义配置同版本：%s %s", v1, v2)
	}
	// 越界的阈值回默认 → 与默认同版本
	c3, _ := ParseConfig(`{"durationTolerance":9}`)
	if VersionOf(c3) != Version {
		t.Fatalf("越界值回默认：%s", VersionOf(c3))
	}
}

func TestTableCodesUniqueAndStaged(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Table {
		if seen[r.Code] || r.Apply == nil || r.Desc == "" {
			t.Fatalf("规则表行非法：%+v", r.Code)
		}
		switch r.Stage {
		case StageLines, StageUnderstand, StageShots:
		default:
			t.Fatalf("%s 阶段非法：%s", r.Code, r.Stage)
		}
		seen[r.Code] = true
	}
}

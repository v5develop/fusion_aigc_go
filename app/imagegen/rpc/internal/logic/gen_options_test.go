package logic

import (
	"context"
	"testing"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
)

// setGenRows 直接写入进程内缓存（绕过数据库），loaded=true 表示「已读到库」。
func setGenRows(rows []*model.GenOption) {
	setGenSnapshot(rows, nil)
}

// setGenSnapshot 同时写入选项与类型开关。
func setGenSnapshot(rows []*model.GenOption, kinds []*model.GenKind) {
	genOpts.mu.Lock()
	defer genOpts.mu.Unlock()
	genOpts.snap = &genSnapshot{options: rows, kinds: kinds}
	genOpts.loaded, genOpts.at = true, time.Now()
}

func resetGenRows(t *testing.T) {
	t.Cleanup(invalidateGenOptions)
}

// 没有任何行（库空或读库失败）：全部回落代码默认值，行为与改造前一致。
func TestGenOptionsFallBackToCodeDefaultsWhenTableEmpty(t *testing.T) {
	resetGenRows(t)
	setGenRows(nil)
	ctx := context.Background()
	var c *svc.ServiceContext

	got := optionsOf(ctx, c, genPosterSizes, posterSizes)
	if len(got) != len(posterSizes) || got[0].Code != posterSizes[0].Code {
		t.Fatalf("expected code defaults, got %v", got)
	}
	if d := defaultOf(ctx, c, genPosterModels, "max"); d != "max" {
		t.Fatalf("expected fallback default, got %q", d)
	}
	if d := durationsOf(ctx, c, genVoiceoverDurations, voiceoverDurations); len(d) != len(voiceoverDurations) || d[0] != voiceoverDurations[0] {
		t.Fatalf("expected duration defaults, got %v", d)
	}
	if d := defaultDurationOf(ctx, c, genVoiceoverDurations, voiceoverDurations[0]); d != voiceoverDurations[0] {
		t.Fatalf("expected first duration as default, got %d", d)
	}
}

// 停用项不下发；全部停用时同样回落默认值（保护页面不至于空白）。
func TestGenOptionsFilterDisabledAndFallBackWhenAllDisabled(t *testing.T) {
	resetGenRows(t)
	setGenRows([]*model.GenOption{
		{Group: genPosterSizes, Code: "9:16", Label: "竖版", SortOrder: 10, Enabled: true},
		{Group: genPosterSizes, Code: "16:9", Label: "横版", SortOrder: 20, Enabled: false},
	})
	ctx := context.Background()
	var c *svc.ServiceContext

	got := optionsOf(ctx, c, genPosterSizes, posterSizes)
	if len(got) != 1 || got[0].Code != "9:16" || got[0].Label != "竖版" {
		t.Fatalf("expected only the enabled size with its edited label, got %v", got)
	}

	setGenRows([]*model.GenOption{
		{Group: genPosterSizes, Code: "9:16", Enabled: false},
	})
	if got := optionsOf(ctx, c, genPosterSizes, posterSizes); len(got) != len(posterSizes) {
		t.Fatalf("all disabled must fall back to defaults, got %v", got)
	}
}

// 默认项只认「启用且标记为默认」的行；标记在停用行上时不生效。
func TestDefaultOfRequiresEnabledDefault(t *testing.T) {
	resetGenRows(t)
	setGenRows([]*model.GenOption{
		{Group: genVoiceoverModels, Code: "seedance2.5", Enabled: true, IsDefault: false},
		{Group: genVoiceoverModels, Code: "lite", Enabled: true, IsDefault: true},
	})
	ctx := context.Background()
	var c *svc.ServiceContext
	if d := defaultOf(ctx, c, genVoiceoverModels, "seedance2.5"); d != "lite" {
		t.Fatalf("expected lite as the configured default, got %q", d)
	}

	setGenRows([]*model.GenOption{
		{Group: genVoiceoverModels, Code: "lite", Enabled: false, IsDefault: true},
		{Group: genVoiceoverModels, Code: "seedance2.5", Enabled: true},
	})
	if d := defaultOf(ctx, c, genVoiceoverModels, "seedance2.5"); d != "seedance2.5" {
		t.Fatalf("a disabled default must not apply, got %q", d)
	}
}

// 口播时长：编码是数字串，只认启用项，并据此校验入参。
func TestDurationsFromRowsAndValidation(t *testing.T) {
	resetGenRows(t)
	setGenRows([]*model.GenOption{
		{Group: genVoiceoverDurations, Code: "15", Enabled: true, IsDefault: true},
		{Group: genVoiceoverDurations, Code: "30", Enabled: false},
		{Group: genVoiceoverDurations, Code: "not-a-number", Enabled: true},
	})
	ctx := context.Background()
	var c *svc.ServiceContext
	d := durationsOf(ctx, c, genVoiceoverDurations, voiceoverDurations)
	if len(d) != 1 || d[0] != 15 {
		t.Fatalf("expected [15], got %v", d)
	}
	if !containsDuration(d, 15) || containsDuration(d, 30) {
		t.Fatalf("validation must follow enabled durations")
	}
}

// 默认比例：组内标记的默认项生效；没有标记则回落 YAML（这里的 fallback）。
func TestDefaultSizeOfUsesGroupDefaultThenFallback(t *testing.T) {
	resetGenRows(t)
	ctx := context.Background()
	var c *svc.ServiceContext
	setGenRows([]*model.GenOption{
		{Group: genPosterSizes, Code: "9:16", Enabled: true},
		{Group: genPosterSizes, Code: "2:3", Enabled: true, IsDefault: true},
	})
	if got := defaultSizeOf(ctx, c, model.KindPoster, "1:1"); got != "2:3" {
		t.Fatalf("poster default size: want 2:3, got %q", got)
	}
	// 动作迁移没有比例组，始终用 YAML 值
	if got := defaultSizeOf(ctx, c, model.KindActionTransfer, ""); got != "" {
		t.Fatalf("action transfer has no size group, got %q", got)
	}
	setGenRows(nil)
	if got := defaultSizeOf(ctx, c, model.KindPoster, "2:3"); got != "2:3" {
		t.Fatalf("empty table must fall back to YAML, got %q", got)
	}
}

// 模板必选：有开关行以开关为准；没有行回落 YAML。
func TestTemplateRequiredPrefersKindRow(t *testing.T) {
	resetGenRows(t)
	ctx := context.Background()
	var c *svc.ServiceContext
	setGenSnapshot(nil, []*model.GenKind{
		{KindCode: model.KindPoster, TemplateRequired: false},
	})
	if templateRequired(ctx, c, model.KindPoster, true) {
		t.Fatalf("kind row says not required, must override YAML true")
	}
	if !templateRequired(ctx, c, model.KindLongImage, true) {
		t.Fatalf("no kind row must fall back to YAML true")
	}
}

// 视频时长按组读取；默认项与启用项一致。
func TestVideoDurationsAreGroupScoped(t *testing.T) {
	resetGenRows(t)
	ctx := context.Background()
	var c *svc.ServiceContext
	setGenRows([]*model.GenOption{
		{Group: genVideoDurations, Code: "15", Enabled: true},
		{Group: genVideoDurations, Code: "30", Enabled: true, IsDefault: true},
		{Group: genVoiceoverDurations, Code: "15", Enabled: false},
	})
	d := durationsOf(ctx, c, genVideoDurations, videoDurations)
	if !containsDuration(d, 15) || !containsDuration(d, 30) {
		t.Fatalf("video durations: want 15 and 30, got %v", d)
	}
	if got := defaultDurationOf(ctx, c, genVideoDurations, 15); got != 30 {
		t.Fatalf("video default duration: want 30, got %d", got)
	}
	// 口播组的停用项不影响视频组
	if got := durationsOf(ctx, c, genVoiceoverDurations, voiceoverDurations); len(got) != len(voiceoverDurations) {
		t.Fatalf("all voiceover durations disabled must fall back, got %v", got)
	}
}

// 海报风格：空编码（AI 自动匹配）恒合法；其它编码须在启用项内。
func TestPosterStyleValidation(t *testing.T) {
	resetGenRows(t)
	ctx := context.Background()
	var c *svc.ServiceContext
	setGenRows([]*model.GenOption{
		{Group: genPosterStyles, Code: "", Enabled: false},
		{Group: genPosterStyles, Code: "film", Enabled: true},
	})
	if !validOption(ctx, c, genPosterStyles, posterStyles, "") {
		t.Fatalf("empty style must always be accepted")
	}
	if !validOption(ctx, c, genPosterStyles, posterStyles, "film") {
		t.Fatalf("enabled style must be accepted")
	}
	if validOption(ctx, c, genPosterStyles, posterStyles, "ink") {
		t.Fatalf("style outside the enabled list must be rejected")
	}
}

package modelgroup

import (
	"slices"
	"testing"
)

func TestCanonical(t *testing.T) {
	for in, want := range map[string]string{
		"img.nano-banana-2.1-dev":              NanoBanana21,
		" IMG.Nano-Banana-2.1-Dev ":            NanoBanana21,
		"img.nano-banana-2.1":                  NanoBanana21,
		"img.gpt-image-2.5-sunburst-developer": GPTImage25Sunburst,
		"img.gpt-image-2.5-sunburst":           GPTImage25Sunburst,
		"img.gpt-image-2.6-sunburst":           "img.gpt-image-2.6-sunburst",
		"":                                     "",
	} {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q)=%q want %q", in, got, want)
		}
	}
}

func TestAliases(t *testing.T) {
	for _, in := range []string{"img.gpt-image-2.5-sunburst", "img.gpt-image-2.5-sunburst-developer"} {
		if got := Aliases(in); !slices.Equal(got, []string{GPTImage25Sunburst, "img.gpt-image-2.5-sunburst-developer"}) {
			t.Errorf("Aliases(%q)=%v", in, got)
		}
	}
	for _, in := range []string{"img.nano-banana-2.1", "img.nano-banana-2.1-dev"} {
		if got := Aliases(in); !slices.Equal(got, []string{NanoBanana21, "img.nano-banana-2.1-dev"}) {
			t.Errorf("Aliases(%q)=%v", in, got)
		}
	}
	if got := Aliases("img.seedream-5.5"); !slices.Equal(got, []string{"img.seedream-5.5"}) {
		t.Errorf("Aliases(seedream)=%v", got)
	}
	if got := Aliases(" IMG.Nano-Banana-2.1 "); !slices.Equal(got, []string{NanoBanana21, "img.nano-banana-2.1-dev"}) {
		t.Errorf("Aliases(大小写)=%v", got)
	}
	if Aliases(" ") != nil {
		t.Error("空串应返回 nil")
	}
}

func TestEqualAndTier(t *testing.T) {
	if !Equal("img.nano-banana-2.1-dev", "img.nano-banana-2.1") || !Is("img.gpt-image-2.5-sunburst-developer", GPTImage25Sunburst) {
		t.Error("旧组与规范组应等价")
	}
	if Equal("img.gpt-image-2.6-sunburst", GPTImage25Sunburst) {
		t.Error("2.6 与 2.5 不等价")
	}
	if CanonicalTier("GPT25Dev") != TierGPT25 || CanonicalTier("gpt25") != TierGPT25 || CanonicalTier("banana") != "banana" {
		t.Error("CanonicalTier")
	}
	if got := TierAliases("gpt25dev"); !slices.Equal(got, []string{"gpt25", "gpt25dev"}) {
		t.Errorf("TierAliases=%v", got)
	}
	if got := TierAliases("cloud"); !slices.Equal(got, []string{"cloud"}) {
		t.Errorf("TierAliases(cloud)=%v", got)
	}
}

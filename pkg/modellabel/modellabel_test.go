package modellabel

import "testing"

func TestOf(t *testing.T) {
	cases := map[string]string{
		"GPT25Dev":                             "GPT Image 2.5", // 旧档次（兼容旧草稿）
		"GPT25":                                "GPT Image 2.5",
		"gpt25":                                "GPT Image 2.5",
		"img.nano-banana-2.1":                  "Nano Banana 2.1",
		"img.gpt-image-2.5-sunburst":           "GPT Image 2.5",
		"img.nano-banana-2.1-dev":              "Nano Banana 2.1",
		"img.gpt-image-2.5-sunburst-developer": "GPT Image 2.5",
		"通用":                                   "通用",
		"Nano Banana 2.1 Developer":            "Nano Banana 2.1",
		"Foo Dev":                              "Foo",
		"img.foo-bar-dev":                      "foo bar",
		"GPT Image 2.6 Sunburst":               "GPT Image 2.6",
		"Image":                                "GPT Image 2.5",
		"Banana":                               "Nano Banana 2.1",
		"SD5P":                                 "Seedream 5.0 Pro",
		"SD55":                                 "Seedream 5.5",
		"image-v1":                             "GPT Image 2.5",
		"banana-v1":                            "Nano Banana 2.1",
		"sd5p-v1":                              "Seedream 5.0 Pro",
		"seedream-5-5-v1":                      "Seedream 5.5",
		"gpt-image-2-6-v1":                     "GPT Image 2.6",
		"img.seedream-5-pro":                   "Seedream 5.0 Pro",
		"img.seedream-5.5":                     "Seedream 5.5",
		"Seedream":                             "Seedream 5.5",
		"Seedream 5 Pro":                       "Seedream 5.0 Pro",
		"":                                     "",
	}
	for in, want := range cases {
		if got := Of(in); got != want {
			t.Errorf("Of(%q)=%q want %q", in, got, want)
		}
	}
}

func TestGroupAndRegisteredAcceptLegacy(t *testing.T) {
	for _, g := range []string{"img.gpt-image-2.5-sunburst", "img.gpt-image-2.5-sunburst-developer"} {
		if Group(g) != "GPT Image 2.5" {
			t.Errorf("Group(%q)=%q", g, Group(g))
		}
		if l, ok := Registered(g); !ok || l != "GPT Image 2.5" {
			t.Errorf("Registered(%q)=%q,%v", g, l, ok)
		}
	}
	for _, g := range []string{"img.nano-banana-2.1", "img.nano-banana-2.1-dev"} {
		if l, ok := Registered(g); !ok || l != "Nano Banana 2.1" {
			t.Errorf("Registered(%q)=%q,%v", g, l, ok)
		}
	}
}

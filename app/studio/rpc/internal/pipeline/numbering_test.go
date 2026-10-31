package pipeline

import (
	"reflect"
	"testing"
)

func keys(list []Image) []string {
	out := make([]string, len(list))
	for i, im := range list {
		out[i] = im.Key
	}
	return out
}

func TestNumberGarmentsOrder(t *testing.T) {
	items := []string{"bottom", "top"}
	in := []GarmentInput{
		{URL: "u0", Role: "上衣", Angle: "back"},
		{URL: "u1", Role: "下装", Angle: "detail"},
		{URL: "u2", Role: "上衣", Angle: "FRONT"},
		{URL: "u3", Role: "bottom", Angle: ""},
		{URL: "u4", Role: "下装", Angle: "front"},
		{URL: "u5", Role: "上衣", Angle: "front"},
		{URL: "u6", Role: "下装", Angle: "side"},
		{URL: "u7", Role: "整套", Angle: "front"},
		{URL: "u8", Role: "鞋子", Angle: "front"},
	}
	got, warns := NumberGarments(items, in)
	want := []string{
		"g-bottom-front-1", "g-bottom-side-1", "g-bottom-detail-1", "g-bottom-unknown-1",
		"g-top-front-1", "g-top-front-2", "g-top-back-1",
		"g-look-front-1",
	}
	if !reflect.DeepEqual(keys(got), want) {
		t.Fatalf("keys = %v\nwant  %v", keys(got), want)
	}
	if got[4].URL != "u2" || got[5].URL != "u5" {
		t.Fatalf("same angle must keep submit order: %v %v", got[4].URL, got[5].URL)
	}
	if got[0].Role != "下装" || got[7].Role != "整套" || got[7].Item != ItemLook {
		t.Fatalf("roles = %+v", got)
	}
	if len(warns) != 1 { // 鞋子不在 items
		t.Fatalf("warns = %v", warns)
	}
}

func TestNumberGarmentsUnknownRole(t *testing.T) {
	got, _ := NumberGarments([]string{"dress"}, []GarmentInput{{URL: "a", Role: ""}, {URL: "b", Role: "??", Angle: "back"}})
	if !reflect.DeepEqual(keys(got), []string{"g-dress-back-1", "g-dress-unknown-1"}) {
		t.Fatalf("single item fallback: %v", keys(got))
	}
	got, warns := NumberGarments([]string{"top", "bottom"}, []GarmentInput{{URL: "a", Role: "??"}})
	if got[0].Item != ItemLook || len(warns) != 1 {
		t.Fatalf("multi item unknown → look: %+v %v", got, warns)
	}
}

func TestNumberModels(t *testing.T) {
	got := NumberModels([]string{"a", " b "})
	if !reflect.DeepEqual(keys(got), []string{"m-1", "m-2"}) || got[1].URL != "b" || got[0].Role != RoleModel || got[1].Source != 1 {
		t.Fatalf("models = %+v", got)
	}
}

func TestFilterByItemsAndCategory(t *testing.T) {
	m := map[string]string{"top": " 及腰 ", "dress": "及膝", "bottom": "  "}
	got := FilterByItems(m, []string{"top", "bottom"})
	if !reflect.DeepEqual(got, map[string]string{"top": "及腰"}) {
		t.Fatalf("filter = %v", got)
	}
	if InferCategory([]string{"dress"}) != "dress" || InferCategory([]string{"top", "shoes"}) != "compose" || InferCategory(nil) != "" {
		t.Fatal("category")
	}
}

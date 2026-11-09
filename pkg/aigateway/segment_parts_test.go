package aigateway

import (
	"encoding/json"
	"testing"
)

// image.segment 按 classes 分割时的逐类抠图：parts 解出 label 与 image；没有 parts 时为 nil（旧结果不受影响）。
func TestResultSegmentParts(t *testing.T) {
	var r Result
	raw := `{"operation":"image.segment","image":{"rid":"a","url":"https://x/a"},` +
		`"parts":[{"label":"tops","image":{"rid":"b","url":"https://x/b"}},{"label":"pants","image":{"rid":"c","url":"https://x/c"}}]}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	if r.Image == nil || r.Image.Rid != "a" || len(r.Parts) != 2 || r.Parts[0].Label != "tops" || r.Parts[1].Image.Rid != "c" {
		t.Fatalf("result=%+v", r)
	}
	var old Result
	if err := json.Unmarshal([]byte(`{"operation":"image.segment","image":{"rid":"a","url":"https://x/a"}}`), &old); err != nil || old.Parts != nil {
		t.Fatalf("old parts=%v err=%v", old.Parts, err)
	}
}

package quote

import (
	"reflect"
	"testing"
)

// EXPERIENCE 0.1 #4 的示例：单价 10 点/秒，32 秒视频 3 段（15+15+4 秒）合计 340 点，先做第 1 段 150 点。
func TestSummarizeExample(t *testing.T) {
	s, err := Summarize([]Line{{1, 15, 150}, {2, 15, 150}, {3, 4, 40}})
	if err != nil {
		t.Fatal(err)
	}
	if s.First != 150 || s.Rest != 190 || s.Total != 340 || s.RestSegments != 2 || s.TotalSeconds != 34 {
		t.Fatalf("%+v", s)
	}
	if s.PointsOf(StageFirst)+s.PointsOf(StageRest) != s.PointsOf(StageAll) {
		t.Fatal("先做 + 继续 必须等于合计")
	}
}

func TestSummarizeRejectsGaps(t *testing.T) {
	if _, err := Summarize([]Line{{1, 15, 150}, {3, 4, 40}}); err == nil {
		t.Fatal("段号不连续应报错")
	}
	if _, err := Summarize([]Line{{1, 0, 150}}); err == nil {
		t.Fatal("0 秒应报错")
	}
}

func TestIndexesAndDefault(t *testing.T) {
	if !reflect.DeepEqual(Indexes(StageFirst, 3), []int{1}) || !reflect.DeepEqual(Indexes(StageRest, 3), []int{2, 3}) ||
		!reflect.DeepEqual(Indexes(StageAll, 2), []int{1, 2}) || Indexes(StageRest, 1) != nil {
		t.Fatal("Indexes 不对")
	}
	if DefaultStage(3, false) != StageFirst || DefaultStage(3, true) != StageAll || DefaultStage(1, false) != StageAll {
		t.Fatal("DefaultStage 不对")
	}
}

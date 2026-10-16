package taskcenter

import "testing"

// 提交执行面的 task_id 里带的 Ref，必须和任务中心同步写进去的 task_id 同一个格式，否则按编号查不到。
func TestRefMatchesSyncedTaskID(t *testing.T) {
	s := Source{Prefix: "vid"}
	if got, want := Ref("vid", 28), s.taskID()+"28"; got != want {
		t.Fatalf("Ref=%s，同步写入的是 %s", got, want)
	}
	if Ref("img", 25) != "AiCreation_life_img_25" {
		t.Fatalf("格式变了: %s", Ref("img", 25))
	}
}

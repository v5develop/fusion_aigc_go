package aigc

import "testing"

// 键的顺序固定、空值省略：运维 grep 才能稳定命中。
func TestTraceKeysOrderAndOmission(t *testing.T) {
	full := Trace{
		Service: "note", Kind: "poster", Stage: "generate", RecordID: 12, UserID: 17,
		TaskCenterID: "AiCreation_LF_abc", ExecTaskID: "AiCreationLife_AiCreation_LF_abc_x",
	}
	want := "service=note kind=poster stage=generate record_id=12 user_id=17 task_center_id=AiCreation_LF_abc exec_task_id=AiCreationLife_AiCreation_LF_abc_x"
	if got := full.keys(); got != want {
		t.Fatalf("keys=\n%s\nwant\n%s", got, want)
	}
	bare := Trace{Service: "digitalhuman", Stage: "dubbing", RecordID: 3}
	if got := bare.keys(); got != "service=digitalhuman stage=dubbing record_id=3" {
		t.Fatalf("空值没有省略: %s", got)
	}
}

package billing

import "testing"

func TestBizIDFitsColumn(t *testing.T) {
	short := Charge{UserID: 1, RequestID: "abc"}.bizID()
	if short != "1_abc" {
		t.Fatalf("短请求号不应改动: %s", short)
	}
	long := Charge{UserID: 1, RequestID: string(make([]byte, 200))}.bizID()
	if len(long) > 64 {
		t.Fatalf("超长要折叠到 64 以内: %d", len(long))
	}
	if long != (Charge{UserID: 1, RequestID: string(make([]byte, 200))}.bizID()) {
		t.Fatal("折叠必须稳定")
	}
}

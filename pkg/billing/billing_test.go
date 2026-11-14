package billing

import (
	"database/sql"
	"testing"
)

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

func TestReplayedAndSnapshotModelGroup(t *testing.T) {
	var nilRes *Reservation
	if nilRes.Replayed() {
		t.Fatal("nil 不是命中旧单")
	}
	q := &Quote{EntryCode: "replicate", ModelGroup: "vid.seedance-2.5", VariantCode: "v1", PricePoints: 130}
	r := toReservation(&reservationRow{No: "BR1", PricePoints: 130, PriceSnapsht: sql.NullString{String: q.Snapshot(), Valid: true}})
	if r.Replayed() {
		t.Fatal("toReservation 本身不标命中（由冻结路径标）")
	}
	if r.Quote == nil || r.Quote.EntryCode != "replicate" || r.Quote.ModelGroup != "vid.seedance-2.5" {
		t.Fatalf("快照应还原入口与模型组: %+v", r.Quote)
	}
	rr := ReplayedReservation("BR2", 5, &Quote{EntryCode: "x"})
	if !rr.Replayed() || rr.No != "BR2" || rr.PricePoints != 5 {
		t.Fatalf("替身构造: %+v", rr)
	}
}

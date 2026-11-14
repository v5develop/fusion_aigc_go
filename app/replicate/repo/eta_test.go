package repo

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestEtaSamplesQuery(t *testing.T) {
	r, mock := newMock(t)
	since := time.UnixMilli(1763000000000)
	mock.ExpectQuery("(?s)" + q("FROM afv_replicate_job j") + ".*" +
		q("JOIN afv_replicate_step s ON s.job_id = j.id AND s.kind = 'gateway' AND s.step_key NOT LIKE '%#u%' AND s.started_at IS NOT NULL") + ".*" +
		q("WHERE j.status = 2 AND j.finished_at >= FROM_UNIXTIME(1763000000000 / 1000)") + ".*" +
		q("GROUP BY j.id, s.seg_index") + ".*" + q("LIMIT 100") + ".*" + q("WHERE t.secs > 0")).
		WillReturnRows(sqlmock.NewRows([]string{"model_group", "seg_total", "gen_seconds", "secs"}).
			AddRow("vid.seedance-2.5", 3, 12, "185.250000").
			AddRow("vid.wan-3.0", 1, 8, "90"))
	got, err := r.EtaSamples(ctx, since, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ModelGroup != "vid.seedance-2.5" || got[0].SegTotal != 3 || got[0].GenSeconds != 12 || got[0].Seconds != 185.25 ||
		got[1].Seconds != 90 {
		t.Fatalf("样本：%+v", got)
	}
}

func TestEtaSamplesDefaultLimit(t *testing.T) {
	r, mock := newMock(t)
	mock.ExpectQuery(q("LIMIT 5000")).WillReturnRows(sqlmock.NewRows([]string{"model_group", "seg_total", "gen_seconds", "secs"}))
	got, err := r.EtaSamples(ctx, time.UnixMilli(1763000000000), 0)
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %+v", err, got)
	}
}

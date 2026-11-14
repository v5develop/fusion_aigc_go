package logic

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	zprom "github.com/zeromicro/go-zero/core/prometheus"
)

// go-zero 的 metric 只在 prometheus 已启用时才记数（生产由 Prometheus 配置 / DevServer 打开），测试里显式打开。
func init() { zprom.Enable() }

// metricValue 读 go-zero 注册到默认 registry 的指标当前值（按 name + 标签匹配，counter 取值 / histogram 取样本数）。
func metricValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m, labels) {
				if m.GetCounter() != nil {
					return m.GetCounter().GetValue()
				}
				if m.GetHistogram() != nil {
					return float64(m.GetHistogram().GetSampleCount())
				}
			}
		}
	}
	return 0
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	for k, v := range want {
		ok := false
		for _, l := range m.GetLabel() {
			if l.GetName() == k && l.GetValue() == v {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// 小快照：计次数与大小，不触发 LARGE。
func TestObserveRevisionWriteSmall(t *testing.T) {
	ctx := context.Background()
	rawBefore := metricValue(t, "canvas_revision_raw_bytes", map[string]string{"source": "t_small"})
	gzBefore := metricValue(t, "canvas_revision_gzip_bytes", map[string]string{"source": "t_small"})
	largeBefore := metricValue(t, "canvas_revision_large_total", map[string]string{"source": "t_small"})
	observeRevisionWrite(ctx, "cv_1", "t_small", 4000, 1000)
	if d := metricValue(t, "canvas_revision_raw_bytes", map[string]string{"source": "t_small"}) - rawBefore; d != 1 {
		t.Fatalf("raw 样本数 +%v", d)
	}
	if d := metricValue(t, "canvas_revision_gzip_bytes", map[string]string{"source": "t_small"}) - gzBefore; d != 1 {
		t.Fatalf("gzip 样本数 +%v", d)
	}
	if d := metricValue(t, "canvas_revision_large_total", map[string]string{"source": "t_small"}) - largeBefore; d != 0 {
		t.Fatalf("小快照不应计 large: +%v", d)
	}
}

// 压缩后恰好等于阈值不算大，超过阈值计 large（WARN 日志同路径）。
func TestObserveRevisionWriteLargeThreshold(t *testing.T) {
	ctx := context.Background()
	l := map[string]string{"source": "t_large"}
	before := metricValue(t, "canvas_revision_large_total", l)
	observeRevisionWrite(ctx, "cv_2", "t_large", 2<<20, snapshotLargeGzipBytes)
	if d := metricValue(t, "canvas_revision_large_total", l) - before; d != 0 {
		t.Fatalf("等于阈值不算大: +%v", d)
	}
	observeRevisionWrite(ctx, "cv_2", "t_large", 2<<20, snapshotLargeGzipBytes+1)
	if d := metricValue(t, "canvas_revision_large_total", l) - before; d != 1 {
		t.Fatalf("超过阈值应计 large: +%v", d)
	}
}

// 压缩失败（gzipLen<0）：只记 raw，不记 gzip、不判大。
func TestObserveRevisionWriteGzipFailed(t *testing.T) {
	l := map[string]string{"source": "t_gzfail"}
	gzBefore := metricValue(t, "canvas_revision_gzip_bytes", l)
	observeRevisionWrite(context.Background(), "cv_3", "t_gzfail", 100, -1)
	if metricValue(t, "canvas_revision_raw_bytes", l) != 1 || metricValue(t, "canvas_revision_gzip_bytes", l) != gzBefore {
		t.Fatal("压缩失败只应记 raw")
	}
}

// 写入结果与裁剪行数计数；n=0 不增，失败单独计。
func TestCountRevisionWriteAndPruned(t *testing.T) {
	w := map[string]string{"result": "ok"}
	b := metricValue(t, "canvas_revision_write_total", w)
	countRevisionWrite("ok")
	if metricValue(t, "canvas_revision_write_total", w)-b != 1 {
		t.Fatal("write_total ok 应 +1")
	}
	d := map[string]string{"result": "deleted"}
	pb := metricValue(t, "canvas_revision_pruned_rows_total", d)
	countRevisionPruned(3, true)
	countRevisionPruned(0, true)
	if metricValue(t, "canvas_revision_pruned_rows_total", d)-pb != 3 {
		t.Fatal("pruned deleted 应 +3")
	}
	f := map[string]string{"result": "failed"}
	fb := metricValue(t, "canvas_revision_pruned_rows_total", f)
	countRevisionPruned(0, false)
	if metricValue(t, "canvas_revision_pruned_rows_total", f)-fb != 1 {
		t.Fatal("pruned failed 应 +1")
	}
}

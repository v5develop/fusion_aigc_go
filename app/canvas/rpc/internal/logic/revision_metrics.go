package logic

// 画布快照写入量观测（写放大判断用）：每次保存写一行 afv_canvas_revision（gzip），按画布裁剪到 RevisionKeep 个。
// 只观测，不改写入语义与保留策略；指标 / 日志的任何失败都吞掉，不得影响保存。
// 指标不带画布 / 用户标签（避免高基数）；按画布 / 按天的明细用 tools/ops/canvas-revision-stats 查库。

import (
	"context"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/metric"
)

// 快照压缩后超过该字节数打 WARN（CANVAS_SNAPSHOT_LARGE）。
const (
	snapshotLargeGzipBytes = 256 << 10
	snapshotLargeCheck     = "CANVAS_SNAPSHOT_LARGE"
)

var snapshotSizeBuckets = []float64{1 << 10, 4 << 10, 16 << 10, 64 << 10, 256 << 10, 1 << 20, 2 << 20}

var (
	// canvas_revision_write_total{result}：result=ok 落库成功 / insert_failed 落库失败 / gzip_failed 压缩失败。
	revisionWrites = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "canvas",
		Subsystem: "revision",
		Name:      "write_total",
		Help:      "画布快照历史写入次数：ok 成功 / insert_failed 落库失败 / gzip_failed 压缩失败",
		Labels:    []string{"result"},
	})
	// canvas_revision_raw_bytes：压缩前（存储态快照）字节数。
	revisionRawBytes = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "canvas",
		Subsystem: "revision",
		Name:      "raw_bytes",
		Help:      "画布快照压缩前字节数",
		Labels:    []string{"source"},
		Buckets:   snapshotSizeBuckets,
	})
	// canvas_revision_gzip_bytes：压缩后（实际入库）字节数。
	revisionGzipBytes = metric.NewHistogramVec(&metric.HistogramVecOpts{
		Namespace: "canvas",
		Subsystem: "revision",
		Name:      "gzip_bytes",
		Help:      "画布快照压缩后字节数（入库大小）",
		Labels:    []string{"source"},
		Buckets:   snapshotSizeBuckets,
	})
	// canvas_revision_pruned_rows_total：被裁剪删除的历史行数。
	revisionPruned = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "canvas",
		Subsystem: "revision",
		Name:      "pruned_rows_total",
		Help:      "画布快照历史被裁剪删除的行数",
		Labels:    []string{"result"},
	})
	// canvas_revision_large_total：压缩后超过 snapshotLargeGzipBytes 的快照次数。
	revisionLarge = metric.NewCounterVec(&metric.CounterVecOpts{
		Namespace: "canvas",
		Subsystem: "revision",
		Name:      "large_total",
		Help:      "压缩后超过 256KB 的画布快照写入次数",
		Labels:    []string{"source"},
	})
)

// observeRevisionWrite 记一次快照写入的大小（gzipLen<0 表示压缩失败，只计 raw）。source 取值有限（user / agent / run 等），低基数。
func observeRevisionWrite(ctx context.Context, canvasID, source string, rawLen, gzipLen int) {
	defer func() { _ = recover() }() // 观测永不影响保存
	revisionRawBytes.Observe(int64(rawLen), source)
	if gzipLen < 0 {
		return
	}
	revisionGzipBytes.Observe(int64(gzipLen), source)
	if gzipLen > snapshotLargeGzipBytes {
		revisionLarge.Inc(source)
		// logx 无 warn 级别：按 error 级别输出、消息以 WARN 开头、带 check 键（仓库惯例）
		logx.WithContext(ctx).Errorf("WARN canvas: 画布快照偏大（check=%s）canvas=%s gzip_bytes=%d raw_bytes=%d threshold=%d source=%s",
			snapshotLargeCheck, canvasID, gzipLen, rawLen, snapshotLargeGzipBytes, source)
	}
}

func countRevisionWrite(result string) {
	defer func() { _ = recover() }()
	revisionWrites.Inc(result)
}

func countRevisionPruned(n int64, ok bool) {
	defer func() { _ = recover() }()
	if !ok {
		revisionPruned.Inc("failed")
		return
	}
	if n > 0 {
		revisionPruned.Add(float64(n), "deleted")
	}
}

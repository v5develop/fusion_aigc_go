package repo

import (
	"context"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
)

var _ core.EtaSampleSource = (*Repo)(nil)

// etaSamplesSQL ETA 历史样本（只读，查询现算，不落表）：since 之后完成的成功 job（status=2，走 idx_status_finished），
// 每段取「非用户重做（step_key 不含 #u）的出片步骤」里 第一次开始 → 成功出片 的耗时（含自动重试 #rN）；
// 原链没成功（靠用户重做才成功）的段 secs 为 NULL，被外层过滤。生成秒数取该段非重做预留镜像的 seconds（first/all/rest）。
// GROUP BY j.id（主键）后 j.input / j.seg_total 函数依赖于它，ONLY_FULL_GROUP_BY 下合法。
const etaSamplesSQL = `SELECT t.model_group, t.seg_total, t.gen_seconds, t.secs FROM (
 SELECT j.id AS job_id, s.seg_index,
  COALESCE(JSON_UNQUOTE(JSON_EXTRACT(j.input, '$.modelGroup')), '') AS model_group,
  j.seg_total AS seg_total,
  COALESCE((SELECT MAX(c.seconds) FROM afv_replicate_charge c WHERE c.job_id = j.id AND c.seg_index = s.seg_index AND c.stage <> 'redo'), 0) AS gen_seconds,
  TIMESTAMPDIFF(MICROSECOND, MIN(s.started_at), MAX(CASE WHEN s.status = 'SUCCEEDED' THEN s.finished_at END)) / 1000000 AS secs
 FROM afv_replicate_job j
 JOIN afv_replicate_step s ON s.job_id = j.id AND s.kind = 'gateway' AND s.step_key NOT LIKE '%#u%' AND s.started_at IS NOT NULL
 WHERE j.status = ? AND j.finished_at >= ?
 GROUP BY j.id, s.seg_index
 ORDER BY j.id DESC
 LIMIT ?
) t WHERE t.secs > 0`

// EtaSamples 实现 core.EtaSampleSource。
func (r *Repo) EtaSamples(ctx context.Context, since time.Time, limit int) ([]core.EtaSample, error) {
	if limit <= 0 {
		limit = 5000
	}
	var rows []struct {
		ModelGroup string  `bun:"model_group"`
		SegTotal   int     `bun:"seg_total"`
		GenSeconds int     `bun:"gen_seconds"`
		Secs       float64 `bun:"secs"`
	}
	// 时间交给库换算（msExpr），与其它写路径同口径，避免 bun 按 UTC 内联 time.Time
	if err := r.db.NewRaw(etaSamplesSQL, model.JobSucceeded, msExpr(since), limit).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]core.EtaSample, 0, len(rows))
	for _, x := range rows {
		out = append(out, core.EtaSample{ModelGroup: x.ModelGroup, SegTotal: x.SegTotal, GenSeconds: x.GenSeconds, Seconds: x.Secs})
	}
	return out, nil
}

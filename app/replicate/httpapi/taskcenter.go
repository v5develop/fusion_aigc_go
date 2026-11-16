package httpapi

import (
	"aigc-local-life/app/replicate/core"
	"aigc-local-life/pkg/taskcenter"
)

// 任务中心（P0-3）：视频做同款 job 作为 pkg/taskcenter 的一个新同步源（afv_replicate_job → afv_creation_task）。
//
// 为什么选「新同步源」而不是「经所属包直接写」：pkg/taskcenter 只提供 Source + Syncer 这一种写法（没有逐行直写的 API），
// SYNC_SPEC 要求任务中心行的全部列（origin_service=LOCAL_LIFE、origin_ref、deleted 只增不减、update_time 跟源表）
// 都由同一条 upsert 子句派生，legacy / new / shadow 三种同步模式与对拍测试也都建立在 Source 上；另写一条直写路径
// 等于在共享表上开第二个写口，违背「共享表只经所属包写」。job 表是 Go 自有表，状态 / 进度 / 点数都能用 SQL 表达式算出。
//
// 映射：
//   - task_id = taskcenter.Ref("rpl", job.id)（确认时已写进 job.task_id，两边同一个值）；origin_ref = rpl:<job id>；type=video；
//     entry_code = video_replicate（同计费 offer）。
//   - status：成功 2 / 失败 3 / 已取消 4 / 部分成功 5，其余（排队 / 执行中 / 提交中 / 待继续 / 继续中）→ 1 处理中。
//   - result_url / cover_url：已入素材库（afv_replicate_delivery，公有桶）的产物地址，优先级 完整成片 > 预览版 > 第 1 个已入库的段；
//     都没入库前为空（私有桶 rid 不进共享表）；封面缺省回退成品地址。
//   - request_params：进度与跳转标识——progress（已完成段 / 总段，0~100）、segDone / segTotal / segFailed、
//     points（已扣除 + 预留中 + 扣费确认中，已退回不算，与历史列表同口径）、jobId、planNo、link（/local-life/video-replicate?job=<id>）、title。
//   - 计费字段（billing_status / price_points）不同步：一个 job 有多张预留单（每段一张），Source.BillingRequest 只能关联一张，
//     关联第 1 段会把单段价格当成整单价格显示，所以写 NONE，点数走 request_params.points。
//   - 同步水位 tc_updated_at（Go_V1.0.39）：job 行任何变化自动刷新；预留镜像 / 入库变化时显式刷新。
func TaskSource() taskcenter.Source {
	// 已入库的产物里挑一个：完整成片（final）> 预览版（preview）> 第 1 个已入库的段（段号最小），部分成功的任务也有可点开的成品
	// 封面取同一行（没有封面回退该行成品地址），不会出现「成片地址 + 某一段的封面」
	pick := func(expr string) string {
		return "(SELECT " + expr + " FROM afv_replicate_delivery d WHERE d.job_id = s.id AND d.status = 1 AND d.video_url IS NOT NULL" +
			" ORDER BY FIELD(d.kind, 'final', 'preview', 'segment'), d.seg_index LIMIT 1)"
	}
	video, cover := pick("d.video_url"), pick("COALESCE(d.cover_url, d.video_url)")
	return taskcenter.Source{
		Table: "afv_replicate_job", Prefix: "rpl", EntryCode: core.EntryCode, Type: "video",
		Include: "task_id IS NOT NULL",
		Status:  "CASE status WHEN 2 THEN 2 WHEN 3 THEN 3 WHEN 4 THEN 4 WHEN 5 THEN 5 ELSE 1 END",
		Result:  video,
		Cover:   cover,
		Fail:    "error_reason",
		RequestPairs: "'title', COALESCE(JSON_UNQUOTE(JSON_EXTRACT(s.input, '$.title')), '视频做同款'), " +
			"'jobId', s.id, 'planNo', s.plan_no, 'link', CONCAT('/local-life/video-replicate?job=', s.id), " +
			"'segTotal', s.seg_total, 'segDone', s.seg_done, 'segFailed', s.seg_failed, " +
			"'progress', IF(s.seg_total > 0, FLOOR(100 * s.seg_done / s.seg_total), 0), " +
			"'points', (SELECT CAST(COALESCE(SUM(c.points), 0) AS SIGNED) FROM afv_replicate_charge c WHERE c.job_id = s.id " +
			"AND (c.status IN (1, 2) OR (c.status = 6 AND c.reason LIKE 'confirm:%')))",
		CreateTimeCol: "created_at", UpdateTimeCol: "tc_updated_at",
	}
}

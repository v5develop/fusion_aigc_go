package logic

// run 事件推送（编辑器从 1.2 秒轮询改为服务端推送优先）：publishRun 在 run 每次状态 / 进度 / 结果变化后，经画布事件频道
// 发一条 source=run 事件，载荷带与 GetRun 同形的执行结果（canvasevent.RunView）。
//   - 内容口径：发布前按 id 重读库，再走 GetRun 同一个 toResult（候选 / 视频 / 缩略图同样现签，有效期同 GetRun）；
//     重读失败只发不带结果的事件（前端收到后立即 GET 一次）。
//   - 推给谁：频道按画布划分，网关 SSE 订阅前已按会话用户校验画布归属、心跳时复核登录态与归属，事件不会到别的用户手里。
//   - 尽力而为：失败只记日志，不影响业务；前端另有低频轮询兜底（SSE 断开时回到 1.2 秒轮询）。

import (
	"context"

	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"

	"github.com/zeromicro/go-zero/core/logx"
)

// runView run → 事件载荷（toResult 同一口径 + 运行中的本地外推参数）。
func (c *core) runView(ctx context.Context, r *model.NodeRun) *canvasevent.RunView {
	v := runViewOf(c.toResult(ctx, r))
	v.Version = r.UpdatedAt.UnixMilli()
	if r.Status == model.RunRunning {
		start := r.CreatedAt
		if r.StartedAt.Valid {
			start = r.StartedAt.Time
		}
		exp, _ := c.budget(r.Kind)
		v.ElapsedMs = max(c.now().Sub(start).Milliseconds(), 1)
		v.ExpectedMs = exp.Milliseconds()
	}
	return v
}

// runViewOf RunResult → RunView（字段一一对应网关 toRunResult）。
func runViewOf(r *canvas.RunResult) *canvasevent.RunView {
	v := &canvasevent.RunView{
		RequestID: r.RequestId, RunID: r.RunId, CanvasID: r.CanvasId, NodeID: r.NodeId, Kind: r.Kind, Status: r.Status,
		Progress: r.Progress, Model: r.Model, SelectedIndex: r.SelectedIndex, VideoURL: r.VideoUrl, Text: r.Text,
		Credits: r.Credits, RequestedCount: r.RequestedCount, Error: r.Error, ErrorCode: r.ErrorCode, Retryable: r.Retryable,
		CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt, LibraryStatus: r.LibraryStatus, CancelState: r.CancelState,
		Candidates: make([]canvasevent.RunCandidate, 0, len(r.Candidates)),
	}
	for _, x := range r.Candidates {
		v.Candidates = append(v.Candidates, canvasevent.RunCandidate{URL: x.Url, Width: x.Width, Height: x.Height, ThumbURL: x.ThumbUrl})
	}
	return v
}

// publishRunID 只有 run 主键时发 run 事件（转存完成 / 永久失败 / 用户重试转存等后台路径）。
func (c *core) publishRunID(ctx context.Context, id int64) {
	if id <= 0 || !c.s.CanvasEvents.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), afterWriteTimeout)
	defer cancel()
	r, err := c.s.Repo.Run.Get(ctx, id)
	if err != nil || r == nil {
		if err != nil {
			logx.WithContext(ctx).Errorf("canvas: run %d 发事件前读库失败（check=canvas.run_event_reload）: %v", id, err)
		}
		return
	}
	c.emitRun(ctx, r, r)
}

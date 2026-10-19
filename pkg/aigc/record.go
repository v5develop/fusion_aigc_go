package aigc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/zeromicro/go-zero/core/logx"
)

// Submission 一次提交执行面的快照：发出去的入参、路由后的类型与受理结果。
// 任务中心的执行参数只读这份快照，不再由源表字段拼凑，所以记录的永远是「实际发出去的」。
type Submission struct {
	TaskID     string // 执行面任务号
	TaskType   int    // 提交的类型，如动作迁移 9
	RoutedType int    // 执行面路由后的类型，如 9 → 86；未受理为 0
	Input      string // 实际提交的入参 JSON（不含 task_id / task_type）
	Error      string // 未受理的原因；空=已受理
}

// Recorder 落库提交快照。由服务在装配时注入（实现在 pkg/taskcenter），失败只打日志，不影响提交结果。
type Recorder interface {
	RecordSubmission(ctx context.Context, s Submission) error
}

// WithRecorder 每次 Submit 都记一份快照，包括被拒绝、未配置的提交：各服务共用同一条规则，不再各写各的。
func (c *Client) WithRecorder(r Recorder) *Client {
	c.rec = r
	return c
}

// Submit 提交任务，并记录快照与 event=submitted 日志。taskID 建议用 NewTaskID 生成并先落库；传空则以执行面返回的为准。
func (c *Client) Submit(ctx context.Context, taskID string, t Task) (*SubmitResult, error) {
	res, err := c.submit(ctx, taskID, t)
	c.record(ctx, taskID, t, res, err)
	return res, err
}

func (c *Client) record(ctx context.Context, taskID string, t Task, res *SubmitResult, err error) {
	s := Submission{TaskID: taskID, TaskType: t.TaskType()}
	if s.TaskID == "" && res != nil {
		s.TaskID = res.TaskID
	}
	if b, merr := json.Marshal(t); merr == nil {
		s.Input = string(b)
	}
	if err != nil {
		s.Error = err.Error()
	} else {
		s.RoutedType = res.TaskType
		logx.WithContext(ctx).Infof("event=submitted exec_task_id=%s task_type=%d routed_type=%d input=%s", s.TaskID, s.TaskType, s.RoutedType, s.Input)
	}
	if c.rec == nil {
		return
	}
	// 调用方的 ctx 可能已经超时或取消，快照仍要落下来，用独立的短超时
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if rerr := c.rec.RecordSubmission(rctx, s); rerr != nil {
		logx.WithContext(ctx).Errorf("event=submission_record_failed exec_task_id=%s cause=%q", s.TaskID, rerr.Error())
	}
}

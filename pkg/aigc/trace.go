package aigc

import (
	"context"
	"fmt"
	"strings"

	"github.com/zeromicro/go-zero/core/logx"
)

// Trace 一条任务的排障坐标。日志里统一打这些键：运维按记录号、任务中心编号、执行面任务号中的任意一个，
// 都能 grep 出整条链路；不用再去数据库里反查是哪一条记录。
type Trace struct {
	Service      string // note / digitalhuman / imagegen
	Kind         string // 类型编码（poster、exploreShop …），没有则省略
	Stage        string // plan / parse / generate / image / portrait / dubbing / rendering
	RecordID     int64  // 源表记录号
	UserID       int64  // 所属用户，0 则省略
	TaskCenterID string // 任务中心编号 AiCreation_LF_…，空则省略
	ExecTaskID   string // 执行面任务号，空则省略
}

// keys 固定顺序的键值串，便于 grep 与日志对齐。
func (t Trace) keys() string {
	parts := []string{"service=" + t.Service}
	if t.Kind != "" {
		parts = append(parts, "kind="+t.Kind)
	}
	parts = append(parts, "stage="+t.Stage, fmt.Sprintf("record_id=%d", t.RecordID))
	if t.UserID != 0 {
		parts = append(parts, fmt.Sprintf("user_id=%d", t.UserID))
	}
	if t.TaskCenterID != "" {
		parts = append(parts, "task_center_id="+t.TaskCenterID)
	}
	if t.ExecTaskID != "" {
		parts = append(parts, "exec_task_id="+t.ExecTaskID)
	}
	return strings.Join(parts, " ")
}

// LogOutcome 执行面终态：失败打 Error（event=exec_failed，带错误码、上游、消息、截断的 traceback），其它打 Info。
func LogOutcome(ctx context.Context, t Trace, r *Result) {
	if r.Status == StatusFailed {
		logx.WithContext(ctx).Errorf("event=exec_failed %s %s", t.keys(), r.Describe())
		return
	}
	logx.WithContext(ctx).Infof("event=exec_outcome %s status=%s", t.keys(), r.Status)
}

// LogSubmitFailed 提交执行面就失败（网络、参数、限流等）：event=submit_failed，带原始错误。
func LogSubmitFailed(ctx context.Context, t Trace, err error) {
	logx.WithContext(ctx).Errorf("event=submit_failed %s cause=%q", t.keys(), err.Error())
}

// LogTaskFailed 业务侧宣告失败：event=task_failed。user_reason 就是用户看到的那句话，运维以此对照；
// cause 是能拿到的技术原因（没有则为空），与前面的 exec_failed / submit_failed 日志通过坐标串起来。
func LogTaskFailed(ctx context.Context, t Trace, userReason string, cause error) {
	c := ""
	if cause != nil {
		c = cause.Error()
	}
	logx.WithContext(ctx).Errorf("event=task_failed %s user_reason=%q cause=%q", t.keys(), userReason, c)
}

// LogStepFailed 流水线中某一步出错（下载、转存、回写等），事件 step_failed，带原始错误与坐标。
// 这一步的失败会紧接着由业务侧宣告为 task_failed；本行只保留技术原因。
func LogStepFailed(ctx context.Context, t Trace, step string, err error) {
	logx.WithContext(ctx).Errorf("event=step_failed %s step=%s cause=%q", t.keys(), step, err.Error())
}

// LogLookupFailed 失败日志补充用户号 / 类型时查库失败（记录已删除、查库超时等）：
// 之后的 task_failed 里对应的键会留空，这条日志说明留空的原因，免得运维误以为记录本来就没有用户。
func LogLookupFailed(ctx context.Context, t Trace, err error) {
	logx.WithContext(ctx).Infof("event=owner_lookup_failed %s cause=%q", t.keys(), err.Error())
}

package logic

// Java 兼容工具的原始请求落库与任务号找回。
//
// 为什么要落库：Java 兼容工具按 (user_id, Idempotency-Key) 幂等（afv_creation_task.uk_creation_user_submit_idem，
// 跟任务行同寿命、没有过期时间）。同键再提交时 Java 先把请求重新受理一遍（校验、读原图算计费事实、构建工作流输入），
// 再按「typed params（含输入地址）+ 能力 + 计费模型与口径 + 工作流名/版本 + 计费事实」的摘要比对：
// 摘要相同 → 返回原 taskId（不新建、不扣费）；不同 → code=1003 PARAM_INVALID「同一幂等键已用于内容不同的请求」。
// 比的是解析后的语义摘要而不是原始字节，但画布每次重算的请求体（图片地址、蒙版框）可能变，所以首次发送前把
// 实际要发的「路径 + 原始 body 字节 + 幂等键」存进 run.backend_request，此后任何重提都逐字节重发它。
//
// 找回：提交遇暂时性错误（超时 / 5xx）时 backend_ref 没写上，但 Java 可能已建任务、已冻结点数。run 在排队中被取消、
// 强制失败、排队超时、所在画布被删时，用同一个幂等键重放落库的请求拿回 taskUid，写回 backend_ref 并转运行中，
// 再走 stopJava 的取消流程（Cancel / 已完成交付 / 退化等口径不变）。
//
// 副作用：若 Java 其实没收到首次请求，重放会真的新建一个任务（并冻结点数），随后立即取消，由 Java 退款。
// 没有 backend_request（连请求都没构造成就失败了）的 run 不重放，直接按原样收尾。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// javaRequest afv_canvas_node_run.backend_request：Body 是发出的原始字节（JSON 字符串存放，往返不变）。
type javaRequest struct {
	Path string `json:"path"`
	Idem string `json:"idem"`
	Body string `json:"body"`
}

func parseJavaRequest(run *model.NodeRun) (javaRequest, bool) {
	var jr javaRequest
	if run == nil || len(run.BackendRequest) == 0 || json.Unmarshal(run.BackendRequest, &jr) != nil {
		return javaRequest{}, false
	}
	if jr.Path == "" || jr.Idem == "" || jr.Body == "" {
		return javaRequest{}, false
	}
	return jr, true
}

// persistJavaRequest 首次发送前落库（仅当 run 仍在原状态）；写不进去就不发。
func (c *core) persistJavaRequest(ctx context.Context, run *model.NodeRun, jr javaRequest) error {
	b, err := json.Marshal(jr)
	if err != nil {
		return err
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	ok, err := c.s.Repo.Run.UpdateIfStatus(bctx, run.ID, run.Status, map[string]any{"backend_request": string(b)})
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 落库 Java 原始请求失败（不发送，稍后重试）: %v", run.ID, err)
		return temporary(err)
	}
	if !ok {
		return xerr.New(xerr.Conflict, "执行已被取消")
	}
	run.BackendRequest = b
	return nil
}

// javaLive 是否有（或可能有）在跑的 Java 任务：运行中且有 taskUid，或排队中但已发过请求（可找回）。
func javaLive(run *model.NodeRun) bool {
	if run.Backend != model.BackendJava {
		return false
	}
	switch run.Status {
	case model.RunRunning:
		return strings.TrimSpace(run.BackendRef.String) != ""
	case model.RunQueued:
		_, ok := parseJavaRequest(run)
		return ok
	}
	return false
}

// recoverResult 找回任务号的结果。
type recoverResult int

const (
	recoverTransient     recoverResult = iota // javaRetryable（网络 / 5xx / 429 / 1004 / 3010 / 5005 / 46011）：计入重试上限
	recoverOK                                 // 已写回 backend_ref 并转运行中
	recoverLost                               // CAS 落空：run 已被别处改变（如提交方同时转了运行中）
	recoverUnauthorized                       // 登录态无效
	recoverNoTask                             // 3003 点数不足：同键在 Java 没有任务（有则先按摘要重放，到不了扣费）
	recoverUnrecoverable                      // 1003 同键内容不同 / 其它业务拒绝：无法找回
)

// Java 明确表示「稍后重试」的业务码（fusion-aigc common/ErrorCode）。
const (
	javaCodeTooFrequent = 1004  // REQUEST_TOO_FREQUENT
	javaCodeProcessing  = 3010  // TASK_PROCESSING：受理成功、仍在处理中
	javaCodeSubmitRetry = 5005  // SUBMIT_RETRY：提交期间配置已变更（HTTP 500）
	javaCodeInProgress  = 46011 // REQUEST_IN_PROGRESS：同键请求处理中
)

// javaRetryable Java 错误是否暂时性（提交 javaErr 与找回 recoverJavaRef 共用一个口径）：网络 / HTTP 429 / HTTP ≥500
// （Java 把 5xxx 业务码映成 HTTP 500，包括 5005 SUBMIT_RETRY），以及 Java 明确「请重试」的业务码。登录失效不算。
func javaRetryable(je *javatool.Error) bool {
	if je == nil || je.Unauthorized() {
		return false
	}
	if je.Temporary() {
		return true
	}
	switch je.Code {
	case javaCodeTooFrequent, javaCodeProcessing, javaCodeSubmitRetry, javaCodeInProgress:
		return true
	}
	return je.Code >= 5000 && je.Code < 6000
}

// recoverJavaRef 排队中、已发过请求的 run：同键逐字节重放拿回 taskUid → CAS 排队→运行中写回 backend_ref。
func (c *core) recoverJavaRef(ctx context.Context, run *model.NodeRun, bearer string) (recoverResult, error) {
	jr, ok := parseJavaRequest(run)
	if !ok {
		return recoverUnrecoverable, errors.New("canvas: 没有可重放的 Java 请求")
	}
	bctx, cancel := detached(ctx)
	defer cancel()
	sub, err := c.s.Java.SubmitRaw(bctx, bearer, jr.Path, []byte(jr.Body), jr.Idem)
	if err != nil {
		var je *javatool.Error
		switch {
		case errors.Is(err, javatool.ErrNoBearer), errors.As(err, &je) && je.Unauthorized():
			return recoverUnauthorized, err
		case errors.As(err, &je) && je.Code == xerr.InsufficientPoints:
			return recoverNoTask, err
		case errors.As(err, &je) && javaRetryable(je):
			return recoverTransient, err
		case errors.As(err, &je):
			return recoverUnrecoverable, err
		}
		return recoverTransient, err
	}
	logx.WithContext(ctx).Infof("canvas: run %d 按幂等键 %s 重放找回 Java 任务 %s", run.ID, jr.Idem, sub.TaskID)
	ok, err = c.s.Repo.Run.Transit(bctx, run.ID, model.RunQueued, model.RunRunning, map[string]any{
		"backend_ref": sub.TaskID, "attempt": run.Attempt + 1, "progress": 5,
	})
	if err != nil {
		return recoverTransient, err
	}
	if !ok {
		return recoverLost, nil
	}
	run.Status, run.Attempt = model.RunRunning, run.Attempt+1
	run.BackendRef.String, run.BackendRef.Valid = sub.TaskID, true
	return recoverOK, nil
}

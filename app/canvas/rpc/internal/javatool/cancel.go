package javatool

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// 取消创作任务：POST /api/v1/creation/{taskId}/cancel（用户 Bearer）。
//
// 出处：fusion-aigc controller/creation/CreationController.java#cancel → CreationTaskService.cancelByUser。
// 兼容工具提交回执里的 taskId（画布存为 backend_ref）就是 Java 创作任务号（CompatTaskService.identityFor：
// creaft v1 的 task_uid 就是提交时下发的任务号），可以直接拿来取消。兼容工具都是工作流任务，Java 侧只置取消标志，
// 由驱动停上游并退款（点数由 Java 退回，Go 不退）。
//
// Java 的回包（CommonResult {code,msg,data}；ErrorCode.httpStatusOf：2001/2004 → HTTP 401，5xxx → 500，其余业务码 HTTP 200）：
//
//	code=0                         已受理取消                                    → CancelCancelled
//	code=4001 RESOURCE_NOT_FOUND   不存在或不属于该用户（按 userId 查）           → CancelNotFound
//	code=3006 TASK_NOT_FOUND       同上（防御）                                  → CancelNotFound
//	code=3001 BUSINESS_ERROR       「任务已完成或失败，无法取消」（status ≥ 2），
//	                               也可能是「任务状态已变化」/「由本地生活服务处理」→ CancelAlreadyFinished
//	HTTP 401 / code 2001 / 2004    登录失效                                       → CancelUnauthorized
//	网络错误 / 5xx / 429 / 其它码   暂时性                                         → CancelTransient
//
// 3001 是 Java 的通用业务失败码，不只表示「已结束」：调用方拿到 CancelAlreadyFinished 后必须立刻查一次任务
// （GetTask）以 Java 的真实状态为准，查到仍未终态的按暂时性处理。
const pathCreation = "/api/v1/creation/"

// CancelOutcome 取消结果分类。
type CancelOutcome int

const (
	// CancelTransient 网络 / 5xx / 429 / 未识别的业务码：稍后重试。
	CancelTransient CancelOutcome = iota
	// CancelCancelled Java 已受理取消（code=0），由 Java 停上游并退款。
	CancelCancelled
	// CancelAlreadyFinished Java 拒绝取消（3001，通常是任务已完成或失败）：以任务查询为准。
	CancelAlreadyFinished
	// CancelNotFound 任务不存在或不属于该用户。
	CancelNotFound
	// CancelUnauthorized 用户凭证失效（401 / 2001 / 2004）或没有凭证。
	CancelUnauthorized
)

func (o CancelOutcome) String() string {
	switch o {
	case CancelCancelled:
		return "cancelled"
	case CancelAlreadyFinished:
		return "already_finished"
	case CancelNotFound:
		return "not_found"
	case CancelUnauthorized:
		return "unauthorized"
	}
	return "transient"
}

// Java 业务码（ErrorCode）。
const (
	codeUnauthorized   = 2001
	codeRefreshInvalid = 2004
	codeBusinessError  = 3001
	codeTaskNotFound   = 3006
	codeNotFound       = 4001
)

// Cancel 代用户取消 Java 任务。除 CancelCancelled 外 error 非空（携带原因，便于日志；不含 Bearer）。
func (c *Client) Cancel(ctx context.Context, bearer, taskUID string) (CancelOutcome, error) {
	if !c.Enabled() {
		return CancelTransient, ErrNotConfigured
	}
	if strings.TrimSpace(bearer) == "" {
		return CancelUnauthorized, ErrNoBearer
	}
	taskUID = strings.TrimSpace(taskUID)
	if taskUID == "" {
		return CancelNotFound, errors.New("javatool: 缺少任务号")
	}
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	_, err := c.do(ctx, http.MethodPost, pathCreation+url.PathEscape(taskUID)+"/cancel", nil, bearer, h, []byte("{}"))
	return ClassifyCancel(err), err
}

// ClassifyCancel 取消接口的错误 → 分类（nil → CancelCancelled）。
func ClassifyCancel(err error) CancelOutcome {
	if err == nil {
		return CancelCancelled
	}
	if errors.Is(err, ErrNoBearer) {
		return CancelUnauthorized
	}
	var je *Error
	if !errors.As(err, &je) {
		return CancelTransient
	}
	switch {
	case je.HTTPStatus == http.StatusUnauthorized || je.Code == codeUnauthorized || je.Code == codeRefreshInvalid:
		return CancelUnauthorized
	case je.Code == codeNotFound || je.Code == codeTaskNotFound:
		return CancelNotFound
	case je.Code == codeBusinessError && je.HTTPStatus < 500:
		return CancelAlreadyFinished
	}
	return CancelTransient
}

package core

import (
	"errors"
	"fmt"
	"strings"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
)

// 段失败的内部码（用户不可见），每个码对应单一失败表（EXPERIENCE §4.8）里的一行。
const (
	FailVendor        = "VENDOR_FAILED"     // 一般失败（已自动重试 1 次）
	FailContentPolicy = "CONTENT_POLICY"    // 内容未通过生成方检查
	FailTimeout       = "DEADLINE_EXCEEDED" // 等太久已停止
	FailNoCapacity    = "NO_CAPACITY"       // 排队过长
	FailInput         = "INPUT_REJECTED"    // 原视频这一段读取失败
	FailUnknown       = "OUTCOME_UNKNOWN"   // 结果无法确定（不自动重试，退点）
	FailRejected      = "GATEWAY_REJECTED"  // 网关拒收请求（参数 / 配置问题，不重试）
	FailBillingGone   = "BILLING_GONE"      // 出片成功但预留已失效，不交付
	FailCut           = "CUT_FAILED"        // 切段失败
)

// Failure 一段失败的分类结果。
type Failure struct {
	Code      string // 内部码
	Class     string // 网关原始分类（排障用）
	Transient bool   // 是否瞬时错误：只有瞬时错误才自动重试
	Text      string // 用户口径：发生了什么
	Action    string // 下一步按钮
}

var failTable = map[string]Failure{
	FailVendor:        {Text: "这一段这次没生成出来", Action: "重做这一段"},
	FailContentPolicy: {Text: "这一段的内容没能通过生成方的检查，可以改一改替换的内容再试", Action: "修改后重做"},
	FailTimeout:       {Text: "这一段等了太久，已停止", Action: "重做这一段"},
	FailNoCapacity:    {Text: "现在排队的人比较多，这一段没能及时开始", Action: "稍后重做"},
	FailInput:         {Text: "原视频的这一段读取失败", Action: "换一条视频"},
	FailUnknown:       {Text: "这一段这次没生成出来", Action: "重做这一段"},
	FailRejected:      {Text: "出了点问题，我们在处理", Action: "联系我们"},
	FailBillingGone:   {Text: "这一段的点数预留已失效，没有扣点数", Action: "重做这一段"},
	FailCut:           {Text: "原视频的这一段读取失败", Action: "换一条视频"},
	FailBadOutput:     {Text: "画面有问题（黑屏 / 定格），这一段没收费，可以重做", Action: "重做这一段"},
}

func failure(code, class string, transient bool) Failure {
	f := failTable[code]
	f.Code, f.Class, f.Transient = code, class, transient
	return f
}

// classifyCall 网关调用终态（非成功）→ 失败分类。
//
// 自动重试只给瞬时错误（DESIGN §5.14）：内容策略 / 输入拒绝不重试（换个键再提只会再被拒、白花平台成本）；
// UNKNOWN（供应商可能已扣费、结果未知）也不重试：不让平台冒双倍成本，直接给用户退点并开放重做。
func classifyCall(c *aigateway.Call) Failure {
	if c.Status == aigateway.StatusUnknown {
		return failure(FailUnknown, "UNKNOWN", false)
	}
	if c.Status == aigateway.StatusCancelled {
		return failure(FailTimeout, "CANCELLED", false)
	}
	class := ""
	if c.Failure != nil {
		class = c.Failure.Class
	}
	switch class {
	case "CONTENT_POLICY":
		return failure(FailContentPolicy, class, false)
	case "INPUT_REJECTED":
		return failure(FailInput, class, false)
	case "DEADLINE_EXCEEDED":
		return failure(FailTimeout, class, true)
	case "NO_CAPACITY", "NO_ROUTE":
		return failure(FailNoCapacity, class, true)
	case "VENDOR_OUTCOME_UNKNOWN":
		return failure(FailUnknown, class, false)
	}
	return failure(FailVendor, class, true) // VENDOR_FAILED / ATTEMPTS_EXHAUSTED / 未知分类
}

// classifySubmit 提交阶段的错误。temporary=true 表示保持 RUNNING、下一轮用同一幂等键重提（网关按键幂等，不会重复调用）。
func classifySubmit(err error) (f Failure, temporary bool) {
	var ge *aigateway.Error
	if errors.As(err, &ge) {
		if ge.Temporary() && !ge.IsIdempotencyConflict() {
			return Failure{}, true
		}
		return failure(FailRejected, ge.Code, false), false
	}
	if errors.Is(err, aigateway.ErrNotConfigured) {
		return failure(FailRejected, "NOT_CONFIGURED", false), false
	}
	return Failure{}, true // 本地 / 网络类错误：重提
}

// ChargeMoneyText 一段的钱的文案（区分结算待重试的两种：扣费待确认 / 退点处理中）。
func ChargeMoneyText(c *model.Charge) string {
	if c.Status == model.ChargeSettleFailed && confirmPending(c) {
		return fmt.Sprintf("这一段做好了，正在确认扣除 %d 点（不会重复扣）", c.Points)
	}
	return MoneyText(c.Status, c.Points)
}

// confirmPending 结算待重试的是「确认扣除失败」（不是退点失败）。
func confirmPending(c *model.Charge) bool {
	return c.Status == model.ChargeSettleFailed && strings.HasPrefix(deref(c.Reason), model.SettleReasonConfirm)
}

// MoneyText 钱的文案：只有退回真正成功才说「已退回」（EXPERIENCE §4.8 末尾 X-4）。
// ChargeSettleFailed 在这里一律按退点口径；扣费待确认请用 ChargeMoneyText。
func MoneyText(chargeStatus int8, points int64) string {
	switch chargeStatus {
	case model.ChargeRefunded:
		return fmt.Sprintf("已退回 %d 点", points)
	case model.ChargeFree:
		return "没有扣点数"
	case model.ChargeConfirmed:
		return fmt.Sprintf("已扣除 %d 点", points)
	case model.ChargeHoldFailed:
		return "没有扣点数"
	}
	return "退点正在处理中，一般 5 分钟内到账"
}

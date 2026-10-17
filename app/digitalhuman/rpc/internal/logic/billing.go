package logic

import (
	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/pkg/billing"
)

// humanCharge 数字人出片的计费请求：按档次、按配音实际时长算。
// 请求号用出片的 task_id：每次出片都不同，轮询器也从记录里拿得到。
func humanCharge(h *model.AfvDigitalHuman, taskID string, seconds int) billing.Charge {
	return billing.Charge{
		UserID: h.UserId, RequestID: taskID, EntryCode: "digitalHuman", Kind: billing.KindVideo,
		Tier: billing.TierOf(h.Model), Count: 1, Seconds: seconds,
	}
}

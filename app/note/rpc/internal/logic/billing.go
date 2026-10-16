package logic

import "aigc-life/pkg/billing"

// noteCharge 图文笔记出图的计费请求：按张数收费。构思（74）不收费。
// 请求号用出图的 task_id：每次出图（含失败后重做）都不同，轮询器也从记录里拿得到。
func noteCharge(userID int64, taskID string, count int) billing.Charge {
	return billing.Charge{UserID: userID, RequestID: taskID, EntryCode: "imageTextNote", Kind: billing.KindQuick, Count: count}
}

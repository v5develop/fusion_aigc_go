package logic

import (
	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/pkg/billing"
)

// imageCharge 图片类记录（海报 / 长图 / 动作迁移 / 数字人口播）的计费请求。入口码就是 kind。
// 请求号用客户端的 X-Request-Id：一条记录只提交一次，轮询器结算时也从记录里拿得到。
func imageCharge(c *model.LocalLifeCreation) billing.Charge {
	ch := billing.Charge{UserID: c.UserId, RequestID: c.RequestId.String, EntryCode: c.Kind, Kind: billing.KindQuick, Count: int(c.ImageCount)}
	switch c.Kind {
	case model.KindActionTransfer, model.KindVoiceover:
		// 视频：按档次算。口播的时长是用户选的 15 / 30 秒；动作迁移不知道动作视频多长，交给档位的默认时长
		ch.Kind, ch.Tier, ch.Count, ch.Seconds = billing.KindVideo, billing.TierOf(c.Model), 1, int(c.Duration)
	}
	return ch
}

// videoCharge 视频类记录（探店 / 短剧 / 复刻）出片的计费请求。
// 请求号用出片的 task_id：每次出片（含失败后重做）都不同，轮询器也从记录里拿得到。
func videoCharge(c *model.AfvVideoCreation, taskID string) billing.Charge {
	return billing.Charge{
		UserID: c.UserId, RequestID: taskID, EntryCode: c.Kind, Kind: billing.KindVideo,
		Tier: billing.TierOf(c.Model), Count: 1, Seconds: int(c.Duration),
	}
}

package core

import (
	"encoding/json"

	"aigc-local-life/app/replicate/model"
)

// 已做好段的付费精修（Redo 带备注作用在 SUCCEEDED 段上）。
//
// 精修步骤与重做步骤同形（#u<n> + 自己的预留 + 一次 #r1 自动重试），区别只在「失败后怎么算这一段」：
// 精修失败（含自动重试用完）→ 退回精修预留，这一段仍是已完成、仍用旧视频；而失败段重做失败 → 这一段仍是失败。
// 不加列、不加标记：链上「链尾失败 / 取消、但更早有成功步骤」只可能是精修留下的（失败段重做之前不存在成功步骤）。
//
// 凡是要回答「这一段现在的结果是哪一步 / 处于什么状态」的地方（汇总、合成、视图、入库、通知、取消预览）都必须走 segResult，
// 不允许各自再看链尾。

// segResult 一段当前有效的结果步骤与是否正在精修。
//   - 链尾成功 → (链尾, false)；
//   - 链尾未终态、且更早有成功步骤 → (最近一次成功, true)：新版本做好前旧视频照常可播放 / 参与合成；
//   - 链尾失败 / 取消、且更早有成功步骤 → (最近一次成功, false)：精修没成功，仍是旧版本；
//   - 其余 → (链尾, false)。
//
// chain 必须是 segChain 的结果（升序、已跳过预留没成功的步骤）；空链返回 (nil, false)。
func segResult(chain []*model.Step) (eff *model.Step, refining bool) {
	n := len(chain)
	if n == 0 {
		return nil, false
	}
	last := chain[n-1]
	if last.Status == model.StepSucceeded {
		return last, false
	}
	for i := n - 2; i >= 0; i-- {
		if chain[i].Status == model.StepSucceeded {
			return chain[i], !model.StepFinal(last.Status)
		}
	}
	return last, false
}

// stepVideo 成功步骤的产物（视频 / 封面 / 时长）；不是成功步骤或没有视频返回 ok=false。
type stepVideo struct {
	VideoRid   string `json:"videoRid"`
	CoverRid   string `json:"coverRid"`
	DurationMs int64  `json:"durationMs"`
}

func videoOf(st *model.Step) (stepVideo, bool) {
	var v stepVideo
	if st == nil || st.Status != model.StepSucceeded || json.Unmarshal(st.Output, &v) != nil || v.VideoRid == "" {
		return stepVideo{}, false
	}
	return v, true
}

// segGenerations 各段的版本号（该段成功步骤数 - 1，没有成功为 0）与全任务合计（成片版本）。
func segGenerations(steps []*model.Step, segTotal int) (map[int]int, int) {
	gens, total := map[int]int{}, 0
	for k := 1; k <= segTotal; k++ {
		n := 0
		for _, st := range segChain(steps, k) {
			if st.Status == model.StepSucceeded {
				n++
			}
		}
		if n > 1 {
			gens[k] = n - 1
			total += n - 1
		}
	}
	return gens, total
}

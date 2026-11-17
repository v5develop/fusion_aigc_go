package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"aigc-local-life/app/replicate/model"
)

// 画面连贯度（CONTRACT §Y）。
//
//   - fast（默认）：各段并行，与旧行为一致；
//   - smooth：第 k 段等第 k−1 段到终态后才开始；k−1 成功就把它「当前有效结果」（segResult）的末帧 lastFrameRid 追加为 k 的最后一张
//     参考图，并在提示词末尾加一句衔接句（dialect.BridgeFmt）。Seedance 首帧与参考视频不可同用，所以走参考图通道。
//   - k−1 失败 / 取消 / 没有末帧：k 照常生成、不带衔接帧（步骤 output.bridgeSkip 记原因，任务视图 SegView.Bridge 展示）；不阻塞。
//   - 参考图已达上限（maxRefImages）：不带衔接帧（bridgeSkip=refs_full），方案卡 risks 提前说明。
//   - 重做 / 精修第 k 段：同一条路径，按 k−1 的当前有效结果取末帧；不级联重做后续段。
//   - 价格不变：网关按输出秒数 × 清晰度计价，参考图不计费。
//   - 钱与时限：所有段的预留在确认时已冻结（续期照旧，等待中的段只要还有未终态步骤就会续）；网关时限 SegmentDeadline 从真正提交那刻起算，
//     所以排队等上一段不会让本段超时；上一段卡住由它自己的网关时限 / 自动重试兜底，不会永久阻塞。

// bridgeSkip 衔接帧没用上的原因（步骤 output.bridgeSkip）。
const (
	bridgeSkipPrevFailed = "prev_failed" // 上一段失败 / 取消
	bridgeSkipNoFrame    = "no_frame"    // 上一段成功但网关没给末帧
	bridgeSkipRefsFull   = "refs_full"   // 参考图已满
)

// refLimit 方案参考图上限（与方案校验同口径：maxRefImages）。
func refLimit() int { return maxRefImages }

func (in *JobInput) smooth() bool { return in.Continuity == ContinuitySmooth }

// bridgeInfo 步骤 output 里的衔接信息。
type bridgeInfo struct {
	Rid  string `json:"bridgeRid,omitempty"`
	Skip string `json:"bridgeSkip,omitempty"`
}

func bridgeOf(out json.RawMessage) bridgeInfo {
	var b bridgeInfo
	if len(out) > 0 {
		_ = json.Unmarshal(out, &b)
	}
	return b
}

// prevBlocking smooth 模式下第 k 段（k ≥ 2）是否还要等上一段：上一段链尾没到终态。
func prevBlocking(steps []*model.Step, k int) bool {
	if k <= 1 {
		return false
	}
	chain := segChain(steps, k-1)
	if len(chain) == 0 {
		return false
	}
	return !model.StepFinal(chain[len(chain)-1].Status)
}

// decideBridge 第 k 段的衔接决定：返回衔接帧 rid（空 = 不带）与不带的原因。
func decideBridge(in *JobInput, steps []*model.Step, k int) (rid, skip string) {
	if !in.smooth() || k <= 1 {
		return "", ""
	}
	if len(in.RefImages) >= refLimit() {
		return "", bridgeSkipRefsFull
	}
	eff, _ := segResult(segChain(steps, k-1))
	v, ok := videoOf(eff)
	if !ok {
		return "", bridgeSkipPrevFailed
	}
	if v.LastFrameRid == "" {
		return "", bridgeSkipNoFrame
	}
	return v.LastFrameRid, ""
}

// withBridge 把衔接决定合进步骤 output（保留已有键，如 redoNote）。
func withBridge(out json.RawMessage, rid, skip string) json.RawMessage {
	m := map[string]any{}
	if len(out) > 0 {
		_ = json.Unmarshal(out, &m)
	}
	delete(m, "bridgeRid")
	delete(m, "bridgeSkip")
	if rid != "" {
		m["bridgeRid"] = rid
	}
	if skip != "" {
		m["bridgeSkip"] = skip
	}
	b, _ := json.Marshal(m)
	return b
}

// applyBridge 出片请求用：有衔接帧就返回追加了衔接句的提示词与衔接帧图号（1 起）；没有原样返回、图号 0。
func applyBridge(in *JobInput, st *model.Step, prompt string) (string, int) {
	b := bridgeOf(st.Output)
	if b.Rid == "" {
		return prompt, 0
	}
	n := len(in.RefImages) + 1
	return prompt + "\n" + bridgeLine(dialectOfInput(in), n), n
}

// bridgeLabel 任务视图里本段衔接情况（空 = 没有可说的）。
func bridgeLabel(out json.RawMessage) string {
	b := bridgeOf(out)
	switch {
	case b.Rid != "":
		return "used"
	case b.Skip != "":
		return "skipped_" + b.Skip
	}
	return ""
}

// continuityRefsFullRisk 方案卡风险：连贯模式但参考图已满。
func continuityRefsFullRisk(ann Annotations, nRefs, nSegs int) string {
	if ann.Continuity == ContinuitySmooth && nSegs > 1 && nRefs >= refLimit() {
		return "参考图已满，连贯模式这次不能用上一段画面衔接"
	}
	return ""
}

func continuityText(c string) string {
	if c == ContinuitySmooth {
		return "更连贯"
	}
	return "更快"
}

var _ = fmt.Sprintf
var _ = strings.TrimSpace

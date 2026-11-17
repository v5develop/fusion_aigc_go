package core

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"

	"aigc-local-life/app/replicate/model"
)

// ---------------------------------------------------------------------------------------------
// 任务来源（CONTRACT §O2）：Java 智能体调写工具时在（签名覆盖的）body 里带 origin {kind, conversation_id, turn_id}。
//   - 只存、只用来拼通知链接（回到原对话）；绝不参与任何业务判断（计费、权限、幂等都不看它）。
//   - 只在确认类入口（confirm / confirm_batch）建 job 时落库；继续 / 重做不建 job，不再写。
//   - 不透明：kind 只认 "agent"；id 只收 [A-Za-z0-9_-]{1,64}（数字 id 也收，按字符串存），其余一律当没带，免得脏值进链接。
// ---------------------------------------------------------------------------------------------

// OriginAgent 智能体来源。
const OriginAgent = "agent"

// Origin 任务来源。JSON 键与 Java 请求体一致（snake_case）。
type Origin struct {
	Kind           string `json:"kind"`
	ConversationID string `json:"conversation_id"`
	TurnID         string `json:"turn_id,omitempty"`
}

var originIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// UnmarshalJSON conversation_id / turn_id 既可以是字符串也可以是数字（Java 侧 id 可能是 long）。
func (o *Origin) UnmarshalJSON(b []byte) error {
	var raw struct {
		Kind           string          `json:"kind"`
		ConversationID json.RawMessage `json:"conversation_id"`
		TurnID         json.RawMessage `json:"turn_id"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	o.Kind, o.ConversationID, o.TurnID = raw.Kind, looseID(raw.ConversationID), looseID(raw.TurnID)
	return nil
}

func looseID(b json.RawMessage) string {
	if len(b) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(b, &n) == nil {
		return n.String()
	}
	return ""
}

// valid 来源可用：kind=agent 且会话号合规；turn_id 不合规时只丢掉它。
func (o Origin) normalized() (Origin, bool) {
	if o.Kind != OriginAgent || !originIDRe.MatchString(o.ConversationID) {
		return Origin{}, false
	}
	if !originIDRe.MatchString(o.TurnID) {
		o.TurnID = ""
	}
	return o, true
}

type originKey struct{}

// WithOrigin 把来源放进 ctx（内部智能体入口设置，确认类入口读取）。不合规的来源直接忽略。
func WithOrigin(ctx context.Context, o *Origin) context.Context {
	if o == nil {
		return ctx
	}
	n, ok := o.normalized()
	if !ok {
		return ctx
	}
	return context.WithValue(ctx, originKey{}, n)
}

// OriginOf ctx 里的来源（没有 / 不合规 → false）。
func OriginOf(ctx context.Context) (Origin, bool) {
	o, ok := ctx.Value(originKey{}).(Origin)
	return o, ok
}

// originJSON ctx 里的来源序列化（没有 / 不合规返回 nil → 列为 NULL）。
func originJSON(ctx context.Context) json.RawMessage {
	o, ok := ctx.Value(originKey{}).(Origin)
	if !ok {
		return nil
	}
	b, _ := json.Marshal(o)
	return b
}

// JobLink 用户端任务页（EXPERIENCE：用 ?job= 回到任务）。
func JobLink(jobID int64) string {
	return "/local-life/video-replicate?job=" + strconv.FormatInt(jobID, 10)
}

// JobLinkFor 通知链接：智能体来源 → /agent?conversation={id}&job={jobId}（回到原对话并定位任务卡）；否则维持任务页链接。
func JobLinkFor(j *model.Job) string {
	if j != nil && len(j.OriginJSON) > 0 {
		var o Origin
		if json.Unmarshal(j.OriginJSON, &o) == nil {
			if n, ok := o.normalized(); ok {
				return fmt.Sprintf("/agent?conversation=%s&job=%d", url.QueryEscape(n.ConversationID), j.ID)
			}
		}
	}
	return JobLink(j.ID)
}

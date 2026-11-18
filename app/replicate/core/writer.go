package core

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/rules/segment"
	"aigc-local-life/pkg/aigateway"

	"github.com/zeromicro/go-zero/core/logx"
)

// 写手（DESIGN §5.4 / §5.6）：方案确认后、出片前，为每段调用网关 chat 模型写出具体提示词。
//
//   - 可重放步骤：步骤键 seg/NN/write，幂等键 {前缀}rpl-{job}:seg/NN/write（第 n 次重写追加 #a<n>）；
//     请求只由确认时冻结的输入构造（JobInput.WriterSystem / WriterModelGroup / JobSegment.WriterBrief），崩溃续跑同键同请求。
//   - 写好的提示词落在步骤 output，该段的出片步骤（含自动重试 #r、用户重做 #u）都用它，重放逐字相同。
//   - 失败一律回落模板（方案阶段按方言渲染、已冻结在 JobSegment.Prompt 的那一稿）：网关不可用 / 被拒 / 超时 /
//     写出来不合格且重写次数用完。回落时步骤照样 SUCCEEDED（output.source=template + reason），出片不受影响。
//   - 不额外收费：写手成本由平台承担（报价只含出片秒数），走网关 limits.max_cost 封顶。

//go:embed prompts/replicate-writer.system.md
var writerSystemPrompt string

// WriterPromptVersion 写手系统提示词版本（冻结进 JobInput.WriterVersion，便于诊断）。
const WriterPromptVersion = "replicate-writer-1"

func writeKey(k int) string { return fmt.Sprintf("seg/%02d/write", k) }

// writerOutput 写手步骤的 output。
type writerOutput struct {
	Prompt  string   `json:"prompt,omitempty"`
	Source  string   `json:"source,omitempty"` // writer / template
	Reason  string   `json:"reason,omitempty"` // 回落原因
	Attempt int      `json:"attempt,omitempty"`
	Issues  []string `json:"issues,omitempty"`
}

// writerOf 某段的写手步骤（没有 = 写手没开，出片用模板）。
func writerOf(steps []*model.Step, k int) *model.Step {
	for _, st := range steps {
		if st.Kind == model.StepKindWriter && st.SegIndex == k {
			return st
		}
	}
	return nil
}

// applyWritten 把已写好的提示词覆盖到内存里的冻结输入（只影响本轮出片请求；库里的 job.input 不改）。
func applyWritten(in *JobInput, steps []*model.Step) {
	for i := range in.Segments {
		w := writerOf(steps, in.Segments[i].Index)
		if w == nil || w.Status != model.StepSucceeded {
			continue
		}
		var o writerOutput
		if json.Unmarshal(w.Output, &o) == nil && o.Source == "writer" && strings.TrimSpace(o.Prompt) != "" {
			in.Segments[i].Prompt = o.Prompt
		}
	}
}

// writerBrief 写手的用户消息（确认时冻结）：方言要求 + 素材标签 + 本段事实 + 用户改动 + 模板稿。
func writerBrief(d *Dialect, an *AnalysisResult, ann Annotations, refCount int, seg segment.Segment, total int, template string) string {
	v := buildPromptView(d, an, ann, refCount, seg, total)
	var b strings.Builder
	fmt.Fprintf(&b, "【目标模型】%s\n写作要求：%s\n", d.Code, d.WriterStyle)
	fmt.Fprintf(&b, "【素材】%s = 原片第 %.1f–%.1f 秒（本段参考视频）", d.VideoTag, seg.GenT0, seg.GenT1)
	for i := 1; i <= refCount; i++ {
		fmt.Fprintf(&b, "；"+d.ImageFmt+" = 第 %d 张参考图", i, i)
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "【这一段】共 %d 段中的第 %d 段，生成 %d 秒\n", total, seg.Index, seg.GenDuration)
	if s := strings.TrimSpace(an.Summary); s != "" {
		fmt.Fprintf(&b, "【原片概述】%s\n", s)
	}
	if len(v.Shots) > 0 {
		b.WriteString("【本段镜头】\n")
		for _, sh := range v.Shots {
			fmt.Fprintf(&b, "- %.1f–%.1f 秒：%s", sh.From, sh.To, sh.Text)
			if sh.Camera != "" {
				fmt.Fprintf(&b, "（镜头：%s）", sh.Camera)
			}
			b.WriteString("\n")
		}
	}
	var lines []string
	for _, l := range an.Lines {
		if l.T1 > seg.GenT0 && l.T0 < seg.GenT1 && strings.TrimSpace(l.Text) != "" {
			lines = append(lines, strings.TrimSpace(l.Text))
		}
	}
	if len(lines) > 0 {
		fmt.Fprintf(&b, "【本段台词】%s\n", strings.Join(lines, " / "))
	}
	if len(an.Subjects) > 0 {
		// 只列本段出镜的人（分析给了出镜时段才能判断，没给 = 每段都在）
		var ss []string
		for _, s := range an.Subjects {
			if subjectPresentIn(s, seg.GenT0, seg.GenT1) {
				ss = append(ss, s.Name+"："+s.Desc)
			}
		}
		if len(ss) > 0 {
			fmt.Fprintf(&b, "【出镜主体】%s\n", strings.Join(ss, "；"))
		}
	}
	b.WriteString("【用户改动】\n")
	for _, c := range v.Replace {
		if c.PerSubject { // 按人物点名：模板稿里已逐人写好，原样给出，要求写手保持逐人对应
			fmt.Fprintf(&b, "- 按人物逐个处理%s（只改点名的人，没点名的人保持原样；本段没出现的人不用写）：%s\n", c.Label, strings.ReplaceAll(c.OccText, "\n", " "))
			continue
		}
		if c.OccText != "" { // 逐套换装：模板稿里已逐套写好，这里原样给出，要求写手保持逐套对应
			fmt.Fprintf(&b, "- 逐套替换%s（只换指定的套，其余套保持原样）：%s\n", c.Label, strings.ReplaceAll(c.OccText, "\n", " "))
			continue
		}
		fmt.Fprintf(&b, "- 替换%s：原片是「%s」，换成 %s（%s）", c.Label, c.Current, c.Tags, c.Hint)
		if c.Note != "" {
			fmt.Fprintf(&b, "；补充：%s", c.Note)
		}
		b.WriteString("\n")
	}
	for _, c := range v.Describe {
		fmt.Fprintf(&b, "- 按描述改%s：%s\n", c.Label, c.Note)
	}
	if len(v.Keep) > 0 {
		var ks []string
		for _, k := range v.Keep {
			ks = append(ks, k.Label+"（"+k.Desc+"）")
		}
		fmt.Fprintf(&b, "【保持不变】%s\n", strings.Join(ks, "；"))
	}
	switch ann.Audio {
	case AudioRevoice:
		b.WriteString("【声音】不要生成新的声音（成片会换成新的配音）\n")
	case AudioKeep:
		b.WriteString("【声音】不要生成新的声音（成片会贴回原声）\n")
	case AudioMute:
		b.WriteString("【声音】保持无声\n")
	default:
		b.WriteString("【声音】重新配乐，与画面节奏匹配\n")
	}
	fmt.Fprintf(&b, "【硬性约束】不超过 %d 字；画面中不要生成任何文字、字幕、水印、Logo\n", d.MaxChars)
	fmt.Fprintf(&b, "【模板稿】\n%s\n", template)
	return b.String()
}

var fenceRe = regexp.MustCompile("(?s)^```[a-zA-Z]*\\s*(.*?)\\s*```$")

// cleanWritten 去掉代码块包裹与首尾空白 / 引号。
func cleanWritten(s string) string {
	s = strings.TrimSpace(s)
	if m := fenceRe.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	return strings.Trim(strings.TrimSpace(s), "\"“”")
}

func tagRegexp(format string) *regexp.Regexp {
	return regexp.MustCompile(strings.Replace(regexp.QuoteMeta(format), "%d", `(\d+)`, 1))
}

func citedRefs(format, text string) map[int]bool {
	out := map[int]bool{}
	for _, m := range tagRegexp(format).FindAllStringSubmatch(text, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil {
			out[n] = true
		}
	}
	return out
}

// CheckWritten 写手产出的确定性校验（v4 writer._check / lint_prompt 的可移植子集，方言参数化）。返回问题清单（空 = 合格）。
func CheckWritten(d *Dialect, text, template string, refCount int) []string {
	var issues []string
	n := len([]rune(text))
	if n < 20 {
		return []string{"内容太短"}
	}
	if n > d.MaxChars {
		issues = append(issues, fmt.Sprintf("全文 %d 字，超过上限 %d 字", n, d.MaxChars))
	}
	if !strings.Contains(text, d.VideoTag) {
		issues = append(issues, "没有引用参考视频 "+d.VideoTag)
	}
	got := citedRefs(d.ImageFmt, text)
	for i := range got {
		if i < 1 || i > refCount {
			issues = append(issues, fmt.Sprintf("引用了没挂上的参考图 "+d.ImageFmt, i))
		}
	}
	for i := range citedRefs(d.ImageFmt, template) {
		if !got[i] {
			issues = append(issues, fmt.Sprintf("漏掉了参考图 "+d.ImageFmt, i))
		}
	}
	for _, w := range d.RiskTerms {
		if strings.Contains(text, w) && !strings.Contains(template, w) {
			issues = append(issues, "出现了风控词「"+w+"」")
		}
	}
	if strings.Contains(text, "```") {
		issues = append(issues, "带了代码块")
	}
	return issues
}

// advanceWriter 推进一个写手步骤（PENDING → RUNNING → SUCCEEDED）。任何失败都回落模板，不会让该段失败。
func (s *Service) advanceWriter(ctx context.Context, j *model.Job, in *JobInput, st *model.Step) {
	if st.SegIndex < 1 || st.SegIndex > len(in.Segments) {
		return
	}
	sg := in.Segments[st.SegIndex-1]
	if st.Status == model.StepPending {
		now := s.now()
		ok, err := s.store.UpdateStep(ctx, st.ID, model.StepPending, map[string]any{"status": model.StepRunning, "attempt": st.Attempt + 1,
			"lease_owner": s.owner, "started_at": now})
		if err != nil || !ok {
			return
		}
		st.Status, st.StartedAt = model.StepRunning, &now
	}
	if st.Status != model.StepRunning {
		return
	}
	if s.gw == nil || in.WriterModelGroup == "" || strings.TrimSpace(in.WriterSystem) == "" {
		s.finishWriter(ctx, j, st, sg.Prompt, "template", "writer_unavailable", 0, nil)
		return
	}
	if st.StartedAt != nil && s.now().Sub(*st.StartedAt) > s.cfg.WriterDeadline {
		s.finishWriter(ctx, j, st, sg.Prompt, "template", "timeout", 0, nil)
		return
	}
	var state writerOutput
	_ = json.Unmarshal(st.Output, &state)
	if state.Attempt <= 0 {
		state.Attempt = 1
	}
	var call *aigateway.Call
	var err error
	if st.GatewayCallID == nil {
		key := st.IdemKey
		if state.Attempt > 1 {
			key += fmt.Sprintf("#a%d", state.Attempt)
		}
		user := sg.WriterBrief
		if len(state.Issues) > 0 {
			user += "\n【上一稿的问题，请改正后重写】\n- " + strings.Join(state.Issues, "\n- ")
		}
		req := aigateway.CallRequest{Operation: "chat.completion", ModelGroup: in.WriterModelGroup,
			Input: map[string]any{"messages": []map[string]any{{"role": "system", "content": in.WriterSystem}, {"role": "user", "content": user}},
				"temperature": 0.4},
			Labels: map[string]string{"caller_biz": "replicate", "job": fmt.Sprint(j.ID), "seg": fmt.Sprint(sg.Index), "stage": "write"}}
		if mc := strings.TrimSpace(in.MaxCostCNY); mc != "" {
			req.Limits = &aigateway.Limits{MaxCost: mc}
		}
		call, err = s.gw.Submit(ctx, req, aigateway.SubmitOptions{IdempotencyKey: key, Wait: s.cfg.PollWait, TraceID: deref(j.TaskID)})
		if err != nil {
			if _, temporary := classifySubmit(err); !temporary {
				logx.WithContext(ctx).Infof("replicate: 写手提交被拒，回落模板 判据=WRITER_REJECTED job=%d step=%s: %v", j.ID, st.StepKey, err)
				s.finishWriter(ctx, j, st, sg.Prompt, "template", "rejected", state.Attempt, nil)
			}
			return
		}
		if ok, _ := s.store.UpdateStep(ctx, st.ID, model.StepRunning, map[string]any{"gateway_call_id": call.ID, "gateway_status": call.Status}); !ok {
			return
		}
		st.GatewayCallID = ptr(call.ID)
	} else if call, err = s.pollCall(ctx, *st.GatewayCallID); err != nil {
		return
	}
	if !call.IsTerminal() {
		return
	}
	text := ""
	if call.Succeeded() && call.Result != nil {
		var out struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if call.Result.Decode(&out) == nil {
			text = cleanWritten(out.Message.Content)
		}
	}
	if text == "" {
		logx.WithContext(ctx).Infof("replicate: 写手调用没拿到正文，回落模板 判据=WRITER_CALL_FAILED job=%d step=%s call=%s", j.ID, st.StepKey, call.ID)
		s.finishWriter(ctx, j, st, sg.Prompt, "template", "call_failed", state.Attempt, nil)
		return
	}
	dl := dialectOfInput(in)
	issues := CheckWritten(dl, text, sg.Prompt, len(in.RefImages))
	if len(issues) == 0 {
		s.finishWriter(ctx, j, st, text, "writer", "", state.Attempt, nil)
		return
	}
	if state.Attempt < s.cfg.WriterMaxAttempts {
		b, _ := json.Marshal(writerOutput{Attempt: state.Attempt + 1, Issues: issues})
		logx.WithContext(ctx).Infof("replicate: 写手稿不合格，回灌重写 判据=WRITER_CHECK job=%d step=%s 第 %d 次 问题=%v", j.ID, st.StepKey, state.Attempt, issues)
		_, _ = s.store.UpdateStep(ctx, st.ID, model.StepRunning, map[string]any{"gateway_call_id": nil, "gateway_status": nil, "output": json.RawMessage(b)})
		return
	}
	logx.WithContext(ctx).Infof("replicate: 写手稿仍不合格，回落模板 判据=WRITER_CHECK_EXHAUSTED job=%d step=%s 问题=%v", j.ID, st.StepKey, issues)
	s.finishWriter(ctx, j, st, sg.Prompt, "template", "check_failed", state.Attempt, issues)
}

func (s *Service) finishWriter(ctx context.Context, j *model.Job, st *model.Step, prompt, source, reason string, attempt int, issues []string) {
	b, _ := json.Marshal(writerOutput{Prompt: prompt, Source: source, Reason: reason, Attempt: attempt, Issues: issues})
	if ok, _ := s.store.UpdateStep(ctx, st.ID, model.StepRunning, map[string]any{"status": model.StepSucceeded, "output": json.RawMessage(b),
		"finished_at": s.now()}); ok {
		st.Status, st.Output = model.StepSucceeded, b
		if source != "writer" {
			logx.WithContext(ctx).Infof("replicate: 写手回落模板 job=%d step=%s 原因=%s", j.ID, st.StepKey, reason)
		}
	}
}

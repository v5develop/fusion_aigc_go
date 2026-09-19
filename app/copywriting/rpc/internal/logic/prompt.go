package logic

import (
	"context"
	"fmt"
	"strings"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

// 提示词模板放在 afv_copywriting_option 的 PROMPT 类型里，运营可在后台改；
// 读不到时回落到下面的内置文本，保证服务可用。
// TODO 若后续统一到 afv_prompt / afv_creation_prompt，改这里的读取来源即可。
const (
	promptGenAiWrite     = "generate_ai_write"
	promptGenImitate     = "generate_imitate"
	promptTopic          = "ai_write_topic"
	promptSellingPoint   = "ai_write_selling_point"
	promptGenerateSuffix = "generate_suffix"
)

var fallbackPrompts = map[string]string{
	promptGenAiWrite:     "你是本地生活商家的社媒文案写手。输出可直接发布的中文文案，不要出现解释、标题标号或 markdown 语法。",
	promptGenImitate:     "你是本地生活商家的社媒文案写手，擅长模仿给定范文的结构、节奏和语气来写新文案。输出可直接发布的中文文案，不要解释。",
	promptTopic:          "你帮商家把粗略想法补写成一句清晰的文案主题，20 字以内，只输出主题本身。",
	promptSellingPoint:   "你帮商家把商品卖点整理得更有吸引力，保留原有事实（价格、时间、地点等），只输出整理后的卖点文本。",
	promptGenerateSuffix: "先输出一行标题，再空一行输出正文。标题不超过 30 字，不要加「标题：」这类前缀。",
}

// prompt 取模板：库里有且非空就用库里的，否则用内置。
func prompt(ctx context.Context, svcCtx *svc.ServiceContext, code string) string {
	o, err := svcCtx.Option.FindOneByOptionTypeCodeDeletedId(ctx, model.OptionPrompt, code, 0)
	if err == nil && o.Status == 1 && strings.TrimSpace(o.Value.String) != "" {
		return o.Value.String
	}
	if err != nil && err != model.ErrNotFound {
		logx.WithContext(ctx).Errorf("load prompt %s: %v", code, err)
	}
	return fallbackPrompts[code]
}

type promptParts struct {
	ContentType *model.AfvCopywritingOption
	Style       *model.AfvCopywritingOption
	Industry    *model.AfvCopywritingOption
	Topic       string
	Reference   string
	TargetWords int
	ShopInfo    string
}

// buildGeneratePrompt 组装生成文案的 system / user 提示词。
func buildGeneratePrompt(ctx context.Context, svcCtx *svc.ServiceContext, source string, p promptParts) (system, user string) {
	code := promptGenAiWrite
	if source == model.SourceImitate {
		code = promptGenImitate
	}
	system = prompt(ctx, svcCtx, code)

	var b strings.Builder
	appendOption(&b, "内容类型", p.ContentType)
	appendOption(&b, "文案风格", p.Style)
	appendOption(&b, "所属行业", p.Industry)
	if p.ShopInfo != "" {
		b.WriteString("店铺信息：" + p.ShopInfo + "\n")
	}
	if p.Reference != "" {
		b.WriteString("参考范文（模仿其结构与语气，不要照抄内容）：\n" + p.Reference + "\n")
	}
	if p.Topic != "" {
		b.WriteString("主题/卖点：" + p.Topic + "\n")
	}
	if p.TargetWords > 0 {
		b.WriteString(fmt.Sprintf("字数要求：%d 字左右\n", p.TargetWords))
	}
	b.WriteString("\n" + prompt(ctx, svcCtx, promptGenerateSuffix))
	return system, b.String()
}

func appendOption(b *strings.Builder, label string, o *model.AfvCopywritingOption) {
	if o == nil {
		return
	}
	b.WriteString(label + "：" + o.Label)
	if o.Value.Valid && o.Value.String != "" {
		b.WriteString("（" + o.Value.String + "）")
	}
	b.WriteString("\n")
}

// splitTitleBody 把模型输出切成标题和正文：第一行是标题，其余是正文。
func splitTitleBody(text string) (title, body string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", ""
	}
	parts := strings.SplitN(text, "\n", 2)
	title = strings.TrimSpace(strings.Trim(parts[0], "#*「」\"' "))
	if len(parts) == 1 {
		return trimRunes(title, 60), text
	}
	body = strings.TrimSpace(parts[1])
	if body == "" {
		body = title
	}
	return trimRunes(title, 60), body
}

func trimRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func runeLen(s string) int { return len([]rune(s)) }

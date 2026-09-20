package logic

import (
	"context"
	"fmt"
	"strings"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AiWriteLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAiWriteLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AiWriteLogic {
	return &AiWriteLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// dhScriptMaxLen 数字人配音内容上限，与 digitalhuman-rpc 的 Limits.MaxScriptLen 一致
const dhScriptMaxLen = 50

// AI 帮写 / AI 优化（同步）
func (l *AiWriteLogic) AiWrite(in *pb.AiWriteReq) (*pb.AiWriteResp, error) {
	code := promptTopic
	switch in.Scene {
	case "", "topic":
	case "selling_point":
		code = promptSellingPoint
	case "dh_script": // 数字人配音内容，默认 50 字以内
		code = promptDhScript
		if in.MaxLen <= 0 {
			in.MaxLen = dhScriptMaxLen
		}
	case "note_topic": // 图文笔记主题/思路
		code = promptNoteTopic
	case "poster_info": // 海报信息润色
		code = promptPosterInfo
	case "long_image": // 长图内容
		code = promptLongImage
	default:
		return nil, xerr.Newf(xerr.ParamError, "不支持的场景 %s", in.Scene)
	}
	if runeLen(in.Context) > l.svcCtx.Config.Generate.MaxTopicLen {
		return nil, xerr.Newf(xerr.ParamError, "输入最多 %d 字", l.svcCtx.Config.Generate.MaxTopicLen)
	}
	if err := checkText(l.ctx, l.svcCtx, "ai_write_input", in.Context); err != nil {
		return nil, err
	}
	system := prompt(l.ctx, l.svcCtx, code)
	mc, err := endpoint(l.ctx, l.svcCtx)
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	if o, _ := findOption(l.ctx, l.svcCtx, model.OptionContentType, in.ContentType, "内容类型"); o != nil {
		b.WriteString("内容类型：" + o.Label + "\n")
	}
	if o, _ := findOption(l.ctx, l.svcCtx, model.OptionIndustry, in.Industry, "行业"); o != nil {
		b.WriteString("所属行业：" + o.Label + "\n")
	}
	if in.NoteType != "" {
		b.WriteString("笔记类型：" + in.NoteType + "\n")
	}
	if in.MaxLen > 0 {
		b.WriteString(fmt.Sprintf("不超过 %d 字\n", in.MaxLen))
	}
	b.WriteString("用户已输入：" + in.Context)

	text, err := mc.chat(l.ctx, l.svcCtx, system, b.String())
	if err != nil {
		l.Errorf("ai write: %v", err)
		return nil, xerr.New(xerr.ServerError, "AI 生成暂时不可用，请稍后再试")
	}
	if in.MaxLen > 0 {
		text = trimRunes(text, int(in.MaxLen))
	}
	if err := checkText(l.ctx, l.svcCtx, "ai_write_output", text); err != nil {
		return nil, err
	}
	return &pb.AiWriteResp{Text: text}, nil
}

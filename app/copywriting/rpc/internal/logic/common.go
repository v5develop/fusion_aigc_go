package logic

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/ctxdata"
	"aigc-life/pkg/llm"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt(i int64) sql.NullInt64 {
	return sql.NullInt64{Int64: i, Valid: i != 0}
}

func toInfo(c *model.AfvCopywriting, labels map[string]string) *pb.CopywritingInfo {
	info := &pb.CopywritingInfo{
		Id: c.Id, Source: c.Source, ContentType: c.ContentType.String, Style: c.Style.String,
		Industry: c.Industry.String, Title: c.Title, Body: c.Body, WordCount: int32(c.WordCount),
		Topic: c.Topic.String, TargetWords: int32(c.TargetWords.Int64), ShopProfileId: c.ShopProfileId.Int64,
		Status: int32(c.Status), FailReason: c.FailReason.String, CreateTime: c.CreateTime.UnixMilli(),
	}
	if labels != nil {
		info.ContentTypeLabel = labels[c.ContentType.String]
	}
	return info
}

func toOption(o *model.AfvCopywritingOption) *pb.OptionInfo {
	return &pb.OptionInfo{
		Id: o.Id, OptionType: o.OptionType, Code: o.Code, Label: o.Label,
		Value: o.Value.String, SortOrder: int32(o.SortOrder), Status: int32(o.Status),
	}
}

// checkText 内容审核。命中敏感词返回参数错误，读词库失败只记日志不拦截。
func checkText(ctx context.Context, svcCtx *svc.ServiceContext, scene, text string) error {
	r, err := svcCtx.Moderation.Check(ctx, scene, text)
	if err != nil {
		logx.WithContext(ctx).Errorf("moderation %s: %v", scene, err)
		return nil
	}
	if !r.Passed {
		logx.WithContext(ctx).Infof("moderation blocked scene=%s label=%s", scene, r.Label)
		logModeration(ctx, svcCtx, scene, text, r.Label, 0)
		return xerr.New(xerr.ParamError, "内容包含敏感信息，请修改后重试")
	}
	return nil
}

// logModeration 审核留痕。默认不写库（Moderation.LogToDb），只记服务日志。
func logModeration(ctx context.Context, svcCtx *svc.ServiceContext, scene, text, label string, passed int64) {
	if !svcCtx.Config.Moderation.LogToDb {
		return
	}
	err := svcCtx.Shared.InsertModerationLog(ctx, model.ModerationLog{
		TaskId: "Copywriting_" + scene, UserId: ctxdata.UserID(ctx), Provider: svcCtx.Config.Moderation.Mode,
		Passed: passed, Scene: scene, Label: label, Reason: "命中敏感词",
		RequestContent: trimRunes(text, 2000),
	})
	if err != nil {
		logx.WithContext(ctx).Errorf("insert moderation log: %v", err)
	}
}

// contentTypeLabels 内容类型 code → 展示名，列表页的类型标签用。
func contentTypeLabels(ctx context.Context, svcCtx *svc.ServiceContext) map[string]string {
	list, err := svcCtx.Option.ListByType(ctx, model.OptionContentType, false)
	if err != nil {
		return nil
	}
	m := make(map[string]string, len(list))
	for _, o := range list {
		m[o.Code] = o.Label
	}
	return m
}

// findOption 取启用中的选项；code 为空返回 nil。
func findOption(ctx context.Context, svcCtx *svc.ServiceContext, optionType, code, name string) (*model.AfvCopywritingOption, error) {
	if code == "" {
		return nil, nil
	}
	o, err := svcCtx.Option.FindOneByOptionTypeCodeDeletedId(ctx, optionType, code, 0)
	if errors.Is(err, model.ErrNotFound) || (err == nil && o.Status != 1) {
		return nil, xerr.Newf(xerr.ParamError, "%s选项无效", name)
	}
	return o, err
}

// modelCall 一次模型调用所需的信息。
type modelCall struct {
	Endpoint       llm.Endpoint
	ModelID        int64
	MaxConcurrency int
}

// chat 取名额 → 调模型 → 放名额，遵守 afv_ai_model.max_concurrency。
func (m modelCall) chat(ctx context.Context, svcCtx *svc.ServiceContext, system, user string) (string, error) {
	release, err := svcCtx.Sem.Acquire(ctx, m.ModelID, m.MaxConcurrency)
	if err != nil {
		return "", xerr.New(xerr.ServerError, "当前生成排队较多，请稍后再试")
	}
	defer release()
	return svcCtx.LLM.Chat(ctx, m.Endpoint, system, user)
}

// endpoint 解析当前写作模型，返回调用所需信息。
func endpoint(ctx context.Context, svcCtx *svc.ServiceContext) (modelCall, error) {
	w, err := svcCtx.Shared.ResolveWritingModel(ctx)
	if err != nil {
		return modelCall{}, xerr.New(xerr.ServerError, err.Error())
	}
	timeout := time.Duration(svcCtx.Config.Generate.TimeoutSeconds) * time.Second
	if w.TimeoutSeconds > 0 && time.Duration(w.TimeoutSeconds)*time.Second < timeout {
		timeout = time.Duration(w.TimeoutSeconds) * time.Second
	}
	return modelCall{
		Endpoint: llm.Endpoint{
			Platform: w.Platform, BaseURL: w.ApiUrl, APIKey: w.ApiKey, Model: w.Code, Timeout: timeout,
		},
		ModelID:        w.Id,
		MaxConcurrency: int(w.MaxConcurrency),
	}, nil
}

// derivativeKinds 生成历史的标签 → 要查哪几种。空 / all 表示全部。
func derivativeKinds(kind string) []string {
	if kind == "" || kind == "all" {
		return nil // nil = 全部，交给 model
	}
	return []string{kind}
}

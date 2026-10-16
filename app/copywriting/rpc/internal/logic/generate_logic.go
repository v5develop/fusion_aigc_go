package logic

import (
	"aigc-life/pkg/billing"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"aigc-life/app/copywriting/model"
	"aigc-life/app/copywriting/rpc/internal/svc"
	"aigc-life/app/copywriting/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/threading"
)

type GenerateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGenerateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateLogic {
	return &GenerateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// AI帮写、爆款仿写：提交生成（异步，轮询 Get）
// AI帮写、爆款仿写：提交生成（异步，轮询 Get）
func (l *GenerateLogic) Generate(in *pb.GenerateReq) (*pb.CopywritingInfo, error) {
	if in.Source != model.SourceAiWrite && in.Source != model.SourceImitate {
		return nil, xerr.Param("source 只能是 AI_WRITE 或 IMITATE")
	}
	if in.Source == model.SourceImitate && strings.TrimSpace(in.ReferenceText) == "" {
		return nil, xerr.Param("请粘贴要仿写的爆款文案")
	}
	if in.RequestId == "" {
		return nil, xerr.Param("缺少 X-Request-Id")
	}
	// 幂等：同一用户同一 request_id 只生成一次（网络重试、连点都只落一条）
	if old, err := l.svcCtx.Copywriting.FindByRequestID(l.ctx, in.UserId, in.RequestId); err == nil {
		return toInfo(old, nil), nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	cfg := l.svcCtx.Config.Generate
	if runeLen(in.Topic) > cfg.MaxTopicLen {
		return nil, xerr.Newf(xerr.ParamError, "主题最多 %d 字", cfg.MaxTopicLen)
	}
	if runeLen(in.ReferenceText) > cfg.MaxReferenceLen {
		return nil, xerr.Newf(xerr.ParamError, "范文最多 %d 字", cfg.MaxReferenceLen)
	}

	if err := checkText(l.ctx, l.svcCtx, "generate_input", in.Topic+"\n"+in.ReferenceText); err != nil {
		return nil, err
	}

	parts := promptParts{Topic: strings.TrimSpace(in.Topic), Reference: strings.TrimSpace(in.ReferenceText)}
	var err error
	if parts.ContentType, err = findOption(l.ctx, l.svcCtx, model.OptionContentType, in.ContentType, "内容类型"); err != nil {
		return nil, err
	}
	if parts.Style, err = findOption(l.ctx, l.svcCtx, model.OptionStyle, in.Style, "文案风格"); err != nil {
		return nil, err
	}
	if parts.Industry, err = findOption(l.ctx, l.svcCtx, model.OptionIndustry, in.Industry, "行业"); err != nil {
		return nil, err
	}
	parts.TargetWords = int(in.TargetWords)
	if parts.TargetWords <= 0 {
		parts.TargetWords = cfg.DefaultWords
	}
	// 自定义字数给个范围：太小模型写不出东西，太大会超时且没人看
	if parts.TargetWords < cfg.MinWords || parts.TargetWords > cfg.MaxWords {
		return nil, xerr.Newf(xerr.ParamError, "字数请填 %d–%d", cfg.MinWords, cfg.MaxWords)
	}
	// 关联的店铺档案拼进提示词（选填；档案不存在或已删就当没选）
	parts.ShopInfo = shopProfileTextOf(l.ctx, l.svcCtx, in.ShopProfileId, in.UserId)

	mc, err := endpoint(l.ctx, l.svcCtx)
	if err != nil {
		return nil, err
	}

	row := &model.AfvCopywriting{
		UserId: in.UserId, OwnerType: nullInt(in.OwnerType), OwnerId: nullInt(in.OwnerId),
		Source: in.Source, ContentType: nullStr(in.ContentType), Style: nullStr(in.Style),
		Industry: nullStr(in.Industry), Title: "", Body: "", Topic: nullStr(parts.Topic),
		ReferenceText: nullStr(parts.Reference), TargetWords: nullInt(int64(parts.TargetWords)),
		ShopProfileId: nullInt(in.ShopProfileId), ModelId: nullInt(mc.ModelID), Status: model.StatusGenerating,
		RequestId: nullStr(in.RequestId),
	}
	// 计费：先只查点数够不够，不冻结；冻结在落库后做（按记录号幂等，1062 重复提交不会多冻）
	if err := l.svcCtx.Billing.Enough(l.ctx, copywritingCharge(l.svcCtx.Config.BillingEntryCode, in.UserId, "")); err != nil {
		return nil, err
	}
	// TODO 任务：等 mode 等枚举确认后，写一条 afv_creation_task，external_ref = 文案 id（Config.Task）
	res, err := l.svcCtx.Copywriting.Insert(l.ctx, row)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 { // 并发重复提交
			old, ferr := l.svcCtx.Copywriting.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toInfo(old, nil), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}

	if _, err := l.svcCtx.Billing.Hold(l.ctx, copywritingCharge(l.svcCtx.Config.BillingEntryCode, in.UserId, billingRequestID(row.Id))); err != nil {
		finishFailed(l.ctx, l.svcCtx, row.Id, in.UserId, "未能开始生成")
		return nil, err
	}
	startGenerate(l.svcCtx, row.Id, in.UserId, in.Source, parts)
	return toInfo(row, nil), nil
}

// startGenerate 后台调用模型并回写结果。文本生成通常几秒，客户端轮询 Get 拿结果。
func startGenerate(svcCtx *svc.ServiceContext, id, userID int64, source string, parts promptParts) {
	threading.GoSafe(func() {
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(svcCtx.Config.Generate.TimeoutSeconds)*time.Second)
		defer cancel()
		// 结算与失败落库不受生成超时影响：生成卡到最后一秒才返回时，ctx 已过期，
		// 用它去确认扣费会失败，用户拿到结果却没扣点（只能等预留单过期回收）。
		settle := context.WithoutCancel(ctx)
		log := logx.WithContext(ctx).WithFields(logx.Field("copywriting_id", id))

		mc, err := endpoint(ctx, svcCtx)
		if err != nil {
			finishFailed(settle, svcCtx, id, userID, "写作模型不可用")
			return
		}
		system, user := buildGeneratePrompt(ctx, svcCtx, source, parts)
		text, err := mc.chat(ctx, svcCtx, system, user)
		if err != nil {
			log.Errorf("generate: %v", err)
			finishFailed(settle, svcCtx, id, userID, "生成失败，请重试")
			return
		}
		title, body := splitTitleBody(text)
		if body == "" {
			finishFailed(settle, svcCtx, id, userID, "生成内容为空，请重试")
			return
		}
		if r, err := svcCtx.Moderation.Check(ctx, "generate_output", title+"\n"+body); err != nil {
			log.Errorf("moderation: %v", err)
		} else if !r.Passed {
			log.Infof("moderation blocked output, label=%s", r.Label)
			finishFailed(settle, svcCtx, id, userID, "生成内容未通过审核，请调整主题后重试")
			return
		}
		if _, err := svcCtx.Copywriting.FinishGenerate(settle, id, model.StatusDone, title, body, int64(runeLen(body)), ""); err != nil {
			log.Errorf("finish generate: %v", err)
			svcCtx.Billing.Fail(settle, userID, billingRequestID(id), "保存结果失败")
			return
		}
		svcCtx.Billing.Done(settle, userID, billingRequestID(id), 1)
	})
}

// finishFailed 记为失败并退回冻结的点数（没冻结过的 Fail 是空操作）
func finishFailed(ctx context.Context, svcCtx *svc.ServiceContext, id, userID int64, reason string) {
	if _, err := svcCtx.Copywriting.FinishGenerate(ctx, id, model.StatusFailed, "", "", 0, reason); err != nil {
		logx.WithContext(ctx).Errorf("mark failed %d: %v", id, err)
	}
	svcCtx.Billing.Fail(ctx, userID, billingRequestID(id), reason)
}

// copywritingCharge 文案一次生成算一次（重新生成算新的一次）。
func copywritingCharge(entry string, userID int64, requestID string) billing.Charge {
	return billing.Charge{UserID: userID, RequestID: requestID, EntryCode: entry, Kind: billing.KindQuick, Count: 1}
}

// billingRequestID 计费请求号按记录号生成：同一条记录只冻结一次，重试不会重复扣。
func billingRequestID(id int64) string { return fmt.Sprintf("copywriting-%d", id) }

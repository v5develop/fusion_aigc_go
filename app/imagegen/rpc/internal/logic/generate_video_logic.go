package logic

import (
	"aigc-local-life/pkg/modellabel"
	"aigc-local-life/pkg/taskcenter"
	"context"
	"database/sql"
	"encoding/json"
	"strings"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/aigc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type GenerateVideoLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGenerateVideoLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GenerateVideoLogic {
	return &GenerateVideoLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// GenerateVideo 确认剧本，出成片（73 / 88 / 视频复刻 71）
func (l *GenerateVideoLogic) GenerateVideo(in *pb.GenerateVideoReq) (*pb.VideoInfo, error) {
	c, err := findOwnedVideo(l.ctx, l.svcCtx, in.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	if c.Status != model.VideoPlanned && c.Status != model.VideoFailed {
		return nil, xerr.New(xerr.Conflict, "请先等剧本生成完成")
	}
	if !c.PlanScript.Valid || c.PlanScript.String == "" {
		return nil, xerr.New(xerr.Conflict, "还没有剧本，请先构思")
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	var task aigc.Task
	if c.Kind == model.KindVideoRemake {
		// 用户可能在剧情分析页改过剧情、换了档位或比例
		if err := l.applyRemakeOverrides(c, in); err != nil {
			return nil, err
		}
		if task, err = remakeTask(c); err != nil {
			return nil, err
		}
	} else {
		cfg, kerr := videoKindConf(l.svcCtx, c.Kind)
		if kerr != nil {
			return nil, kerr
		}
		if task, err = genTask(c, cfg); err != nil {
			return nil, err
		}
	}
	taskID := aigc.ExactTaskID(l.svcCtx.Config.TaskIDPrefix, taskcenter.Ref("vid", c.Id), c.GenTaskId.Valid) // 出片只有这一个执行面任务，和任务中心编号一致；失败重做才加随机码
	// 冻结点数（点数不够就到此为止，状态不动）。出片只提交这一次，用 task_id 当请求号，失败重做的 task_id 不同
	if _, err := l.svcCtx.Billing.Hold(l.ctx, videoCharge(c, taskID)); err != nil {
		return nil, err
	}
	ok, err := l.svcCtx.Video.StartGenerate(l.ctx, c.Id, taskID)
	if err != nil || !ok {
		l.svcCtx.Billing.Fail(l.ctx, c.UserId, taskID, "未能开始生成")
		if err != nil {
			return nil, err
		}
		return nil, xerr.New(xerr.Conflict, "当前状态不能生成")
	}
	if _, err := l.svcCtx.Aigc.Submit(l.ctx, taskID, task); err != nil {
		aigc.LogSubmitFailed(l.ctx, aigc.Trace{Service: "imagegen", Kind: c.Kind, Stage: "generate", RecordID: c.Id, UserID: c.UserId, TaskCenterID: taskcenter.Ref("vid", c.Id), ExecTaskID: taskID}, err)
		failVideo(l.ctx, l.svcCtx, c.Id, model.VideoGenerating, "提交生成失败，请重试")
		return nil, xerr.New(xerr.ServerError, "提交生成失败，请重试")
	}
	cur, err := findOwnedVideo(l.ctx, l.svcCtx, c.Id, in.UserId)
	if err != nil {
		return nil, err
	}
	return toVideoInfo(cur), nil
}

// applyRemakeOverrides 把用户改过的剧情 / 档位 / 比例落库，再按新值出片。
// 三个都选填：没传就沿用解析结果与默认值。
func (l *GenerateVideoLogic) applyRemakeOverrides(c *model.AfvVideoCreation, in *pb.GenerateVideoReq) error {
	prompt, mdl, size := strings.TrimSpace(in.Prompt), strings.TrimSpace(in.Model), strings.TrimSpace(in.Size)
	if prompt == "" && mdl == "" && size == "" {
		return nil
	}
	if mdl != "" && !validRemakeModel(l.ctx, l.svcCtx, mdl) {
		return xerr.Newf(xerr.ParamError, "不支持的模型 %s", modellabel.Of(mdl))
	}
	if size != "" && !validModel(optionsOf(l.ctx, l.svcCtx, genVideoSizes, videoSizes), size) {
		return xerr.Newf(xerr.ParamError, "不支持的比例 %s", size)
	}
	if prompt != "" {
		if n := len([]rune(prompt)); n > maxRemakePromptLen {
			return xerr.Newf(xerr.ParamError, "剧情内容最多 %d 字", maxRemakePromptLen)
		}
		b, err := json.Marshal(prompt)
		if err != nil {
			return xerr.New(xerr.ServerError, "剧情内容异常")
		}
		c.PlanScript = sql.NullString{String: string(b), Valid: true}
	}
	if mdl != "" {
		c.Model = mdl
	}
	if size != "" {
		c.Size = size
	}
	if err := l.svcCtx.Video.UpdateRemakeSettings(l.ctx, c.Id, c.PlanScript.String, c.Model, c.Size); err != nil {
		l.Errorf("update remake settings %d: %v", c.Id, err)
		return xerr.New(xerr.ServerError, "保存失败，请重试")
	}
	return nil
}

// validRemakeModel 复刻出片的档位是否在后台选项里。出片与报价共用。
func validRemakeModel(ctx context.Context, svcCtx *svc.ServiceContext, mdl string) bool {
	return validModel(optionsOf(ctx, svcCtx, genRemakeModels, remakeModels), mdl)
}

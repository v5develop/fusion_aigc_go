package logic

import (
	"context"
	"encoding/json"
	"strings"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type UpdateTaskColorFixLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewUpdateTaskColorFixLogic(ctx context.Context, svcCtx *svc.ServiceContext) *UpdateTaskColorFixLogic {
	return &UpdateTaskColorFixLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 保存子任务的校色参数（仅本人、仅成功的任务）：前端浏览器里烘图，服务端只存五个滑杆值；返回该子任务
func (l *UpdateTaskColorFixLogic) UpdateTaskColorFix(in *studio.UpdateTaskColorFixReq) (*studio.TaskInfo, error) {
	return updateColorFix(l.ctx, l.svcCtx.Repo, in)
}

// colorFixRange 校色五参数的取值范围，与前端 fusion-aigc-web src/utils/colorfix.ts 的 COLORFIX_RANGES 一致
// （前端键名 sat / light / bright 对应这里的 saturation / value / brightness），滑杆步长 1，只收整数。
var colorFixRange = []struct {
	key, name string
	min, max  int32
	get       func(*studio.ColorFixParams) int32
}{
	{"hue", "色相", -30, 30, func(p *studio.ColorFixParams) int32 { return p.Hue }},
	{"saturation", "饱和度", -60, 70, func(p *studio.ColorFixParams) int32 { return p.Saturation }},
	{"value", "明度", -40, 40, func(p *studio.ColorFixParams) int32 { return p.Value }},
	{"brightness", "亮度", -80, 80, func(p *studio.ColorFixParams) int32 { return p.Brightness }},
	{"contrast", "对比度", -50, 50, func(p *studio.ColorFixParams) int32 { return p.Contrast }},
}

// colorFixJSON 列里存的形态（键名同接口）。
type colorFixJSON struct {
	Hue        int32 `json:"hue"`
	Saturation int32 `json:"saturation"`
	Value      int32 `json:"value"`
	Brightness int32 `json:"brightness"`
	Contrast   int32 `json:"contrast"`
}

// ValidateColorFix 校验五参数范围；第一个越界的就是返回的错误。
func ValidateColorFix(p *studio.ColorFixParams) error {
	if p == nil {
		return xerr.New(xerr.ParamMissing, "缺少校色参数")
	}
	for _, r := range colorFixRange {
		if v := r.get(p); v < r.min || v > r.max {
			return xerr.Newf(xerr.ParamError, "%s（%s）取值范围 %d–%d", r.name, r.key, r.min, r.max)
		}
	}
	return nil
}

// parseColorFix 列值 → 接口结构；NULL / 解析失败为 nil（= 没保存过）。
func parseColorFix(raw json.RawMessage) *studio.ColorFixParams {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var v colorFixJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	return &studio.ColorFixParams{Hue: v.Hue, Saturation: v.Saturation, Value: v.Value, Brightness: v.Brightness, Contrast: v.Contrast}
}

// updateColorFix 校验 → 取本人任务 → 只允许成功的任务 → 写列 → 回最新任务（带 asset_id）。
func updateColorFix(ctx context.Context, r *repo.Repo, in *studio.UpdateTaskColorFixReq) (*studio.TaskInfo, error) {
	if in.UserId <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	taskID := strings.TrimSpace(in.TaskId)
	if taskID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	if err := ValidateColorFix(in.Params); err != nil {
		return nil, err
	}
	t, err := r.Task.GetByTaskID(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if t == nil || t.UserID != in.UserId || t.Deleted != 0 {
		return nil, xerr.NotFoundf("任务")
	}
	if t.Status != model.TaskSucceeded {
		return nil, xerr.New(xerr.Conflict, "只有生成成功的图片可以保存校色参数")
	}
	p := in.Params
	raw, err := json.Marshal(colorFixJSON{Hue: p.Hue, Saturation: p.Saturation, Value: p.Value, Brightness: p.Brightness, Contrast: p.Contrast})
	if err != nil {
		return nil, err
	}
	ok, err := r.Task.SetColorFix(ctx, t.ID, in.UserId, raw)
	if err != nil {
		return nil, err
	}
	if !ok {
		// 读和写之间被删除了（成功的任务不会再变状态）
		return nil, xerr.NotFoundf("任务")
	}
	t.ColorFixParams = raw
	out := toTaskInfo(t, model.BatchDone) // 成功的任务进度恒为 100，与批次阶段无关，不必再读批次
	attachAssetIDs(ctx, r, in.UserId, []*studio.TaskInfo{out})
	return out, nil
}

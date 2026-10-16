package logic

import (
	"aigc-life/pkg/billing"
	"context"
	"errors"
	"strings"
	"time"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateLogic {
	return &CreateLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 开始生成：配音（百炼 TTS）→ 口播视频（执行面 85），轮询 Get
func (l *CreateLogic) Create(in *pb.CreateReq) (*pb.DigitalHumanInfo, error) {
	if in.RequestId == "" {
		return nil, xerr.Param("缺少 X-Request-Id")
	}
	if old, err := l.svcCtx.Human.FindByRequestID(l.ctx, in.UserId, in.RequestId); err == nil {
		return toInfo(old, voiceNames(l.ctx, l.svcCtx, []*model.AfvDigitalHuman{old})), nil
	} else if !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	if err := requireAgreement(l.ctx, l.svcCtx, in.UserId); err != nil {
		return nil, err
	}
	if err := requireAigc(l.svcCtx); err != nil {
		return nil, err
	}
	lim := l.svcCtx.Config.Limits
	script := strings.TrimSpace(in.Script)
	switch {
	case script == "":
		return nil, xerr.Param("请输入配音内容")
	case runeLen(script) > lim.MaxScriptLen:
		return nil, xerr.Newf(xerr.ParamError, "配音内容最多 %d 字", lim.MaxScriptLen)
	}
	name := strings.TrimSpace(in.Name)
	if runeLen(name) > lim.MaxNameLen {
		return nil, xerr.Newf(xerr.ParamError, "名称最多 %d 字", lim.MaxNameLen)
	}
	if err := checkText(l.ctx, l.svcCtx, "digital_human_input", name+"\n"+script); err != nil {
		return nil, err
	}

	mdl, err := normalizeModel(in.Model)
	if err != nil {
		return nil, err
	}

	v, err := l.svcCtx.Voice.FindAlive(l.ctx, in.VoiceId)
	if errors.Is(err, model.ErrNotFound) || (err == nil && v.Status != 1) {
		return nil, xerr.Param("音色不可用，请重新选择")
	}
	if err != nil {
		return nil, err
	}
	src, err := resolvePortraitSource(l.ctx, l.svcCtx, in)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = src.DefaultName
		if name == "" {
			name = v.Name
		}
	}

	// 时长要等配音出来才知道，先按最短时长拦一道明显不够的；真正冻结在配音之后
	if err := l.svcCtx.Billing.Enough(l.ctx, billing.Charge{
		UserID: in.UserId, EntryCode: "digitalHuman", Kind: billing.KindVideo,
		Tier: billing.TierOf(mdl), Count: 1, Seconds: minVideoSeconds,
	}); err != nil {
		return nil, err
	}

	row := &model.AfvDigitalHuman{
		CopywritingId: nullInt(in.CopywritingId),
		UserId: in.UserId, Name: name, PortraitSource: in.PortraitSource, PortraitRefId: nullInt(src.RefID),
		ImageUrl: src.ImageURL, VoiceId: v.Id, Script: script, Model: mdl, TtsTaskId: nullStr(newTaskID(l.svcCtx)),
		RequestId: nullStr(in.RequestId), Status: model.DhStatusDubbing,
	}
	res, err := l.svcCtx.Human.Insert(l.ctx, row)
	if err != nil {
		if isDuplicate(err) {
			old, ferr := l.svcCtx.Human.FindByRequestID(l.ctx, in.UserId, in.RequestId)
			if ferr != nil {
				return nil, ferr
			}
			return toInfo(old, voiceNames(l.ctx, l.svcCtx, []*model.AfvDigitalHuman{old})), nil
		}
		return nil, err
	}
	if row.Id, err = res.LastInsertId(); err != nil {
		return nil, err
	}
	row.CreateTime = time.Now()
	startDigitalHuman(l.svcCtx, row, v.Voice)
	return toInfo(row, map[int64]string{v.Id: v.Name}), nil
}

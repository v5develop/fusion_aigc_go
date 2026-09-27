package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"aigc-life/app/digitalhuman/model"
	"aigc-life/app/digitalhuman/rpc/internal/svc"
	"aigc-life/app/digitalhuman/rpc/pb"
	"aigc-life/pkg/aigc"
	"aigc-life/pkg/tts"
	"aigc-life/pkg/xerr"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

const agreementAigc = "aigc"

// 出片模型（执行面 85 的 model 参数）。缺省留在云端 85；lite 被路由到本地 GPU 的 45。
// 第一项是默认值——前端不写死，从 options 接口取。
const (
	modelCloud = "seedance2.5"
	modelLite  = "lite"
)

var videoModels = []*pb.OptionInfo{
	{OptionType: "MODEL", Code: modelCloud, Label: "seedance 2.5", Status: 1},
	{OptionType: "MODEL", Code: modelLite, Label: "XQ Video 1.0 Lite", Status: 1},
}

// wireModel 发给执行面的 model 值。
// 云端这档**发空**：85 的默认就是 doubao-seedance-2-5-260628，文档没说认不认 "seedance2.5"
// 这种简写，不如不发，让它用自己的默认。lite 是文档明写的路由值，照发。
func wireModel(code string) string {
	if code == modelLite {
		return modelLite
	}
	return ""
}

// normalizeModel 空值按默认档；不认识的值直接报错，不要默默改成云端——
// 那样用户以为选了便宜的 lite，结果按云端扣钱。
func normalizeModel(code string) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return videoModels[0].Code, nil
	}
	for _, m := range videoModels {
		if m.Code == code {
			return code, nil
		}
	}
	return "", xerr.Param("出片模型不可用，请重新选择")
}

func nullStr(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

func nullInt(i int64) sql.NullInt64 {
	return sql.NullInt64{Int64: i, Valid: i != 0}
}

func runeLen(s string) int { return len([]rune(s)) }

func isDuplicate(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && me.Number == 1062
}

func toOption(o *model.AfvDigitalHumanOption) *pb.OptionInfo {
	return &pb.OptionInfo{
		Id: o.Id, OptionType: o.OptionType, Code: o.Code, Label: o.Label,
		Value: o.Value.String, SortOrder: int32(o.SortOrder), Status: int32(o.Status),
	}
}

func toPublic(p *model.PublicModel) *pb.PublicInfo {
	avatar := p.AvatarUrl
	if avatar == "" {
		avatar = p.ImageUrl
	}
	return &pb.PublicInfo{Id: p.Id, Name: p.Name, RoleLabel: p.RoleLabel, ImageUrl: p.ImageUrl, AvatarUrl: avatar}
}

// publicSlot 公共数字人所在的模板素材投放位（配置 PublicModel）。
func publicSlot(svcCtx *svc.ServiceContext) model.PublicModelSlot {
	c := svcCtx.Config.PublicModel
	return model.PublicModelSlot{BizType: c.BizType, Module: c.Module, Slot: c.Slot}
}

// toVoice withParam=false 时不返回 TTS 参数值（App 端只用我们的 id）。
func toVoice(v *model.AfvDigitalHumanVoice, withParam bool) *pb.VoiceInfo {
	var cats []string
	_ = json.Unmarshal([]byte(v.Categories), &cats)
	info := &pb.VoiceInfo{
		Id: v.Id, Name: v.Name, Gender: v.Gender, Style: v.Style, Categories: cats,
		PreviewUrl: v.PreviewUrl.String, SortOrder: int32(v.SortOrder), Status: int32(v.Status),
	}
	if withParam {
		info.Voice = v.Voice
	}
	return info
}

func toPortrait(p *model.AfvDigitalHumanPortrait) *pb.PortraitInfo {
	var urls []string
	if p.ImageUrls.Valid {
		_ = json.Unmarshal([]byte(p.ImageUrls.String), &urls)
	}
	return &pb.PortraitInfo{
		Id: p.Id, Gender: p.Gender, Age: p.Age, SkinTone: p.SkinTone, Role: p.Role.String,
		Description: p.Description.String, Status: int32(p.Status), ImageUrls: urls,
		FailReason: p.FailReason.String, CreateTime: p.CreateTime.UnixMilli(),
	}
}

// toInfo voiceNames 为 nil 时不填音色名。
func toInfo(h *model.AfvDigitalHuman, voiceNames map[int64]string) *pb.DigitalHumanInfo {
	return &pb.DigitalHumanInfo{
		Id: h.Id, UserId: h.UserId, Name: h.Name, PortraitSource: h.PortraitSource, ImageUrl: h.ImageUrl,
		VoiceId: h.VoiceId, VoiceName: voiceNames[h.VoiceId], Script: h.Script, VideoUrl: h.VideoUrl.String,
		CoverUrl: h.ImageUrl, Duration: int32(h.Duration.Int64), Model: h.Model, Status: int32(h.Status),
		FailReason: h.FailReason.String, CreateTime: h.CreateTime.UnixMilli(),
	}
}

// voiceNames 批量取音色名（含已停用/删除的，历史记录照常显示）。
func voiceNames(ctx context.Context, svcCtx *svc.ServiceContext, list []*model.AfvDigitalHuman) map[int64]string {
	names := map[int64]string{}
	for _, h := range list {
		if _, ok := names[h.VoiceId]; ok {
			continue
		}
		if v, err := svcCtx.Voice.FindOne(ctx, h.VoiceId); err == nil {
			names[h.VoiceId] = v.Name
		} else {
			names[h.VoiceId] = ""
		}
	}
	return names
}

// requireAgreement 生成类操作前必须已同意当前版本的 AIGC 使用约定。
func requireAgreement(ctx context.Context, svcCtx *svc.ServiceContext, userID int64) error {
	_, err := svcCtx.Agreement.FindOneByUserIdAgreementCodeVersion(ctx, userID, agreementAigc, int64(svcCtx.Config.Agreement.AigcVersion))
	if errors.Is(err, model.ErrNotFound) {
		return xerr.New(xerr.AgreementReq, "请先阅读并同意《AIGC功能使用约定》")
	}
	return err
}

// requireAigc 执行面未配置时，依赖它的功能直接拒绝，不落记录。
func requireAigc(svcCtx *svc.ServiceContext) error {
	if !svcCtx.Aigc.Enabled() {
		return xerr.New(xerr.NotReady, "AI 生成服务暂未开放，请稍后再试")
	}
	return nil
}

// checkText 内容审核。命中敏感词返回参数错误，读词库失败只记日志不拦截。
func checkText(ctx context.Context, svcCtx *svc.ServiceContext, scene, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	r, err := svcCtx.Moderation.Check(ctx, scene, text)
	if err != nil {
		logx.WithContext(ctx).Errorf("moderation %s: %v", scene, err)
		return nil
	}
	if !r.Passed {
		logx.WithContext(ctx).Infof("moderation blocked scene=%s label=%s", scene, r.Label)
		return xerr.New(xerr.ParamError, "内容包含敏感信息，请修改后重试")
	}
	return nil
}

func newTaskID(svcCtx *svc.ServiceContext) string {
	return aigc.NewTaskID(svcCtx.Config.TaskIDPrefix)
}

// ttsCall 一次配音调用所需的信息。
type ttsCall struct {
	Endpoint       tts.Endpoint
	ModelID        int64
	MaxConcurrency int
}

// ttsEndpoint 解析当前配音模型（afv_system_config.ai_tts_model_id）。
func ttsEndpoint(ctx context.Context, svcCtx *svc.ServiceContext) (ttsCall, error) {
	m, err := svcCtx.Shared.ResolveTTSModel(ctx)
	if err != nil {
		return ttsCall{}, err
	}
	timeout := time.Duration(svcCtx.Config.TTS.TimeoutSeconds) * time.Second
	if m.TimeoutSeconds > 0 {
		timeout = time.Duration(m.TimeoutSeconds) * time.Second
	}
	return ttsCall{
		Endpoint:       tts.Endpoint{BaseURL: m.ApiUrl, APIKey: m.ApiKey, Model: m.Code, Timeout: timeout},
		ModelID:        m.Id,
		MaxConcurrency: int(m.MaxConcurrency),
	}, nil
}

// dubbed 一段已转存的配音。
type dubbed struct {
	URL      string
	Duration time.Duration
}

// synthesize 取名额 → 调百炼合成 → 下载 → 转存。百炼的音频地址 24 小时失效且是 http，必须转存。
func synthesize(ctx context.Context, svcCtx *svc.ServiceContext, userID int64, text, voice string) (*dubbed, error) {
	call, err := ttsEndpoint(ctx, svcCtx)
	if err != nil {
		return nil, err
	}
	release, err := svcCtx.Sem.Acquire(ctx, call.ModelID, call.MaxConcurrency)
	if err != nil {
		return nil, err
	}
	src, err := svcCtx.TTS.Synthesize(ctx, call.Endpoint, text, voice)
	release()
	if err != nil {
		return nil, err
	}
	data, err := svcCtx.TTS.Download(ctx, src)
	if err != nil {
		return nil, err
	}
	d, err := tts.WavDuration(data)
	if err != nil {
		return nil, err
	}
	key := storageKey(svcCtx, "audio", userID, "wav")
	u, err := svcCtx.Storage.Put(ctx, key, "audio/wav", data)
	if err != nil {
		return nil, err
	}
	return &dubbed{URL: u, Duration: d}, nil
}

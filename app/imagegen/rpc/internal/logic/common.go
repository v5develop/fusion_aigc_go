package logic

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/config"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"

	"github.com/go-sql-driver/mysql"
	"github.com/zeromicro/go-zero/core/logx"
)

// posterTypeFilter 海报类型在模板筛选维度字典里的编码（afv_public_template_filter）
const posterTypeFilter = "posterType"

// longImageTypeFilter 长图类型（喜庆 / 营销 / 社媒）在模板筛选维度字典里的编码。与海报类型是两个维度：选项编码可以相同，但互不相通。
const longImageTypeFilter = "longImageType"

// templateTypeFilter 模板列表按哪个筛选维度过滤：海报用 posterType，长图用 longImageType。
func templateTypeFilter(kind string) string {
	if kind == model.KindLongImage {
		return longImageTypeFilter
	}
	return posterTypeFilter
}

// posterStyles 海报风格（设计稿「海报偏好」弹窗）。空 code = AI自动匹配，不传给执行面。
var posterStyles = []*pb.Option{
	{Code: "", Label: "AI自动匹配"},
	{Code: "portrait", Label: "人像摄影"},
	{Code: "film", Label: "电影"},
	{Code: "comic", Label: "漫画"},
	{Code: "flat", Label: "扁平插画"},
	{Code: "pencil", Label: "彩铅"},
	{Code: "cyberpunk", Label: "赛博朋克"},
	{Code: "cg3d", Label: "3D CG"},
	{Code: "clay", Label: "橡皮泥"},
	{Code: "watercolor", Label: "水彩"},
	{Code: "ghibli", Label: "吉卜力"},
	{Code: "chinese", Label: "中国风"},
	{Code: "oil", Label: "油画"},
	{Code: "ink", Label: "水墨画"},
	{Code: "sketch", Label: "简笔画"},
}

// actionModels 动作迁移的模型选择（设计稿），值直接作为执行面 9 的 model：
// seedance 2.0 会被路由到 86（入参须公网 URL）；lite 走本地 GPU 的 9。
var actionModels = []*pb.Option{
	{Code: "doubao-seedance-2-0-260128", Label: "seedance 2.0"},
	{Code: "lite", Label: "xiaoqi-video-1.0-lite"},
}

// voiceoverModels 数字人口播的模型选择（设计稿），值直接作为执行面 92 的 model：
// seedance2.5 留在云端 92；lite 路由到本地 GPU 的 91。第一项为默认。
var voiceoverModels = []*pb.Option{
	{Code: "seedance2.5", Label: "seedance 2.5"},
	{Code: "lite", Label: "XQ Video 1.0 Lite"},
}

// voiceoverDurations 数字人口播的视频时长（秒），执行面 92 只支持这两档
var voiceoverDurations = []int32{15, 30}

// voiceoverSizes 数字人口播的视频比例
var voiceoverSizes = []*pb.Option{{Code: "9:16", Label: "9:16"}, {Code: "16:9", Label: "16:9"}}

func validModel(models []*pb.Option, code string) bool {
	for _, m := range models {
		if m.Code == code {
			return true
		}
	}
	return false
}

// posterModels 海报出图档位。78 支持 mini / plus / max，本版只开 max（已定规则），
// 但仍由后端下发，前端不写死——以后放开别的档位改这里即可。长图（76）没有 model 参数，不下发。
var posterModels = []*pb.Option{
	{Code: "max", Label: "max"},
}

// remakeModels 爆款视频复刻的模型选择，值直接作为执行面 71 的 model：
// 缺省的 doubao-seedance-2-0 走云端，lite 路由到本地 GPU 的 83。与动作迁移保持一致的两档。
var remakeModels = []*pb.Option{
	{Code: "doubao-seedance-2-0-260128", Label: "seedance 2.0"},
	{Code: "lite", Label: "XQ Video 1.0 Lite"},
}

// posterSizes 海报图片比例（设计稿「生成设置」弹窗）
var posterSizes = []*pb.Option{
	{Code: "9:16", Label: "9:16"}, {Code: "16:9", Label: "16:9"},
	{Code: "3:4", Label: "3:4"}, {Code: "4:3", Label: "4:3"},
	{Code: "2:3", Label: "2:3"}, {Code: "1:1", Label: "1:1"},
}

func styleLabel(code string) (string, bool) {
	for _, s := range posterStyles {
		if s.Code == code {
			return s.Label, true
		}
	}
	return "", false
}

// posterTypeLabel 海报类型编码 → 展示名（模板筛选维度字典）；找不到时返回原值。
func posterTypeLabel(ctx context.Context, svcCtx *svc.ServiceContext, bizType, code string) string {
	if code == "" {
		return ""
	}
	opts, err := svcCtx.Shared.ListFilterOptions(ctx, bizType, posterTypeFilter)
	if err != nil {
		logx.WithContext(ctx).Errorf("load poster types: %v", err)
		return code
	}
	for _, o := range opts {
		if o.Code == code {
			return o.Name
		}
	}
	return code
}

// kindConf 取某种成品（海报 / 长图）的配置；kind 非法返回错误。
func kindConf(svcCtx *svc.ServiceContext, kind string) (*config.KindConf, error) {
	switch kind {
	case model.KindPoster:
		return &svcCtx.Config.Poster, nil
	case model.KindLongImage:
		return &svcCtx.Config.LongImage, nil
	case model.KindActionTransfer:
		return &svcCtx.Config.ActionTransfer, nil
	case model.KindVoiceover:
		return &svcCtx.Config.Voiceover, nil
	}
	return nil, xerr.Newf(xerr.ParamError, "不支持的类型 %s", kind)
}

func templateSlot(c *config.KindConf) model.TemplateSlot {
	return model.TemplateSlot{BizType: c.BizType, Module: c.Module, Slot: c.Slot}
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

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func parseList(s sql.NullString) []string {
	var v []string
	if s.Valid {
		_ = json.Unmarshal([]byte(s.String), &v)
	}
	return v
}

func toInfo(c *model.LocalLifeCreation) *pb.CreationInfo {
	return &pb.CreationInfo{
		Id: c.Id, UserId: c.UserId, Kind: c.Kind, Content: c.Content, MaterialUrls: parseList(c.MaterialUrls),
		TemplateId: c.TemplateAssetId.Int64, TemplateUrl: c.TemplateUrl.String, PosterType: c.PosterType.String,
		InputImageUrl: c.InputImageUrl.String, InputVideoUrl: c.InputVideoUrl.String, Model: c.Model,
		InputBgUrl: c.InputBgUrl.String, Duration: int32(c.Duration),
		PosterStyle: c.PosterStyle.String, Size: c.Size, ImageCount: int32(c.ImageCount),
		ImageUrls: parseList(c.ImageUrls), Status: int32(c.Status), FailReason: c.FailReason.String,
		CreateTime: c.CreateTime.UnixMilli(),
	}
}

func findOwned(ctx context.Context, svcCtx *svc.ServiceContext, id, userID int64) (*model.LocalLifeCreation, error) {
	c, err := svcCtx.Creation.FindOwned(ctx, id, userID)
	if errors.Is(err, model.ErrNotFound) {
		return nil, xerr.NotFoundf("记录")
	}
	return c, err
}

// requireAigc 执行面未配置时直接拒绝，不落记录。
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

// materials 校验素材：只接受本存储签发的上传地址，去重，不超过上限。
func materials(ctx context.Context, svcCtx *svc.ServiceContext, urls []string) ([]string, error) {
	out := make([]string, 0, len(urls))
	seen := map[string]bool{}
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		if err := storage.Owned(svcCtx.Storage, u); err != nil {
			return nil, xerr.Param(err.Error())
		}
		seen[u] = true
		out = append(out, u)
	}
	if max := svcCtx.Config.Limits.MaxMaterials; len(out) > max {
		return nil, xerr.Newf(xerr.ParamError, "素材最多 %d 张", max)
	}
	return out, nil
}

func list(ctx context.Context, svcCtx *svc.ServiceContext, in *pb.ListReq) (*pb.CreationList, error) {
	limit, offset := pagexOffset(in.Page, in.PageSize)
	rows, total, err := svcCtx.Creation.List(ctx, model.ListFilter{
		UserID: in.UserId, Kind: in.Kind, Media: in.Media, Status: int64(in.Status), Limit: limit, Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	resp := &pb.CreationList{List: make([]*pb.CreationInfo, 0, len(rows)), Total: total}
	for _, c := range rows {
		resp.List = append(resp.List, toInfo(c))
	}
	return resp, nil
}

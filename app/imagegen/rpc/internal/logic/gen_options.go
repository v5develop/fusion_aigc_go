package logic

import (
	"context"
	"strconv"
	"sync"
	"time"

	"aigc-local-life/app/imagegen/model"
	"aigc-local-life/app/imagegen/rpc/internal/svc"
	"aigc-local-life/app/imagegen/rpc/pb"

	"github.com/zeromicro/go-zero/core/logx"
)

// 生成选项组名，与 afv_local_life_gen_option.option_group 一一对应。
const (
	genPosterModels       = "poster_models"
	genPosterSizes        = "poster_sizes"
	genPosterStyles       = "poster_styles"
	genLongImageSizes     = "longimage_sizes"
	genActionModels       = "action_models"
	genVoiceoverModels    = "voiceover_models"
	genVoiceoverSizes     = "voiceover_sizes"
	genVoiceoverDurations = "voiceover_durations"
	genRemakeModels       = "remake_models"
	genVideoModels        = "video_models"
	genVideoSizes         = "video_sizes"
	genVideoDurations     = "video_durations"
)

// 缓存 30 秒：运营改完最多 30 秒生效；本进程内的改动立即失效（见 invalidateGenOptions）。
// 读库失败时不缓存，回落代码 / YAML 默认值，下一次请求再试。
const genCatalogTTL = 30 * time.Second

// genSnapshot 一次读库的结果：全部选项（含停用）与全部类型开关。
type genSnapshot struct {
	options []*model.GenOption
	kinds   []*model.GenKind
}

type genCatalog struct {
	mu     sync.Mutex
	loaded bool
	at     time.Time
	snap   *genSnapshot
}

var genOpts genCatalog

func genSnap(ctx context.Context, svcCtx *svc.ServiceContext) *genSnapshot {
	genOpts.mu.Lock()
	defer genOpts.mu.Unlock()
	if genOpts.loaded && time.Since(genOpts.at) < genCatalogTTL {
		return genOpts.snap
	}
	options, err := svcCtx.GenOpts.ListGenOptions(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("load gen options, fall back to code defaults: %v", err)
		return nil
	}
	kinds, err := svcCtx.GenOpts.ListGenKinds(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("load gen kinds, fall back to YAML: %v", err)
		return nil
	}
	genOpts.snap = &genSnapshot{options: options, kinds: kinds}
	genOpts.loaded, genOpts.at = true, time.Now()
	return genOpts.snap
}

// invalidateGenOptions 运营后台改动后调用：下一次读取直接回库。
func invalidateGenOptions() {
	genOpts.mu.Lock()
	defer genOpts.mu.Unlock()
	genOpts.loaded = false
}

// optionsOf 某组启用中的选项（按排序）；库里没有启用项时回落代码里的默认列表。
func optionsOf(ctx context.Context, svcCtx *svc.ServiceContext, group string, defaults []*pb.Option) []*pb.Option {
	var out []*pb.Option
	if s := genSnap(ctx, svcCtx); s != nil {
		for _, r := range s.options {
			if r.Group == group && r.Enabled {
				out = append(out, &pb.Option{Code: r.Code, Label: r.Label})
			}
		}
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

// validOption 编码是否在该组启用项内。海报风格的空编码（AI 自动匹配）恒合法。
func validOption(ctx context.Context, svcCtx *svc.ServiceContext, group string, defaults []*pb.Option, code string) bool {
	if group == genPosterStyles && code == "" {
		return true
	}
	return validModel(optionsOf(ctx, svcCtx, group, defaults), code)
}

// defaultOf 某组标记为默认且启用的编码；没有则回落 fallback（即代码 / YAML 原有的默认口径）。
func defaultOf(ctx context.Context, svcCtx *svc.ServiceContext, group, fallback string) string {
	if s := genSnap(ctx, svcCtx); s != nil {
		for _, r := range s.options {
			if r.Group == group && r.Enabled && r.IsDefault {
				return r.Code
			}
		}
	}
	return fallback
}

// durationsOf 某组启用的时长（秒）。编码是数字串，解析失败的行忽略。
func durationsOf(ctx context.Context, svcCtx *svc.ServiceContext, group string, defaults []int32) []int32 {
	var out []int32
	if s := genSnap(ctx, svcCtx); s != nil {
		for _, r := range s.options {
			if r.Group != group || !r.Enabled {
				continue
			}
			if n, err := strconv.Atoi(r.Code); err == nil {
				out = append(out, int32(n))
			}
		}
	}
	if len(out) == 0 {
		return defaults
	}
	return out
}

// defaultDurationOf 某组默认时长；没有配置默认则取该组第一个启用项，全部停用时回落 fallback。
func defaultDurationOf(ctx context.Context, svcCtx *svc.ServiceContext, group string, fallback int32) int32 {
	if s := genSnap(ctx, svcCtx); s != nil {
		for _, r := range s.options {
			if r.Group == group && r.Enabled && r.IsDefault {
				if n, err := strconv.Atoi(r.Code); err == nil {
					return int32(n)
				}
			}
		}
	}
	return fallback
}

func containsDuration(list []int32, d int32) bool {
	for _, v := range list {
		if v == d {
			return true
		}
	}
	return false
}

// sizeGroupOf 类型对应的比例组；没有比例组的类型（如动作迁移）返回空串。
func sizeGroupOf(kind string) string {
	switch kind {
	case model.KindPoster:
		return genPosterSizes
	case model.KindLongImage:
		return genLongImageSizes
	case model.KindVoiceover:
		return genVoiceoverSizes
	case model.KindExploreShop, model.KindShortDrama:
		return genVideoSizes
	}
	return ""
}

// defaultSizeOf 类型的默认比例：组内默认项优先，否则回落 YAML 的 DefaultSize。
func defaultSizeOf(ctx context.Context, svcCtx *svc.ServiceContext, kind, fallback string) string {
	group := sizeGroupOf(kind)
	if group == "" {
		return fallback
	}
	return defaultOf(ctx, svcCtx, group, fallback)
}

// templateRequired 类型是否必须选模板：运营开关优先，没有行则回落 YAML。
func templateRequired(ctx context.Context, svcCtx *svc.ServiceContext, kind string, fallback bool) bool {
	if s := genSnap(ctx, svcCtx); s != nil {
		for _, k := range s.kinds {
			if k.KindCode == kind {
				return k.TemplateRequired
			}
		}
	}
	return fallback
}

func genItem(r *model.GenOption) *pb.GenOptionItem {
	return &pb.GenOptionItem{
		Id: r.Id, Group: r.Group, Code: r.Code, Label: r.Label,
		SortOrder: r.SortOrder, Enabled: r.Enabled, IsDefault: r.IsDefault,
	}
}

func genKindItem(k *model.GenKind) *pb.GenKindItem {
	return &pb.GenKindItem{Kind: k.KindCode, TemplateRequired: k.TemplateRequired}
}

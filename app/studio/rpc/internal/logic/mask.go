package logic

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "image/jpeg" // 网关产物按 PNG 返回；防御性地也认 JPEG（无 alpha 时视为整图不透明）

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/repo"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 区域蒙版（局部校色）：ai-gateway image.segment 产出「只含该区域的透明 PNG」，这里取它的 alpha 生成
// 「白色 + 软 alpha」蒙版（背景 = 主体 alpha 取反），转存素材库桶（公开地址），按（规范化地址, 区域）缓存。
// 子任务的服装蒙版（成品置成功后异步补）与 POST /api/v1/studio/masks 共用这一套。
//
// 服装可细分部位（parts）：一次网关调用带 input.classes（= 阿里云 SegmentCloth ClothClass），产物 image 是恰好这些部位的
// 合并抠图、result.parts 是逐件抠图；合并蒙版按（地址, garment, 部位串）缓存，逐件蒙版另按（地址, garment, 单个部位）落缓存，
// 之后单选某件直接命中。不带 parts 与之前完全一致（不发 classes，幂等键 / 缓存键不变）。
//
// 成本：分割调用的网关成本由平台承担（不向用户收费、不走计费），记进 afv_studio_mask_cache.cost_cny 与用量日志。

// 蒙版区域。
const (
	MaskGarment    = "garment"    // 服装
	MaskProduct    = "product"    // 商品主体
	MaskPerson     = "person"     // 人物
	MaskBackground = "background" // 背景（主体之外）
)

// maskGroups 区域 → ai-gateway 模型组（网关种子 deploy/seed/ai-gateway-seed-diff-imageseg-sync.sql）。
// 背景用通用高清抠图（主体）再取反。
var maskGroups = map[string]string{
	MaskGarment:    "imgseg.aliyun-cloth",     // 阿里云 SegmentCloth
	MaskProduct:    "imgseg.aliyun-commodity", // 阿里云 SegmentCommodity
	MaskPerson:     "imgseg.aliyun-body",      // 阿里云 SegmentBody
	MaskBackground: "imgseg.aliyun-hd",        // 阿里云 SegmentHDCommonImage（取反）
}

// garmentParts 服装细分部位（= ai-gateway image.segment classes = 阿里云 SegmentCloth ClothClass）；顺序即固定顺序
// （缓存键 / 幂等键 / 返回顺序都按它）。上衣 tops / 外套 coat / 下装 pants / 裙装 skirt / 鞋 shoes / 包 bag / 帽 hat。
var garmentParts = []string{"tops", "coat", "pants", "skirt", "shoes", "bag", "hat"}

// tryonItemParts 商拍换装单品键 → 服装部位（成品自动蒙版只取用户选的单品）。配饰没法再细分，按包 + 帽取；
// 文胸 / 内裤分别归上衣 / 下装（供应商没有内衣类）。
var tryonItemParts = map[string][]string{
	"top": {"tops"}, "bottom": {"pants"}, "dress": {"skirt"}, "outer": {"coat"}, "shoes": {"shoes"},
	"accessory": {"bag", "hat"}, "bra": {"tops"}, "panties": {"pants"},
}

const (
	// maskMinCoverage 蒙版不透明（软）占比低于它 = 没识别到对应区域
	maskMinCoverage = 0.002
	// maskMissRetryAfter 「没识别到」的缓存多久后允许重新识别
	maskMissRetryAfter = 24 * time.Hour
	// maskTimeout 单次识别（网关调用 + 下载 + 转存）的总时限
	maskTimeout = 120 * time.Second
	// maskMaxSourceURL 规范化地址落库上限（列宽 2048）
	maskMaxSourceURL = 2048
)

// 用户可见文案。
const (
	msgMaskNoRegion    = "没识别到对应区域"
	msgMaskUnavailable = "区域识别暂不可用，请稍后再试"
)

var (
	errMaskNoRegion    = xerr.New(xerr.NotFound, msgMaskNoRegion)
	errMaskUnavailable = xerr.New(xerr.ThirdPartyError, msgMaskUnavailable)
)

// maskGateway / maskMedia 蒙版用到的网关与媒体服务能力；*aigateway.Client、*mediasvc.Client 满足。
type maskGateway interface {
	Submit(ctx context.Context, req aigateway.CallRequest, opt aigateway.SubmitOptions) (*aigateway.Call, error)
	Get(ctx context.Context, id string, wait time.Duration) (*aigateway.Call, error)
}

type maskMedia interface {
	FetchBytes(ctx context.Context, rid string) ([]byte, string, error)
	IngestBytes(ctx context.Context, name string, data []byte, purpose, idem string) (mediasvc.Resource, error)
	CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error)
}

var (
	_ maskGateway = (*aigateway.Client)(nil)
	_ maskMedia   = (*mediasvc.Client)(nil)
)

// masker 区域蒙版服务。gw / media 为 nil 表示未配置（Ready 报未开放）。
type masker struct {
	repo    *repo.MaskRepo
	gw      maskGateway
	media   maskMedia
	wait    time.Duration // 网关长轮询单次等待
	maxCost string        // limits.max_cost（人民币，空 = 不封顶）
	now     func() time.Time
}

// maskPart 一个部位的蒙版（也是 afv_studio_mask_cache.part_masks / afv_studio_task.mask_parts 的 JSON 元素）。
type maskPart struct {
	Part string `json:"part"`
	URL  string `json:"mask_url"`
}

// maskResult 识别结果。带部位时 URL 是这些部位的合并蒙版，Parts 是逐件蒙版（固定顺序，图里没有的部位缺省）。
type maskResult struct {
	URL    string
	Target string
	Cached bool
	Parts  []maskPart
}

// Ready 依赖是否就绪。
func (m *masker) Ready() bool { return m != nil && m.gw != nil && m.media != nil && m.repo != nil }

// normalizeMaskTarget 区域归一（小写、去空白）；不认识返回 false。
func normalizeMaskTarget(s string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(s))
	_, ok := maskGroups[t]
	return t, ok
}

// normalizeMaskParts 部位归一：小写去空白、去重、按 garmentParts 固定顺序；空 = 不细分（nil）。
// 只有 garment 支持细分；不认识的部位报参数错误。
func normalizeMaskParts(target string, parts []string) ([]string, error) {
	want := map[string]bool{}
	for _, p := range parts {
		k := strings.ToLower(strings.TrimSpace(p))
		if k == "" {
			continue
		}
		if !slices.Contains(garmentParts, k) {
			return nil, xerr.Param("不支持的服装部位：" + k)
		}
		want[k] = true
	}
	if len(want) == 0 {
		return nil, nil
	}
	if target != MaskGarment {
		return nil, xerr.Param("只有服装区域支持细分部位")
	}
	out := make([]string, 0, len(want))
	for _, p := range garmentParts {
		if want[p] {
			out = append(out, p)
		}
	}
	return out, nil
}

// availableMaskParts 区域支持的细分部位（前端据此决定是否显示部位选择）；只有 garment 有。
func availableMaskParts(target string) []string {
	if target != MaskGarment {
		return nil
	}
	return append([]string(nil), garmentParts...)
}

// tryonMaskParts 商拍换装批次 payload 里用户选的单品 → 服装部位（固定顺序）；非商拍 / 没有单品 / 都映射不上 → nil（全部服装）。
func tryonMaskParts(kind model.Kind, payload json.RawMessage) []string {
	if kind != model.KindTryon || len(payload) == 0 {
		return nil
	}
	var p struct {
		Items []string `json:"items"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	var parts []string
	for _, it := range p.Items {
		parts = append(parts, tryonItemParts[strings.ToLower(strings.TrimSpace(it))]...)
	}
	out, _ := normalizeMaskParts(MaskGarment, parts)
	return out
}

// normalizeMaskURL 缓存键用的规范化地址：去查询串与锚点（签名参数每次不同，对象相同），scheme / host 小写；
// 返回规范化地址与其 sha256 hex。不是 http(s) 绝对地址返回错误。
func normalizeMaskURL(raw string) (string, string, error) {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return "", "", errors.New("图片地址不合法")
	}
	p.Scheme, p.Host = strings.ToLower(p.Scheme), strings.ToLower(p.Host)
	// 签名参数每次不同要去掉；但 x-oss-process（图片处理样式，如水印 / 缩放）改变像素内容，必须留在键里，
	// 否则「带处理」与「原图」会共用同一份蒙版（尺寸 / 内容对不上）。
	proc := p.Query().Get("x-oss-process")
	p.RawQuery, p.ForceQuery, p.Fragment, p.RawFragment, p.User = "", false, "", "", nil
	if proc != "" {
		p.RawQuery = url.Values{"x-oss-process": {proc}}.Encode()
	}
	norm := p.String()
	sum := sha256.Sum256([]byte(norm))
	return norm, hex.EncodeToString(sum[:]), nil
}

// Get 取 imageURL 的 target 区域蒙版（parts 非空 = 只要这些服装部位，见 normalizeMaskParts）：先查缓存，没有再调网关识别、
// 生成蒙版、转存、落缓存。imageURL 的来源校验（本存储 / 素材库域名）由调用方做。错误：没识别到 → errMaskNoRegion（4001）；
// 网关 / 媒体服务失败 → errMaskUnavailable（6001）。
func (m *masker) Get(ctx context.Context, userID int64, imageURL, target string, parts []string) (maskResult, error) {
	log := logx.WithContext(ctx)
	target, ok := normalizeMaskTarget(target)
	if !ok {
		return maskResult{}, xerr.Param("区域只支持 garment / product / person / background")
	}
	parts, err := normalizeMaskParts(target, parts)
	if err != nil {
		return maskResult{}, err
	}
	partsKey := strings.Join(parts, ",")
	norm, key, err := normalizeMaskURL(imageURL)
	if err != nil {
		return maskResult{}, xerr.Param(err.Error())
	}
	if !m.Ready() {
		return maskResult{}, xerr.New(xerr.NotReady, "区域识别暂未开放，请稍后再试")
	}
	row, err := m.repo.Get(ctx, key, target, partsKey)
	if err != nil {
		log.Errorf("studio mask: 读缓存失败，直接识别 (check=mask_cache_read): %v", err)
	}
	if row != nil {
		switch {
		case row.Status == model.MaskOK && row.MaskURL.Valid && row.MaskURL.String != "":
			m.hit(ctx, row)
			log.Infow("studio mask usage", logx.Field("user_id", userID), logx.Field("target", target),
				logx.Field("parts", partsKey), logx.Field("cached", true), logx.Field("cost_cny", "0"))
			return maskResult{URL: row.MaskURL.String, Target: target, Cached: true, Parts: cachedParts(ctx, row, parts)}, nil
		case row.Status == model.MaskNoRegion && m.now().Sub(row.UpdatedAt) < maskMissRetryAfter:
			m.hit(ctx, row)
			log.Infow("studio mask: cached miss (check=mask_no_region_cached)", logx.Field("user_id", userID),
				logx.Field("target", target), logx.Field("parts", partsKey))
			return maskResult{}, errMaskNoRegion
		}
	}
	cctx, cancel := context.WithTimeout(ctx, maskTimeout)
	defer cancel()
	return m.compute(cctx, userID, imageURL, norm, key, target, parts)
}

// cachedParts 缓存行的逐件蒙版：多部位读 part_masks；单部位就是合并蒙版本身；不细分为 nil。
func cachedParts(ctx context.Context, row *model.MaskCache, parts []string) []maskPart {
	switch {
	case len(parts) == 1:
		return []maskPart{{Part: parts[0], URL: row.MaskURL.String}}
	case len(parts) > 1 && row.PartMasks.Valid && row.PartMasks.String != "":
		var out []maskPart
		if err := json.Unmarshal([]byte(row.PartMasks.String), &out); err != nil {
			logx.WithContext(ctx).Errorf("studio mask: 逐件蒙版 JSON 解析失败，按没有逐件结果返回 (check=mask_part_masks_json): %v", err)
			return nil
		}
		return out
	}
	return nil
}

func (m *masker) hit(ctx context.Context, row *model.MaskCache) {
	if err := m.repo.Hit(ctx, row.ID); err != nil {
		logx.WithContext(ctx).Errorf("studio mask: 命中计数失败 (check=mask_cache_hit): %v", err)
	}
}

func (m *masker) compute(ctx context.Context, userID int64, imageURL, norm, key, target string, parts []string) (maskResult, error) {
	log := logx.WithContext(ctx)
	group := maskGroups[target]
	partsKey := strings.Join(parts, ",")
	call, err := m.segment(ctx, imageURL, key, target, group, parts)
	if err != nil {
		log.Errorw("studio mask: gateway call failed (check=mask_gateway_call)", logx.Field("target", target),
			logx.Field("model_group", group), logx.Field("err", err.Error()))
		return maskResult{}, errMaskUnavailable
	}
	cost := pipeline.CostCNY(call.Cost)
	row := &model.MaskCache{
		URLKey: key, Target: target, Parts: partsKey, SourceURL: truncateRunes(norm, maskMaxSourceURL), ModelGroup: group,
		GatewayCallID: maskNull(call.ID), CostCNY: strconv.FormatFloat(cost, 'f', 6, 64), UserID: userID,
		Coverage: "0",
	}
	if !call.Succeeded() {
		class, code := "", ""
		if call.Failure != nil {
			class, code = call.Failure.Class, call.Failure.Code
		}
		log.Infow("studio mask usage", logx.Field("user_id", userID), logx.Field("target", target),
			logx.Field("cached", false), logx.Field("cost_cny", row.CostCNY), logx.Field("gateway_call_id", call.ID),
			logx.Field("status", call.Status))
		if class == "INPUT_REJECTED" {
			// 供应商判这张图做不了该区域的分割（如图里没有服装 / 人物）：按「没识别到」缓存，24 小时内不再重复调用
			log.Infow("studio mask: vendor rejected input (check=mask_input_rejected)", logx.Field("target", target),
				logx.Field("failure_code", code), logx.Field("gateway_call_id", call.ID))
			m.save(ctx, row, model.MaskNoRegion)
			return maskResult{}, errMaskNoRegion
		}
		log.Errorw("studio mask: gateway call not succeeded (check=mask_gateway_failed)", logx.Field("target", target),
			logx.Field("status", call.Status), logx.Field("failure_class", class), logx.Field("failure_code", code),
			logx.Field("gateway_call_id", call.ID))
		return maskResult{}, errMaskUnavailable
	}
	if call.Result == nil || call.Result.Image == nil || call.Result.Image.Rid == "" {
		log.Errorw("studio mask: segment result has no image (check=mask_result_image)", logx.Field("gateway_call_id", call.ID))
		return maskResult{}, errMaskUnavailable
	}
	data, _, err := m.media.FetchBytes(ctx, call.Result.Image.Rid)
	if err != nil {
		log.Errorw("studio mask: download cut-out failed (check=mask_fetch)", logx.Field("gateway_call_id", call.ID),
			logx.Field("err", err.Error()))
		return maskResult{}, errMaskUnavailable
	}
	out, cov, w, h, err := buildMask(data, target == MaskBackground)
	if err != nil {
		log.Errorw("studio mask: decode cut-out failed (check=mask_decode)", logx.Field("gateway_call_id", call.ID),
			logx.Field("err", err.Error()))
		return maskResult{}, errMaskUnavailable
	}
	row.Coverage, row.Width, row.Height = strconv.FormatFloat(cov, 'f', 6, 64), int32(w), int32(h)
	log.Infow("studio mask usage", logx.Field("user_id", userID), logx.Field("target", target),
		logx.Field("cached", false), logx.Field("cost_cny", row.CostCNY), logx.Field("gateway_call_id", call.ID),
		logx.Field("coverage", row.Coverage))
	if cov < maskMinCoverage {
		log.Infow("studio mask: region too small (check=mask_coverage)", logx.Field("target", target),
			logx.Field("coverage", row.Coverage), logx.Field("min", maskMinCoverage))
		m.save(ctx, row, model.MaskNoRegion)
		return maskResult{}, errMaskNoRegion
	}
	idem := maskIdem(target, partsKey, key)
	rid, maskURL, err := m.store(ctx, out, idem)
	if err != nil {
		log.Errorw("studio mask: store mask failed (check=mask_store)", logx.Field("target", target), logx.Field("err", err.Error()))
		return maskResult{}, errMaskUnavailable
	}
	row.MaskRid, row.MaskURL = maskNull(rid), maskNull(maskURL)
	res := maskResult{URL: maskURL, Target: target}
	var partRows []*model.MaskCache
	switch {
	case len(parts) == 1: // 单部位：合并蒙版就是这一件
		res.Parts = []maskPart{{Part: parts[0], URL: maskURL}}
	case len(parts) > 1:
		res.Parts, partRows = m.partMasks(ctx, call, row, parts, idem)
		if len(res.Parts) > 0 {
			b, _ := json.Marshal(res.Parts)
			row.PartMasks = maskNull(string(b))
		}
	}
	m.save(ctx, row, model.MaskOK)
	for _, pr := range partRows { // 逐件也落缓存：之后单选某一件直接命中，不再调网关
		m.save(ctx, pr, model.MaskOK)
	}
	return res, nil
}

// partMasks 多部位调用的逐件蒙版：逐个下载网关 result.parts、生成蒙版、转存；单件失败 / 太小只跳过这一件（不影响合并蒙版）。
// 返回逐件结果（固定顺序）与要落的单部位缓存行（成本只记在合并那一行，这里记 0）。
func (m *masker) partMasks(ctx context.Context, call *aigateway.Call, combined *model.MaskCache, parts []string,
	idem string) ([]maskPart, []*model.MaskCache) {
	log := logx.WithContext(ctx)
	byLabel := map[string]*aigateway.MediaOut{}
	for _, p := range call.Result.Parts {
		if l := strings.ToLower(strings.TrimSpace(p.Label)); p.Image != nil && p.Image.Rid != "" && byLabel[l] == nil {
			byLabel[l] = p.Image
		}
	}
	var out []maskPart
	var rows []*model.MaskCache
	for _, part := range parts {
		img := byLabel[part]
		if img == nil {
			log.Infow("studio mask: part not in segment result (check=mask_part_missing)", logx.Field("part", part),
				logx.Field("gateway_call_id", call.ID))
			continue
		}
		data, _, err := m.media.FetchBytes(ctx, img.Rid)
		if err != nil {
			log.Errorw("studio mask: download part cut-out failed (check=mask_part_fetch)", logx.Field("part", part),
				logx.Field("gateway_call_id", call.ID), logx.Field("err", err.Error()))
			continue
		}
		maskPNG, cov, w, h, err := buildMask(data, false)
		if err != nil {
			log.Errorw("studio mask: decode part cut-out failed (check=mask_part_decode)", logx.Field("part", part),
				logx.Field("gateway_call_id", call.ID), logx.Field("err", err.Error()))
			continue
		}
		if cov < maskMinCoverage {
			log.Infow("studio mask: part region too small (check=mask_part_coverage)", logx.Field("part", part),
				logx.Field("coverage", cov), logx.Field("min", maskMinCoverage))
			continue
		}
		rid, u, err := m.store(ctx, maskPNG, idem+":part:"+part)
		if err != nil {
			log.Errorw("studio mask: store part mask failed (check=mask_part_store)", logx.Field("part", part),
				logx.Field("err", err.Error()))
			continue
		}
		out = append(out, maskPart{Part: part, URL: u})
		rows = append(rows, &model.MaskCache{
			URLKey: combined.URLKey, Target: combined.Target, Parts: part, SourceURL: combined.SourceURL,
			ModelGroup: combined.ModelGroup, GatewayCallID: combined.GatewayCallID, CostCNY: "0", UserID: combined.UserID,
			MaskRid: maskNull(rid), MaskURL: maskNull(u), Coverage: strconv.FormatFloat(cov, 'f', 6, 64),
			Width: int32(w), Height: int32(h),
		})
	}
	return out, rows
}

// store 蒙版 PNG 入媒体服务（中间产物）再转存素材库桶，返回素材库 rid 与公开地址。
func (m *masker) store(ctx context.Context, data []byte, idem string) (string, string, error) {
	in, err := m.media.IngestBytes(ctx, "mask.png", data, mediasvc.PurposeIntermediate, idem)
	if err != nil {
		return "", "", err
	}
	lib, err := m.media.CopyToLibrary(ctx, in.RID, mediasvc.PurposeAssetLibrary, idem+":lib")
	if err != nil {
		return "", "", err
	}
	if lib.URL == "" {
		return "", "", errors.New("素材库桶没有公开地址")
	}
	return lib.RID, lib.URL, nil
}

// maskIdem 网关调用 / 转存的幂等键：不细分时与旧版逐字一致（studio-mask:{target}:{key}），细分时插入部位串。
func maskIdem(target, partsKey, key string) string {
	if partsKey == "" {
		return "studio-mask:" + target + ":" + key
	}
	return "studio-mask:" + target + ":" + partsKey + ":" + key
}

// segment 提交 image.segment 并长轮询到终态。幂等键按（区域, 部位, 规范化地址）确定：并发 / 重复请求在网关侧合并成同一次调用，
// 只付一次钱；失败过的调用用 RetryFailed 重开（网关只对 retry_safe 的失败开新一轮）。
func (m *masker) segment(ctx context.Context, imageURL, key, target, group string, parts []string) (*aigateway.Call, error) {
	input := map[string]any{"image": aigateway.MediaRef{URI: imageURL}}
	req := aigateway.CallRequest{
		Operation: "image.segment", ModelGroup: group,
		Input:        input,
		Labels:       map[string]string{"kind": "mask", "target": target},
		MediaPurpose: aigateway.MediaPurposeIntermediate,
	}
	partsKey := strings.Join(parts, ",")
	if len(parts) > 0 {
		input["classes"] = parts
		req.Labels["parts"] = partsKey
	}
	if dl, ok := ctx.Deadline(); ok {
		d := dl.UTC()
		req.Constraints = &aigateway.Constraints{DeadlineAt: &d}
	}
	if mc := strings.TrimSpace(m.maxCost); mc != "" {
		req.Limits = &aigateway.Limits{MaxCost: mc}
	}
	call, err := m.gw.Submit(ctx, req, aigateway.SubmitOptions{
		IdempotencyKey: maskIdem(target, partsKey, key), Wait: m.wait, RetryFailed: true,
	})
	for err == nil && !call.IsTerminal() {
		call, err = m.gw.Get(ctx, call.ID, m.wait)
	}
	if err != nil {
		return nil, err
	}
	return call, nil
}

func (m *masker) save(ctx context.Context, row *model.MaskCache, status int8) {
	row.Status = status
	if err := m.repo.Put(ctx, row); err != nil {
		logx.WithContext(ctx).Errorf("studio mask: 写缓存失败 (check=mask_cache_write): %v", err)
	}
}

// buildMask 网关产物（只含区域的透明 PNG）→「白色 + 软 alpha」PNG；invert=true 时 alpha 取反（背景）。
// 返回蒙版 PNG、软覆盖率（Σalpha / 255N）与尺寸。
func buildMask(data []byte, invert bool) ([]byte, float64, int, int, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, 0, 0, 0, err
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, 0, 0, 0, errors.New("空图")
	}
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	var sum uint64
	set := func(i int, a uint8) {
		if invert {
			a = 255 - a
		}
		dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = 255, 255, 255, a
		sum += uint64(a)
	}
	switch s := src.(type) {
	case *image.NRGBA:
		for y := 0; y < h; y++ {
			row := s.Pix[(y)*s.Stride : (y)*s.Stride+w*4]
			for x := 0; x < w; x++ {
				set((y*w+x)*4, row[x*4+3])
			}
		}
	case *image.RGBA:
		for y := 0; y < h; y++ {
			row := s.Pix[(y)*s.Stride : (y)*s.Stride+w*4]
			for x := 0; x < w; x++ {
				set((y*w+x)*4, row[x*4+3])
			}
		}
	default:
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				a := color.NRGBAModel.Convert(src.At(b.Min.X+x, b.Min.Y+y)).(color.NRGBA).A
				set((y*w+x)*4, a)
			}
		}
	}
	var buf bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestSpeed}).Encode(&buf, dst); err != nil {
		return nil, 0, 0, 0, fmt.Errorf("编码蒙版失败: %w", err)
	}
	return buf.Bytes(), float64(sum) / (255 * float64(w) * float64(h)), w, h, nil
}

// maskNull 空串 → NULL。
func maskNull(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}

// truncateRunes 按字节上限截断，不切开多字节字符。
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	for len(string(r)) > n {
		r = r[:len(r)-1]
	}
	return string(r)
}

// newMasker 从服务上下文组装蒙版服务；网关未启用或媒体服务未配置时 gw / media 留 nil（Ready=false）。
func newMasker(s *svc.ServiceContext) *masker {
	m := &masker{
		repo: s.Repo.Mask, maxCost: s.Config.AiGateway.MaxCostCNY, now: time.Now,
		wait: min(secs(s.Config.AiGateway.LongPollWait, 20*time.Second), 25*time.Second),
	}
	if s.Gateway != nil && s.Gateway.Enabled() {
		m.gw = s.Gateway
	}
	if s.MediaSvc != nil { // 别把 nil 指针装进接口
		m.media = s.MediaSvc
	}
	return m
}

// maskKinds 成品置成功后自动补服装蒙版的任务类型（商拍换装 / 姿态复刻的成品是穿着服装的人物）。
var maskKinds = map[model.Kind]bool{model.KindTryon: true, model.KindPose: true}

// attachMaskAsync 成品置成功后异步补服装蒙版：可选的后置步骤，不阻塞交付、失败不影响任务状态与计费（只记 WARN）。
// 识别走与 POST /api/v1/studio/masks 同一套缓存（按结果地址 + garment + 部位），重复触发不会重复付费。
// parts = 用户选的单品对应的部位（tryonMaskParts）：mask_url 是恰好这些单品的合并蒙版、mask_parts 存逐件；
// 带部位识别失败（服务不可用，如网关还不支持 classes）时退回不细分的整体服装蒙版，没识别到则不退（不拿别的衣服冒充）。
func (f *flow) attachMaskAsync(t *model.Task, userID int64, resultURL string, parts []string) {
	if f.mask == nil || !f.mask.Ready() || !maskKinds[t.Kind] || strings.TrimSpace(resultURL) == "" {
		return
	}
	id, taskID := t.ID, t.TaskID.String
	f.goClaimed(fmt.Sprintf("m:%d", id), func() {
		ctx, cancel := context.WithTimeout(context.Background(), maskTimeout+10*time.Second)
		defer cancel()
		log := logx.WithContext(ctx)
		// 免费档水印视图 mrw://：分割在原件上做（短期干净签名只交给网关，不落库、不下发）；签不出就跳过蒙版
		src := f.maskSourceURL(ctx, resultURL)
		if src == "" {
			log.Infow("studio mask: warning no source url for watermark view, mask skipped (check=task_mask_source)",
				logx.Field("task_id", taskID))
			return
		}
		res, err := f.mask.Get(ctx, userID, src, MaskGarment, parts)
		if len(parts) > 0 && err != nil && !errors.Is(err, errMaskNoRegion) {
			log.Infow("studio mask: warning per-item mask unavailable, fall back to whole garment (check=task_mask_parts_fallback)",
				logx.Field("task_id", taskID), logx.Field("parts", strings.Join(parts, ",")), logx.Field("reason", err.Error()))
			res, err = f.mask.Get(ctx, userID, src, MaskGarment, nil)
		}
		if errors.Is(err, errMaskNoRegion) {
			log.Infow("studio mask: warning task has no garment region, mask_url stays empty (check=task_mask_no_region)",
				logx.Field("task_id", taskID))
			return
		}
		if err != nil {
			log.Errorw("studio mask: warning task garment mask skipped (check=task_mask_attach)", logx.Field("task_id", taskID),
				logx.Field("reason", err.Error()))
			return
		}
		var partsJSON json.RawMessage
		if len(res.Parts) > 0 {
			partsJSON, _ = json.Marshal(res.Parts)
		}
		if _, err := f.repo.Task.SetMask(ctx, id, res.URL, partsJSON); err != nil {
			log.Errorw("studio mask: warning write task mask_url failed (check=task_mask_write)", logx.Field("task_id", taskID),
				logx.Field("err", err.Error()))
		}
	})
}

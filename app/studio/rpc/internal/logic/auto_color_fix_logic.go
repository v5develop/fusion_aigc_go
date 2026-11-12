package logic

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strings"
	"sync"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/pipeline"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/colorfixsolver"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AutoColorFixLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAutoColorFixLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AutoColorFixLogic {
	return &AutoColorFixLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 参考图驱动的自动校色（只增，2026-11-12；不落库，用户点「应用」才走 UpdateTaskColorFix）：按子任务（本人、成功的商拍换装）
// 取成品、各单品的服装参考图与逐件蒙版，交给 colorfix-solver 求五个滑杆初值。Endpoint 没配 → 7003 · COLORFIX_AUTO_DISABLED
func (l *AutoColorFixLogic) AutoColorFix(in *studio.AutoColorFixReq) (*studio.AutoColorFixResp, error) {
	return autoColorFix(l.ctx, newAutoFixDeps(l.svcCtx), in)
}

// 自动校色单件没解的原因（AutoColorFixSkipped.reason，只增）。
const (
	skipNoReference     = "no_reference"     // 该单品没有可用的服装参考图
	skipNoMask          = "no_mask"          // 成品里没识别到该单品区域
	skipMaskUnavailable = "mask_unavailable" // 区域识别暂不可用（可重试）
	skipUnsolved        = "unsolved"         // 求解端没给这件的结果
)

// autoFixMaxPieces 一次最多解几件（商拍单品上限 3，求解端上限 8，留余量）。
const autoFixMaxPieces = 8

// 用户可见文案。
const (
	msgAutoFixDisabled    = "自动校色暂未开放"
	msgAutoFixNoReference = "这张图没有可用的服装参考图，无法自动校色"
	msgAutoFixNoPiece     = "没能识别出可自动校色的服装区域，请手动校色"
	msgAutoFixBusy        = "自动校色服务繁忙，请稍后再试"
	msgAutoFixTimeout     = "自动校色超时，请稍后再试"
	msgAutoFixUnavailable = "自动校色暂不可用，请稍后再试"
)

// autoFixSolver 求解服务（*colorfixsolver.Client 满足）。
type autoFixSolver interface {
	Enabled() bool
	Solve(ctx context.Context, req colorfixsolver.Request) (*colorfixsolver.Response, error)
}

// autoFixDeps 自动校色的外部依赖，拆成函数字段是为了测试时不碰库 / 网关 / 媒体服务。
type autoFixDeps struct {
	solver autoFixSolver
	// loadTask 取本人未删除的子任务及其批次 payload；不存在 / 不是本人 / 已删除返回 (nil, nil, nil)
	loadTask func(ctx context.Context, userID int64, taskID string) (*model.Task, json.RawMessage, error)
	// source 成品原件：私有成品给 (rid, 现签干净地址)，公开成品给 public（见 masker.resolveTask）；没有成品 ok=false
	source func(ctx context.Context, userID int64, taskID string) (rid, src, public string, ok bool, err error)
	// pieceMask 该单品（parts）在成品里的蒙版地址（走区域蒙版缓存，未命中才调分割）
	pieceMask func(ctx context.Context, userID int64, rid, src, public string, parts []string) (string, error)
	// owns 参考图地址是否本存储签发 / 白名单前缀：载荷里的地址提交时校验过，这里再认一遍（不信任落库后的任何地址）
	owns func(string) bool
}

func newAutoFixDeps(s *svc.ServiceContext) *autoFixDeps {
	d := &autoFixDeps{owns: newDeps(s).owns}
	if s.ColorFixSolver != nil { // 别把 nil 指针装进接口
		d.solver = s.ColorFixSolver
	}
	m := newMasker(s)
	d.loadTask = func(ctx context.Context, userID int64, taskID string) (*model.Task, json.RawMessage, error) {
		t, err := s.Repo.Task.GetByTaskID(ctx, taskID)
		if err != nil {
			return nil, nil, err
		}
		if t == nil || t.UserID != userID || t.Deleted != 0 {
			return nil, nil, nil
		}
		b, err := s.Repo.Batch.GetByID(ctx, t.BatchID, userID)
		if err != nil {
			return nil, nil, err
		}
		if b == nil {
			return nil, nil, nil
		}
		return t, b.Payload, nil
	}
	d.source = func(ctx context.Context, userID int64, taskID string) (string, string, string, bool, error) {
		return m.resolveTask(ctx, userID, taskID, "")
	}
	d.pieceMask = func(ctx context.Context, userID int64, rid, src, public string, parts []string) (string, error) {
		var (
			res maskResult
			err error
		)
		if rid != "" {
			res, err = m.GetPrivate(ctx, userID, rid, src, MaskGarment, parts)
		} else {
			res, err = m.Get(ctx, userID, public, MaskGarment, parts)
		}
		return res.URL, err
	}
	return d
}

// autoFixItems 请求里的单品（码或中文名）→ 去重的单品码；不认识 / 整套 → 参数错误。
func autoFixItems(in []string) ([]string, error) {
	var out []string
	for _, raw := range in {
		k := pipeline.ItemOf(raw)
		if k == "" || k == pipeline.ItemLook {
			return nil, xerr.Param("不支持的单品：" + strings.TrimSpace(raw))
		}
		if !slices.Contains(out, k) {
			out = append(out, k)
		}
	}
	return out, nil
}

func autoColorFix(ctx context.Context, d *autoFixDeps, in *studio.AutoColorFixReq) (*studio.AutoColorFixResp, error) {
	log := logx.WithContext(ctx)
	if d.solver == nil || !d.solver.Enabled() {
		log.Infow("studio colorfix auto: solver not configured (check=colorfix_auto_enabled)", logx.Field("user_id", in.GetUserId()))
		return nil, xerr.NewReason(xerr.NotReady, xerr.ReasonColorFixAutoDisabled, msgAutoFixDisabled)
	}
	if in.GetUserId() <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少用户")
	}
	taskID := strings.TrimSpace(in.GetTaskId())
	if taskID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少任务号")
	}
	want, err := autoFixItems(in.GetPieces())
	if err != nil {
		return nil, err
	}
	t, payload, err := d.loadTask(ctx, in.GetUserId(), taskID)
	if err != nil {
		log.Errorw("studio colorfix auto: load task failed (check=colorfix_auto_task)", logx.Field("err", err.Error()))
		return nil, xerr.New(xerr.ServerError, "暂时无法读取任务，请稍后再试")
	}
	if t == nil {
		log.Infow("studio colorfix auto: task not owned / deleted (check=colorfix_auto_task_owned)",
			logx.Field("user_id", in.GetUserId()), logx.Field("task_id", taskID))
		return nil, xerr.NotFoundf("任务")
	}
	if t.Status != model.TaskSucceeded {
		return nil, xerr.NewReason(xerr.Conflict, xerr.ReasonColorFixAutoNotReady, "只有生成成功的图片可以自动校色")
	}
	if t.Kind != model.KindTryon {
		return nil, xerr.NewReason(xerr.ParamError, xerr.ReasonColorFixAutoNoReference, "只有商拍换装的成品可以按参考图自动校色")
	}
	refs := tryonPieceRefs(payload)
	if len(want) > 0 {
		refs = slices.DeleteFunc(refs, func(r pieceRef) bool { return !slices.Contains(want, r.Item) })
	}
	if len(refs) == 0 {
		return nil, xerr.NewReason(xerr.ParamError, xerr.ReasonColorFixAutoNoReference, msgAutoFixNoReference)
	}
	if len(refs) > autoFixMaxPieces {
		refs = refs[:autoFixMaxPieces]
	}
	rid, src, public, ok, err := d.source(ctx, in.GetUserId(), taskID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.NotFoundf("成品")
	}
	imageURL := public
	if rid != "" {
		imageURL = src
	}

	// 逐件取蒙版（并行；未命中缓存的才会调分割）
	type cand struct {
		ref  pieceRef
		mask string
	}
	var (
		mu      sync.Mutex
		wg      sync.WaitGroup
		cands   []cand
		skipped []*studio.AutoColorFixSkipped
		skipSet = func(key, reason string) {
			mu.Lock()
			skipped = append(skipped, &studio.AutoColorFixSkipped{Key: key, Reason: reason})
			mu.Unlock()
		}
	)
	for _, r := range refs {
		if r.URL == "" || !d.owns(r.URL) {
			log.Infow("studio colorfix auto: reference not usable (check=colorfix_auto_reference_owned)",
				logx.Field("task_id", taskID), logx.Field("piece", r.Item))
			skipSet(r.Item, skipNoReference)
			continue
		}
		wg.Add(1)
		go func(r pieceRef) {
			defer wg.Done()
			u, merr := d.maskFor(ctx, in.GetUserId(), t, rid, src, public, r.Item)
			switch {
			case merr == nil && u != "":
				mu.Lock()
				cands = append(cands, cand{r, u})
				mu.Unlock()
			case merr == nil || errors.Is(merr, errMaskNoRegion):
				skipSet(r.Item, skipNoMask)
			default:
				log.Infow("studio colorfix auto: piece mask unavailable (check=colorfix_auto_mask)", logx.Field("task_id", taskID),
					logx.Field("piece", r.Item), logx.Field("reason", merr.Error()))
				skipSet(r.Item, skipMaskUnavailable)
			}
		}(r)
	}
	wg.Wait()
	// 保持载荷里的单品顺序（并行收集会乱序；也是求解端「重叠时前者优先」的顺序）
	slices.SortFunc(cands, func(a, b cand) int { return indexOfRef(refs, a.ref.Item) - indexOfRef(refs, b.ref.Item) })
	slices.SortFunc(skipped, func(a, b *studio.AutoColorFixSkipped) int { return indexOfRef(refs, a.Key) - indexOfRef(refs, b.Key) })
	if len(cands) == 0 {
		return nil, noPieceError(skipped)
	}

	req := colorfixsolver.Request{ImageURL: imageURL}
	for _, c := range cands {
		req.Reference = append(req.Reference, colorfixsolver.Ref{Key: c.ref.Item, URL: c.ref.URL})
		req.Masks = append(req.Masks, colorfixsolver.Mask{Key: c.ref.Item, URL: c.mask})
	}
	resp, err := d.solver.Solve(ctx, req)
	if err != nil {
		return nil, solverError(ctx, taskID, err)
	}

	out := &studio.AutoColorFixResp{TaskId: taskID, Version: resp.Version, Skipped: skipped}
	got := map[string]colorfixsolver.Piece{}
	for _, p := range resp.Pieces {
		got[strings.ToLower(strings.TrimSpace(p.Key))] = p
	}
	for _, c := range cands {
		p, ok := got[c.ref.Item]
		params, valid := clampColorFix(p.Params)
		if !ok || !valid {
			log.Infow("studio colorfix auto: solver gave no usable result for piece (check=colorfix_auto_result)",
				logx.Field("task_id", taskID), logx.Field("piece", c.ref.Item), logx.Field("returned", ok))
			out.Skipped = append(out.Skipped, &studio.AutoColorFixSkipped{Key: c.ref.Item, Reason: skipUnsolved})
			continue
		}
		out.Pieces = append(out.Pieces, &studio.AutoColorFixPiece{Key: c.ref.Item, Params: params, Score: clampScore(p.Score)})
	}
	if len(out.Pieces) == 0 {
		return nil, noPieceError(out.Skipped)
	}
	log.Infow("studio colorfix auto usage", logx.Field("user_id", in.GetUserId()), logx.Field("task_id", taskID),
		logx.Field("solved", len(out.Pieces)), logx.Field("skipped", len(out.Skipped)), logx.Field("version", resp.Version))
	return out, nil
}

func indexOfRef(refs []pieceRef, item string) int {
	for i, r := range refs {
		if r.Item == item {
			return i
		}
	}
	return len(refs)
}

// maskFor 单品蒙版：单部位优先取子任务已存的逐件蒙版（自动补的，免费、不再识别），否则走区域蒙版（命中缓存即返回）。
func (d *autoFixDeps) maskFor(ctx context.Context, userID int64, t *model.Task, rid, src, public, item string) (string, error) {
	parts := tryonItemParts[item]
	if len(parts) == 0 {
		return "", nil
	}
	if len(parts) == 1 && t.MaskURL.Valid && len(t.MaskParts) > 0 {
		var stored []maskPart
		if json.Unmarshal(t.MaskParts, &stored) == nil {
			for _, p := range stored {
				if p.Part == parts[0] && p.URL != "" {
					return p.URL, nil
				}
			}
		}
	}
	return d.pieceMask(ctx, userID, rid, src, public, parts)
}

// noPieceError 一件都没法解时按原因选错误：全是没参考图 → 1003；有识别服务不可用的 → 6001（可重试）；其余 → 4001。
func noPieceError(skipped []*studio.AutoColorFixSkipped) error {
	allNoRef, anyUnavailable := len(skipped) > 0, false
	for _, s := range skipped {
		allNoRef = allNoRef && s.Reason == skipNoReference
		anyUnavailable = anyUnavailable || s.Reason == skipMaskUnavailable
	}
	switch {
	case allNoRef:
		return xerr.NewReason(xerr.ParamError, xerr.ReasonColorFixAutoNoReference, msgAutoFixNoReference)
	case anyUnavailable:
		return xerr.NewReason(xerr.ThirdPartyError, xerr.ReasonColorFixAutoUnavailable, msgAutoFixUnavailable)
	}
	return xerr.NewReason(xerr.NotFound, xerr.ReasonColorFixAutoNoPiece, msgAutoFixNoPiece)
}

// solverError 求解服务错误 → 业务错误；日志写明类别、HTTP 状态与求解端错误码（原文只进日志）。
func solverError(ctx context.Context, taskID string, err error) error {
	var se *colorfixsolver.Error
	status, code, msg := 0, "", err.Error()
	kind := colorfixsolver.KindUnavailable
	if errors.As(err, &se) {
		kind, status, code, msg = se.Kind, se.Status, se.Code, se.Message
	}
	logx.WithContext(ctx).Errorw("studio colorfix auto: solver call failed (check=colorfix_auto_solve)", logx.Field("task_id", taskID),
		logx.Field("kind", string(kind)), logx.Field("status", status), logx.Field("code", code), logx.Field("err", msg))
	switch kind {
	case colorfixsolver.KindBusy:
		return xerr.NewReason(xerr.ThirdPartyError, xerr.ReasonColorFixAutoBusy, msgAutoFixBusy)
	case colorfixsolver.KindTimeout:
		return xerr.NewReason(xerr.ThirdPartyError, xerr.ReasonColorFixAutoTimeout, msgAutoFixTimeout)
	case colorfixsolver.KindUnusable:
		return xerr.NewReason(xerr.NotFound, xerr.ReasonColorFixAutoNoPiece, msgAutoFixNoPiece)
	}
	return xerr.NewReason(xerr.ThirdPartyError, xerr.ReasonColorFixAutoUnavailable, msgAutoFixUnavailable)
}

// clampColorFix 求解端的五参数 → 取整并夹到 UpdateTaskColorFix 的取值范围；有 NaN / Inf 视为无效。
func clampColorFix(p colorfixsolver.Params) (*studio.ColorFixParams, bool) {
	vals := map[string]float64{"hue": p.Hue, "saturation": p.Saturation, "value": p.Value, "brightness": p.Brightness, "contrast": p.Contrast}
	out := map[string]int32{}
	for _, r := range colorFixRange {
		v := vals[r.key]
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false
		}
		out[r.key] = int32(math.Max(float64(r.min), math.Min(float64(r.max), math.Round(v))))
	}
	return &studio.ColorFixParams{Hue: out["hue"], Saturation: out["saturation"], Value: out["value"],
		Brightness: out["brightness"], Contrast: out["contrast"]}, true
}

// clampScore 分数夹到 0–100，保留一位小数；NaN 按 0。
func clampScore(s float64) float64 {
	if math.IsNaN(s) {
		return 0
	}
	return math.Round(math.Max(0, math.Min(100, s))*10) / 10
}

// pieceRef 一件单品的参考图。
type pieceRef struct {
	Item string
	URL  string
}

// refRank 参考图作主图的角度优先级：正面最优，平铺 / 未标次之，其它最后（与前端 tryonReferences 同口径）。
func refRank(angle string) int {
	switch pipeline.NormalizeAngle(angle) {
	case "front":
		return 0
	case "flat", "unknown":
		return 1
	}
	return 2
}

// tryonPieceRefs 批次 payload 的 items + garment_images → 每件单品一张主参考图（按 items 顺序；
// 图按 role 归属，某单品没有自己的图时退回「整套」图；都没有则 URL 为空）。解析失败 / 没有单品 → nil。
func tryonPieceRefs(payload json.RawMessage) []pieceRef {
	var p batchPayload
	if json.Unmarshal(payload, &p) != nil {
		return nil
	}
	var out []pieceRef
	seen := map[string]bool{}
	for _, raw := range p.Items {
		item := pipeline.ItemOf(raw)
		if item == "" || item == pipeline.ItemLook || seen[item] {
			continue
		}
		seen[item] = true
		best := func(match func(role string) bool) string {
			u, rank := "", 99
			for _, g := range p.GarmentImages {
				u2 := strings.TrimSpace(g.URL)
				if u2 == "" || !match(g.Role) {
					continue
				}
				if r := refRank(g.Angle); r < rank {
					u, rank = u2, r
				}
			}
			return u
		}
		u := best(func(role string) bool { return pipeline.ItemOf(role) == item })
		if u == "" {
			u = best(func(role string) bool { return pipeline.ItemOf(role) == pipeline.ItemLook })
		}
		out = append(out, pieceRef{Item: item, URL: u}) // 没有图的单品也留着（URL 空 → skipped no_reference）
	}
	return out
}

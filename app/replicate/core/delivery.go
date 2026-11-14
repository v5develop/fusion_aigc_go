package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/asset"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ---------------------------------------------------------------------------------------------
// 成片 / 预览版 / 已扣费的单段入素材库（P0-3，EXPERIENCE §3.8.3）：
//
//   - 落桶规则：素材库 → 公有桶。成片在媒体服务私有桶（intermediate），先 CopyToLibrary(purpose=asset_library) 得到公有地址，
//     再经 pkg/asset 写 afv_asset（共享表只经所属包写）。source_task_id = job 的任务中心编号（任务中心 assetGroupId 据此找素材）。
//   - 幂等：发件箱 afv_replicate_delivery 唯一键 (job_id, kind, seg_index) → 同 job 的成片只入队一次、只入库一次；
//     复制用固定幂等键（同一源同一公有对象），写库用 asset.InsertOnceByURL（同任务同地址已有行就不写，含用户已删的）。
//     入库后、标记完成前崩溃：重做时复制得到同一地址、写库查重命中，不会写出第二条。
//   - 不阻塞：入库与 job 收尾完全解耦——job 先到终态、先能下载；入库失败按指数退避重试（30 秒起、最长 30 分钟），
//     媒体服务 4xx 或超过 DeliveryMaxAttempts 次放弃（日志 ERROR 判据=DELIVERY_GAVE_UP，后台详情可见）。
//   - 认领：CAS 带一次性令牌（lease_token），认领有效期内其它实例不取；完成 / 退避都要求令牌还是自己的，被接手的旧执行者写入落空。
//   - 收尾自动入库（花了钱的产物必须进素材库）：job 到成功 / 部分成功（含后台强制收尾后的汇总）时，每个**已确认扣费**的段一条 segment、
//     成片一条 final（完整版 / 用户不继续或到期的部分成片），预览版（失败段占位）一条 preview（标题「部分完成」、properties.preview=true）；
//     之后重做成功拼出完整成片，完整版另入一条 final，预览版保留。已退点 / 不计费 / 扣费未确认的段不入；示例试看不建 job，不入。
// ---------------------------------------------------------------------------------------------

// DeliveryStore 入库发件箱（生产实现 app/replicate/repo，bun）。
type DeliveryStore interface {
	// EnqueueDelivery 按 (job_id, kind, seg_index) 插入（已存在不动），返回是否新建。
	EnqueueDelivery(ctx context.Context, d *model.Delivery) (bool, error)
	ListDeliveries(ctx context.Context, jobID int64) ([]*model.Delivery, error)
	// DueDeliveries 待入库且到期、认领空闲 / 过期的（next_run_at 升序，NULL 最先）。
	DueDeliveries(ctx context.Context, limit int) ([]int64, error)
	// ClaimDelivery CAS 认领：status=0 且认领空闲 / 过期 → 写令牌与到期、attempts+1。没抢到返回 (nil, nil)。
	ClaimDelivery(ctx context.Context, id int64, token string, ttl time.Duration) (*model.Delivery, error)
	// FinishDelivery CAS（WHERE id=? AND lease_token=?）写结果；retryAfter>0 时同时写 next_run_at = 库时钟 + retryAfter。
	FinishDelivery(ctx context.Context, id int64, token string, set map[string]any, retryAfter time.Duration) (bool, error)
	// RenewDelivery 续认领（WHERE id=? AND lease_token=? AND status=0）：写素材库前确认认领还是自己的，并顺延到期。
	RenewDelivery(ctx context.Context, id int64, token string, ttl time.Duration) (bool, error)
	// TouchTaskCenter 刷新 job 的任务中心同步水位（tc_updated_at；不动 updated_at）。
	TouchTaskCenter(ctx context.Context, jobID int64) error
}

// Librarian 复制到素材库公有桶（*mediasvc.Client 直接满足）。
type Librarian interface {
	CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error)
}

// AssetWriter 写素材库（*asset.Writer 直接满足）。
type AssetWriter interface {
	InsertOnceByURL(ctx context.Context, a asset.Asset) (bool, error)
}

// DeliveryConfig 入库参数。
type DeliveryConfig struct {
	EntryCode   string        // afv_asset.entry_code（默认 video_replicate）
	MaxAttempts int           // 放弃前最多尝试几次（默认 8）
	ClaimTTL    time.Duration // 认领有效期（默认 20 分钟：大于一次入库的最坏耗时 deliveryWorkTimeout，见 runDelivery）
}

// WithDelivery 装配入库（store / lib / assets 任一为 nil 则不入库，结果页显示「未开启」）。
func WithDelivery(store DeliveryStore, lib Librarian, assets AssetWriter, cfg DeliveryConfig) Option {
	return func(s *Service) {
		if store == nil || lib == nil || assets == nil {
			return
		}
		if cfg.EntryCode == "" {
			cfg.EntryCode = EntryCode
		}
		if cfg.MaxAttempts <= 0 {
			cfg.MaxAttempts = 8
		}
		if cfg.ClaimTTL < deliveryWorkTimeout+2*time.Minute {
			cfg.ClaimTTL = 20 * time.Minute // 认领有效期必须长于一次入库的最坏耗时，否则会被别的实例接手重复写库
		}
		s.delivery = &deliverer{store: store, lib: lib, assets: assets, cfg: cfg}
	}
}

type deliverer struct {
	store  DeliveryStore
	lib    Librarian
	assets AssetWriter
	cfg    DeliveryConfig
}

// LibraryView 素材库保存状态（结果页「已保存到素材库 · 去看」等，EXPERIENCE §4.9）。
type LibraryView struct {
	Status string `json:"status"` // saved / saving / failed / off
	Text   string `json:"text,omitempty"`
}

func libraryView(d *model.Delivery) *LibraryView {
	if d == nil {
		return nil
	}
	switch d.Status {
	case model.DeliveryDone:
		return &LibraryView{Status: "saved", Text: "已保存到素材库 · 去看"}
	case model.DeliveryGaveUp:
		return &LibraryView{Status: "failed", Text: "保存到素材库没有成功，你仍可下载"}
	}
	if d.Attempts > 0 && d.LastError != nil {
		return &LibraryView{Status: "saving", Text: "保存到素材库失败，已在重试。你仍可下载"}
	}
	return &LibraryView{Status: "saving", Text: "正在保存到素材库…"}
}

// paidSegments 已确认扣费的成功段：段号 → 该段最后一次成功步骤的产物。只认镜像「已扣除」（ChargeConfirmed）：
// 已退点 / 不计费（Billing off / shadow、0 点）/ 扣费还没确认的都不算——没收钱的产物不进素材库。
type paidSeg struct{ videoRid, coverRid string }

func (s *Service) paidSegments(ctx context.Context, jobID int64) (map[int]paidSeg, error) {
	steps, err := s.store.ListSteps(ctx, jobID)
	if err != nil {
		return nil, err
	}
	charges, err := s.store.ListCharges(ctx, jobID)
	if err != nil {
		return nil, err
	}
	out := map[int]paidSeg{}
	for _, st := range steps {
		if st.Kind != model.StepKindGateway || st.Status != model.StepSucceeded {
			continue
		}
		c := chargeOf(charges, st.BillingRequestID)
		if c == nil || c.Status != model.ChargeConfirmed {
			continue
		}
		var v struct {
			VideoRid string `json:"videoRid"`
			CoverRid string `json:"coverRid"`
		}
		if json.Unmarshal(st.Output, &v) == nil && v.VideoRid != "" {
			out[st.SegIndex] = paidSeg{videoRid: v.VideoRid, coverRid: v.CoverRid} // 按 id 升序：取最后一次成功（重做后的）
		}
	}
	return out, nil
}

// enqueueOnJobDone job 收尾（成功 / 部分成功，含后台强制收尾后的汇总）时入队素材库，幂等（唯一键 job+kind+段号，已有不动）：
//   - 每个已确认扣费的段一条 segment（不再依赖用户手动保存；手动保存命中同一行）；
//   - 成片可用时：完整版 / 部分成片一条 final，预览版（失败段占位）一条 preview。成片只在有已扣费的段时入（没收钱的不入）。
//
// 返回错误时调用方本轮不写合成结果，下一轮重做（同一依据同一 rid）再入队，不会出现「结果写好了、入库永远没入队」。没装配入库时什么都不做。
func (s *Service) enqueueOnJobDone(ctx context.Context, j *model.Job, doc *ResultDoc) error {
	if s.delivery == nil {
		return nil
	}
	paid, err := s.paidSegments(ctx, j.ID)
	if err != nil {
		return err
	}
	if len(paid) == 0 {
		return nil
	}
	// 按内容去重：同一个源 rid 只入一条（单段且无叠字的任务，成片就是那一段，同一 rid / 地址只入 final）
	have, haveRid := map[string]bool{}, map[string]bool{}
	if list, err := s.delivery.store.ListDeliveries(ctx, j.ID); err == nil {
		for _, d := range list {
			have[fmt.Sprintf("%s|%d", d.Kind, d.SegIndex)] = true
			haveRid[d.SrcRid] = true
		}
	}
	put := func(kind string, seg int, rid, cover string) error {
		if have[fmt.Sprintf("%s|%d", kind, seg)] || haveRid[rid] {
			return nil
		}
		d := &model.Delivery{JobID: j.ID, UserID: j.UserID, Kind: kind, SegIndex: seg, SrcRid: rid, Status: model.DeliveryPending}
		if cover != "" {
			d.CoverRid = ptr(cover)
		}
		created, err := s.delivery.store.EnqueueDelivery(ctx, d)
		if err == nil {
			have[fmt.Sprintf("%s|%d", kind, seg)], haveRid[rid] = true, true
			if created {
				logx.WithContext(ctx).Infof("replicate: 入素材库已入队 job=%d kind=%s seg=%d rid=%s", j.ID, kind, seg, rid)
			}
		}
		return err
	}
	// 成片先入：与某一段同 rid 时（单段无叠字）只保留 final，段跳过
	if doc != nil && doc.Status == ResultReady && doc.VideoRid != "" {
		kind := model.DeliveryFinal
		if doc.Kind == ResultPreview {
			kind = model.DeliveryPreview
		}
		if err := put(kind, 0, doc.VideoRid, doc.CoverRid); err != nil {
			return err
		}
	}
	for k := 1; k <= j.SegTotal; k++ {
		if p, ok := paid[k]; ok {
			if err := put(model.DeliverySegment, k, p.videoRid, p.coverRid); err != nil {
				return err
			}
		}
	}
	return nil
}

// SaveSegmentView POST /jobs/{id}/segments/{k}/save 的结果。
type SaveSegmentView struct {
	SegIndex int          `json:"segIndex"`
	Library  *LibraryView `json:"library"`
}

// SaveSegment 用户选择把某一段（已成功）保存到素材库：入队，幂等（同一段只入库一次，重复点返回当前状态）。
func (s *Service) SaveSegment(ctx context.Context, userID, jobID int64, k int) (*SaveSegmentView, error) {
	j, err := s.ownJob(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	if s.delivery == nil {
		return nil, xerr.New(xerr.NotReady, "保存到素材库暂未开放")
	}
	if k < 1 || k > j.SegTotal {
		return nil, xerr.New(xerr.NotFound, "没有这一段")
	}
	paid, err := s.paidSegments(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	p, ok := paid[k]
	// 已入库 / 已入队（收尾时自动入的）：直接返回已有。同内容（同 rid，如单段任务的成片就是这一段）算同一条
	find := func() (*SaveSegmentView, error) {
		list, err := s.delivery.store.ListDeliveries(ctx, j.ID)
		if err != nil {
			return nil, err
		}
		for _, x := range list {
			if (x.Kind == model.DeliverySegment && x.SegIndex == k) || (ok && x.SrcRid == p.videoRid) {
				return &SaveSegmentView{SegIndex: k, Library: libraryView(x)}, nil
			}
		}
		return nil, nil
	}
	if v, err := find(); err != nil || v != nil {
		return v, err
	}
	if !ok {
		steps, err := s.store.ListSteps(ctx, j.ID)
		if err != nil {
			return nil, err
		}
		for _, st := range segChain(steps, k) {
			if st.Status == model.StepSucceeded {
				// 出了片但没有确认扣费（不计费 / 已退点）：没收钱的产物不进素材库，仍可下载
				return nil, xerr.NewReason(xerr.Conflict, "SEGMENT_NOT_PAID", "这一段没有收费，不保存到素材库，你仍可下载")
			}
		}
		return nil, xerr.NewReason(xerr.Conflict, "SEGMENT_NOT_READY", "这一段还没做好，做好后再保存")
	}
	d := &model.Delivery{JobID: j.ID, UserID: j.UserID, Kind: model.DeliverySegment, SegIndex: k, SrcRid: p.videoRid, Status: model.DeliveryPending}
	if p.coverRid != "" {
		d.CoverRid = ptr(p.coverRid)
	}
	if _, err := s.delivery.store.EnqueueDelivery(ctx, d); err != nil {
		return nil, err
	}
	if v, err := find(); err != nil || v != nil {
		return v, err
	}
	return &SaveSegmentView{SegIndex: k, Library: &LibraryView{Status: "saving", Text: "正在保存到素材库…"}}, nil
}

func newToken() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// deliveryBackoff 第 n 次失败后的等待：30 秒 × 2^(n-1)，最长 30 分钟。
func deliveryBackoff(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// deliverySysTags 视频做同款入库的系统标签：来源功能、用途（成片 / 视频片段）、方案标题、清晰度，
// 分段加「第N段」，预览版（有失败段占位的成片）加「部分完成」。
func deliverySysTags(d *model.Delivery, in JobInput) []string {
	t := asset.SysTags{Feature: "视频做同款", Usage: asset.UsageVideoFinal, Template: trimRunes(in.Title, 29), Resolution: in.Resolution}
	switch d.Kind {
	case model.DeliverySegment:
		t.Usage = asset.UsageVideoSegment
		t.Extra = []string{asset.SegmentTag(d.SegIndex)}
	case model.DeliveryPreview:
		t.Extra = []string{asset.TagPartial}
	}
	return t.List()
}

// TickDeliveries 轮询器每轮处理到期的入库（各自认领，多实例安全）。
func (s *Service) TickDeliveries(ctx context.Context, limit int) {
	if s.delivery == nil {
		return
	}
	ids, err := s.delivery.store.DueDeliveries(ctx, limit)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 取待入库失败: %v", err)
		return
	}
	for _, id := range ids {
		s.runDelivery(ctx, id)
	}
}

func permanentMediaErr(err error) bool {
	var me *mediasvc.Error
	return errors.As(err, &me) && me.Permanent()
}

// deliveryWorkTimeout 一次入库（复制成片 + 封面 + 写库）的总时限；认领有效期 ClaimTTL 强制大于它 + 2 分钟余量，
// 媒体服务调用都在这个上下文里，超时就放弃本轮（退避重试），不会在认领过期后还去写素材库。
const deliveryWorkTimeout = 12 * time.Minute

func (s *Service) runDelivery(parent context.Context, id int64) {
	ctx, cancel := context.WithTimeout(parent, deliveryWorkTimeout)
	defer cancel()
	dl := s.delivery
	token := newToken()
	d, err := dl.store.ClaimDelivery(ctx, id, token, dl.cfg.ClaimTTL)
	if err != nil || d == nil {
		if err != nil {
			logx.WithContext(ctx).Errorf("replicate: 认领入库失败 delivery=%d: %v", id, err)
		}
		return
	}
	fail := func(err error, permanent bool) {
		msg := trimRunes(err.Error(), 250)
		if permanent || d.Attempts >= dl.cfg.MaxAttempts {
			logx.WithContext(ctx).Errorf("replicate: 入素材库放弃 判据=DELIVERY_GAVE_UP job=%d kind=%s seg=%d 次数=%d 永久=%v: %v",
				d.JobID, d.Kind, d.SegIndex, d.Attempts, permanent, err)
			_, _ = dl.store.FinishDelivery(ctx, d.ID, token, map[string]any{"status": model.DeliveryGaveUp, "last_error": msg,
				"finished_at": s.now(), "lease_token": nil, "lease_until": nil}, 0)
			return
		}
		wait := deliveryBackoff(d.Attempts)
		logx.WithContext(ctx).Infof("replicate: 入素材库暂时失败，%s 后重试 判据=DELIVERY_RETRY job=%d kind=%s seg=%d 第 %d 次: %v",
			wait, d.JobID, d.Kind, d.SegIndex, d.Attempts, err)
		_, _ = dl.store.FinishDelivery(ctx, d.ID, token, map[string]any{"last_error": msg, "lease_token": nil, "lease_until": nil}, wait)
	}
	j, err := s.store.GetJob(ctx, d.JobID)
	if err != nil {
		fail(err, false)
		return
	}
	if j == nil {
		fail(errors.New("job 不存在"), true)
		return
	}
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	idem := func(what string) string {
		return fmt.Sprintf("%srpl-lib:%d:%s:%d:%s", in.KeyPrefix, d.JobID, d.Kind, d.SegIndex, what)
	}
	lib, err := dl.lib.CopyToLibrary(ctx, d.SrcRid, mediasvc.PurposeAssetLibrary, idem("video"))
	if err == nil && strings.TrimSpace(lib.URL) == "" {
		err = &mediasvc.Error{Status: 422, Code: "no_public_url", Message: "素材库桶没有公开地址"}
	}
	if err != nil {
		fail(fmt.Errorf("复制成片到素材库失败: %w", err), permanentMediaErr(err))
		return
	}
	coverURL := ""
	if d.CoverRid != nil && *d.CoverRid != "" {
		c, cerr := dl.lib.CopyToLibrary(ctx, *d.CoverRid, mediasvc.PurposeAssetLibrary, idem("cover"))
		switch {
		case cerr == nil:
			coverURL = c.URL
		case permanentMediaErr(cerr):
			logx.WithContext(ctx).Infof("replicate: 封面复制永久失败，素材不带封面 job=%d: %v", d.JobID, cerr)
		default:
			fail(fmt.Errorf("复制封面到素材库失败: %w", cerr), false)
			return
		}
	}
	name := orText(trimRunes(in.Title, 100), "视频做同款")
	switch d.Kind {
	case model.DeliverySegment:
		name = trimRunes(fmt.Sprintf("%s · 第 %d 段", name, d.SegIndex), 128)
	case model.DeliveryPreview:
		name = trimRunes(name+" · 部分完成", 128)
	}
	// 写素材库前确认认领还在并续期（复制可能很慢）：被接手就不写，接手者会用同一幂等键复制、按地址查重写库
	if ok, err := dl.store.RenewDelivery(ctx, d.ID, token, dl.cfg.ClaimTTL); err != nil || !ok {
		logx.WithContext(ctx).Infof("replicate: 入库认领已失效，本轮不写素材库 判据=DELIVERY_CLAIM_LOST job=%d ok=%v err=%v", d.JobID, ok, err)
		return
	}
	props, _ := json.Marshal(map[string]any{"source": "replicate", "jobId": d.JobID, "planNo": j.PlanNo, "kind": d.Kind,
		"segIndex": d.SegIndex, "rid": d.SrcRid, "preview": d.Kind == model.DeliveryPreview})
	if _, err := dl.assets.InsertOnceByURL(ctx, asset.Asset{UserID: j.UserID, SourceTaskID: deref(j.TaskID), Type: "video",
		EntryCode: dl.cfg.EntryCode, Name: name, CoverURL: coverURL, ResultURL: lib.URL, AspectRatio: in.AspectRatio,
		Properties: string(props), SysTags: deliverySysTags(d, in)}); err != nil {
		fail(fmt.Errorf("写素材库失败: %w", err), false)
		return
	}
	set := map[string]any{"status": model.DeliveryDone, "video_url": lib.URL, "finished_at": s.now(), "last_error": nil,
		"lease_token": nil, "lease_until": nil}
	if coverURL != "" {
		set["cover_url"] = coverURL
	}
	ok, err := dl.store.FinishDelivery(ctx, d.ID, token, set, 0)
	if err != nil || !ok {
		// 认领已被接手（复制太慢）：接手者会用同一幂等键复制、查重写库，不会重复
		logx.WithContext(ctx).Infof("replicate: 入库完成写回没成功（认领已被接手或库错误，接手者幂等重做） job=%d ok=%v err=%v", d.JobID, ok, err)
		return
	}
	logx.WithContext(ctx).Infof("replicate: 已入素材库 job=%d kind=%s seg=%d", d.JobID, d.Kind, d.SegIndex)
	{ // 任务中心 result_url 取 完整成片 > 预览版 > 第 1 个已入库的段，任何一种入库完成都可能改变它
		if err := dl.store.TouchTaskCenter(ctx, d.JobID); err != nil {
			logx.WithContext(ctx).Errorf("replicate: 刷新任务中心水位失败（下次 job 变化时补上） job=%d: %v", d.JobID, err)
		}
	}
}

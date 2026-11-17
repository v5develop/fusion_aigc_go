package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 本文件：多比例导出（CONTRACT §N）。已交付的成片（完整版 / 部分版，不含预览版）改成另一个画幅（pad 模糊底 / crop 居中裁切），
// 音轨原样拷贝，免费、无同意卡。每个成片每种（比例, 模式）只做一次（幂等），结果自动入素材库（名称带比例后缀）。
//
// 入库走现有发件箱 afv_replicate_delivery：kind='ratio'、seg_index=0、version 按「代数 + 比例 + 模式」确定性编码：
//
//	version = 100 * 代数 + 10 * 比例序号 + 模式序号
//	比例序号：9:16=1  1:1=2  16:9=3  3:4=4  4:3=5；模式序号：pad=0  crop=1
//	代数：同一任务同一（比例, 模式）按导出行 id 升序数第几份（0 起）。成片被换掉（部分成片重做成功换成完整版）后，
//	新成片另做一份导出，代数 +1，version 不与旧的冲突；例：9:16 pad 首份=10，9:16 crop=11，4:3 pad=50，首份换成片后再做 9:16 pad=110。
//
// 唯一键 (job, ratio, mode, 源成片 rid)：同一成片同一（比例, 模式）并发调用只有一个真正去算，其余回放。

// 导出占坑回收时限：须大于一次改画幅的最坏耗时。
const exportClaimStale = 10 * time.Minute

// ReasonExportFailed 改画幅被媒体服务判定为永久失败（源片不满足条件等），重试没有用。
const ReasonExportFailed = "EXPORT_FAILED"

// ExportStore 多比例导出的占坑与读取（生产实现 app/replicate/repo；不装配时导出回 7003）。
type ExportStore interface {
	GetExport(ctx context.Context, jobID int64, ratio, mode, srcRid string) (*model.Export, error)
	// ListExports 任务的全部导出行（id 升序）。
	ListExports(ctx context.Context, jobID int64) ([]*model.Export, error)
	// ClaimExport 占坑（(job, ratio, mode, src_rid) 唯一）：占到返回 true 并回填 e.ID；已有完成的 / 别人正在做且未超过 staleAfter 返回 false。
	ClaimExport(ctx context.Context, e *model.Export, staleAfter time.Duration) (bool, error)
	FinishExport(ctx context.Context, id int64, videoRid string, version int) (bool, error)
	DeleteExportClaim(ctx context.Context, id int64) error
}

// Reframer 改画幅（*mediasvc.Client 满足，经 WithComposer 装配的合成器断言得到；合成器没实现它时导出回 7003）。
type Reframer interface {
	Reframe(ctx context.Context, rid, ratio, mode, idem string) (mediasvc.Resource, error)
}

// WithExport 装配多比例导出。
func WithExport(e ExportStore) Option { return func(s *Service) { s.exports = e } }

// ExportView 一份导出（agent 工具 replicate_export_ratio 的结果；Java 帧 entity {type:"export", id:"{jobId}:{ratio}:{mode}"}）。
type ExportView struct {
	JobID        int64        `json:"jobId"`
	Ratio        string       `json:"ratio"`
	Mode         string       `json:"mode"`
	VideoURL     string       `json:"videoUrl,omitempty"` // 现签短时地址（过期后重调工具 / 读 exports 即可）
	URLExpiresAt *time.Time   `json:"urlExpiresAt,omitempty"`
	Library      *LibraryView `json:"library,omitempty"`
	// Original true = 成片本来就是这个比例，返回的就是成片本身（没有新建导出）。add-only。
	Original bool `json:"original,omitempty"`
	// Replay true = 这份导出已经做过，这次直接返回。add-only。
	Replay bool `json:"replay,omitempty"`
}

// ExportList GET /api/v1/replicate/jobs/:id/exports：当前成片的已导出清单（id 升序；没有 / 成片未交付 = 空数组）。
type ExportList struct {
	JobID   int64        `json:"jobId"`
	Exports []ExportView `json:"exports"`
}

// ratioIndex 比例序号（1 起）；不支持返回 0。
func ratioIndex(r string) int {
	for i, x := range mediasvc.ReframeRatios {
		if x == r {
			return i + 1
		}
	}
	return 0
}

// exportVersion 发件箱 version 编码，见文件头。
func exportVersion(ratio, mode string, gen int) int {
	m := 0
	if mode == mediasvc.ReframeCrop {
		m = 1
	}
	return 100*gen + 10*ratioIndex(ratio) + m
}

// exportOfVersion 反解 version → 比例 / 模式（入库时拼名称、标签用）。
func exportOfVersion(v int) (ratio, mode string, ok bool) {
	i := (v % 100) / 10
	if i < 1 || i > len(mediasvc.ReframeRatios) {
		return "", "", false
	}
	mode = mediasvc.ReframePad
	if v%10 == 1 {
		mode = mediasvc.ReframeCrop
	}
	return mediasvc.ReframeRatios[i-1], mode, true
}

func (s *Service) reframer() Reframer {
	if s.composer == nil {
		return nil
	}
	r, _ := s.composer.(Reframer)
	return r
}

// sameRatio 画面宽高比是否已经等于目标画幅（容差 2%）。
func sameRatio(w, h int, ratio string) bool {
	var rw, rh int
	if _, err := fmt.Sscanf(ratio, "%d:%d", &rw, &rh); err != nil || w < 1 || h < 1 || rw < 1 || rh < 1 {
		return false
	}
	return math.Abs(float64(w)/float64(h)-float64(rw)/float64(rh)) <= 0.02*float64(rw)/float64(rh)
}

// ExportRatio 把已交付成片导出成目标画幅。mode 空 = pad。
func (s *Service) ExportRatio(ctx context.Context, userID, jobID int64, ratio, mode string) (*ExportView, error) {
	ratio, mode = strings.TrimSpace(ratio), strings.TrimSpace(mode)
	if ratioIndex(ratio) == 0 {
		return nil, xerr.Param("比例只能是 9:16、1:1、16:9、3:4、4:3")
	}
	if mode == "" {
		mode = mediasvc.ReframePad
	}
	if mode != mediasvc.ReframePad && mode != mediasvc.ReframeCrop {
		return nil, xerr.Param("模式只能是 pad（模糊底铺满）或 crop（居中裁切）")
	}
	rf := s.reframer()
	if s.exports == nil || rf == nil {
		return nil, xerr.New(xerr.NotReady, "多比例导出暂未开放")
	}
	j, doc, err := s.deliveredFinal(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	// 已做过：回放（同时补一次入库入队，幂等，自愈上次入队失败）
	if cur, err := s.exports.GetExport(ctx, j.ID, ratio, mode, doc.VideoRid); err != nil {
		return nil, err
	} else if cur != nil && cur.Status == model.ClaimDone {
		s.enqueueExportDelivery(ctx, j, cur)
		return s.exportView(ctx, j, cur, true), nil
	}
	// 与成片同比例：返回成片本身（不新建导出、不改画面）
	if s.media != nil {
		if p, perr := s.media.Probe(ctx, doc.VideoRid); perr == nil && sameRatio(p.Width, p.Height, ratio) {
			return s.originalExportView(ctx, j, doc, ratio, mode), nil
		}
	}
	e := &model.Export{UserID: userID, JobID: j.ID, Ratio: ratio, Mode: mode, SrcRid: doc.VideoRid}
	claimed, err := s.exports.ClaimExport(ctx, e, exportClaimStale)
	if err != nil {
		return nil, err
	}
	if !claimed {
		if cur, gerr := s.exports.GetExport(ctx, j.ID, ratio, mode, doc.VideoRid); gerr == nil && cur != nil && cur.Status == model.ClaimDone {
			s.enqueueExportDelivery(ctx, j, cur)
			return s.exportView(ctx, j, cur, true), nil
		}
		return nil, xerr.NewReason(xerr.Conflict, reasonInProgress, "这个比例正在导出，几十秒后再看一下")
	}
	var in JobInput
	_ = json.Unmarshal(j.Input, &in)
	h := sha256.Sum256([]byte(doc.VideoRid))
	idem := fmt.Sprintf("%srpl-ratio:%d:%s:%s:%s", in.KeyPrefix, j.ID, strings.ReplaceAll(ratio, ":", "x"), mode, hex.EncodeToString(h[:8]))
	res, rerr := rf.Reframe(ctx, doc.VideoRid, ratio, mode, idem)
	if rerr == nil && !mediasvc.ValidRID(res.RID) {
		rerr = fmt.Errorf("媒体服务返回的 rid 不合法")
	}
	if rerr != nil {
		if derr := s.exports.DeleteExportClaim(context.WithoutCancel(ctx), e.ID); derr != nil {
			logx.WithContext(ctx).Errorf("replicate: 导出占坑释放失败 判据=EXPORT_CLAIM_RELEASE_FAILED user=%d job=%d: %v", userID, j.ID, derr)
		}
		logx.WithContext(ctx).Errorf("replicate: 多比例导出失败 判据=EXPORT_REFRAME_FAILED user=%d job=%d ratio=%s mode=%s 永久=%v: %v",
			userID, j.ID, ratio, mode, permanentMediaErr(rerr), rerr)
		if permanentMediaErr(rerr) {
			return nil, xerr.NewReason(xerr.Conflict, ReasonExportFailed, "这条成片暂时导不出这个比例")
		}
		return nil, xerr.NewReason(xerr.ThirdPartyError, reasonThirdParty, "导出这次没做成，稍后再试一次")
	}
	version := exportVersion(ratio, mode, s.exportGeneration(ctx, j.ID, e))
	e.Status, e.VideoRid, e.Version = model.ClaimDone, res.RID, version
	if ok, ferr := s.exports.FinishExport(context.WithoutCancel(ctx), e.ID, res.RID, version); ferr != nil || !ok {
		// 导出做出来了但没落成库（占坑被接手 / 库故障）：本次仍把结果给用户；下次调用会重做（同幂等键，媒体服务回放同一 rid）
		logx.WithContext(ctx).Errorf("replicate: 导出落库失败 判据=EXPORT_FINISH_FAILED user=%d job=%d ok=%v: %v", userID, j.ID, ok, ferr)
	}
	s.enqueueExportDelivery(ctx, j, e)
	s.funnel(ctx, funnelEvent{Stage: StageExportRatio, UserID: userID, PlanID: j.PlanID, JobID: j.ID, Dedupe: fmt.Sprintf("ex:%d", e.ID)})
	return s.exportView(ctx, j, e, false), nil
}

// exportGeneration 同任务同（比例, 模式）里，id 比它小的导出行有几份 = 代数。
func (s *Service) exportGeneration(ctx context.Context, jobID int64, e *model.Export) int {
	list, err := s.exports.ListExports(ctx, jobID)
	if err != nil {
		logx.WithContext(ctx).Errorf("replicate: 取导出代数失败，按首份处理 判据=EXPORT_GEN_FAILED job=%d: %v", jobID, err)
		return 0
	}
	gen := 0
	for _, x := range list {
		if x.Ratio == e.Ratio && x.Mode == e.Mode && x.ID < e.ID {
			gen++
		}
	}
	return gen
}

// enqueueExportDelivery 把导出入素材库发件箱（幂等；没装配入库什么都不做；失败只打日志，下次调用补）。
func (s *Service) enqueueExportDelivery(ctx context.Context, j *model.Job, e *model.Export) {
	if s.delivery == nil || e.VideoRid == "" {
		return
	}
	d := &model.Delivery{JobID: j.ID, UserID: j.UserID, Kind: model.DeliveryRatio, SegIndex: 0, Version: e.Version, SrcRid: e.VideoRid, Status: model.DeliveryPending}
	if created, err := s.delivery.store.EnqueueDelivery(ctx, d); err != nil {
		logx.WithContext(ctx).Errorf("replicate: 导出入素材库入队失败（下次调用补） 判据=EXPORT_ENQUEUE_FAILED job=%d ratio=%s mode=%s: %v", j.ID, e.Ratio, e.Mode, err)
	} else if created {
		logx.WithContext(ctx).Infof("replicate: 导出入素材库已入队 job=%d ratio=%s mode=%s ver=%d rid=%s", j.ID, e.Ratio, e.Mode, e.Version, e.VideoRid)
	}
}

func (s *Service) exportLibrary(ctx context.Context, jobID int64, match func(*model.Delivery) bool) *LibraryView {
	if s.delivery == nil {
		return &LibraryView{Status: "off", Text: "保存到素材库暂未开放，你仍可下载"}
	}
	list, err := s.delivery.store.ListDeliveries(ctx, jobID)
	if err != nil {
		return &LibraryView{Status: "saving", Text: "正在保存到素材库…"}
	}
	for i := len(list) - 1; i >= 0; i-- {
		if match(list[i]) {
			return libraryView(list[i])
		}
	}
	return &LibraryView{Status: "saving", Text: "正在保存到素材库…"}
}

func (s *Service) exportView(ctx context.Context, j *model.Job, e *model.Export, replay bool) *ExportView {
	v := &ExportView{JobID: j.ID, Ratio: e.Ratio, Mode: e.Mode, Replay: replay}
	if urls, exp := s.signRIDs(ctx, []string{e.VideoRid}); urls != nil {
		v.VideoURL, v.URLExpiresAt = urls[e.VideoRid], exp
	}
	v.Library = s.exportLibrary(ctx, j.ID, func(d *model.Delivery) bool { return d.Kind == model.DeliveryRatio && d.Version == e.Version })
	return v
}

// originalExportView 成片本身已经是目标比例：返回成片的地址与它的素材库状态。
func (s *Service) originalExportView(ctx context.Context, j *model.Job, doc *ResultDoc, ratio, mode string) *ExportView {
	v := &ExportView{JobID: j.ID, Ratio: ratio, Mode: mode, Original: true}
	if urls, exp := s.signRIDs(ctx, []string{doc.VideoRid}); urls != nil {
		v.VideoURL, v.URLExpiresAt = urls[doc.VideoRid], exp
	}
	v.Library = s.exportLibrary(ctx, j.ID, func(d *model.Delivery) bool {
		return (d.Kind == model.DeliveryFinal) && d.SrcRid == doc.VideoRid
	})
	return v
}

// ListExports 当前成片的已导出清单（只看已完成、且源成片就是当前成片的；成片未交付返回空清单）。本人任务才能看。
func (s *Service) ListExports(ctx context.Context, userID, jobID int64) (*ExportList, error) {
	j, err := s.ownJob(ctx, userID, jobID)
	if err != nil {
		return nil, err
	}
	out := &ExportList{JobID: j.ID, Exports: []ExportView{}}
	if s.exports == nil {
		return out, nil
	}
	_, doc, err := s.deliveredFinal(ctx, userID, jobID)
	if err != nil {
		if xerr.ReasonOf(err) == ReasonJobNotDelivered {
			return out, nil
		}
		return nil, err
	}
	list, err := s.exports.ListExports(ctx, j.ID)
	if err != nil {
		return nil, err
	}
	for _, e := range list {
		if e.Status == model.ClaimDone && e.SrcRid == doc.VideoRid && e.VideoRid != "" {
			out.Exports = append(out.Exports, *s.exportView(ctx, j, e, false))
		}
	}
	return out, nil
}

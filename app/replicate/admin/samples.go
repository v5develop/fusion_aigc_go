package admin

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"aigc-local-life/app/replicate/core"
	"aigc-local-life/app/replicate/model"
	"aigc-local-life/app/replicate/repo"
	"aigc-local-life/app/replicate/rules/analysisfix"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 示例视频管理（/api/v1/replicate/admin/samples/**，docs/api/api-replicate.md §11）：增删改、触发预分析、看状态。
// 预分析 = 以系统身份（user_id=0）在 afv_replicate_analysis 插一行，由 gateway-api 的轮询器推进；admin 只插行、读状态，
// 读的时候惰性发布（core.SyncSample，与用户端同一套 CAS）。权限：读 replicate:view；写由 handler 另查 replicate:sample。

// ReasonSampleAnalyzing 上一轮预分析还在跑。
const ReasonSampleAnalyzing = "SAMPLE_ANALYZING"

// SampleStore 示例管理用到的读写（*repo.Repo 直接满足）。
type SampleStore interface {
	core.SampleSyncStore
	AdminListSamples(ctx context.Context, f repo.SampleFilter, offset, limit int) ([]*model.Sample, int64, error)
	InsertSample(ctx context.Context, s *model.Sample) error
	UpdateSample(ctx context.Context, id int64, set map[string]any, sourceChanged bool) (bool, error)
	DeleteSample(ctx context.Context, id, adminID int64) (bool, error)
	BeginSampleAnalysis(ctx context.Context, sampleID, adminID int64, build func(round int, sourceRid string) *model.Analysis) (*model.Analysis, bool, error)
	AnalysesByIDs(ctx context.Context, ids []int64) (map[int64]*model.Analysis, error)
}

var _ SampleStore = (*repo.Repo)(nil)

func (s *Service) sampleStore() (SampleStore, error) {
	ss, ok := s.store.(SampleStore)
	if !ok || ss == nil {
		return nil, xerr.New(xerr.NotReady, "功能暂未开放")
	}
	return ss, nil
}

// SampleRow 后台示例一行。
type SampleRow struct {
	ID                  int64             `json:"id"`
	Title               string            `json:"title"`
	Industry            string            `json:"industry"`
	SourceRid           string            `json:"sourceRid"`
	CoverRid            string            `json:"coverRid"`
	ResultRid           string            `json:"resultRid"`
	Annotations         *core.Annotations `json:"annotations"`
	SortOrder           int               `json:"sortOrder"`
	Enabled             bool              `json:"enabled"`
	AnalysisStatus      string            `json:"analysisStatus"` // none / queued / running / ready / failed
	AnalysisStatusLabel string            `json:"analysisStatusLabel"`
	AnalysisStage       string            `json:"analysisStage,omitempty"`
	AnalysisError       string            `json:"analysisError,omitempty"`
	AnalysisNo          string            `json:"analysisNo,omitempty"`
	PendingAnalysisNo   string            `json:"pendingAnalysisNo,omitempty"`
	Round               int               `json:"round"`
	DurationS           float64           `json:"durationS,omitempty"`
	Summary             string            `json:"summary,omitempty"`
	AnalyzedAt          *time.Time        `json:"analyzedAt,omitempty"`
	Published           bool              `json:"published"`
	UserVisible         bool              `json:"userVisible"`
	CreatedAt           time.Time         `json:"createdAt"`
	UpdatedAt           time.Time         `json:"updatedAt"`
	// Analysis 详情接口才带：已发布的分析结果（预览）
	Analysis *core.AnalysisResult `json:"analysis,omitempty"`
}

var sampleStatusLabel = map[string]string{"none": "未分析", "queued": "排队中", "running": "分析中", "ready": "已就绪", "failed": "分析失败"}

// SampleListReq 后台示例检索。
type SampleListReq struct {
	Industry string
	Enabled  *bool
	Page     int
	Size     int
}

// SampleList 后台示例列表。
type SampleList struct {
	Items []SampleRow `json:"items"`
	Total int64       `json:"total"`
	Page  int         `json:"page"`
	Size  int         `json:"size"`
}

// ListSamples 未删除的示例（先惰性发布已跑完的预分析）。
func (s *Service) ListSamples(ctx context.Context, req SampleListReq) (*SampleList, error) {
	ss, err := s.sampleStore()
	if err != nil {
		return nil, err
	}
	if req.Page <= 0 {
		req.Page = 1
	}
	if req.Size <= 0 {
		req.Size = 20
	}
	req.Size = min(req.Size, 100)
	list, total, err := ss.AdminListSamples(ctx, repo.SampleFilter{Industry: strings.TrimSpace(req.Industry), Enabled: req.Enabled}, (req.Page-1)*req.Size, req.Size)
	if err != nil {
		return nil, err
	}
	for i, smp := range list {
		if list[i], err = core.SyncSample(ctx, ss, smp); err != nil {
			return nil, err
		}
	}
	rows, err := s.sampleRows(ctx, ss, list, false)
	if err != nil {
		return nil, err
	}
	return &SampleList{Items: rows, Total: total, Page: req.Page, Size: req.Size}, nil
}

// GetSample 详情（带已发布的分析结果）。
func (s *Service) GetSample(ctx context.Context, id int64) (*SampleRow, error) {
	ss, err := s.sampleStore()
	if err != nil {
		return nil, err
	}
	smp, err := s.loadSample(ctx, ss, id)
	if err != nil {
		return nil, err
	}
	rows, err := s.sampleRows(ctx, ss, []*model.Sample{smp}, true)
	if err != nil {
		return nil, err
	}
	return &rows[0], nil
}

func (s *Service) loadSample(ctx context.Context, ss SampleStore, id int64) (*model.Sample, error) {
	smp, err := ss.GetSample(ctx, id)
	if err != nil {
		return nil, err
	}
	if smp == nil || smp.Deleted {
		return nil, xerr.New(xerr.NotFound, "找不到这个示例")
	}
	if smp, err = core.SyncSample(ctx, ss, smp); err != nil {
		return nil, err
	}
	if smp == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这个示例")
	}
	return smp, nil
}

// AnalysisResolver 按当前运行参数的纠偏规则读出分析结果（httpapi.AdminForcer 实现）。
type AnalysisResolver interface {
	ResolveAnalysis(ctx context.Context, a *model.Analysis) (*core.AnalysisResult, error)
}

// resolveAnalysis 读取时套用纠偏：forcer 能给当前规则配置就用它，否则按默认规则。
func (s *Service) resolveAnalysis(ctx context.Context, a *model.Analysis) (*core.AnalysisResult, error) {
	if r, ok := s.forcer.(AnalysisResolver); ok {
		return r.ResolveAnalysis(ctx, a)
	}
	return core.ResolveAnalysisWith(a, analysisfix.Config{})
}

func (s *Service) sampleRows(ctx context.Context, ss SampleStore, list []*model.Sample, withAnalysis bool) ([]SampleRow, error) {
	var ids []int64
	for _, smp := range list {
		if smp.AnalysisID != nil {
			ids = append(ids, *smp.AnalysisID)
		}
		if smp.PendingAnalysisID != nil {
			ids = append(ids, *smp.PendingAnalysisID)
		}
	}
	as, err := ss.AnalysesByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	rows := make([]SampleRow, 0, len(list))
	for _, smp := range list {
		row := SampleRow{ID: smp.ID, Title: smp.Title, Industry: smp.Industry, SourceRid: smp.SourceRid, CoverRid: deref(smp.CoverRid),
			ResultRid: deref(smp.ResultRid), SortOrder: smp.SortOrder, Enabled: smp.Enabled, AnalysisError: deref(smp.AnalysisError),
			Round: smp.AnalysisRound, AnalyzedAt: smp.AnalyzedAt, CreatedAt: smp.CreatedAt, UpdatedAt: smp.UpdatedAt}
		if len(smp.Annotations) > 0 {
			var ann core.Annotations
			if json.Unmarshal(smp.Annotations, &ann) == nil {
				row.Annotations = &ann
			}
		}
		var pub *model.Analysis
		if smp.AnalysisID != nil {
			pub = as[*smp.AnalysisID]
		}
		if pub != nil && pub.Status == model.AnalysisSucceeded {
			row.Published, row.AnalysisNo = true, pub.AnalysisNo
			if an, err := s.resolveAnalysis(ctx, pub); err == nil { // 读取时按当前规则纠偏，后台带 corrections / rulesVersion
				row.DurationS, row.Summary = an.DurationS, an.Summary
				if withAnalysis {
					row.Analysis = an
				}
			}
		}
		switch {
		case smp.PendingAnalysisID != nil && as[*smp.PendingAnalysisID] != nil:
			pa := as[*smp.PendingAnalysisID]
			row.PendingAnalysisNo, row.AnalysisStage = pa.AnalysisNo, pa.Stage
			row.AnalysisStatus = "running"
			if pa.Status == model.AnalysisQueued {
				row.AnalysisStatus = "queued"
			}
		case row.Published:
			row.AnalysisStatus = "ready"
		case row.AnalysisError != "":
			row.AnalysisStatus = "failed"
		default:
			row.AnalysisStatus = "none"
		}
		row.AnalysisStatusLabel = sampleStatusLabel[row.AnalysisStatus]
		row.UserVisible = row.Enabled && row.Published
		rows = append(rows, row)
	}
	return rows, nil
}

// SampleReq 新建 / 修改（修改为整体覆盖）。
type SampleReq struct {
	ID          int64 // 修改时
	AdminID     int64
	Title       string
	Industry    string
	SourceRid   string
	CoverRid    string
	ResultRid   string
	Annotations json.RawMessage
	SortOrder   int
	Enabled     bool
	Analyze     bool // 保存后立即预分析
}

type sampleFields struct {
	title, industry, source string
	cover, result           *string
	ann                     json.RawMessage
}

func checkSample(req SampleReq) (sampleFields, error) {
	var f sampleFields
	f.title = strings.TrimSpace(req.Title)
	if f.title == "" {
		return f, xerr.New(xerr.ParamMissing, "请填写标题")
	}
	if n := len([]rune(f.title)); n > 40 {
		return f, xerr.New(xerr.ParamError, "标题写短一点（40 字以内）")
	}
	f.industry = strings.TrimSpace(req.Industry)
	if len(f.industry) > 64 {
		return f, xerr.New(xerr.ParamError, "行业 code 太长了")
	}
	src, why := core.SampleRidOf(req.SourceRid)
	if src == "" {
		return f, xerr.New(xerr.ParamMissing, "请填写示例原片 rid")
	}
	if why != "" {
		logx.Infof("replicate: 后台示例拒收 判据=SAMPLE_RID_CATEGORY 字段=示例原片 原因=%s", why)
		return f, xerr.New(xerr.ParamError, "示例原片"+why)
	}
	f.source = src
	for _, x := range []struct {
		raw, label string
		dst        **string
	}{{req.CoverRid, "封面", &f.cover}, {req.ResultRid, "示例成片", &f.result}} {
		rid, why := core.SampleRidOf(x.raw)
		if why != "" {
			logx.Infof("replicate: 后台示例拒收 判据=SAMPLE_RID_CATEGORY 字段=%s 原因=%s", x.label, why)
			return f, xerr.New(xerr.ParamError, x.label+why)
		}
		if rid != "" {
			*x.dst = &rid
		}
	}
	ann, err := core.ValidSampleAnnotations(req.Annotations)
	if err != nil {
		return f, xerr.New(xerr.ParamError, err.Error())
	}
	if ann != nil {
		f.ann, _ = json.Marshal(ann)
	}
	return f, nil
}

// CreateSample 新建示例（Analyze=true 时立即开一轮预分析）。
func (s *Service) CreateSample(ctx context.Context, req SampleReq) (*SampleRow, error) {
	ss, err := s.sampleStore()
	if err != nil {
		return nil, err
	}
	f, err := checkSample(req)
	if err != nil {
		return nil, err
	}
	smp := &model.Sample{Title: f.title, Industry: f.industry, SourceRid: f.source, CoverRid: f.cover, ResultRid: f.result, Annotations: f.ann,
		SortOrder: req.SortOrder, Enabled: req.Enabled, CreatedBy: req.AdminID, UpdatedBy: req.AdminID}
	if err := ss.InsertSample(ctx, smp); err != nil {
		return nil, err
	}
	logx.WithContext(ctx).Infof("replicate: 后台新建示例 sample=%d admin=%d", smp.ID, req.AdminID)
	if req.Analyze {
		if _, err := s.AnalyzeSample(ctx, smp.ID, req.AdminID); err != nil {
			return nil, err
		}
	}
	return s.GetSample(ctx, smp.ID)
}

// UpdateSample 修改示例（整体覆盖；换了原片会清掉已发布的分析，须重新预分析）。
func (s *Service) UpdateSample(ctx context.Context, req SampleReq) (*SampleRow, error) {
	ss, err := s.sampleStore()
	if err != nil {
		return nil, err
	}
	f, err := checkSample(req)
	if err != nil {
		return nil, err
	}
	cur, err := s.loadSample(ctx, ss, req.ID)
	if err != nil {
		return nil, err
	}
	var ann any // nil → NULL
	if len(f.ann) > 0 {
		ann = json.RawMessage(f.ann)
	}
	set := map[string]any{"title": f.title, "industry": f.industry, "source_rid": f.source, "cover_rid": nullStr(f.cover), "result_rid": nullStr(f.result),
		"annotations": ann, "sort_order": req.SortOrder, "enabled": req.Enabled, "updated_by": req.AdminID}
	changed := cur.SourceRid != f.source
	ok, err := ss.UpdateSample(ctx, req.ID, set, changed)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, xerr.New(xerr.NotFound, "找不到这个示例")
	}
	logx.WithContext(ctx).Infof("replicate: 后台修改示例 sample=%d admin=%d 换原片=%v", req.ID, req.AdminID, changed)
	if req.Analyze {
		if _, err := s.AnalyzeSample(ctx, req.ID, req.AdminID); err != nil {
			return nil, err
		}
	}
	return s.GetSample(ctx, req.ID)
}

// DeleteSample 逻辑删除（已建方案引用的分析行不受影响）。
func (s *Service) DeleteSample(ctx context.Context, id, adminID int64) error {
	ss, err := s.sampleStore()
	if err != nil {
		return err
	}
	ok, err := ss.DeleteSample(ctx, id, adminID)
	if err != nil {
		return err
	}
	if !ok {
		return xerr.New(xerr.NotFound, "找不到这个示例")
	}
	logx.WithContext(ctx).Infof("replicate: 后台删除示例 sample=%d admin=%d", id, adminID)
	return nil
}

// AnalyzeSample 开一轮预分析（系统身份，不扣点、不计任何用户限频）。上一轮还在跑 → 409 SAMPLE_ANALYZING。
func (s *Service) AnalyzeSample(ctx context.Context, id, adminID int64) (*SampleRow, error) {
	ss, err := s.sampleStore()
	if err != nil {
		return nil, err
	}
	a, busy, err := ss.BeginSampleAnalysis(ctx, id, adminID, func(round int, rid string) *model.Analysis {
		return core.NewSampleAnalysis(id, round, rid)
	})
	if err != nil {
		return nil, err
	}
	if busy {
		return nil, xerr.NewReason(xerr.Conflict, ReasonSampleAnalyzing, "上一轮预分析还在跑，跑完再试")
	}
	if a == nil {
		return nil, xerr.New(xerr.NotFound, "找不到这个示例")
	}
	logx.WithContext(ctx).Infof("replicate: 后台触发示例预分析 sample=%d analysis=%s admin=%d", id, a.AnalysisNo, adminID)
	return s.GetSample(ctx, id)
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

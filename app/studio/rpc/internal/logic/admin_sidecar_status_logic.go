package logic

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/studio/model"
	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/aigateway"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminSidecarStatusLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminSidecarStatusLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminSidecarStatusLogic {
	return &AdminSidecarStatusLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// probeTimeout 每个依赖的探测时限。
const probeTimeout = 3 * time.Second

// AdminSidecarStatus 并发探测三个依赖（各 3s）：反推 sidecar GET /api/status（Key 是否已配）、
// ai-gateway POST /v1/catalog/readiness（生图三档模型组按 image.edit、润色组按 chat.completion 是否可路由；不可用时退回 model-groups 只验连通与签名）、
// 媒体服务 GET /healthz；外加生成中子任务数与 排队/切图/反推中 的批次数。
func (l *AdminSidecarStatusLogic) AdminSidecarStatus(in *studio.Empty) (*studio.SidecarStatusResp, error) {
	s := l.svcCtx
	probes := []func(ctx context.Context) *studio.DependencyStatus{l.probeSidecar, l.probeGateway, l.probeMedia}
	deps := make([]*studio.DependencyStatus, len(probes))
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func(i int, p func(ctx context.Context) *studio.DependencyStatus) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(l.ctx, probeTimeout)
			defer cancel()
			start := time.Now()
			d := p(ctx)
			d.LatencyMs = time.Since(start).Milliseconds()
			deps[i] = d
		}(i, p)
	}
	wg.Wait()
	out := &studio.SidecarStatusResp{Deps: deps}
	if n, err := s.Repo.Task.CountByStatus(l.ctx, model.TaskRunning); err == nil {
		out.RunningTasks = int32(n)
	} else {
		l.Errorf("studio: 统计生成中子任务失败: %v", err)
	}
	if n, err := s.Repo.Batch.CountByStatuses(l.ctx, model.BatchQueued, model.BatchCropping, model.BatchPrompting); err == nil {
		out.QueuedBatches = int32(n)
	} else {
		l.Errorf("studio: 统计排队批次失败: %v", err)
	}
	return out, nil
}

func (l *AdminSidecarStatusLogic) probeSidecar(ctx context.Context) *studio.DependencyStatus {
	c := l.svcCtx.Config.Reverse
	d := &studio.DependencyStatus{Name: "sidecar", BaseUrl: c.BaseURL}
	if l.svcCtx.Reverse == nil || !l.svcCtx.Reverse.Enabled() {
		d.Detail = "未配置 Reverse.BaseURL"
		return d
	}
	st, err := l.svcCtx.Reverse.Status(ctx)
	if err != nil {
		d.Detail = err.Error()
		return d
	}
	d.Version = st.Version
	d.Ok = st.LLMConfigured || st.Mock
	d.Detail = fmt.Sprintf("model=%s vision=%v llm_configured=%v mock=%v", st.Model, st.Vision, st.LLMConfigured, st.Mock)
	if !st.LLMConfigured && !st.Mock {
		d.Detail = "sidecar 未配置 ai-gateway（AIG_BASE_URL / AIG_SECRET）；" + d.Detail
	}
	return d
}

func (l *AdminSidecarStatusLogic) probeGateway(ctx context.Context) *studio.DependencyStatus {
	s := l.svcCtx
	d := &studio.DependencyStatus{Name: "ai-gateway", BaseUrl: s.Config.AiGateway.BaseURL}
	if s.Gateway == nil || !s.Gateway.Enabled() {
		d.Detail = "未配置 AiGateway.BaseURL"
		return d
	}
	// 生图组按 image.edit 查（商拍 / 姿态 / 带参考图的图片生成都走它），润色组按 chat.completion 查
	op := map[string]string{}
	for _, g := range s.Config.ModelGroupMap() {
		if g != "" {
			op[g] = "image.edit"
		}
	}
	for _, g := range []string{s.Config.Rewrite.ModelGroup, s.Config.Rewrite.VisionModelGroup} {
		if g != "" {
			op[g] = "chat.completion"
		}
	}
	groups := make([]string, 0, len(op))
	for g := range op {
		groups = append(groups, g)
	}
	sort.Strings(groups)
	queries := make([]aigateway.ReadinessQuery, len(groups))
	for i, g := range groups {
		queries[i] = aigateway.ReadinessQuery{ModelGroup: g, Operation: op[g]}
	}
	items, err := s.Gateway.ReadinessOp(ctx, queries)
	if err != nil {
		// readiness 不可用（老版本网关 / mock）：退回 model-groups，只验证连通与签名
		mgs, err2 := s.Gateway.ModelGroups(ctx)
		if err2 != nil {
			d.Detail = err.Error()
			return d
		}
		have := map[string]bool{}
		for _, m := range mgs {
			if m.Enabled {
				have[m.Code] = true
			}
		}
		var missing []string
		for _, g := range groups {
			if !have[g] {
				missing = append(missing, g)
			}
		}
		d.Ok = len(missing) == 0
		d.Detail = "readiness 不可用，按 model-groups 判断"
		if len(missing) > 0 {
			d.Detail += "；未启用：" + strings.Join(missing, ",")
		}
		return d
	}
	var bad []string
	for _, it := range items {
		if !it.Ready {
			bad = append(bad, it.ModelGroup+"/"+op[it.ModelGroup]+"("+it.Reason+")")
		}
	}
	d.Ok = len(bad) == 0 && len(items) == len(groups)
	if len(bad) > 0 {
		d.Detail = "未就绪：" + strings.Join(bad, ",")
	} else {
		d.Detail = "就绪：" + strings.Join(groups, ",")
	}
	return d
}

func (l *AdminSidecarStatusLogic) probeMedia(ctx context.Context) *studio.DependencyStatus {
	s := l.svcCtx
	d := &studio.DependencyStatus{Name: "media", BaseUrl: s.Config.Media.BaseURL}
	if s.MediaSvc == nil {
		d.Detail = "未配置媒体服务"
		return d
	}
	bases := s.MediaSvc.Instances()
	if len(bases) == 0 {
		d.Detail = "没有可用的媒体服务实例"
		return d
	}
	d.BaseUrl = bases[0]
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(bases[0], "/")+"/healthz", nil)
	if err != nil {
		d.Detail = err.Error()
		return d
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		d.Detail = err.Error()
		return d
	}
	resp.Body.Close()
	d.Ok = resp.StatusCode == http.StatusOK
	d.Detail = fmt.Sprintf("GET /healthz → %d，实例 %d 个", resp.StatusCode, len(bases))
	return d
}

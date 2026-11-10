package logic

import (
	"context"
	"math/big"
	"time"

	"aigc-local-life/app/studio/rpc/internal/svc"
	"aigc-local-life/app/studio/rpc/studio"
	"aigc-local-life/pkg/auditdb"
	"aigc-local-life/pkg/modelgroup"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type AdminCostSummaryLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAdminCostSummaryLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AdminCostSummaryLogic {
	return &AdminCostSummaryLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// costSummaryDefaultDays 未给时间范围时默认最近 30 天。
const costSummaryDefaultDays = 30

// AdminCostSummary 成本汇总（只读，sqlc pkg/auditdb）：group_by=kind（默认）/ model_group。
// 时间按子任务创建时间左闭右开；points 汇总查询不含预留单，这里恒为 0（单用户点数见 StudioUserSpend）。
func (l *AdminCostSummaryLogic) AdminCostSummary(in *studio.AdminCostSummaryReq) (*studio.AdminCostSummaryResp, error) {
	switch in.GroupBy {
	case "", "kind", "model_group":
	default:
		return nil, xerr.Param("分组只支持 kind / model_group")
	}
	if r := in.DateRange; r != nil && r.Start > 0 && r.End > 0 && r.Start >= r.End {
		return nil, xerr.Param("时间范围不合法")
	}
	if l.svcCtx.Audit == nil {
		return nil, xerr.New(xerr.ServerError, "审计查询未初始化")
	}
	now := time.Now()
	from, to := now.AddDate(0, 0, -costSummaryDefaultDays), now.Add(24*time.Hour)
	if r := in.DateRange; r != nil {
		if r.Start > 0 {
			from = time.UnixMilli(r.Start)
		}
		if r.End > 0 {
			to = time.UnixMilli(r.End)
		}
	}
	out := &studio.AdminCostSummaryResp{Rows: []*studio.CostRow{}, Total: &studio.CostRow{Key: "total"}}
	total := new(big.Rat)
	add := func(key string, n, ok, failed int64, cost string) {
		out.Rows = append(out.Rows, &studio.CostRow{Key: key, TaskTotal: int32(n), TaskSucceeded: int32(ok), TaskFailed: int32(failed), CostCny: cost})
		out.Total.TaskTotal += int32(n)
		out.Total.TaskSucceeded += int32(ok)
		out.Total.TaskFailed += int32(failed)
		if v, ok := new(big.Rat).SetString(cost); ok {
			total.Add(total, v)
		}
	}
	q := l.svcCtx.Audit
	if in.GroupBy == "model_group" {
		rows, err := q.StudioCostSummaryByModelGroup(l.ctx, auditdb.StudioCostSummaryByModelGroupParams{DateFrom: from, DateTo: to})
		if err != nil {
			return nil, err
		}
		// 换代前任务上的旧 Dev 组码与规范组是同一组（pkg/modelgroup）：按规范组合并成一行
		type acc struct {
			n, ok, failed int64
			cost          []string
		}
		var keys []string
		merged := map[string]*acc{}
		for _, r := range rows {
			k := modelgroup.Canonical(r.ModelGroup)
			a := merged[k]
			if a == nil {
				a = &acc{}
				merged[k] = a
				keys = append(keys, k)
			}
			a.n, a.ok, a.failed = a.n+r.TaskCount, a.ok+r.SucceededCount, a.failed+r.FailedCount
			a.cost = append(a.cost, r.CostCnyTotal)
		}
		for _, k := range keys {
			a := merged[k]
			cost := a.cost[0]
			if len(a.cost) > 1 {
				sum := new(big.Rat)
				for _, c := range a.cost {
					if v, ok := new(big.Rat).SetString(c); ok {
						sum.Add(sum, v)
					}
				}
				cost = sum.FloatString(6)
			}
			add(k, a.n, a.ok, a.failed, cost)
		}
	} else {
		rows, err := q.StudioCostSummaryByKind(l.ctx, auditdb.StudioCostSummaryByKindParams{DateFrom: from, DateTo: to})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			add(string(r.Kind), r.TaskCount, r.SucceededCount, r.FailedCount, r.CostCnyTotal)
		}
	}
	out.Total.CostCny = total.FloatString(6)
	return out, nil
}

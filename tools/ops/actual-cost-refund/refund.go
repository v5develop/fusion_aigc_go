package main

import (
	"context"
	"fmt"
	"io"
	"math/big"
	"sort"
	"strings"
	"time"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
)

// candidate 一笔由 Go（画布 / 商拍）冻结、可能按最坏上限多扣了的结算：一个计费请求号 + 它下面的 gateway 调用。
type candidate struct {
	Source    string // canvas / studio
	RefID     int64  // afv_canvas_node_run.id / afv_studio_task.id
	UserID    int64
	RequestID string   // billing_request_id
	CallIDs   []string // 这笔预留下的全部 gateway 调用
	CreatedAt time.Time
}

// biller 补退用到的计费操作（*billing.Service 满足），抽出来便于用夹具单测。
type biller interface {
	LoadReservation(ctx context.Context, userID int64, requestID string) (*billing.ReservationBrief, error)
	CarrierOf(ctx context.Context, b *billing.ReservationBrief) (string, func(usd *big.Rat) int64)
	ConsumedPoints(ctx context.Context, b *billing.ReservationBrief) (consumed, refundedBefore int64, err error)
	RefundOvercharge(ctx context.Context, reservationNo string, target int64, reason string) (int64, error)
}

// callGetter 批量查 gateway 调用（*aigateway.Client 满足）。
type callGetter interface {
	BatchGet(ctx context.Context, ids []string) ([]aigateway.Call, error)
}

// outcome 每笔的处理结论（汇总按它计数）。
const (
	outNoReservation = "no_reservation"   // 找不到预留单（未计费 / 计费关闭时提交的）
	outNotConfirmed  = "not_confirmed"    // 预留单不是 CONFIRMED / PARTIALLY_REFUNDED（退了、过期了、还在 HELD）
	outNotGo         = "not_go"           // 不是 Go 冻结的单（Java 的单归 Java 的历史补退）
	outFixed         = "fixed"            // 载体不是 ACTUAL_COST（不在本次范围）
	outNoCost        = "no_reported_cost" // gateway 没有实报终值（先跑网关回填脚本）
	outAlready       = "already_refunded" // 已补退过（幂等）
	outFair          = "fair"             // 实扣 ≤ 公平点数，不用退
	outRefund        = "refund"           // dry-run：将退；apply：已退
	outError         = "error"            // 查询 / 退款出错
)

type row struct {
	c        candidate
	outcome  string
	no       string
	reserved int64
	consumed int64
	fair     int64
	refund   int64
	usd      string
	err      error
}

type summary struct {
	Scanned      int
	ByOutcome    map[string]int
	RefundPoints int64 // dry-run：将退点数合计；apply：实退点数合计
	Rows         []row
}

// run 补退主流程。apply=false 只算不写（默认）。
func run(ctx context.Context, b biller, gw callGetter, cands []candidate, apply bool, out io.Writer) summary {
	s := summary{ByOutcome: map[string]int{}}
	// 先把要查的调用批量取回（≤ 200 一批）
	calls := fetchCalls(ctx, gw, cands, out)
	cands = mergeByReservation(cands)
	for _, c := range cands {
		s.Scanned++
		r := row{c: c}
		evaluate(ctx, b, calls, &r, apply)
		s.ByOutcome[r.outcome]++
		if r.outcome == outRefund {
			s.RefundPoints += r.refund
		}
		s.Rows = append(s.Rows, r)
		printRow(out, r, apply)
	}
	return s
}

func evaluate(ctx context.Context, b biller, calls map[string]*aigateway.Call, r *row, apply bool) {
	br, err := b.LoadReservation(ctx, r.c.UserID, r.c.RequestID)
	if err != nil {
		r.outcome, r.err = outError, err
		return
	}
	if br == nil {
		r.outcome = outNoReservation
		return
	}
	r.no, r.reserved = br.No, br.PricePoints
	if !br.GoCreated {
		r.outcome = outNotGo
		return
	}
	if br.Status != "CONFIRMED" && br.Status != "PARTIALLY_REFUNDED" {
		r.outcome = outNotConfirmed
		return
	}
	mode, fairOf := b.CarrierOf(ctx, br)
	if mode != billing.SettleActualCost {
		r.outcome = outFixed
		return
	}
	consumed, before, err := b.ConsumedPoints(ctx, br)
	if err != nil {
		r.outcome, r.err = outError, err
		return
	}
	r.consumed = consumed
	if before > 0 {
		r.outcome, r.refund = outAlready, before
		return
	}
	var cs []*aigateway.Call
	for _, id := range r.c.CallIDs {
		cs = append(cs, calls[id]) // 没查到的是 nil → ReportedUSD 返回不可用
	}
	usd, ok := aigateway.ReportedUSD(cs)
	if !ok {
		r.outcome = outNoCost
		return
	}
	r.usd = usd.FloatString(6)
	r.fair = fairOf(usd)
	if consumed <= r.fair {
		r.outcome = outFair
		return
	}
	r.refund = consumed - r.fair
	if !apply {
		r.outcome = outRefund
		return
	}
	reason := fmt.Sprintf("按实际用量补退：实报成本 $%s，应扣 %d 点，原扣 %d 点", r.usd, r.fair, consumed)
	got, err := b.RefundOvercharge(ctx, br.No, r.fair, reason)
	switch {
	case err != nil:
		r.outcome, r.err = outError, err
	case got == 0:
		r.outcome = outAlready // 并发 / 重复执行：别处已补退
	default:
		r.outcome, r.refund = outRefund, got
	}
}

// mergeByReservation 多条记录共用一个计费请求号（同一张预留单）时合成一笔，调用号取并集：
// 成本必须覆盖这张单下的全部调用，也不能把同一张单算两次「将退」。保留首次出现的顺序与来源。
func mergeByReservation(cands []candidate) []candidate {
	idx := map[string]int{}
	out := make([]candidate, 0, len(cands))
	for _, c := range cands {
		k := billing.BizID(c.UserID, c.RequestID)
		i, ok := idx[k]
		if !ok {
			idx[k] = len(out)
			c.CallIDs = append([]string(nil), c.CallIDs...)
			out = append(out, c)
			continue
		}
		have := map[string]bool{}
		for _, id := range out[i].CallIDs {
			have[id] = true
		}
		for _, id := range c.CallIDs {
			if !have[id] {
				out[i].CallIDs = append(out[i].CallIDs, id)
				have[id] = true
			}
		}
	}
	return out
}

func fetchCalls(ctx context.Context, gw callGetter, cands []candidate, out io.Writer) map[string]*aigateway.Call {
	seen := map[string]bool{}
	var ids []string
	for _, c := range cands {
		for _, id := range c.CallIDs {
			if id != "" && !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}
	got := make(map[string]*aigateway.Call, len(ids))
	if gw == nil {
		return got
	}
	for i := 0; i < len(ids); i += aigateway.MaxBatchGet {
		chunk := ids[i:min(len(ids), i+aigateway.MaxBatchGet)]
		list, err := gw.BatchGet(ctx, chunk)
		if err != nil {
			fmt.Fprintf(out, "WARN gateway batchGet 失败（这 %d 个调用按无实报成本处理）: %v\n", len(chunk), err)
			continue
		}
		for j := range list {
			got[list[j].ID] = &list[j]
		}
	}
	return got
}

func printRow(out io.Writer, r row, apply bool) {
	verb := "将退"
	if apply {
		verb = "已退"
	}
	head := fmt.Sprintf("%-6s #%-8d user=%-10d req=%s no=%s", r.c.Source, r.c.RefID, r.c.UserID, r.c.RequestID, r.no)
	switch r.outcome {
	case outRefund:
		fmt.Fprintf(out, "%s 冻结=%d 实扣=%d 成本=$%s 应扣=%d %s=%d\n", head, r.reserved, r.consumed, r.usd, r.fair, verb, r.refund)
	case outFair:
		fmt.Fprintf(out, "%s 实扣=%d 成本=$%s 应扣=%d 不用退\n", head, r.consumed, r.usd, r.fair)
	case outNoCost:
		fmt.Fprintf(out, "%s 调用 %s 没有实报成本（source=reported&final），跳过\n", head, strings.Join(r.c.CallIDs, ","))
	case outAlready:
		fmt.Fprintf(out, "%s 已补退过（%d 点），跳过\n", head, r.refund)
	case outError:
		fmt.Fprintf(out, "%s ERROR %v\n", head, r.err)
	}
}

func printSummary(out io.Writer, s summary, apply bool) {
	keys := make([]string, 0, len(s.ByOutcome))
	for k := range s.ByOutcome {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	mode := "DRY-RUN（未写库，加 -apply 才退）"
	if apply {
		mode = "APPLY"
	}
	fmt.Fprintf(out, "\n== 按实际用量补退汇总 [%s] ==\n扫描 %d 笔\n", mode, s.Scanned)
	for _, k := range keys {
		fmt.Fprintf(out, "  %-18s %d\n", k, s.ByOutcome[k])
	}
	verb := "将退"
	if apply {
		verb = "实退"
	}
	fmt.Fprintf(out, "%s %d 笔，合计 %d 点\n", verb, s.ByOutcome[outRefund], s.RefundPoints)
	if s.ByOutcome[outNoCost] > 0 {
		fmt.Fprintln(out, "提示：有调用没有实报成本——先在 ai-gateway 跑历史调用成本回填脚本，再重跑本工具（已退的不会重复退）")
	}
}

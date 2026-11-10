// actual-cost-refund 按实际用量补退历史多扣的点数。
//
// 背景：GPT Image 2.6 的载体（canvas-gpt-image-2.6-sunburst、studio-gpt-image-2.6-sunburst-tryon / -2ref、
// creaft-gpt-image-2.6-sunburst）在 Java V1.0.215 上线后按「冻结上限 × 最坏情况」定价，Go 的画布 / 商拍成功后全额确认，
// 一张实际 $0.0145 的图扣了 836 点。按实际用量结算上线后新单自动按实报成本结算；这个工具把上线前已确认的单补退到同一口径：
//
//	应扣 = min(冻结, max(1, ceil(实报成本USD × 汇率 × 加价倍率 × 100)))   （与在线结算 billing.SettleCost 同一函数）
//	补退 = 实扣 − 应扣（> 0 才退）
//
// 范围：Go 写的预留单（biz_type=LOCALLIFE、golife: 幂等键），来源是画布 gateway run（afv_canvas_node_run）与
// 商拍 / 姿态 / 图片生成子任务（afv_studio_task），状态 CONFIRMED / PARTIALLY_REFUNDED，载体 settle_mode=ACTUAL_COST。
//
// ⚠️ 前置条件（按顺序）：
//  1. Java 迁移已上（afv_ai_model.settle_mode / cost_markup，四个载体置 ACTUAL_COST）——否则全部判为 fixed 跳过；
//  2. ai-gateway 的历史调用成本回填脚本已跑完（GET /v1/calls/{id} 的 cost 变成 source=reported, final=true）——
//     否则这些单判为 no_reported_cost 跳过，回填后重跑即可。
//
// 幂等：每张预留单补退一次，流水幂等键 actual-cost-refund:{reservationNo}（afv_points_log 唯一键）；重复执行、并发执行都安全。
//
//	go run ./tools/ops/actual-cost-refund -dsn '<mysql dsn>' -gateway http://ai-gateway:8080 -key-id fusion-go-v1 \
//	    -secret "$AIG_SECRET" -since '2026-11-05 00:00:00'            # 默认 dry-run，只打印
//	go run ./tools/ops/actual-cost-refund ... -apply                   # 真退
//
// 退出码：有 error 行为 1，否则 0。
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/stores/sqlx"
)

func main() {
	dsn := flag.String("dsn", "", "MySQL DSN（需 parseTime=true），如 user:pass@tcp(host:3306)/fusion_aigc?parseTime=true&loc=Asia%2FShanghai")
	gwBase := flag.String("gateway", "", "ai-gateway 地址，如 http://ai-gateway:8080")
	keyID := flag.String("key-id", "fusion-go-v1", "ai-gateway 密钥 ID（calls scope）")
	secret := flag.String("secret", os.Getenv("AIG_SECRET"), "ai-gateway HMAC 密钥（默认读环境变量 AIG_SECRET）")
	since := flag.String("since", "", "只看这之后创建的 run / 子任务（V1.0.215 上线时间），格式 2006-01-02 15:04:05")
	until := flag.String("until", "", "可选：只看这之前创建的（如按实际用量结算的 Go 版本上线时间）")
	userID := flag.Int64("user", 0, "可选：只处理这个用户")
	limit := flag.Int("limit", 5000, "每个来源最多扫多少条")
	apply := flag.Bool("apply", false, "真退（默认 dry-run 只打印）")
	flag.Parse()

	if *dsn == "" || *since == "" || *gwBase == "" {
		fmt.Fprintln(os.Stderr, "缺少 -dsn / -since / -gateway")
		flag.Usage()
		os.Exit(2)
	}
	from, err := time.ParseInLocation(time.DateTime, *since, time.Local)
	if err != nil {
		fmt.Fprintf(os.Stderr, "-since 格式不对: %v\n", err)
		os.Exit(2)
	}
	to := time.Date(9999, 1, 1, 0, 0, 0, 0, time.Local)
	if *until != "" {
		if to, err = time.ParseInLocation(time.DateTime, *until, time.Local); err != nil {
			fmt.Fprintf(os.Stderr, "-until 格式不对: %v\n", err)
			os.Exit(2)
		}
	}
	logx.Disable()
	ctx := context.Background()
	conn := sqlx.NewMysql(*dsn)
	cands, err := loadCandidates(ctx, conn, from, to, *userID, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "查候选失败: %v\n", err)
		os.Exit(1)
	}
	// 计费服务按 on 构造：只用到 LoadReservation / CarrierOf / ConsumedPoints / RefundOvercharge，不会冻结
	svc := billing.New(billing.Conf{Mode: billing.ModeOn, CheckEntitlement: false}, conn)
	gw := aigateway.New(aigateway.Config{BaseURL: *gwBase, KeyID: *keyID, Secret: *secret})
	s := run(ctx, svc, gw, cands, *apply, os.Stdout)
	printSummary(os.Stdout, s, *apply)
	reportUnmatched(ctx, svc, cands, from, to, *userID, *limit)
	if s.ByOutcome[outError] > 0 {
		os.Exit(1)
	}
}

// loadCandidates 画布 gateway run + studio 子任务里已成功、有计费请求号的。逻辑删除的也算（用户删了记录，钱照样要退）。
func loadCandidates(ctx context.Context, conn sqlx.SqlConn, from, to time.Time, userID int64, limit int) ([]candidate, error) {
	var out []candidate
	userCond, args := "", []any{from, to}
	if userID > 0 {
		userCond = " AND `user_id` = ?"
		args = append(args, userID)
	}
	args = append(args, limit)

	var runs []struct {
		ID        int64          `db:"id"`
		UserID    int64          `db:"user_id"`
		ReqID     string         `db:"billing_request_id"`
		Ref       sql.NullString `db:"backend_ref"`
		CreatedAt time.Time      `db:"created_at"`
	}
	q := "SELECT `id`, `user_id`, `billing_request_id`, `backend_ref`, `created_at` FROM `afv_canvas_node_run` " +
		"WHERE `backend` = 'gateway' AND `status` = 2 AND `billing_request_id` IS NOT NULL AND `billing_request_id` <> '' " +
		"AND `created_at` >= ? AND `created_at` < ?" + userCond + " ORDER BY `id` LIMIT ?"
	if err := conn.QueryRowsPartialCtx(ctx, &runs, q, args...); err != nil {
		return nil, fmt.Errorf("canvas: %w", err)
	}
	for _, r := range runs {
		out = append(out, candidate{Source: "canvas", RefID: r.ID, UserID: r.UserID, RequestID: r.ReqID,
			CallIDs: splitIDs(r.Ref.String), CreatedAt: r.CreatedAt})
	}

	var tasks []struct {
		ID        int64          `db:"id"`
		UserID    int64          `db:"user_id"`
		ReqID     string         `db:"billing_request_id"`
		CallID    sql.NullString `db:"gateway_call_id"`
		CreatedAt time.Time      `db:"created_at"`
	}
	q = "SELECT `id`, `user_id`, `billing_request_id`, `gateway_call_id`, `created_at` FROM `afv_studio_task` " +
		"WHERE `status` = 2 AND `billing_request_id` IS NOT NULL AND `billing_request_id` <> '' " +
		"AND `created_at` >= ? AND `created_at` < ?" + userCond + " ORDER BY `id` LIMIT ?"
	if err := conn.QueryRowsPartialCtx(ctx, &tasks, q, args...); err != nil {
		return nil, fmt.Errorf("studio: %w", err)
	}
	for _, t := range tasks {
		out = append(out, candidate{Source: "studio", RefID: t.ID, UserID: t.UserID, RequestID: t.ReqID,
			CallIDs: splitIDs(t.CallID.String), CreatedAt: t.CreatedAt})
	}
	return out, nil
}

// reportUnmatched 窗口内已确认、载体 ACTUAL_COST、却对不上任何当前 run / 子任务的 Go 预留单：典型是商拍成功后「重新生成」，
// 子任务的计费请求号与 gateway 调用号被新一轮覆盖，旧轮次的单找不到调用号算不出实报成本。只列出来给人工处理，不自动退。
func reportUnmatched(ctx context.Context, svc *billing.Service, cands []candidate, from, to time.Time, userID int64, limit int) {
	list, err := svc.ListSettledGoReservations(ctx, from, to, limit*2)
	if err != nil {
		fmt.Printf("WARN 列已确认预留单失败（跳过对不上的单的检查）: %v\n", err)
		return
	}
	known := make(map[string]bool, len(cands))
	for _, c := range cands {
		known[billing.BizID(c.UserID, c.RequestID)] = true
	}
	n := 0
	for _, b := range list {
		if known[b.BizID] || (userID > 0 && b.UserID != userID) {
			continue
		}
		if mode, _ := svc.CarrierOf(ctx, b); mode != billing.SettleActualCost {
			continue
		}
		n++
		fmt.Printf("UNMATCHED no=%s user=%d biz_id=%s 冻结=%d（对不上当前 run / 子任务，需人工按调用成本处理）\n", b.No, b.UserID, b.BizID, b.PricePoints)
	}
	fmt.Printf("对不上当前记录的 ACTUAL_COST 已确认单：%d 笔（只报告，不自动退）\n", n)
}

func splitIDs(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

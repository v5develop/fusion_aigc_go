package logic

import (
	"context"
	"reflect"
	"regexp"
	"testing"
	"time"

	"aigc-local-life/pkg/aigateway"

	"github.com/DATA-DOG/go-sqlmock"
)

func reportedCost(usd string) *aigateway.Cost {
	return &aigateway.Cost{Amount: usd, Currency: "USD", AmountCNY: "0.1", Source: aigateway.CostSourceReported, Final: true}
}

func expectDeliverOK(mock sqlmock.Sqlmock) {
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 5")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_studio_task` SET `status` = 2")).WillReturnResult(ok1)
	mock.ExpectExec(regexp.QuoteMeta("task_done = task_done + 1")).WillReturnResult(ok1)
	mock.ExpectExec("SET step_log = JSON_ARRAY_APPEND").WillReturnResult(ok1)
	expectConvergeFinal(mock)
}

// 商拍 / 图片生成子任务按实际用量结算：交付时手上的调用已有实报终值 → 直接用，不再查 gateway。
func TestDeliverActualCostUsesCallCost(t *testing.T) {
	bill := onBill()
	bill.actualCost = true
	f, mock, gw := newTestFlow(t, bill)
	expectDeliverOK(mock)
	call := succeededCall()
	call.Cost = reportedCost("0.014536")
	f.deliver(context.Background(), runningTask(), batch9, call, time.Now())
	if !reflect.DeepEqual(bill.costUSD, []string{"0.014536"}) || gw.gets != 0 {
		t.Fatalf("cost=%v gets=%d", bill.costUSD, gw.gets)
	}
	met(t, mock)
}

// 手上的调用成本还没定稿（网关刚回填）：按 gateway_call_id 再查一次；多次尝试求和。
func TestDeliverActualCostRefetchesCall(t *testing.T) {
	bill := onBill()
	bill.actualCost = true
	f, mock, gw := newTestFlow(t, bill)
	expectDeliverOK(mock)
	refreshed := succeededCall()
	refreshed.Attempts = []aigateway.Attempt{
		{Seq: 1, Charged: aigateway.ChargedYes, Cost: reportedCost("0.01")},
		{Seq: 2, Charged: aigateway.ChargedYes, Cost: reportedCost("0.004536")},
	}
	gw.getCall = refreshed
	call := succeededCall()
	call.Cost = &aigateway.Cost{Amount: "0.02", Currency: "USD", Source: aigateway.CostSourceEstimated}
	f.deliver(context.Background(), runningTask(), batch9, call, time.Now())
	if !reflect.DeepEqual(bill.costUSD, []string{"0.014536"}) || gw.gets != 1 {
		t.Fatalf("cost=%v gets=%d", bill.costUSD, gw.gets)
	}
	met(t, mock)
}

// 查不到实报成本：成本来源返回不可用（billing 侧按冻结全额确认并记 ACTUAL_COST_UNAVAILABLE），交付照常。
func TestDeliverActualCostUnavailable(t *testing.T) {
	bill := onBill()
	bill.actualCost = true
	f, mock, gw := newTestFlow(t, bill)
	expectDeliverOK(mock)
	f.deliver(context.Background(), runningTask(), batch9, succeededCall(), time.Now()) // 无 cost，gateway Get 报错
	if !reflect.DeepEqual(bill.costUSD, []string{"unavailable"}) || gw.gets != 1 {
		t.Fatalf("cost=%v gets=%d", bill.costUSD, gw.gets)
	}
	if !reflect.DeepEqual(bill.settles, []string{"studio-simg_1"}) {
		t.Fatalf("settles=%v", bill.settles)
	}
	met(t, mock)
}

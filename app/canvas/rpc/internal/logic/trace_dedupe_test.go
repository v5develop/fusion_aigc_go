package logic

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/taskevent"

	"github.com/DATA-DOG/go-sqlmock"
)

func dedupes(rows []evRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.dedupe)
	}
	return out
}

func distinct(ss []string) bool {
	seen := map[string]bool{}
	for _, s := range ss {
		if seen[s] {
			return false
		}
		seen[s] = true
	}
	return true
}

// 结算：先失败（unsettled）后重试成功，两次尝试各占一个序号，成功的 span_end 不会被失败行的 dedupe 吞掉。
func TestTraceSettleFailedThenOKBothRecorded(t *testing.T) {
	c, mock, bmock := reconcileCore(t)
	ev := withEvents(c)
	run := gwRun()
	calls := []*aigateway.Call{{ID: "call_a", Status: aigateway.StatusSucceeded}}

	bmock.ExpectQuery(qResvStatus).WillReturnError(errors.New("conn reset"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` AS `r` SET `billing_status` = 'unsettled' WHERE (id = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.settle(context.Background(), run, 1, calls)
	time.Sleep(3 * time.Millisecond) // 序号是毫秒时间

	expectCanvasSettle(bmock, 836, false)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE `afv_canvas_node_run` AS `r` SET `billing_status` = 'settled' WHERE (id = 1)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	c.settle(context.Background(), run, 1, calls)

	ends := ev.find(taskevent.KindSpanEnd, stSettle)
	if len(ends) != 2 || ends[0].status != taskevent.StatusFailed || ends[1].status != taskevent.StatusOK {
		t.Fatalf("应各记一次 failed / ok: %+v", ends)
	}
	if !distinct(dedupes(ends)) {
		t.Fatalf("两次结算的 dedupe_key 必须不同: %v", dedupes(ends))
	}
	if a := attrsOf(t, ends[0].payload); a["points"] == nil {
		t.Fatalf("attrs=%v", a)
	}
}

// 超时延长：原因没变不再写；原因变了才写，A→B→A 的第二个 A 也记（dedupe 带原因与变化序号）；run 终态后清掉记忆。
func TestTraceTimeoutExtendOnlyWhenReasonChanges(t *testing.T) {
	c, _ := hardCapCore(t)
	ev := withEvents(c)
	run := timedRun(777, callA)
	timeoutReasons.forget(run.ID)
	defer timeoutReasons.forget(run.ID)
	apply := func(reason string) {
		c.applyGatewayTimeout(context.Background(), run, nil, timeoutVerdict{extend: true, reason: reason, attrs: taskevent.Attrs{"calls": 1}})
	}
	for _, r := range []string{"a", "a", "a", "b", "b", "a"} {
		apply(r)
	}
	ends := ev.find(taskevent.KindSpanEnd, stTimeout)
	if len(ends) != 3 {
		t.Fatalf("a,a,a,b,b,a 只应写 3 次（a / b / a）: %v", dedupes(ends))
	}
	if !distinct(dedupes(ends)) {
		t.Fatalf("dedupe 必须互不相同: %v", dedupes(ends))
	}
	for i, want := range []string{"a#1", "b#2", "a#3"} {
		if !strings.HasSuffix(ends[i].dedupe, ":"+want) {
			t.Fatalf("第 %d 条 dedupe 应以 :%s 结尾: %s", i, want, ends[i].dedupe)
		}
	}
	// 终态后清掉：同一 run 再进入延长又会记
	timeoutReasons.forget(run.ID)
	apply("a")
	if n := len(ev.find(taskevent.KindSpanEnd, stTimeout)); n != 4 {
		t.Fatalf("清记忆后应再记一次，实际 %d", n)
	}
}

// 超时失败分支：这次没抢到（skipped）与之后真正失败（timeout）各占一条，status 进 dedupe。
func TestTraceTimeoutFailBranchStatusInDedupe(t *testing.T) {
	c, mock := hardCapCore(t)
	ev := withEvents(c)
	c.s.Billing = billingOn(t)
	gw, _, _ := timeoutGateway(t, func(id string) []byte { return runningCall(id, true) })
	c.s.Gateway = gw
	run := timedRun(901, callA)
	// 第一次 CAS 没抢到
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*").WillReturnResult(sqlmock.NewResult(0, 0))
	c.applyGatewayTimeout(context.Background(), run, []string{callA}, timeoutVerdict{fail: true, reason: "hard_cap"})
	// 第二次抢到
	mock.ExpectExec("UPDATE `afv_canvas_node_run` SET `status` = 3, .*").WillReturnResult(sqlmock.NewResult(0, 1))
	c.applyGatewayTimeout(context.Background(), run, []string{callA}, timeoutVerdict{fail: true, reason: "hard_cap"})
	ends := ev.find(taskevent.KindSpanEnd, stTimeout)
	if len(ends) != 2 || !distinct(dedupes(ends)) {
		t.Fatalf("skipped / timeout 应各记一条: %v", dedupes(ends))
	}
}

// 退款区间在 Billing.Fail 之后才收尾，status=ok 且 result=requested（只表示已请求退款）。
func TestTraceRefundResultRequested(t *testing.T) {
	c, _ := hardCapCore(t)
	ev := withEvents(c)
	c.s.Billing = billingOn(t)
	run := timedRun(55, callA)
	c.refundTrace(context.Background(), run, "测试退款")
	ends := ev.find(taskevent.KindSpanEnd, stRefund)
	if len(ends) != 1 || ends[0].status != taskevent.StatusOK {
		t.Fatalf("%+v", ends)
	}
	if a := attrsOf(t, ends[0].payload); a["result"] != "requested" {
		t.Fatalf("attrs=%v", a)
	}
}

// 网关 / Java 错误进事件只有码与固定句式，不带原始响应体（可能回显提示词）。
func TestTraceDispatchFailNeverCarriesVendorBody(t *testing.T) {
	ge := &aigateway.Error{HTTPStatus: 400, Code: "content_policy", Message: "prompt 'secret 提示词' rejected https://x.com/a?sig=1", Check: "c"}
	f := dispatchFail(ge)
	if f.Code != "content_policy" || strings.Contains(f.Message, "提示词") || strings.Contains(f.Message, "sig=") {
		t.Fatalf("%+v", f)
	}
}

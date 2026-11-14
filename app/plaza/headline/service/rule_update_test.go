package service

import (
	"context"
	"testing"

	"aigc-local-life/pkg/xerr"
)

// L12：规则整体 PUT 起止日期必填（缺省不再悄悄补「今天起 7 天」覆盖原窗口），且在碰库之前拒收。
func TestUpdateRuleRequiresDates(t *testing.T) {
	s, mock, _ := newSvc(t)
	for _, req := range []UpdateRuleReq{
		{Why: "好用", EndDate: "2026-11-20"},
		{Why: "好用", StartDate: "2026-11-14"},
		{Why: "好用"},
	} {
		_, err := s.UpdateRule(context.Background(), Meta{OperatorID: 1}, 9, req)
		if c, _, _ := xerr.Parse(err); c != xerr.ParamMissing {
			t.Fatalf("%+v 应 PARAM_MISSING: %v", req, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

package core

import (
	"context"
	"errors"
	"testing"
)

// 确认时没带工作空间：回落用户个人空间；查不到 / 出错记 0、不挡确认。
func TestConfirmWorkspaceFallsBackToPersonal(t *testing.T) {
	e := newOpsEnv(t, false, nil, func(*env) Option {
		return WithWorkspace(func(_ context.Context, userID int64) (int64, error) { return 1000 + userID, nil })
	})
	jv := e.confirm(e.plan(e.seedAnalysis(10)), "", "k1")
	if j, _ := e.store.GetJob(e.ctx, jv.JobID); j.WorkspaceID != 1000+uid {
		t.Fatalf("应写个人空间：%d", j.WorkspaceID)
	}

	e2 := newOpsEnv(t, false, nil, func(*env) Option {
		return WithWorkspace(func(context.Context, int64) (int64, error) { return 0, errors.New("db down") })
	})
	jv2 := e2.confirm(e2.plan(e2.seedAnalysis(10)), "", "k1")
	if j, _ := e2.store.GetJob(e2.ctx, jv2.JobID); j.WorkspaceID != 0 || jv2.Money.Held != 100 {
		t.Fatalf("查空间失败记 0 且照常确认：ws=%d %+v", j.WorkspaceID, jv2.Money)
	}
}

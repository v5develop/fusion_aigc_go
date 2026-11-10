package logic

import (
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"aigc-local-life/app/studio/model"
)

func diagBatch(status int8, logs string, now time.Time) *model.Batch {
	return &model.Batch{
		ID:        7,
		RequestID: "req-7",
		Kind:      model.KindTryon,
		Status:    status,
		StepLog:   []byte(logs),
		CreatedAt: now.Add(-30 * time.Minute),
		UpdatedAt: now.Add(-30 * time.Minute),
	}
}

func stageStatus(d adminDiagnostics, key string) string {
	for _, s := range d.Trace.Stages {
		if s.Key == key {
			return s.Status
		}
	}
	return "missing"
}

func TestBuildAdminDiagnosticsDoneBatch(t *testing.T) {
	now := time.Now()
	ms := now.Add(-20 * time.Minute).UnixMilli()
	logs := `[{"step":"submitted","ms":5,"task_total":2,"at":` + itoa(ms) + `},` +
		`{"step":"analyze","ms":900,"at":` + itoa(ms+1000) + `,"degraded":true},` +
		`{"step":"generate","ms":3000,"at":` + itoa(ms+2000) + `}]`
	b := diagBatch(model.BatchDone, logs, now)
	tasks := []*model.Task{
		{ID: 1, BatchID: 7, ModelIndex: 0, ShotIndex: 0, Status: model.TaskSucceeded, BillingStatus: model.BillingSettled,
			GatewayCallID: sql.NullString{String: "gc-1", Valid: true}, Attempt: 1, CreatedAt: now.Add(-20 * time.Minute)},
		{ID: 2, BatchID: 7, ModelIndex: 0, ShotIndex: 1, Status: model.TaskFailed, BillingStatus: model.BillingRefunded,
			GatewayCallID: sql.NullString{String: "gc-2", Valid: true}, Attempt: 2,
			ErrorReason: sql.NullString{String: "生成失败，点数已退回", Valid: true}, CreatedAt: now.Add(-20 * time.Minute)},
	}

	d := buildAdminDiagnostics(b, tasks, now)

	if got := stageStatus(d, "batch.submit"); got != "done" {
		t.Errorf("batch.submit = %s, want done", got)
	}
	if got := stageStatus(d, "prompt.analyze"); got != "done" {
		t.Errorf("prompt.analyze = %s, want done (degraded still done)", got)
	}
	if got := stageStatus(d, "gateway.call.created"); got != "done" {
		t.Errorf("gateway.call.created = %s, want done", got)
	}
	if got := stageStatus(d, "task.settle"); got != "done" {
		t.Errorf("task.settle = %s, want done", got)
	}
	if d.Trace.StuckAt != nil {
		t.Errorf("terminal batch must not report stuck, got %+v", d.Trace.StuckAt)
	}
	if !d.Trace.Terminal {
		t.Errorf("trace.terminal = false, want true")
	}
	if len(d.WorkflowSteps) != 2 || d.WorkflowSteps[0].Status != "SUCCEEDED" || d.WorkflowSteps[1].Status != "FAILED" {
		t.Errorf("workflow steps = %+v", d.WorkflowSteps)
	}
	if d.Diagnosis.FailingStep == nil || d.Diagnosis.FailingStep.StepPath != "generate/m0-s1-p0" {
		t.Errorf("failing step = %+v", d.Diagnosis.FailingStep)
	}
	if d.Diagnosis.FailingStep.GatewayCallID == nil || *d.Diagnosis.FailingStep.GatewayCallID != "gc-2" {
		t.Errorf("failing step gateway call = %+v", d.Diagnosis.FailingStep.GatewayCallID)
	}
	if d.Diagnosis.Billing.BillingStatus != "SETTLED" {
		t.Errorf("billing status = %s, want SETTLED", d.Diagnosis.Billing.BillingStatus)
	}
	for i := 1; i < len(d.Timeline); i++ {
		if d.Timeline[i-1].Ts > d.Timeline[i].Ts {
			t.Fatalf("timeline not sorted at %d: %s > %s", i, d.Timeline[i-1].Ts, d.Timeline[i].Ts)
		}
	}
	if len(d.Timeline) == 0 || d.Timeline[0].ID != 1 {
		t.Errorf("timeline ids not assigned: %+v", d.Timeline)
	}

	raw := encodeAdminDiagnostics(b, tasks, now)
	var back adminDiagnostics
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("encoded diagnostics not valid JSON: %v", err)
	}
	if back.Trace.TraceID != "req-7" {
		t.Errorf("trace id = %s, want request id", back.Trace.TraceID)
	}
}

func TestBuildAdminDiagnosticsStuckGenerating(t *testing.T) {
	now := time.Now()
	logs := `[{"step":"submitted","ms":5,"task_total":1,"at":` + itoa(now.Add(-40*time.Minute).UnixMilli()) + `}]`
	b := diagBatch(model.BatchGenerating, logs, now)
	tasks := []*model.Task{
		{ID: 1, BatchID: 7, Status: model.TaskRunning, BillingStatus: model.BillingHeld,
			GatewayCallID: sql.NullString{String: "gc-1", Valid: true}, CreatedAt: now.Add(-40 * time.Minute)},
	}

	d := buildAdminDiagnostics(b, tasks, now)

	if d.Trace.StuckAt == nil {
		t.Fatalf("expected stuck report for generating batch with no progress")
	}
	if d.Trace.StuckAt.Stage != "generate.deliver" {
		t.Errorf("stuck stage = %s, want generate.deliver", d.Trace.StuckAt.Stage)
	}
	if d.Trace.StuckAt.ElapsedMs < int64(10*time.Minute/time.Millisecond) {
		t.Errorf("elapsed too small: %d", d.Trace.StuckAt.ElapsedMs)
	}
	if len(d.Diagnosis.Hints) == 0 {
		t.Errorf("expected at least one hint for stuck batch")
	}
}

func TestBuildAdminDiagnosticsHeldAfterTerminal(t *testing.T) {
	now := time.Now()
	b := diagBatch(model.BatchFailed, `[]`, now)
	tasks := []*model.Task{
		{ID: 1, BatchID: 7, Status: model.TaskFailed, BillingStatus: model.BillingHeld, CreatedAt: now},
	}
	d := buildAdminDiagnostics(b, tasks, now)
	if got := stageStatus(d, "task.settle"); got != "failed" {
		t.Errorf("task.settle = %s, want failed (held on terminal batch)", got)
	}
	if d.Trace.StuckAt != nil {
		t.Errorf("terminal batch must not report stuck")
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

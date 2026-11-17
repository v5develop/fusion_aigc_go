package core

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"aigc-local-life/app/replicate/model"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

// ---- 内存 Store：语义对齐 repo（CAS、唯一键、事务原子性），读出的是副本 ----

type memStore struct {
	// onTouch / beforeFinish 测试钩子：在心跳 / 终态迁移前插入并发动作（模拟回收器、并发重做）
	onTouch      func(jobID int64)
	beforeFinish func(jobID int64)
	// chargeWriteErr 接下来 N 次写镜像报错
	chargeWriteErr int

	mu       sync.Mutex
	now      func() time.Time
	id       int64
	analyses []*model.Analysis
	plans    []*model.Plan
	jobs     []*model.Job
	steps    []*model.Step
	charges  []*model.Charge
	prefs    map[int64]*model.UserPref
}

func newMemStore(now func() time.Time) *memStore {
	return &memStore{now: now, prefs: map[int64]*model.UserPref{}}
}

func (m *memStore) nextID() int64 { m.id++; return m.id }

func clone[T any](p *T) *T {
	if p == nil {
		return nil
	}
	c := *p
	return &c
}

// apply 按 bun 列名把 set 写进结构体（只用于测试替身）。
func apply(obj any, set map[string]any) {
	v := reflect.ValueOf(obj).Elem()
	t := v.Type()
	for col, val := range set {
		found := false
		for i := 0; i < t.NumField(); i++ {
			tag := strings.Split(t.Field(i).Tag.Get("bun"), ",")[0]
			if tag != col {
				continue
			}
			found = true
			f := v.Field(i)
			if val == nil {
				f.Set(reflect.Zero(f.Type()))
				break
			}
			rv := reflect.ValueOf(val)
			switch {
			case f.Kind() == reflect.Ptr && rv.Type() != f.Type():
				p := reflect.New(f.Type().Elem())
				p.Elem().Set(rv.Convert(f.Type().Elem()))
				f.Set(p)
			default:
				f.Set(rv.Convert(f.Type()))
			}
			break
		}
		if !found {
			panic("memStore: 未知列 " + col)
		}
	}
}

func (m *memStore) InsertAnalysis(_ context.Context, a *model.Analysis) (*model.Analysis, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.analyses {
		if x.UserID == a.UserID && x.SourceKey == a.SourceKey {
			return clone(x), false, nil
		}
	}
	c := clone(a)
	c.ID, c.CreatedAt, c.UpdatedAt = m.nextID(), m.now(), m.now()
	m.analyses = append(m.analyses, c)
	return clone(c), true, nil
}

func (m *memStore) GetAnalysis(_ context.Context, id int64) (*model.Analysis, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.analyses {
		if x.ID == id {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (m *memStore) GetAnalysisByNo(_ context.Context, userID int64, no string) (*model.Analysis, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.analyses {
		if x.UserID == userID && x.AnalysisNo == no {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (m *memStore) UpdateAnalysis(_ context.Context, id int64, stage string, attempt int, set map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.analyses {
		if x.ID == id && x.Stage == stage && x.Attempt == attempt && (x.Status == 0 || x.Status == 1) {
			apply(x, set)
			x.UpdatedAt = m.now()
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) RestartAnalysis(_ context.Context, id int64, attempt int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.analyses {
		if x.ID == id && x.Status == model.AnalysisFailed && x.Attempt == attempt {
			x.Status, x.Stage, x.Attempt, x.ErrorCode, x.ErrorMsg, x.State, x.FinishedAt = 0, model.StageIngest, attempt+1, nil, nil, nil, nil
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) DueAnalyses(_ context.Context, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var due []*model.Analysis
	for _, x := range m.analyses {
		if (x.Status == 0 || x.Status == 1) && (x.NextRunAt == nil || !x.NextRunAt.After(m.now())) {
			due = append(due, x)
		}
	}
	sort.SliceStable(due, func(a, b int) bool {
		na, nb := due[a].NextRunAt, due[b].NextRunAt
		if (na == nil) != (nb == nil) {
			return na == nil
		}
		if na != nil && !na.Equal(*nb) {
			return na.Before(*nb)
		}
		return due[a].ID < due[b].ID
	})
	var out []int64
	for _, x := range due {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, x.ID)
	}
	return out, nil
}

func (m *memStore) InsertPlan(_ context.Context, p *model.Plan) (*model.Plan, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.plans {
		if x.UserID == p.UserID && x.PlanHash == p.PlanHash {
			return clone(x), false, nil
		}
	}
	c := clone(p)
	c.ID, c.CreatedAt = m.nextID(), m.now()
	m.plans = append(m.plans, c)
	return clone(c), true, nil
}

func (m *memStore) GetPlanByNo(_ context.Context, userID int64, no string) (*model.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.plans {
		if x.UserID == userID && x.PlanNo == no {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (m *memStore) GetPlan(_ context.Context, id int64) (*model.Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.plans {
		if x.ID == id {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (m *memStore) ConfirmPlan(_ context.Context, id, jobID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.plans {
		if x.ID == id && x.Status == model.PlanReady {
			x.Status, x.JobID = model.PlanConfirmed, &jobID
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) ReleasePlan(_ context.Context, id, jobID int64) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.plans {
		if x.ID == id && x.Status == model.PlanConfirmed && x.JobID != nil && *x.JobID == jobID {
			x.Status, x.JobID, x.SupersededBy = model.PlanReady, nil, nil
			for _, c := range m.plans { // 占用期间改标注出的子方案：放回后是 STALE（同 repo）
				if c.ParentID != nil && *c.ParentID == id && (x.SupersededBy == nil || c.ID > *x.SupersededBy) {
					cid := c.ID
					x.Status, x.SupersededBy = model.PlanStale, &cid
				}
			}
			var newest *model.Plan // READY 子方案只留最新一张（同 repo）
			for _, c := range m.plans {
				if c.ParentID != nil && *c.ParentID == id && c.Status == model.PlanReady && (newest == nil || c.ID > newest.ID) {
					newest = c
				}
			}
			for _, c := range m.plans {
				if newest != nil && c.ParentID != nil && *c.ParentID == id && c.Status == model.PlanReady && c.ID < newest.ID {
					nid := newest.ID
					c.Status, c.SupersededBy = model.PlanStale, &nid
				}
			}
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) CreateJob(_ context.Context, j *model.Job, build JobBuilder) (*model.Job, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.UserID == j.UserID && x.RequestID == j.RequestID {
			return clone(x), false, nil
		}
	}
	c := clone(j)
	c.ID, c.CreatedAt, c.UpdatedAt = m.nextID(), m.now(), m.now()
	task, steps, charges := build(c.ID)
	c.TaskID = &task
	seen := map[string]bool{}
	for _, s := range steps {
		if seen[s.StepKey] {
			return nil, false, errors.New("dup step key")
		}
		seen[s.StepKey] = true
	}
	m.jobs = append(m.jobs, c)
	for _, s := range steps {
		s.ID, s.CreatedAt, s.UpdatedAt = m.nextID(), m.now(), m.now()
		m.steps = append(m.steps, clone(s))
	}
	for _, ch := range charges {
		ch.ID, ch.CreatedAt, ch.UpdatedAt = m.nextID(), m.now(), m.now()
		m.charges = append(m.charges, clone(ch))
	}
	return clone(c), true, nil
}

func (m *memStore) GetJob(_ context.Context, id int64) (*model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID == id {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (m *memStore) GetJobByRequest(_ context.Context, userID int64, req string) (*model.Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.UserID == userID && x.RequestID == req {
			return clone(x), nil
		}
	}
	return nil, nil
}

func (m *memStore) TransitJob(_ context.Context, id int64, from []int8, to int8, set map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID != id {
			continue
		}
		for _, f := range from {
			if x.Status == f {
				apply(x, set)
				x.Status, x.UpdatedAt = to, m.now()
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *memStore) SwapResultDoc(_ context.Context, id int64, old, new json.RawMessage) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID != id {
			continue
		}
		var a, b any
		if json.Unmarshal(x.ResultDocument, &a) != nil || json.Unmarshal(old, &b) != nil || !reflect.DeepEqual(a, b) {
			return false, nil
		}
		x.ResultDocument = append(json.RawMessage(nil), new...)
		x.UpdatedAt = m.now()
		return true, nil
	}
	return false, nil
}

func (m *memStore) ClaimContinue(_ context.Context, id int64, attempt int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID == id && x.Status == model.JobWaiting && x.ContinueAttempt == attempt {
			x.Status, x.ContinueAttempt, x.UpdatedAt = model.JobContinuing, attempt+1, m.now()
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) ListSteps(_ context.Context, jobID int64) ([]*model.Step, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.Step
	for _, x := range m.steps {
		if x.JobID == jobID {
			out = append(out, clone(x))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

func (m *memStore) ListCharges(_ context.Context, jobID int64) ([]*model.Charge, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.Charge
	for _, x := range m.charges {
		if x.JobID == jobID {
			out = append(out, clone(x))
		}
	}
	return out, nil
}

func (m *memStore) UpdateStep(_ context.Context, id int64, from string, set map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.steps {
		if x.ID == id && x.Status == from {
			apply(x, set)
			x.UpdatedAt = m.now()
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) UpdateCharge(_ context.Context, id int64, from []int8, set map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.chargeWriteErr > 0 {
		m.chargeWriteErr--
		return false, errors.New("db write timeout")
	}
	for _, x := range m.charges {
		if x.ID != id {
			continue
		}
		for _, f := range from {
			if x.Status == f {
				apply(x, set)
				x.UpdatedAt = m.now()
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *memStore) AddCharges(_ context.Context, charges []*model.Charge, stepReq map[int64]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range charges {
		for _, x := range m.charges {
			if x.RequestID == c.RequestID {
				return errors.New("dup request id")
			}
		}
	}
	for _, c := range charges {
		c.ID, c.CreatedAt, c.UpdatedAt = m.nextID(), m.now(), m.now()
		m.charges = append(m.charges, clone(c))
	}
	for _, x := range m.steps {
		if r, ok := stepReq[x.ID]; ok {
			x.BillingRequestID = ptr(r)
		}
	}
	return nil
}

func (m *memStore) AddRetryStep(_ context.Context, failed *model.Step, failSet map[string]any, retry *model.Step) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.steps {
		if x.JobID == retry.JobID && x.StepKey == retry.StepKey {
			return false, errors.New("dup step key")
		}
	}
	for _, x := range m.steps {
		if x.ID == failed.ID && x.Status == model.StepRunning {
			apply(x, failSet)
			retry.ID, retry.CreatedAt, retry.UpdatedAt = m.nextID(), m.now(), m.now()
			m.steps = append(m.steps, clone(retry))
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) AddRedo(_ context.Context, step *model.Step, charge *model.Charge) (*model.Charge, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.charges {
		if x.JobID == charge.JobID && x.ClientKey != nil && charge.ClientKey != nil && *x.ClientKey == *charge.ClientKey {
			return clone(x), false, nil
		}
	}
	for _, x := range m.steps {
		if x.JobID == step.JobID && x.StepKey == step.StepKey {
			return nil, false, errors.New("dup step key")
		}
	}
	step.ID, step.CreatedAt, step.UpdatedAt = m.nextID(), m.now(), m.now()
	charge.ID, charge.CreatedAt, charge.UpdatedAt = m.nextID(), m.now(), m.now()
	m.steps = append(m.steps, clone(step))
	m.charges = append(m.charges, clone(charge))
	return nil, true, nil
}

// DueJobs 与 repo 同口径（不含租约）：在途；待继续且到期；有未结镜像；终态未合成 / 合成待重试。按 next_run_at 升序、NULL 最先。
func (m *memStore) DueJobs(_ context.Context, limit int) ([]int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var due []*model.Job
	for _, x := range m.jobs {
		if x.NextRunAt != nil && x.NextRunAt.After(now) {
			continue
		}
		ok := false
		switch x.Status {
		case model.JobQueued, model.JobRunning, model.JobSubmitting, model.JobContinuing:
			ok = true
		case model.JobWaiting:
			ok = (x.ContinueDeadline != nil && !x.ContinueDeadline.After(now)) || (x.DecisionRemindAt != nil && !x.DecisionRemindAt.After(now))
		case model.JobSucceeded, model.JobPartial:
			var d ResultDoc
			_ = json.Unmarshal(x.ResultDocument, &d)
			ok = (len(x.ResultDocument) == 0 && x.FinishedAt != nil && now.Sub(*x.FinishedAt) < 24*time.Hour) || d.Status == ResultRetry
		}
		for _, c := range m.charges {
			if c.JobID == x.ID && model.ChargeOpen(c.Status) {
				ok = true
			}
		}
		if ok {
			due = append(due, x)
		}
	}
	sort.SliceStable(due, func(a, b int) bool {
		na, nb := due[a].NextRunAt, due[b].NextRunAt
		switch {
		case na == nil && nb == nil:
			return due[a].ID < due[b].ID
		case na == nil:
			return true
		case nb == nil:
			return false
		case !na.Equal(*nb):
			return na.Before(*nb)
		}
		return due[a].ID < due[b].ID
	})
	var out []int64
	for _, x := range due {
		if limit > 0 && len(out) >= limit {
			break
		}
		out = append(out, x.ID)
	}
	return out, nil
}

func (m *memStore) RescheduleJob(_ context.Context, id int64, delay time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID == id {
			t := m.now().Add(delay)
			x.NextRunAt = &t // 不动 updated_at
		}
	}
	return nil
}

func (m *memStore) RescheduleAnalysis(_ context.Context, id int64, delay time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.analyses {
		if x.ID == id {
			t := m.now().Add(delay)
			x.NextRunAt = &t
		}
	}
	return nil
}

func (m *memStore) TouchJob(_ context.Context, id int64, statuses []int8) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.onTouch != nil {
		m.onTouch(id)
	}
	for _, x := range m.jobs {
		if x.ID == id {
			for _, s := range statuses {
				if x.Status == s {
					x.UpdatedAt = m.now()
					return true, nil
				}
			}
		}
	}
	return false, nil
}

func (m *memStore) FinishJob(ctx context.Context, id int64, from []int8, to int8, maxSeg int, set map[string]any) (bool, error) {
	if h := m.beforeFinish; h != nil {
		m.beforeFinish = nil // 只触发一次
		h(id)
	}
	m.mu.Lock()
	for _, st := range m.steps {
		if st.JobID == id && st.Kind == model.StepKindGateway && st.SegIndex <= maxSeg {
			switch st.Status {
			case model.StepHolding, model.StepPending, model.StepRunning, model.StepDelivering:
				m.mu.Unlock()
				return false, nil
			}
		}
	}
	m.mu.Unlock()
	return m.TransitJob(ctx, id, from, to, set)
}

func (m *memStore) ClaimRedo(_ context.Context, id int64, max int, statuses []int8) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID != id || x.RedoCount >= max {
			continue
		}
		for _, s := range statuses {
			if x.Status == s {
				x.RedoCount++
				x.Status, x.FinishedAt, x.NextRunAt = model.JobRunning, nil, nil
				return true, nil
			}
		}
	}
	return false, nil
}

func (m *memStore) CancelOrphanSteps(_ context.Context, jobID int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	final := false
	for _, x := range m.jobs {
		if x.ID == jobID {
			final = model.JobFinal(x.Status)
		}
	}
	if !final {
		return 0, nil
	}
	var n int64
	for _, st := range m.steps {
		if st.JobID == jobID && st.Kind == model.StepKindGateway && st.Status == model.StepPending && st.BillingRequestID != nil {
			st.Status, st.ErrorCode = model.StepCanceled, ptr("NOT_SUBMITTED")
			n++
		}
	}
	return n, nil
}

func (m *memStore) ReleaseRedo(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.ID == id && x.RedoCount > 0 {
			x.RedoCount--
		}
	}
	return nil
}

func (m *memStore) ListJobs(_ context.Context, userID int64, offset, limit int) ([]*model.Job, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []*model.Job
	for i := len(m.jobs) - 1; i >= 0; i-- {
		if m.jobs[i].UserID == userID {
			all = append(all, clone(m.jobs[i]))
		}
	}
	total := int64(len(all))
	if offset >= len(all) {
		return nil, total, nil
	}
	end := min(offset+limit, len(all))
	return all[offset:end], total, nil
}

func (m *memStore) RefreshPlan(_ context.Context, id int64, expiresAt time.Time, quote []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.plans {
		if x.ID == id && x.Status == model.PlanReady {
			x.ExpiresAt, x.Quote = expiresAt, quote
			return true, nil
		}
	}
	return false, nil
}

func (m *memStore) GetPref(_ context.Context, userID int64) (*model.UserPref, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clone(m.prefs[userID]), nil
}

func (m *memStore) SetAutoContinue(_ context.Context, userID int64, on bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prefs[userID] = &model.UserPref{UserID: userID, AutoContinue: on}
	return nil
}

// ---- 计费替身：按请求号幂等，记录每张单的状态，用来断言「只为成功段付费」 ----

type fakeRes struct {
	status string
	points int64
}

type fakeLedger struct {
	mu          sync.Mutex
	unit        int64 // 每秒点数
	balance     int64 // <0 表示不限
	res         map[string]*fakeRes
	holds       int
	refundErrs  int // 接下来 N 次退回报错
	confirmErr  int // 接下来 N 次确认报错
	confirmRace int // 接下来 N 次确认：单已被并发扣除，但返回 ErrChargeGone
	off         bool
	holdUnknown int              // 接下来 N 次冻结成功但返回 ErrHoldUnknown（冻结后读状态失败）
	onHold      func(req string) // 测试钩子：冻结前（不持锁）
	renews      map[string]int
	aliveErr    error
	groupUnit   map[string]int64 // 按模型组的每秒点数（报价随模型变化；未登记用 unit）
	quoteErr    map[string]error // 按模型组的报价错误（模拟未配价）
	foreign     map[string]bool  // 请求号已被别处的单先占（BillingLedger 核对不一致 → ErrHoldMismatch，不冻结）
}

func newLedger() *fakeLedger {
	return &fakeLedger{unit: 10, balance: -1, res: map[string]*fakeRes{}, renews: map[string]int{}}
}

func (l *fakeLedger) Renew(_ context.Context, _ int64, req string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.renews[req]++
	return nil
}

func (l *fakeLedger) Status(_ context.Context, _ int64, req string) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.aliveErr != nil {
		return "", l.aliveErr
	}
	if r, ok := l.res[req]; ok {
		return r.status, nil
	}
	return "", nil
}

func (l *fakeLedger) Quote(_ context.Context, c billing.Charge) (int64, error) {
	if err := l.quoteErr[c.ModelGroup]; err != nil {
		return 0, err
	}
	if u, ok := l.groupUnit[c.ModelGroup]; ok {
		return int64(c.Seconds) * u, nil
	}
	return int64(c.Seconds) * l.unit, nil
}

func (l *fakeLedger) Hold(ctx context.Context, c billing.Charge) (bool, error) {
	if h := l.onHold; h != nil {
		h(c.RequestID) // 不持锁调用：钩子里可以并发推进 job（会退款 / 查状态）
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if c.RequestID == "" || c.EntryCode != EntryCode || c.Kind != billing.KindVideo {
		return false, errors.New("bad charge")
	}
	if l.off {
		return false, nil
	}
	if l.foreign[c.RequestID] {
		return false, fmt.Errorf("%w: 请求号 %s 入口不一致", ErrHoldMismatch, c.RequestID)
	}
	if r, ok := l.res[c.RequestID]; ok {
		return r.status == "HELD", nil
	}
	p := int64(c.Seconds) * l.unit
	if l.balance >= 0 {
		if l.balance < p {
			return false, xerr.Newf(xerr.InsufficientPoints, "点数不足，还差 %d 点", p-l.balance)
		}
		l.balance -= p
	}
	l.holds++
	l.res[c.RequestID] = &fakeRes{status: "HELD", points: p}
	if l.holdUnknown > 0 {
		l.holdUnknown--
		return false, fmt.Errorf("%w: status read timeout", ErrHoldUnknown)
	}
	return true, nil
}

func (l *fakeLedger) Confirm(_ context.Context, _ int64, req string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.confirmErr > 0 {
		l.confirmErr--
		return errors.New("db down")
	}
	if l.confirmRace > 0 { // 并发的另一次确认先扣了，这一次误报 Gone
		l.confirmRace--
		if r, ok := l.res[req]; ok {
			r.status = "CONFIRMED"
		}
		return fmt.Errorf("%w: 状态已变化", ErrChargeGone)
	}
	r, ok := l.res[req]
	if !ok {
		return nil // off 模式没有单
	}
	switch r.status {
	case "HELD":
		r.status = "CONFIRMED"
		return nil
	case "CONFIRMED":
		return nil
	}
	return fmt.Errorf("%w: %s", ErrChargeGone, r.status)
}

func (l *fakeLedger) Refund(ctx context.Context, _ int64, req, _ string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err // 与真实 DB 调用一致：调用方上下文已取消就失败
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.refundErrs > 0 {
		l.refundErrs--
		return false, errors.New("db down")
	}
	r, ok := l.res[req]
	if !ok {
		return false, nil
	}
	switch r.status {
	case "HELD":
		r.status = "REFUNDED"
		if l.balance >= 0 {
			l.balance += r.points
		}
		return true, nil
	case "REFUNDED", "EXPIRED":
		return true, nil
	}
	return false, ErrChargeConfirmed
}

func (l *fakeLedger) expire(req string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.res[req].status = "EXPIRED"
}

func (l *fakeLedger) sum(status string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	var t int64
	for _, r := range l.res {
		if r.status == status {
			t += r.points
		}
	}
	return t
}

func (l *fakeLedger) status(req string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if r, ok := l.res[req]; ok {
		return r.status
	}
	return ""
}

// ---- 网关替身：Submit 返回 RUNNING，第一次 Get 按脚本给终态；同幂等键重提返回同一条 ----

type outcome struct {
	status string // SUCCEEDED / FAILED / UNKNOWN
	class  string
}

type fakeGW struct {
	mu        sync.Mutex
	byKey     map[string]*aigateway.Call
	byID      map[string]*aigateway.Call
	script    map[string][]outcome // 步骤键后缀（如 seg/02/generate）→ 依次的结果；也可用完整幂等键
	submits   []string
	reqs      map[string]aigateway.CallRequest
	submitErr error
	analysis  map[string]json.RawMessage // asr / vision 结果（按 operation）
	modGate   chan struct{}              // 非 nil：审核调用（moderation.*）的 Get 阻塞到它关闭（模拟审核慢）
	// ttsHook 非 nil：tts.synthesize 调用由它决定结果（rid、时长毫秒、是否失败）
	ttsHook func(req aigateway.CallRequest) (rid string, durMs int64, fail bool)
}

func newGW() *fakeGW {
	return &fakeGW{byKey: map[string]*aigateway.Call{}, byID: map[string]*aigateway.Call{}, script: map[string][]outcome{},
		reqs: map[string]aigateway.CallRequest{}, analysis: map[string]json.RawMessage{}}
}

func (g *fakeGW) Submit(_ context.Context, req aigateway.CallRequest, opt aigateway.SubmitOptions) (*aigateway.Call, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.submitErr != nil {
		return nil, g.submitErr
	}
	if c, ok := g.byKey[opt.IdempotencyKey]; ok {
		cc := *c
		return &cc, nil
	}
	c := &aigateway.Call{ID: fmt.Sprintf("call_%d", len(g.byKey)+1), IdempotencyKey: opt.IdempotencyKey, Status: aigateway.StatusRunning, Operation: req.Operation}
	g.byKey[opt.IdempotencyKey], g.byID[c.ID] = c, c
	g.submits = append(g.submits, opt.IdempotencyKey)
	g.reqs[opt.IdempotencyKey] = req
	cc := *c
	return &cc, nil
}

func (g *fakeGW) Get(ctx context.Context, id string, _ time.Duration) (*aigateway.Call, error) {
	g.mu.Lock()
	gate, c0 := g.modGate, g.byID[id]
	g.mu.Unlock()
	if gate != nil && c0 != nil && strings.HasPrefix(c0.Operation, "moderation.") {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.byID[id]
	if !ok {
		return nil, &aigateway.Error{HTTPStatus: 404, Code: aigateway.CodeNotFound}
	}
	if !c.IsTerminal() {
		g.finish(c)
	}
	cc := *c
	return &cc, nil
}

func (g *fakeGW) finish(c *aigateway.Call) {
	if c.Operation == "tts.synthesize" && g.ttsHook != nil {
		rid, dur, fail := g.ttsHook(g.reqs[c.IdempotencyKey])
		if fail {
			c.Status, c.Failure = aigateway.StatusFailed, &aigateway.Failure{Class: "VENDOR_FAILED", Code: "x"}
			return
		}
		c.Status, c.Result = aigateway.StatusSucceeded, &aigateway.Result{Operation: "tts.synthesize", Audio: &aigateway.MediaOut{Rid: rid, DurationMs: dur}}
		return
	}
	if raw, ok := g.analysis[c.Operation]; ok {
		var r aigateway.Result
		_ = json.Unmarshal(raw, &r)
		c.Status, c.Result = aigateway.StatusSucceeded, &r
		return
	}
	key := c.IdempotencyKey[strings.Index(c.IdempotencyKey, ":")+1:]
	o := outcome{status: aigateway.StatusSucceeded}
	for _, k := range []string{c.IdempotencyKey, key} {
		if q := g.script[k]; len(q) > 0 {
			o, g.script[k] = q[0], q[1:]
			break
		}
	}
	c.Status = o.status
	if o.status == aigateway.StatusSucceeded {
		var r aigateway.Result
		_ = json.Unmarshal([]byte(fmt.Sprintf(`{"operation":"video.generate","video":{"rid":"%s","duration_ms":5000},"cover":{"rid":"%s"}}`,
			workRid("v|"+key), workRid("c|"+key))), &r)
		c.Result = &r
		c.Cost = &aigateway.Cost{AmountCNY: "1.20"}
		return
	}
	c.Failure = &aigateway.Failure{Class: o.class, Code: "x"}
}

func (g *fakeGW) submitted(suffix string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, k := range g.submits {
		if strings.HasSuffix(k, suffix) {
			n++
		}
	}
	return n
}

// ---- 签名替身 ----

type fakeSigner struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeSigner) ResourceURLs(_ context.Context, rids []string, ttl time.Duration) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]string{}
	for _, r := range rids {
		out[r] = fmt.Sprintf("https://cdn.test/%s?ttl=%d", r, int(ttl.Seconds()))
	}
	return out, nil
}

// ---- 媒体替身 ----

type fakeMedia struct {
	probe     mediasvc.ProbeInfo
	frames    int
	gridErr   error
	ingests   int
	ingestErr error
	idems     []string
	// inspect 坏片检测的脚本：nil = 一律正常；inspected 记录被检查过的 rid
	inspect   func(rid string) (mediasvc.VideoInspect, error)
	inspected []string
	// motion 运动量曲线的脚本（CONTRACT §R）：nil = 不实现该能力的行为由 noMotionMedia 包装；motionCalls 记录调用次数
	motion      func(rid string) (mediasvc.MotionProfile, error)
	motionCalls int
}

func (m *fakeMedia) VideoMotionProfile(_ context.Context, rid string, fps, from, to float64) (mediasvc.MotionProfile, error) {
	m.motionCalls++
	if m.motion != nil {
		return m.motion(rid)
	}
	return mediasvc.MotionProfile{}, errors.New("motion 未脚本化")
}

// InspectVideo 默认返回「正常片」（可解码、无黑屏 / 定格）。
func (m *fakeMedia) InspectVideo(_ context.Context, rid string) (mediasvc.VideoInspect, error) {
	m.inspected = append(m.inspected, rid)
	if m.inspect != nil {
		return m.inspect(rid)
	}
	return mediasvc.VideoInspect{Decodable: true, DurationS: 5}, nil
}

func (m *fakeMedia) IngestURL(_ context.Context, src, purpose, idem string) (mediasvc.Resource, error) {
	m.ingests++
	m.idems = append(m.idems, idem)
	if m.ingestErr != nil {
		return mediasvc.Resource{}, m.ingestErr
	}
	if strings.Contains(src, ".png") || strings.Contains(src, ".jpg") {
		return mediasvc.Resource{RID: testRid("i" + fmt.Sprint(m.ingests))}, nil
	}
	return mediasvc.Resource{RID: "src-rid"}, nil
}

// workRid 由 key 派生的合法产物 rid（{6 位}-work-{26 位 a-z2-7}），可签名。
func workRid(key string) string {
	sum := sha256.Sum256([]byte(key))
	const alpha = "abcdefghijklmnopqrstuvwxyz234567"
	b := make([]byte, 26)
	for i := range b {
		b[i] = alpha[int(sum[i])%32]
	}
	return "abc123-work-" + string(b)
}

// testRid 合法的用户上传类 rid（{6 位}-user-{26 位 a-z2-7}）。
func testRid(tag string) string {
	b := []byte("aaaaaaaaaaaaaaaaaaaaaaaaaa")
	for i, c := range []byte(tag) {
		if i < len(b) && (c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			b[i] = c
		}
	}
	return "abc123-user-" + string(b)
}

// testRef 测试用参考图引用。
var testRef = "mr://" + testRid("ref")

// testOwner 测试用归属：https://own.test/ 下、路径含 /<uid>/ 的才算本人。
func testOwner(_ context.Context, userID int64, u string) bool {
	return strings.HasPrefix(u, "https://own.test/") && strings.Contains(u, fmt.Sprintf("/%d/", userID))
}

func (m *fakeMedia) Probe(context.Context, string) (mediasvc.ProbeInfo, error) { return m.probe, nil }

func (m *fakeMedia) Frames(_ context.Context, rid string, n int, _ string) ([]mediasvc.Frame, error) {
	var out []mediasvc.Frame
	for i := 0; i < n; i++ {
		out = append(out, mediasvc.Frame{TimestampS: float64(i) * m.probe.DurationS / float64(n), RID: fmt.Sprintf("f%d", i)})
	}
	return out, nil
}

func (m *fakeMedia) Grid(context.Context, []string, mediasvc.GridParams, string) (mediasvc.Resource, error) {
	if m.gridErr != nil {
		return mediasvc.Resource{}, m.gridErr
	}
	return mediasvc.Resource{RID: "sheet"}, nil
}

type fakeCutter struct{ cuts []string }

func (c *fakeCutter) Cut(_ context.Context, src string, t0, t1 float64, idem string) (string, error) {
	c.cuts = append(c.cuts, idem)
	return fmt.Sprintf("cut-%.1f-%.1f", t0, t1), nil
}

// ---- 场景装配 ----

type env struct {
	t      *testing.T
	clock  time.Time
	store  *memStore
	ledger *fakeLedger
	gw     *fakeGW
	media  *fakeMedia
	cutter *fakeCutter
	signer *fakeSigner
	svc    *Service
	ctx    context.Context
}

const uid = int64(7)

func newEnv(t *testing.T, withCutter bool) *env {
	e := &env{t: t, clock: time.Date(2026, 11, 13, 10, 0, 0, 0, time.Local), ledger: newLedger(), gw: newGW(),
		media: &fakeMedia{}, ctx: context.Background()}
	e.store = newMemStore(func() time.Time { return e.clock })
	e.signer = &fakeSigner{}
	opts := []Option{WithGateway(e.gw), WithMedia(e.media), WithClock(func() time.Time { return e.clock }), WithOwner("test-owner"),
		WithURLOwner(testOwner), WithSigner(e.signer)}
	if withCutter {
		e.cutter = &fakeCutter{}
		opts = append(opts, WithCutter(e.cutter))
	}
	cfg := DefaultConfig()
	cfg.ASRModelGroup = "asr.paraformer"
	e.svc = New(cfg, e.store, e.ledger, opts...)
	return e
}

// seedAnalysis 直接放一条成功的分析（时长 d，分镜边界按 cuts）。
func (e *env) seedAnalysis(d float64, cuts ...float64) *model.Analysis {
	r := AnalysisResult{PromptVersion: AnalysisPromptVersion, DurationS: d, Width: 720, Height: 1280, HasAudio: true, AudioKind: "speech",
		Dims: map[string]Dim{}, DimOrder: DimKeys}
	for _, k := range DimKeys {
		r.Dims[k] = Dim{Present: true, Desc: DimLabel[k] + "现状", Risk: "low"}
	}
	r.Dims["product"] = Dim{Present: true, Desc: "白色运动鞋", Risk: "mid"}
	prev := 0.0
	for i, c := range append(cuts, d) {
		r.Sections = append(r.Sections, Section{Index: i + 1, T0: prev, T1: c, Desc: fmt.Sprintf("镜头%d", i+1)})
		prev = c
	}
	b, _ := json.Marshal(r)
	a, _, _ := e.store.InsertAnalysis(e.ctx, &model.Analysis{AnalysisNo: fmt.Sprintf("VRAtest%v", d), UserID: uid, SourceKey: fmt.Sprint(d),
		SourceURL: "https://x/v.mp4", SourceRid: ptr("src-rid"), Status: model.AnalysisSucceeded, Stage: model.StageDone, Result: b})
	return a
}

func (e *env) plan(a *model.Analysis) *PlanView {
	e.t.Helper()
	pv, err := e.svc.CreatePlan(e.ctx, PlanReq{UserID: uid, AnalysisNo: a.AnalysisNo, ModelGroup: "vid.seedance-2.5", RefImages: []string{testRef},
		Annotations: Annotations{Dims: map[string]DimEdit{"product": {Mode: ModeReplace, Refs: []int{1}}}}})
	if err != nil {
		e.t.Fatalf("CreatePlan: %v", err)
	}
	return pv
}

func (e *env) confirm(pv *PlanView, stage, key string) *JobView {
	e.t.Helper()
	jv, err := e.svc.Confirm(e.ctx, ConfirmReq{UserID: uid, PlanNo: pv.PlanNo, PlanHash: pv.PlanHash, Stage: stage, IdempotencyKey: key})
	if err != nil {
		e.t.Fatalf("Confirm: %v", err)
	}
	return jv
}

// run 推进到 job 不再变化（最多 n 轮）。
func (e *env) run(jobID int64, n int) *JobView {
	e.t.Helper()
	for i := 0; i < n; i++ {
		if err := e.svc.Advance(e.ctx, jobID); err != nil {
			e.t.Fatalf("Advance: %v", err)
		}
	}
	jv, err := e.svc.JobView(e.ctx, uid, jobID)
	if err != nil {
		e.t.Fatal(err)
	}
	return jv
}

func (e *env) charges(jobID int64) []*model.Charge {
	cs, _ := e.store.ListCharges(e.ctx, jobID)
	return cs
}

func (e *env) steps(jobID int64) []*model.Step {
	ss, _ := e.store.ListSteps(e.ctx, jobID)
	return ss
}

// salt job 冻结的预留请求号盐（JobInput.BillingSalt；手工建的 job 输入为空 = 旧格式）。
func (e *env) salt(jobID int64) string {
	j, _ := e.store.GetJob(e.ctx, jobID)
	var in JobInput
	if j != nil {
		_ = json.Unmarshal(j.Input, &in)
	}
	return in.BillingSalt
}

func (e *env) rFirst(jobID int64, k int) string { return reqFirst(e.salt(jobID), jobID, k) }
func (e *env) rRest(jobID int64, k, attempt int) string {
	return reqRest(e.salt(jobID), jobID, k, attempt)
}
func (e *env) rRedo(jobID int64, k, n int) string { return reqRedo(e.salt(jobID), jobID, k, n) }

// assertMoney 钱的不变量：终态 job 没有 HELD 单；已扣除 = 各段「当前成功步骤」所用预留单之和；镜像与 billing 一致。
func (e *env) assertMoney(jobID int64) {
	e.t.Helper()
	j, _ := e.store.GetJob(e.ctx, jobID)
	steps := e.steps(jobID)
	var want int64
	// 每个成功步骤都已扣费（精修成功的段：原版本与精修版本各扣一笔）
	for k := 1; k <= j.SegTotal; k++ {
		for _, st := range segChain(steps, k) {
			if st.Status == model.StepSucceeded && st.BillingRequestID != nil {
				if r := e.ledger.res[*st.BillingRequestID]; r != nil {
					want += r.points
				}
			}
		}
	}
	if got := e.ledger.sum("CONFIRMED"); got != want {
		e.t.Fatalf("已扣除 %d，应为成功段之和 %d", got, want)
	}
	if model.JobFinal(j.Status) {
		for _, c := range e.charges(jobID) {
			if st := e.ledger.status(c.RequestID); st == "HELD" {
				e.t.Fatalf("终态 job 仍有冻结中的预留单 %s", c.RequestID)
			}
		}
	}
	for _, c := range e.charges(jobID) {
		st := e.ledger.status(c.RequestID)
		switch c.Status {
		case model.ChargeConfirmed:
			if st != "CONFIRMED" {
				e.t.Fatalf("镜像已扣除但预留单是 %s（%s）", st, c.RequestID)
			}
		case model.ChargeRefunded:
			if st != "REFUNDED" && st != "EXPIRED" {
				e.t.Fatalf("镜像已退回但预留单是 %s（%s）", st, c.RequestID)
			}
		}
	}
}

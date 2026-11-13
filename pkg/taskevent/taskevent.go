// Package taskevent 把 Go 侧创作任务的生命周期写进与 Java 共用的 afv_task_event（V1.0.194），
// 管理端据此按 task_id 展示一条完整、统一的轨迹。
//
// 约定（与 fusion-aigc docs/ops/task-trace-contract.md 一致）：
//   - task_id = trace_id = 创作任务号 = taskcenter.Ref(prefix, id)，source 固定 'go'；
//   - 区间（span）= 同一 step_path 的 span_start / span_end 两行，step_path 形如 go.<组件>/<阶段>[/<子项>]；
//     span_end.status ∈ ok|failed|cancelled|timeout|skipped，非 ok 时带 failure_class / failure_code / check_code / message；
//   - 点事件：kind=mark，以及每创建一个网关调用就写一条 gateway_call_linked（external_type=gateway_call）；
//   - payload_json = {"attrs": {...}}，只放脱敏后的扁平标量，绝不放提示词、签名 URL、用户内容；
//   - dedupe_key = <kind>:<step_path>:<attempt|seq>，INSERT IGNORE，行可以先于任务行落库（无外键）；
//   - 写入完全异步、尽力而为：调用只入进程内有界队列即返回（不等库），一个后台 worker 批量 INSERT IGNORE；
//     队列满 / 库持续失败时丢弃并计数（判据=TASK_EVENT_DROPPED，限频日志），永远不阻塞、不拖慢业务路径，也不碰任何资金表。
package taskevent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zeromicro/go-zero/core/logx"
	"github.com/zeromicro/go-zero/core/proc"
)

// 事件类型。
const (
	KindSpanStart         = "span_start"
	KindSpanEnd           = "span_end"
	KindMark              = "mark"
	KindGatewayCallLinked = "gateway_call_linked"
)

// span_end.status 取值。
const (
	StatusOK        = "ok"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
	StatusTimeout   = "timeout"
	StatusSkipped   = "skipped"
)

// ExternalGatewayCall external_type：网关调用。
const ExternalGatewayCall = "gateway_call"

// SourceGo source 列取值。
const SourceGo = "go"

// 列长度上限（与表定义一致）。
const (
	maxMessageRunes = 500
	maxStepRunes    = 191
	maxCodeRunes    = 128
	maxClassRunes   = 64
	maxRefRunes     = 128
	maxDedupeRunes  = 255
	maxAttrRunes    = 256
	maxAttrs        = 32
	// maxFailMessageRunes 事件 message 列的上限（失败说明只给简短脱敏文案，不放供应商 / Java 的原始响应体）。
	maxFailMessageRunes = 120

	// DefaultTimeout 后台 worker 单批写入的超时。
	DefaultTimeout = 3 * time.Second
	// DefaultQueueSize 进程内队列容量；满了丢弃新事件。
	DefaultQueueSize = 20000
	// DefaultBatchSize 单条多行 INSERT 的最大行数。
	DefaultBatchSize = 200
	// DefaultFlushTimeout 关停时尽力冲刷的最长时间。
	DefaultFlushTimeout = 5 * time.Second
	// 单批失败后的重试次数（不含首次）与退避。
	batchRetries = 2
	dropLogEvery = 10 * time.Second
)

// retryBackoff 第 i 次重试前的等待。
var retryBackoff = []time.Duration{200 * time.Millisecond, 1 * time.Second}

// Execer sqlx.SqlConn 满足。
type Execer interface {
	ExecCtx(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Attrs payload_json.attrs 的内容。
type Attrs map[string]any

// Writer 事件写入器。并发安全；nil *Writer 的所有方法都是空操作。
// 所有写入先入有界队列（Write 立即返回），一个后台 worker（首次写入时懒启动）批量 INSERT IGNORE。
type Writer struct {
	db      Execer
	timeout time.Duration
	now     func() time.Time

	batchSize int
	backoff   []time.Duration
	queue     chan []any // 每个元素是一行的参数（顺序见 insertSQL）

	startOnce sync.Once
	stopOnce  sync.Once
	closed    atomic.Bool
	stopCh    chan struct{}
	doneCh    chan struct{}

	pending  atomic.Int64 // 已入队、尚未落库或丢弃的行数
	dropped  atomic.Int64
	written  atomic.Int64
	lastDrop atomic.Int64 // 上次打丢弃日志的时刻（UnixNano）
	dropSeen atomic.Int64 // 距上次日志新丢的行数
}

// New db 为空返回 nil（空操作写入器）。
func New(db Execer) *Writer {
	if db == nil {
		return nil
	}
	return &Writer{
		db: db, timeout: DefaultTimeout, now: time.Now, batchSize: DefaultBatchSize, backoff: retryBackoff,
		queue: make(chan []any, DefaultQueueSize), stopCh: make(chan struct{}), doneCh: make(chan struct{}),
	}
}

// WithTimeout 单批写入超时（≤0 保持默认）。
func (w *Writer) WithTimeout(d time.Duration) *Writer {
	if w != nil && d > 0 {
		w.timeout = d
	}
	return w
}

// Dropped 累计丢弃的行数（队列满 / 库持续失败 / 已关闭）。
func (w *Writer) Dropped() int64 {
	if w == nil {
		return 0
	}
	return w.dropped.Load()
}

// Written 累计成功提交给库的行数。
func (w *Writer) Written() int64 {
	if w == nil {
		return 0
	}
	return w.written.Load()
}

// recoverTrace 公共方法最外层的 panic 兜底：追踪永远不能让业务 panic。
func recoverTrace(where string) {
	if r := recover(); r != nil {
		logx.Errorf("taskevent: %s panic（已吞掉，不影响业务） 判据=TASK_EVENT_WRITE: %v", where, r)
	}
}

// Event 一行事件。TaskID、Kind 必填。
type Event struct {
	TaskID       string
	TS           time.Time // 零值=写入时刻
	StepPath     string
	Kind         string
	Status       string
	FailureClass string
	FailureCode  string
	CheckCode    string
	Message      string
	Attempt      int
	RunNo        int
	ExternalType string
	ExternalRef  string
	Actor        string
	Seq          int    // dedupe_key 的末段；0 时取 Attempt
	Dedupe       string // 整个 dedupe_key 覆盖（空=按 DedupeKey 规则生成）
	Attrs        Attrs
}

// Fail 非 ok 结局的说明。
type Fail struct {
	Class   string
	Code    string
	Check   string
	Message string
}

// DedupeKey <kind>:<step_path>:<n>。
func DedupeKey(kind, stepPath string, n int) string {
	return TruncRunes(kind+":"+stepPath+":"+strconv.Itoa(n), maxDedupeRunes)
}

// Write 入队一行（最终 INSERT IGNORE）。立即返回；队列满则丢弃并计数。
func (w *Writer) Write(ctx context.Context, e Event) {
	defer recoverTrace("Write")
	if w == nil || w.db == nil || strings.TrimSpace(e.TaskID) == "" || e.Kind == "" {
		return
	}
	ts := e.TS
	if ts.IsZero() {
		ts = w.now()
	}
	_, args := buildInsert(e, ts)
	w.enqueue(args)
}

func (w *Writer) enqueue(args []any) {
	if w.closed.Load() {
		w.noteDrop(1, "writer 已关闭")
		return
	}
	w.startOnce.Do(w.startWorker)
	w.pending.Add(1)
	select {
	case w.queue <- args:
	default:
		w.pending.Add(-1)
		w.noteDrop(1, "队列已满")
	}
}

// noteDrop 计数 + 限频日志（判据=TASK_EVENT_DROPPED）。
func (w *Writer) noteDrop(n int, why string) {
	w.dropped.Add(int64(n))
	w.dropSeen.Add(int64(n))
	now := time.Now().UnixNano()
	last := w.lastDrop.Load()
	if now-last < int64(dropLogEvery) || !w.lastDrop.CompareAndSwap(last, now) {
		return
	}
	logx.Errorf("taskevent: 丢弃任务轨迹事件（不影响业务） 判据=TASK_EVENT_DROPPED 原因=%s 本轮丢弃=%d 累计丢弃=%d 队列=%d",
		why, w.dropSeen.Swap(0), w.dropped.Load(), len(w.queue))
}

func (w *Writer) startWorker() {
	go w.run()
	proc.AddShutdownListener(func() {
		ctx, cancel := context.WithTimeout(context.Background(), DefaultFlushTimeout)
		defer cancel()
		_ = w.Close(ctx)
	})
}

// run 后台 worker：取一行，再尽量多取（≤batchSize），一条多行 INSERT 写入。
func (w *Writer) run() {
	defer close(w.doneCh)
	for {
		select {
		case first := <-w.queue:
			w.flushBatch(context.Background(), w.collect(first))
		case <-w.stopCh:
			// 关停：尽力把队列里剩下的写完（有界时间）
			ctx, cancel := context.WithTimeout(context.Background(), DefaultFlushTimeout)
			defer cancel()
			for {
				select {
				case first := <-w.queue:
					if ctx.Err() != nil {
						w.pending.Add(-1)
						w.noteDrop(1, "关停冲刷超时")
						continue
					}
					w.flushBatch(ctx, w.collect(first))
				default:
					return
				}
			}
		}
	}
}

func (w *Writer) collect(first []any) [][]any {
	batch := make([][]any, 0, w.batchSize)
	batch = append(batch, first)
	for len(batch) < w.batchSize {
		select {
		case r := <-w.queue:
			batch = append(batch, r)
		default:
			return batch
		}
	}
	return batch
}

// flushBatch 写一批；失败按退避重试，仍失败则整批丢弃并记日志。绝不 panic、不阻塞调用方。
func (w *Writer) flushBatch(ctx context.Context, batch [][]any) {
	n := len(batch)
	defer w.pending.Add(int64(-n))
	defer func() {
		if r := recover(); r != nil {
			w.noteDrop(n, fmt.Sprintf("worker panic: %v", r))
		}
	}()
	query, args := buildMultiInsert(batch)
	var err error
	for i := 0; i <= batchRetries; i++ {
		if i > 0 {
			d := time.Second
			if i-1 < len(w.backoff) {
				d = w.backoff[i-1]
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				w.noteDrop(n, "冲刷超时："+errStr(err))
				return
			}
		}
		bctx, cancel := context.WithTimeout(ctx, w.timeout)
		_, err = w.db.ExecCtx(bctx, query, args...)
		cancel()
		if err == nil {
			w.written.Add(int64(n))
			return
		}
	}
	w.noteDrop(n, "写库失败："+errStr(err))
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return TruncRunes(Sanitize(err.Error()), 160)
}

// Flush 等队列里已入队的事件全部落库（或丢弃）；测试与关停用。ctx 到期返回其错误。
func (w *Writer) Flush(ctx context.Context) error {
	if w == nil {
		return nil
	}
	for w.pending.Load() > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
	return nil
}

// Close 停止接收新事件，尽力冲刷剩余事件（受 ctx 约束），随后结束 worker。可重复调用。
func (w *Writer) Close(ctx context.Context) error {
	if w == nil {
		return nil
	}
	w.closed.Store(true)
	err := w.Flush(ctx)
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.startOnce.Do(func() { close(w.doneCh) }) // 从未启动过 worker
	select {
	case <-w.doneCh:
	case <-ctx.Done():
		if err == nil {
			err = ctx.Err()
		}
	}
	return err
}

// cst 与 Java 一致的库时区（Asia/Shanghai，无夏令时）：datetime 列不带时区，按字符串写入避免驱动换算。
var cst = time.FixedZone("CST", 8*3600)

const (
	insertHead = "INSERT IGNORE INTO afv_task_event " +
		"(task_id, ts, step_path, kind, status, failure_class, failure_code, check_code, message, attempt, run_no, " +
		"external_type, external_ref, trace_id, source, actor, dedupe_key, payload_json) VALUES "
	// RowArgs 一行事件占用的参数个数（测试里按它把多行 INSERT 的参数切回一行一行）。
	RowArgs         = 18
	rowPlaceholders = "(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"
	insertSQL       = insertHead + rowPlaceholders
)

// buildMultiInsert 多行 INSERT IGNORE（一行一组占位符）。
func buildMultiInsert(rows [][]any) (string, []any) {
	var sb strings.Builder
	sb.WriteString(insertHead)
	args := make([]any, 0, len(rows)*RowArgs)
	for i, r := range rows {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(rowPlaceholders)
		args = append(args, r...)
	}
	return sb.String(), args
}

func buildInsert(e Event, ts time.Time) (string, []any) {
	task := TruncRunes(strings.TrimSpace(e.TaskID), 64)
	step := TruncRunes(strings.TrimSpace(e.StepPath), maxStepRunes)
	seq := e.Seq
	if seq == 0 {
		seq = e.Attempt
	}
	dedupe := e.Dedupe
	if dedupe == "" {
		dedupe = DedupeKey(e.Kind, step, seq)
	}
	var attempt, runNo sql.NullInt64
	if e.Attempt > 0 {
		attempt = sql.NullInt64{Int64: int64(e.Attempt), Valid: true}
	}
	if e.RunNo > 0 {
		runNo = sql.NullInt64{Int64: int64(e.RunNo), Valid: true}
	}
	return insertSQL, []any{
		task, ts.In(cst).Format("2006-01-02 15:04:05.000"), nullStr(step), e.Kind,
		nullStr(TruncRunes(e.Status, 32)), nullStr(TruncRunes(Sanitize(e.FailureClass), maxClassRunes)),
		nullStr(TruncRunes(Sanitize(e.FailureCode), maxCodeRunes)), nullStr(TruncRunes(Sanitize(e.CheckCode), maxCodeRunes)),
		nullStr(SanitizeN(e.Message, maxFailMessageRunes)), attempt, runNo,
		nullStr(TruncRunes(e.ExternalType, 32)), nullStr(TruncRunes(e.ExternalRef, maxRefRunes)),
		task, SourceGo, nullStr(TruncRunes(e.Actor, 64)), TruncRunes(dedupe, maxDedupeRunes), payloadJSON(e.Attrs),
	}
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// payloadJSON {"attrs": {...}}；没有可写的属性返回 NULL。
func payloadJSON(a Attrs) sql.NullString {
	clean := CleanAttrs(a)
	if len(clean) == 0 {
		return sql.NullString{}
	}
	b, err := json.Marshal(map[string]any{"attrs": clean})
	if err != nil {
		return sql.NullString{}
	}
	return sql.NullString{String: string(b), Valid: true}
}

// ---------------- 便捷方法 ----------------

// Span 一个打开的区间。nil 安全。
type Span struct {
	w       *Writer
	task    string
	step    string
	attempt int
	extType string
	extRef  string
	runNo   int
	actor   string
	tail    string // dedupe_key 末尾追加（区分同一 attempt 下不同结局 / 原因）
	ended   sync.Once
}

// dedupe 该区间事件的 dedupe_key：没设 tail 时返回空（走默认 <kind>:<step>:<attempt>）。
func (s *Span) dedupe(kind string) string {
	if s.tail == "" {
		return ""
	}
	return TruncRunes(DedupeKey(kind, s.step, s.attempt)+":"+s.tail, maxDedupeRunes)
}

// SpanOpt 区间选项。
type SpanOpt func(*Span)

// WithExternal 区间对应的外部引用（网关调用：ExternalGatewayCall + 调用号）。
func WithExternal(typ, ref string) SpanOpt { return func(s *Span) { s.extType, s.extRef = typ, ref } }

// WithRunNo 网关调用的 run 序号。
func WithRunNo(n int) SpanOpt { return func(s *Span) { s.runNo = n } }

// WithDedupeSuffix 在 dedupe_key 末尾追加 tail（如结局状态 / 原因）：同一 attempt 下后来的不同结局不会被先到的吞掉。
func WithDedupeSuffix(tail string) SpanOpt { return func(s *Span) { s.tail = tail } }

// WithActor 操作者。
func WithActor(a string) SpanOpt { return func(s *Span) { s.actor = a } }

// Start 写 span_start 并返回区间；attempt≥0 用于重试（dedupe 末段）。
func (w *Writer) Start(ctx context.Context, task, step string, attempt int, attrs Attrs, opts ...SpanOpt) (s *Span) {
	s = &Span{w: w, task: task, step: step, attempt: attempt}
	defer recoverTrace("Start")
	for _, o := range opts {
		o(s)
	}
	w.Write(ctx, Event{TaskID: task, StepPath: step, Kind: KindSpanStart, Attempt: attempt, ExternalType: s.extType,
		ExternalRef: s.extRef, RunNo: s.runNo, Actor: s.actor, Dedupe: s.dedupe(KindSpanStart), Attrs: attrs})
	return s
}

// End 写 span_end（同一区间只写一次）。
func (s *Span) End(ctx context.Context, status string, f Fail, attrs Attrs) {
	defer recoverTrace("End")
	if s == nil || s.w == nil {
		return
	}
	s.ended.Do(func() {
		s.w.Write(ctx, Event{TaskID: s.task, StepPath: s.step, Kind: KindSpanEnd, Status: status, FailureClass: f.Class,
			FailureCode: f.Code, CheckCode: f.Check, Message: f.Message, Attempt: s.attempt, ExternalType: s.extType,
			ExternalRef: s.extRef, RunNo: s.runNo, Actor: s.actor, Dedupe: s.dedupe(KindSpanEnd), Attrs: attrs})
	})
}

// Finish 无状态地写 span_end：区间跨函数 / 跨进程（如排队、等待）时没法持有 Span，直接按 (task, step, attempt) 收尾。
func (w *Writer) Finish(ctx context.Context, task, step string, attempt int, status string, f Fail, attrs Attrs, opts ...SpanOpt) {
	defer recoverTrace("Finish")
	if w == nil {
		return
	}
	s := &Span{w: w, task: task, step: step, attempt: attempt}
	for _, o := range opts {
		o(s)
	}
	s.End(ctx, status, f, attrs)
}

// OK 以 ok 结束。
func (s *Span) OK(ctx context.Context, attrs Attrs) { s.End(ctx, StatusOK, Fail{}, attrs) }

// Failed 以 failed 结束。
func (s *Span) Failed(ctx context.Context, f Fail, attrs Attrs) { s.End(ctx, StatusFailed, f, attrs) }

// SetExternal 区间拿到外部引用后补上（只影响随后的 span_end）。
func (s *Span) SetExternal(typ, ref string) {
	defer recoverTrace("SetExternal")
	if s != nil {
		s.extType, s.extRef = typ, ref
	}
}

// Mark 写一条点事件。step 为空时写成任务级事件；seq 区分同一 step 下的多条。
func (w *Writer) Mark(ctx context.Context, task, step, message string, seq int, attrs Attrs) {
	defer recoverTrace("Mark")
	w.Write(ctx, Event{TaskID: task, StepPath: step, Kind: KindMark, Message: message, Seq: seq, Attrs: attrs})
}

// LinkGatewayCall 网关调用创建后立即写：gateway_call_linked。同一调用号只会有一条（dedupe 以调用号区分）。
func (w *Writer) LinkGatewayCall(ctx context.Context, task, step, callID string, attempt int, attrs Attrs) {
	defer recoverTrace("LinkGatewayCall")
	if callID == "" {
		return
	}
	w.Write(ctx, Event{TaskID: task, StepPath: step, Kind: KindGatewayCallLinked, Attempt: attempt,
		Dedupe:       KindGatewayCallLinked + ":" + step + ":" + callID,
		ExternalType: ExternalGatewayCall, ExternalRef: callID, Attrs: attrs})
}

// ---------------- 脱敏 ----------------

var (
	// 任何 scheme://... 整段换成 [url]（含查询串、签名、用户信息；全仓同一条规则）
	urlRe = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)
	// JWT 形态：eyJ<header>.<payload>[.<sig>]
	jwtRe = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}(?:\.[A-Za-z0-9_-]*)?`)
	// Bearer <token>
	bearRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{6,}`)
	// key=value / key: value / "key":"value" 形式的凭证
	credRe = regexp.MustCompile(`(?i)\b(authorization|token|access[_-]?token|refresh[_-]?token|secret|client[_-]?secret|password|passwd|pwd|api[_-]?key|access[_-]?key|private[_-]?key|signature|x-aig-signature|sk)\b["']?\s*[=:]\s*["']?[^\s"',;}&]+`)
	skRe   = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`)
)

// Sanitize 全仓唯一的脱敏器：scheme URL → [url]，JWT / Bearer / 各类凭证 → ***，换行压成空格，截断到 500 个字符（rune）。
func Sanitize(s string) string { return SanitizeN(s, maxMessageRunes) }

// SanitizeN 同 Sanitize，截断到 n 个字符。
func SanitizeN(s string, n int) string {
	if s == "" {
		return ""
	}
	s = urlRe.ReplaceAllString(s, "[url]")
	s = jwtRe.ReplaceAllString(s, "[jwt]")
	s = bearRe.ReplaceAllString(s, "Bearer ***")
	s = credRe.ReplaceAllString(s, "${1}=***")
	s = skRe.ReplaceAllString(s, "sk-***")
	s = strings.Join(strings.Fields(s), " ")
	return TruncRunes(s, n)
}

// TruncRunes 按字符（rune）截断，不会切坏多字节字符。
func TruncRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

// 属性名里出现这些片段的一律丢弃（防止误把内容 / 凭证放进来）。
var deniedKeyParts = []string{"prompt", "url", "uri", "secret", "token", "password", "content", "authorization", "signature", "text", "body"}

var attrKeyRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// CleanAttrs 只保留扁平、脱敏后的标量；非法键名、敏感键名、嵌套值丢弃；最多 32 个。
func CleanAttrs(a Attrs) map[string]any {
	if len(a) == 0 {
		return nil
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]any{}
	for _, k := range keys {
		if len(out) >= maxAttrs || !attrKeyRe.MatchString(k) || deniedKey(k) {
			continue
		}
		switch v := a[k].(type) {
		case nil:
		case string:
			if v = TruncRunes(Sanitize(v), maxAttrRunes); v != "" {
				out[k] = v
			}
		case bool:
			out[k] = v
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			out[k] = v
		case float32:
			out[k] = float64(v)
		case float64:
			out[k] = v
		case time.Duration:
			out[k] = v.Milliseconds()
		case fmt.Stringer:
			if rv := reflect.ValueOf(v); rv.Kind() == reflect.Ptr && rv.IsNil() {
				continue // typed-nil：调 String() 会 panic
			}
			out[k] = TruncRunes(Sanitize(v.String()), maxAttrRunes)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func deniedKey(k string) bool {
	for _, p := range deniedKeyParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

package core

// 服务端自动叠字：叠字不再依赖用户页面开着。
//
//	做同款提交（前端发智能体流之前）→ POST /overlay-intents 冻结「是否叠字 + 文字值 + 模板文字位版式」，键 = 本轮 X-Request-Id
//	→ 后台扫描：Java 的智能体工具产物落 afv_creation_task（request_params.source.runId = "run-" + requestId，
//	  会话内对失败图的重试任务带 retryOf 指回原任务）→ 产物入库（afv_asset，source_task_id 指向这些任务）后每张图建一条叠字作业
//	→ 作业行租约认领 → 复用 overlay.go 的渲染与登记（媒体服务 image/text → 转存素材库桶 → pkg/asset 登记叠字版）
//	→ 失败退避重试，次数用完落 failed 叠加记录（前端照旧「文字没加上，可重试」，PUT /overlays/{id} 手动重试）。
//
// 为什么在 Go 侧冻结而不读 Java：Java 的轮次快照只存渲染好的入参文本（inputs_block），没有结构化的文字值，
// 也收不到「本次给图片加字」开关；按约束不去拉用户当前设置，所以由提交方在提交那一刻登记，之后与页面无关。
// 版式也随意图冻结：提交后模板下架 / 改版式，这一轮的自动叠字仍按提交时的版式做。
//
// 不收费：全程只走媒体服务与素材登记，不占用户叠字配额（产物本身已按出图计费，一张图最多一条作业）。
// 与前端手动叠字并存：同一（用户, 模板, 成品图）只有一条叠加记录；作业开始前已有成功记录（前端已叠 / 用户改过字）就直接收尾，不覆盖。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

const (
	// ReasonOverlayAutoOff 自动叠字没开（Plaza.Overlay.Auto.Enabled=false）：7003，前端退回逐张手动叠字。
	ReasonOverlayAutoOff = "PLAZA_OVERLAY_AUTO_OFF"

	overlayJobPending = 0
	overlayJobDone    = 1
	overlayJobFailed  = 2

	overlayJobLease    = 5 * time.Minute
	overlayJobTimeout  = 3 * time.Minute
	overlayAutoTick    = 10 * time.Second
	overlayRunPrefix   = "run-" // Java：runId = "run-" + X-Request-Id（CreaftV1CoreAgentController.agentRunId）
	overlayDefaultCode = "agentImage"
	overlayScanTaskMax = 200
	overlayScanAssetMx = 100
	overlayErrMax      = 200
)

// overlayAutoBackoff 第 n 次失败后（n 从 1 起）到下次重试的等待；超出列表长度按最后一档。
var overlayAutoBackoff = []time.Duration{30 * time.Second, 2 * time.Minute, 10 * time.Minute, 30 * time.Minute}

func overlayBackoffAfter(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > len(overlayAutoBackoff) {
		attempt = len(overlayAutoBackoff)
	}
	return overlayAutoBackoff[attempt-1]
}

// overlayScanInterval 意图登记后多久再扫一次：前 30 分钟每 15 秒（一轮出图通常在这里面），3 小时内每分钟，之后每 5 分钟。
func overlayScanInterval(age time.Duration) time.Duration {
	switch {
	case age < 30*time.Minute:
		return 15 * time.Second
	case age < 3*time.Hour:
		return time.Minute
	default:
		return 5 * time.Minute
	}
}

var overlayRequestIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,60}$`)

// OverlayIntentInput POST /overlay-intents 的请求体。
type OverlayIntentInput struct {
	RequestID  string            `json:"requestId"`
	TemplateID int64             `json:"templateId"`
	Enabled    *bool             `json:"enabled"` // 缺省 = true（「给图片加字」默认开）
	Values     map[string]string `json:"values"`
}

// OverlayIntent 已冻结的叠字意图（对外）。
type OverlayIntent struct {
	ID         int64             `json:"id"`
	RequestID  string            `json:"requestId"`
	TemplateID int64             `json:"templateId"`
	Enabled    bool              `json:"enabled"`
	Values     map[string]string `json:"values"`
	CreatedAt  string            `json:"createdAt"`
}

// overlaySlotSnap 冻结进意图的文字位（原始 layout + 长度上限），作业时重新解析。
type overlaySlotSnap struct {
	Key    string          `json:"key"`
	Layout json.RawMessage `json:"layout"`
	MaxLen int             `json:"maxLength"`
}

func snapSlots(slots []overlaySlot) []overlaySlotSnap {
	out := make([]overlaySlotSnap, 0, len(slots))
	for _, sl := range slots {
		out = append(out, overlaySlotSnap{Key: sl.key, Layout: sl.raw, MaxLen: sl.maxLen})
	}
	return out
}

// slotsFromSnap 解析冻结的文字位；解析不出任何位 = 快照坏了（永久失败）。
func slotsFromSnap(raw []byte) ([]overlaySlot, error) {
	var snaps []overlaySlotSnap
	if err := json.Unmarshal(raw, &snaps); err != nil {
		return nil, fmt.Errorf("文字位快照不是合法 JSON: %w", err)
	}
	var out []overlaySlot
	for _, sn := range snaps {
		l, errs := ParseLayout(sn.Layout, "layout")
		if len(errs) > 0 || l == nil {
			continue
		}
		out = append(out, overlaySlot{key: sn.Key, layout: l, maxLen: sn.MaxLen, raw: sn.Layout})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("文字位快照里没有可用的版式")
	}
	return out, nil
}

// pickOverlaySource 素材行上用户看到的那张：有水印图（免费档）用水印图，否则原图；都不是 http(s) 地址则跳过。
// 与前端展示 / 手动叠字传的 sourceUrl 同一口径，叠加记录按它的哈希对得上。
func pickOverlaySource(resultURL, watermarkURL string) (string, bool) {
	for _, u := range []string{watermarkURL, resultURL} {
		if strings.TrimSpace(u) == "" {
			continue
		}
		if src, err := checkOverlaySource(u); err == nil {
			return src, true
		}
		return "", false // 用户看到的就是这张，但它不是可摄入的地址（私有引用等），不拿另一张顶替
	}
	return "", false
}

func (c OverlayAutoConfig) entryCodes() []string {
	if len(c.EntryCodes) == 0 {
		return []string{overlayDefaultCode}
	}
	return c.EntryCodes
}

func (c OverlayAutoConfig) maxAttempts() int {
	if c.MaxAttempts < 1 {
		return 5
	}
	return c.MaxAttempts
}

func (c OverlayAutoConfig) batch() int {
	if c.Batch < 1 {
		return 10
	}
	return c.Batch
}

func (c OverlayAutoConfig) matchWindow() time.Duration {
	if c.MatchWindowMinutes < 1 {
		return 24 * time.Hour
	}
	return time.Duration(c.MatchWindowMinutes) * time.Minute
}

// OverlayAutoReady 自动叠字是否可用（叠字依赖已注入且开关打开）；gateway-api 据此决定起不起后台循环。
func (s *Service) OverlayAutoReady() bool {
	return s != nil && s.db != nil && s.ovMedia != nil && s.cfg.Overlay.Auto.Enabled
}

type overlayIntentRow struct {
	ID         int64     `bun:"id"`
	UserID     int64     `bun:"user_id"`
	RequestID  string    `bun:"request_id"`
	RunID      string    `bun:"run_id"`
	TemplateID int64     `bun:"template_id"`
	Enabled    int       `bun:"enabled"`
	ValuesJSON []byte    `bun:"values_json"`
	SlotsJSON  []byte    `bun:"slots_json"`
	CreateTime time.Time `bun:"create_time"`
}

func (r *overlayIntentRow) intent() OverlayIntent {
	vals := map[string]string{}
	if len(r.ValuesJSON) > 0 {
		_ = json.Unmarshal(r.ValuesJSON, &vals)
	}
	return OverlayIntent{ID: r.ID, RequestID: r.RequestID, TemplateID: r.TemplateID, Enabled: r.Enabled == 1,
		Values: vals, CreatedAt: r.CreateTime.Format(timeLayout)}
}

const overlayIntentCols = "id, user_id, request_id, run_id, template_id, enabled, values_json, slots_json, create_time"

// CreateOverlayIntent 冻结本轮做同款的叠字意图。同一（用户, requestId）先到先得：重复登记原样返回已冻结的那条
// （同一 requestId 的重发是同一轮，不能改字、也不能翻开关）。enabled=false 也登记（明确「本次不加字」），后台不会为它叠字。
func (s *Service) CreateOverlayIntent(ctx context.Context, userID int64, in OverlayIntentInput) (*OverlayIntent, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if s.ovMedia == nil {
		return nil, errOverlayNotReady()
	}
	if !s.cfg.Overlay.Auto.Enabled {
		return nil, xerr.NewReason(xerr.NotReady, ReasonOverlayAutoOff, "自动加字暂未开放")
	}
	reqID := strings.TrimSpace(in.RequestID)
	if reqID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少 requestId")
	}
	if !overlayRequestIDRe.MatchString(reqID) {
		return nil, xerr.Param("requestId 格式不合法")
	}
	if in.TemplateID <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少模板")
	}
	if old, err := s.overlayIntentByRequest(ctx, userID, reqID); err != nil {
		return nil, err
	} else if old != nil {
		o := old.intent()
		return &o, nil
	}
	enabled := in.Enabled == nil || *in.Enabled
	row := &overlayIntentRow{UserID: userID, RequestID: reqID, RunID: overlayRunPrefix + reqID, TemplateID: in.TemplateID}
	if enabled {
		slots, err := s.overlaySlots(ctx, in.TemplateID)
		if err != nil {
			return nil, err
		}
		vals, err := cleanOverlayValues(slots, in.Values)
		if err != nil {
			return nil, err
		}
		row.Enabled = 1
		row.ValuesJSON, _ = json.Marshal(vals)
		row.SlotsJSON, _ = json.Marshal(snapSlots(slots))
	}
	if s.ovIntentLim != nil && s.ovIntentLim.Take(ctx, userID) != userquota.Allowed {
		return nil, xerr.NewReason(xerr.Conflict, ReasonOverlayTooFrequent, "加字太频繁了，请稍后再试")
	}
	var vj, sj any // 关闭的意图不存文字与版式（NULL）
	if row.Enabled == 1 {
		vj, sj = string(row.ValuesJSON), string(row.SlotsJSON)
	}
	if _, err := s.db.NewRaw("INSERT IGNORE INTO afv_plaza_overlay_intent (user_id, request_id, run_id, template_id, enabled, values_json, slots_json, next_scan_at, expire_at) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, NOW(), DATE_ADD(NOW(), INTERVAL ? MINUTE))",
		row.UserID, row.RequestID, row.RunID, row.TemplateID, row.Enabled, vj, sj, int(s.cfg.Overlay.Auto.matchWindow().Minutes())).Exec(ctx); err != nil {
		return nil, err
	}
	saved, err := s.overlayIntentByRequest(ctx, userID, reqID) // 并发登记时读回先到的那条
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, xerr.New(xerr.ServerError, "叠字意图保存失败")
	}
	o := saved.intent()
	return &o, nil
}

func (s *Service) overlayIntentByRequest(ctx context.Context, userID int64, reqID string) (*overlayIntentRow, error) {
	var rows []overlayIntentRow
	if err := s.db.NewRaw("SELECT "+overlayIntentCols+" FROM afv_plaza_overlay_intent WHERE user_id = ? AND request_id = ? LIMIT 1",
		userID, reqID).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

// RunOverlayAutoLoop 后台循环（gateway-api 起一个 goroutine）：每 10 秒扫一轮意图、处理一批作业；ctx 取消即退出。
func (s *Service) RunOverlayAutoLoop(ctx context.Context) {
	if !s.OverlayAutoReady() {
		return
	}
	tick := time.NewTicker(overlayAutoTick)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if _, err := s.ScanOverlayIntents(ctx); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 自动叠字扫描意图本轮失败: %v", err)
		}
		if _, err := s.RunOverlayJobs(ctx); err != nil {
			logx.WithContext(ctx).Errorf("plaza: 自动叠字作业本轮失败: %v", err)
		}
	}
}

// ScanOverlayIntents 一轮：认领到期的有效意图，找它这一轮已入库的产物，每张图建一条作业（已有则忽略）。返回新建的作业数。
func (s *Service) ScanOverlayIntents(ctx context.Context) (int, error) {
	if !s.OverlayAutoReady() {
		return 0, nil
	}
	var rows []overlayIntentRow
	if err := s.db.NewRaw("SELECT "+overlayIntentCols+" FROM afv_plaza_overlay_intent "+
		"WHERE enabled = 1 AND expire_at > NOW() AND next_scan_at <= NOW() ORDER BY next_scan_at LIMIT ?",
		s.cfg.Overlay.Auto.batch()).Scan(ctx, &rows); err != nil {
		return 0, err
	}
	total := 0
	for i := range rows {
		if ctx.Err() != nil {
			break
		}
		r := &rows[i]
		// 认领：条件 UPDATE 推后下次扫描时间，多副本只有一个拿到这一轮
		res, err := s.db.NewRaw("UPDATE afv_plaza_overlay_intent SET next_scan_at = DATE_ADD(NOW(), INTERVAL ? SECOND), scan_count = scan_count + 1 "+
			"WHERE id = ? AND enabled = 1 AND next_scan_at <= NOW()", int(overlayScanInterval(s.now().Sub(r.CreateTime)).Seconds()), r.ID).Exec(ctx)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza: 自动叠字认领意图 %d 失败: %v", r.ID, err)
			continue
		}
		if n, _ := res.RowsAffected(); n != 1 {
			continue
		}
		n, err := s.scanOverlayIntent(ctx, r)
		if err != nil {
			logx.WithContext(ctx).Errorf("plaza: 自动叠字扫描意图 %d 失败（下一轮再试）: %v", r.ID, err)
			continue
		}
		total += n
	}
	return total, nil
}

type overlayTurnRef struct {
	PlazaTemplateID *int64 `bun:"plaza_template_id"`
}

type overlayTaskRef struct {
	TaskID       string  `bun:"task_id"`
	ParentTaskID *string `bun:"parent_task_id"`
}

type overlayAssetRef struct {
	ID           int64   `bun:"id"`
	ResultURL    *string `bun:"result_url"`
	WatermarkURL *string `bun:"watermark_url"`
}

// scanOverlayIntent 找一条意图的产物并建作业。
func (s *Service) scanOverlayIntent(ctx context.Context, r *overlayIntentRow) (int, error) {
	// 防错配：这一轮的 Java 轮次做的不是意图里的模板（前端登记错了）→ 关掉意图，不给别的模板的图叠字
	var turns []overlayTurnRef
	if err := s.db.NewRaw("SELECT plaza_template_id FROM afv_agent_turn WHERE user_id = ? AND run_id = ? LIMIT 1", r.UserID, r.RunID).Scan(ctx, &turns); err != nil {
		return 0, err
	}
	if len(turns) > 0 && (turns[0].PlazaTemplateID == nil || *turns[0].PlazaTemplateID != r.TemplateID) {
		got := int64(0)
		if turns[0].PlazaTemplateID != nil {
			got = *turns[0].PlazaTemplateID
		}
		logx.WithContext(ctx).Errorf("plaza: 自动叠字意图作废 判据=TURN_TEMPLATE_MISMATCH intent=%d user=%d run=%s 意图模板=%d 轮次模板=%d",
			r.ID, r.UserID, r.RunID, r.TemplateID, got)
		_, err := s.db.NewRaw("UPDATE afv_plaza_overlay_intent SET enabled = 0 WHERE id = ?", r.ID).Exec(ctx)
		return 0, err
	}
	since := r.CreateTime.Add(-5 * time.Minute) // 容忍两端时钟差
	codes := s.cfg.Overlay.Auto.entryCodes()
	var tasks []overlayTaskRef
	if err := s.db.NewRaw("SELECT task_id, parent_task_id FROM afv_creation_task WHERE user_id = ? AND deleted = 0 AND create_time >= ? "+
		"AND JSON_UNQUOTE(JSON_EXTRACT(request_params, '$.source.runId')) = ? LIMIT ?",
		r.UserID, since, r.RunID, overlayScanTaskMax).Scan(ctx, &tasks); err != nil {
		return 0, err
	}
	if len(tasks) == 0 {
		return 0, nil
	}
	ids := overlayTaskIDs(tasks)
	// 会话里对失败图的重试：新任务的 request_params.retryOf 指回原任务（一跳）
	var retries []overlayTaskRef
	if err := s.db.NewRaw("SELECT task_id, parent_task_id FROM afv_creation_task WHERE user_id = ? AND deleted = 0 AND create_time >= ? "+
		"AND JSON_UNQUOTE(JSON_EXTRACT(request_params, '$.retryOf')) IN (?) LIMIT ?",
		r.UserID, since, bun.In(ids), overlayScanTaskMax).Scan(ctx, &retries); err != nil {
		return 0, err
	}
	ids = append(ids, overlayTaskIDs(retries)...)
	// 产物入库后才排队：素材行（Java 的 source_task_id = 分组父任务，没有父任务时 = 任务自身）；排除叠字版自己与视频
	var assets []overlayAssetRef
	if err := s.db.NewRaw("SELECT id, result_url, watermark_url FROM afv_asset WHERE user_id = ? AND deleted = 0 AND source_task_id IN (?) "+
		"AND entry_code IN (?) AND type <> 'video' "+
		"AND COALESCE(JSON_UNQUOTE(JSON_EXTRACT(IF(JSON_VALID(properties), properties, '{}'), '$.textVariant')), '') <> 'overlay' ORDER BY id LIMIT ?",
		r.UserID, bun.In(ids), bun.In(codes), overlayScanAssetMx).Scan(ctx, &assets); err != nil {
		return 0, err
	}
	created := 0
	for _, a := range assets {
		src, ok := pickOverlaySource(deref(a.ResultURL), deref(a.WatermarkURL))
		if !ok {
			continue
		}
		res, err := s.db.NewRaw("INSERT IGNORE INTO afv_plaza_overlay_job (user_id, intent_id, template_id, asset_id, source_url, source_hash, state, attempts) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, 0)", r.UserID, r.ID, r.TemplateID, a.ID, src, sourceHash(src), overlayJobPending).Exec(ctx)
		if err != nil {
			return created, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			created++
		}
	}
	if created > 0 {
		logx.WithContext(ctx).Infof("plaza: 自动叠字排队 %d 张 intent=%d user=%d template=%d", created, r.ID, r.UserID, r.TemplateID)
	}
	return created, nil
}

// overlayTaskIDs 任务号 + 分组父任务号（去重）。
func overlayTaskIDs(tasks []overlayTaskRef) []string {
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, t := range tasks {
		add(t.TaskID)
		if t.ParentTaskID != nil {
			add(*t.ParentTaskID)
		}
	}
	return out
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type overlayJobRow struct {
	ID         int64  `bun:"id"`
	UserID     int64  `bun:"user_id"`
	IntentID   int64  `bun:"intent_id"`
	TemplateID int64  `bun:"template_id"`
	SourceURL  string `bun:"source_url"`
	SourceHash string `bun:"source_hash"`
	Attempts   int    `bun:"attempts"`
	ValuesJSON []byte `bun:"values_json"`
	SlotsJSON  []byte `bun:"slots_json"`
}

// RunOverlayJobs 一轮：先把卡死的作业收尾成最终失败，再认领至多 Batch 条到期作业逐个处理。返回本轮处理数。
func (s *Service) RunOverlayJobs(ctx context.Context) (int, error) {
	if !s.OverlayAutoReady() {
		return 0, nil
	}
	maxA := s.cfg.Overlay.Auto.maxAttempts()
	if err := s.reapOverlayJobs(ctx, maxA); err != nil {
		return 0, err
	}
	var ids []int64
	if err := s.db.NewRaw("SELECT id FROM afv_plaza_overlay_job WHERE state = ? AND attempts < ? "+
		"AND (lease_until IS NULL OR lease_until < NOW()) AND (next_at IS NULL OR next_at <= NOW()) ORDER BY id LIMIT ?",
		overlayJobPending, maxA, s.cfg.Overlay.Auto.batch()).Scan(ctx, &ids); err != nil {
		return 0, err
	}
	done := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if s.runOverlayJob(ctx, id, maxA) {
			done++
		}
	}
	return done, nil
}

// reapOverlayJobs 次数已满、没有活租约还停在待处理的作业（进程反复崩溃 / 超时留下的）→ 最终失败，并落 failed 叠加记录供手动重试。
func (s *Service) reapOverlayJobs(ctx context.Context, maxA int) error {
	var rows []overlayJobRow
	if err := s.db.NewRaw("SELECT j.id, j.user_id, j.intent_id, j.template_id, j.source_url, j.source_hash, j.attempts, i.values_json, i.slots_json "+
		"FROM afv_plaza_overlay_job j JOIN afv_plaza_overlay_intent i ON i.id = j.intent_id "+
		"WHERE j.state = ? AND j.attempts >= ? AND (j.lease_until IS NULL OR j.lease_until < NOW()) ORDER BY j.id LIMIT 20",
		overlayJobPending, maxA).Scan(ctx, &rows); err != nil {
		return err
	}
	for i := range rows {
		j := &rows[i]
		reason := "叠字多次中断或超时"
		ovID := s.saveAutoFailure(ctx, j, reason)
		if _, err := s.db.NewRaw("UPDATE afv_plaza_overlay_job SET state = ?, last_error = ?, overlay_id = ?, lease_until = NULL, lease_token = NULL "+
			"WHERE id = ? AND state = ? AND (lease_until IS NULL OR lease_until < NOW())",
			overlayJobFailed, reason, nullID(ovID), j.ID, overlayJobPending).Exec(ctx); err != nil {
			return err
		}
		logx.WithContext(ctx).Errorf("plaza: 自动叠字最终失败 判据=ATTEMPTS_EXHAUSTED_NO_LEASE job=%d user=%d 次数=%d", j.ID, j.UserID, j.Attempts)
	}
	return nil
}

func nullID(id int64) any {
	if id <= 0 {
		return nil
	}
	return id
}

// runOverlayJob 认领并处理一条作业；认领不到（别的副本抢了）返回 false。
func (s *Service) runOverlayJob(ctx context.Context, id int64, maxA int) bool {
	token := newLeaseToken()
	res, err := s.db.NewRaw("UPDATE afv_plaza_overlay_job SET lease_token = ?, lease_until = DATE_ADD(NOW(), INTERVAL ? SECOND), attempts = attempts + 1 "+
		"WHERE id = ? AND state = ? AND attempts < ? AND (lease_until IS NULL OR lease_until < NOW()) AND (next_at IS NULL OR next_at <= NOW())",
		token, int(overlayJobLease.Seconds()), id, overlayJobPending, maxA).Exec(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字认领作业 %d 失败: %v", id, err)
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false
	}
	var rows []overlayJobRow
	if err := s.db.NewRaw("SELECT j.id, j.user_id, j.intent_id, j.template_id, j.source_url, j.source_hash, j.attempts, i.values_json, i.slots_json "+
		"FROM afv_plaza_overlay_job j JOIN afv_plaza_overlay_intent i ON i.id = j.intent_id WHERE j.id = ? AND j.lease_token = ? LIMIT 1",
		id, token).Scan(ctx, &rows); err != nil || len(rows) == 0 {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字读取作业 %d 失败（租约到期后重新认领）: %v", id, err)
		return false
	}
	jctx, cancel := context.WithTimeout(ctx, overlayJobTimeout)
	defer cancel()
	out := s.processOverlayJob(jctx, &rows[0], maxA)
	s.finishOverlayJob(ctx, &rows[0], token, out)
	return true
}

// overlayJobOutcome 一次作业尝试的结果。
type overlayJobOutcome struct {
	state     int    // 收尾后的状态：done / failed / pending（退避重试）
	overlayID int64  // 关联的叠加记录
	err       string // 最近一次失败原因（成功也可能带「素材登记未完成」备注）
	retryIn   time.Duration
}

// processOverlayJob 一次尝试：
//   - 已有成功叠加记录（前端已叠 / 用户已改字）→ 不重渲染，只补素材登记；
//   - 否则按意图冻结的版式与文字渲染（不扣配额、不验所有权：来源就是该用户自己的素材行）；
//   - 中间失败不落 failed 记录、退避重试；最后一次 / 永久失败落 failed 记录供手动重试。
func (s *Service) processOverlayJob(ctx context.Context, j *overlayJobRow, maxA int) overlayJobOutcome {
	final := j.Attempts >= maxA // 认领时已 +1，这里就是本次是第几次
	retry := func(reason string) overlayJobOutcome {
		if final {
			return overlayJobOutcome{state: overlayJobFailed, overlayID: s.saveAutoFailure(ctx, j, reason), err: reason}
		}
		return overlayJobOutcome{state: overlayJobPending, err: reason, retryIn: overlayBackoffAfter(j.Attempts)}
	}
	existing, err := s.overlayBySource(ctx, j.UserID, j.TemplateID, j.SourceHash)
	if err != nil {
		return retry("读取叠加记录失败")
	}
	if existing != nil && existing.Status == overlayStatusOK && existing.RenderedURL != nil {
		return s.afterOverlayOK(ctx, existing, final, j.Attempts, true)
	}
	slots, serr := slotsFromSnap(j.SlotsJSON)
	vals := map[string]string{}
	if serr == nil {
		if err := json.Unmarshal(j.ValuesJSON, &vals); err != nil {
			serr = err
		}
	}
	if serr == nil {
		vals, serr = cleanOverlayValues(slots, vals)
	}
	if serr != nil { // 快照坏了：重试也不会好
		logx.WithContext(ctx).Errorf("plaza: 自动叠字终止 判据=INTENT_SNAPSHOT_INVALID job=%d intent=%d: %v", j.ID, j.IntentID, serr)
		reason := "叠字意图里的文字或版式无效"
		return overlayJobOutcome{state: overlayJobFailed, overlayID: s.saveAutoFailure(ctx, j, reason), err: reason}
	}
	row := existing
	if row == nil {
		row = &overlayRow{UserID: j.UserID, TemplateID: j.TemplateID, SourceURL: j.SourceURL, SourceHash: j.SourceHash}
	}
	rec, ferr, err := s.renderOverlayOpt(ctx, row, slots, vals, false, true)
	if errors.Is(err, errOverlayTaken) {
		// 渲染期间用户手动叠了字：放弃本次渲染结果，按已有成功记录收尾（只补素材登记）
		cur, rerr := s.overlayBySource(ctx, j.UserID, j.TemplateID, j.SourceHash)
		if rerr != nil || cur == nil {
			return overlayJobOutcome{state: overlayJobPending, err: "读取叠加记录失败", retryIn: overlayBackoffAfter(j.Attempts)}
		}
		return s.afterOverlayOK(ctx, cur, final, j.Attempts, true)
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字保存失败 job=%d: %v", j.ID, err)
		return retry("叠加记录保存失败")
	}
	if ferr != nil {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字失败 job=%d 第 %d/%d 次 永久=%v 判据=%s: %v", j.ID, j.Attempts, maxA, ferr.permanent, ferr.reason, ferr.cause)
		if ferr.permanent {
			return overlayJobOutcome{state: overlayJobFailed, overlayID: s.saveAutoFailure(ctx, j, ferr.reason), err: ferr.reason}
		}
		return retry(ferr.reason)
	}
	saved, err := s.overlayByID(ctx, j.UserID, rec.ID)
	if err != nil {
		return overlayJobOutcome{state: overlayJobPending, overlayID: rec.ID, err: "读取叠加记录失败", retryIn: overlayBackoffAfter(j.Attempts)}
	}
	return s.afterOverlayOK(ctx, saved, final, j.Attempts, false) // 渲染里刚登记过一次，没登记上就退避再补
}

// afterOverlayOK 叠字已成功：素材登记完成（或不登记）即收尾；没登记上（无字版素材行还没同步等）退避再补，
// 次数用完也按完成收尾（叠字本身已成功，之后读取时 healOverlayAssets 还会补登记）。
func (s *Service) afterOverlayOK(ctx context.Context, row *overlayRow, final bool, attempt int, trySync bool) overlayJobOutcome {
	if s.ovAssets == nil || row.AssetSynced == 1 || (trySync && s.syncOverlayAsset(ctx, row, "")) {
		return overlayJobOutcome{state: overlayJobDone, overlayID: row.ID}
	}
	if final {
		return overlayJobOutcome{state: overlayJobDone, overlayID: row.ID, err: "叠字版登记素材库未完成（读取时补登记）"}
	}
	return overlayJobOutcome{state: overlayJobPending, overlayID: row.ID, err: "叠字版登记素材库未完成", retryIn: overlayBackoffAfter(attempt)}
}

// saveAutoFailure 最终失败：落（或保留）一条 failed 叠加记录，前端照旧显示「文字没加上，可重试」并可 PUT 手动重试。
// 已有记录（成功的绝不覆盖；失败的只更新原因）。返回记录 id，落库失败返回 0。
func (s *Service) saveAutoFailure(ctx context.Context, j *overlayJobRow, reason string) int64 {
	existing, err := s.overlayBySource(ctx, j.UserID, j.TemplateID, j.SourceHash)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字落失败记录前读取失败 job=%d: %v", j.ID, err)
		return 0
	}
	if existing != nil && existing.Status == overlayStatusOK {
		return existing.ID
	}
	row := existing
	if row == nil {
		vals := j.ValuesJSON
		if len(vals) == 0 || !json.Valid(vals) {
			vals = []byte("{}")
		}
		row = &overlayRow{UserID: j.UserID, TemplateID: j.TemplateID, SourceURL: j.SourceURL, SourceHash: j.SourceHash, ValuesJSON: vals}
	}
	r := clipRunes(reason, overlayErrMax)
	row.Status, row.FailReason, row.AssetSynced = overlayStatusFailed, &r, 0
	// 同一事务复查：读取之后用户手动叠成功了就保留那条成功记录，绝不用失败覆盖
	taken, err := s.saveAutoOverlayRow(ctx, row)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字落失败记录失败 job=%d: %v", j.ID, err)
		return 0
	}
	if taken != nil {
		return taken.ID
	}
	return row.ID
}

func clipRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// finishOverlayJob 写回作业状态；围栏 = 仍持有本次租约 token（被 reap / 重新认领后旧结果作废）。
func (s *Service) finishOverlayJob(ctx context.Context, j *overlayJobRow, token string, out overlayJobOutcome) {
	var errMsg any
	if out.err != "" {
		errMsg = clipRunes(out.err, overlayErrMax)
	}
	q := "UPDATE afv_plaza_overlay_job SET state = ?, overlay_id = COALESCE(?, overlay_id), last_error = ?, lease_until = NULL, lease_token = NULL, "
	args := []any{out.state, nullID(out.overlayID), errMsg}
	if out.state == overlayJobPending {
		q += "next_at = DATE_ADD(NOW(), INTERVAL ? SECOND) "
		args = append(args, int(out.retryIn.Seconds()))
	} else {
		q += "next_at = NULL "
	}
	q += "WHERE id = ? AND lease_token = ?"
	args = append(args, j.ID, token)
	res, err := s.db.NewRaw(q, args...).Exec(ctx)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字写回作业 %d 失败（租约到期后重做；已有成功记录时会直接收尾）: %v", j.ID, err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		logx.WithContext(ctx).Infof("plaza: 自动叠字结果作废 判据=LEASE_LOST job=%d", j.ID)
		return
	}
	if out.state == overlayJobFailed {
		logx.WithContext(ctx).Errorf("plaza: 自动叠字最终失败 job=%d user=%d template=%d 次数=%d 原因=%s（已落 failed 叠加记录，可手动重试）",
			j.ID, j.UserID, j.TemplateID, j.Attempts, out.err)
	}
}

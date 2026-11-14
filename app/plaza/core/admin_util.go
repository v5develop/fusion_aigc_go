package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/app/plaza/model"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// Operator 当前操作人（审计用）。
type Operator struct {
	ID        int64
	Name      string
	IP        string
	RequestID string
	// CanPublish 调用方已核过当前管理员有发布权限（plaza:publish；后台 handler 按 middleware.Allowed 填）。
	// 改变已发布 / 已有行业模板的行业关系等同于改变线上可见范围，没有它时拒绝（见 requireIndustryRelationPerm）。
	CanPublish bool
}

// PermPublish 发布权限码（与后台中间件 routePerm 同一个码）。
const PermPublish = "plaza:publish"

// industryChangeNeedsPublish 这次行业关系变更是否要求发布权限：关系确有变化，且模板已发布或变更前已挂行业。
// 未发布、也还没挂任何行业的模板（草稿首次归类）不受影响。
func industryChangeNeedsPublish(state string, before, after []int64) bool {
	if sameIDSet(before, after) {
		return false
	}
	return state == model.StatePublished || len(before) > 0
}

// requireIndustryRelationPerm 见 industryChangeNeedsPublish；需要而 op 没有发布权限 → Forbidden（事务内调用，返回错误即回滚）。
func requireIndustryRelationPerm(ctx context.Context, op Operator, t *model.Template, before, after []int64) error {
	if op.CanPublish || !industryChangeNeedsPublish(t.State, before, after) {
		return nil
	}
	logx.WithContext(ctx).Infof("plaza: 改行业关系被拒 判据=NEED_PLAZA_PUBLISH admin=%d template=%d state=%s 原行业=%v 新行业=%v",
		op.ID, t.ID, t.State, before, after)
	return xerr.New(xerr.Forbidden, "没有权限："+PermPublish+"（已发布或已归类的模板改行业需要发布权限）")
}

// 审计动作 / 资源类型。
const (
	ResTemplate   = "TEMPLATE"
	ResIndustry   = "INDUSTRY"
	ResChannel    = "CHANNEL"
	ResSample     = "SAMPLE"
	ResSuggestion = "SUGGESTION"
	ResSearchMiss = "SEARCH_MISS"
)

var industryCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{1,63}$`)

// validateIndustryCode 行业 code：小写字母开头，字母数字下划线，2~64 字符，不得为保留字 ALL，不得含删除标记。
func validateIndustryCode(code string) error {
	if !industryCodeRe.MatchString(code) {
		return errParam("行业 code 只能是小写字母开头的字母、数字、下划线，长度 2~64")
	}
	if strings.EqualFold(code, ReservedAll) {
		return xerr.NewReason(xerr.ParamError, ReasonIndustryCodeDuplicate, "ALL 是保留字，不能作为行业 code")
	}
	return nil
}

// requireWrite 模板写权属开关：交接前 Java 仍是 afv_compat_plaza_template 的写者。
func (s *Service) requireWrite() error {
	if s.db == nil {
		return ErrNotReady()
	}
	if !s.cfg.AdminWriteEnabled {
		return xerr.NewReason(xerr.NotReady, ReasonWriteDisabled, "模板管理尚未迁移到新后台，请先在原后台操作")
	}
	return nil
}

// handover 交接期：写权属还在 Java（AdminWriteEnabled=false）。此时只放行 A 类写（Go 独占的表 / 列，见 docs/api/api-plaza.md「交接期可写范围」），
// 且 A 类写不得改动 Java 也写的列——包括 update_time（见 keepUpdateTime）。
func (s *Service) handover() bool { return !s.cfg.AdminWriteEnabled }

// keepUpdateTime A 类写模板行时是否冻结 update_time。该列 DDL 带 ON UPDATE CURRENT_TIMESTAMP，任何实际改值的 UPDATE 都会把它推进；
// 交接期它是 Java 的列（Java 管理端列表展示它、MyBatis-Plus 也在 Java 写时填充它），Go 的 A 类写不应让它动，
// 所以显式 `update_time = update_time` 抑制自动更新，并发判据改用 revision。
func (s *Service) keepUpdateTime() bool { return s.handover() }

func (s *Service) requireDB() error {
	if s.db == nil {
		return ErrNotReady()
	}
	return nil
}

// newBatchID 批量操作各行共用的 id（UUID v4 形态，36 字符）。
func newBatchID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// recomputeEnabled enabled 是派生列：当前是否对用户可见（Java 的老 v1 列表与 Agent 快照只认 enabled=1）。
// 任何写模板的路径都必须经它；边界任务也调同一函数。
func recomputeEnabled(state string, onlineAt, offlineAt sql.NullTime, deleted bool, now time.Time) int8 {
	if deleted || state != model.StatePublished {
		return 0
	}
	if onlineAt.Valid && now.Before(onlineAt.Time) {
		return 0
	}
	if offlineAt.Valid && !now.Before(offlineAt.Time) {
		return 0
	}
	return 1
}

// parseAdminTime 后台时间入参：空串 = 清空；支持 RFC3339 与 "2006-01-02 15:04:05" / "2006-01-02T15:04:05"（按北京时间）。
func parseAdminTime(s string) (sql.NullTime, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullTime{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return sql.NullTime{Time: t, Valid: true}, nil
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05", "2006-01-02 15:04"} {
		if t, err := time.ParseInLocation(layout, s, cst); err == nil {
			return sql.NullTime{Time: t, Valid: true}, nil
		}
	}
	return sql.NullTime{}, errParam("时间格式不对：" + s)
}

func timeStr(n sql.NullTime) *string {
	if !n.Valid {
		return nil
	}
	s := n.Time.In(cst).Format(timeLayout)
	return &s
}

func nullStr(p *string) sql.NullString {
	if p == nil {
		return sql.NullString{}
	}
	v := strings.TrimSpace(*p)
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func strPtr(n sql.NullString) *string {
	if !n.Valid {
		return nil
	}
	s := n.String
	return &s
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// ---------------------------------------------------------------- 审计

// bigFieldLimit 超过它的字符串字段在 diff 里只记长度与哈希（快照里保留全文）。
const bigFieldLimit = 200

// diffMaps 字段级变化 {field:{from,to}}；大字段只记长度与哈希。
func diffMaps(before, after map[string]any) map[string]any {
	out := map[string]any{}
	keys := map[string]bool{}
	for k := range before {
		keys[k] = true
	}
	for k := range after {
		keys[k] = true
	}
	for k := range keys {
		b, a := normalizeJSON(before[k]), normalizeJSON(after[k])
		if jsonEqual(b, a) {
			continue
		}
		out[k] = map[string]any{"from": shrink(b), "to": shrink(a)}
	}
	return out
}

func normalizeJSON(v any) any {
	if v == nil {
		return nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		var x any
		if json.Unmarshal(raw, &x) == nil {
			return x
		}
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	var x any
	_ = json.Unmarshal(b, &x)
	return x
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

func shrink(v any) any {
	if v == nil {
		return nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case map[string]any, []any:
		b, _ := json.Marshal(x)
		s = string(b)
		if len(s) <= bigFieldLimit {
			return v
		}
	default:
		return v
	}
	if len(s) <= bigFieldLimit {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return map[string]any{"len": len(s), "sha256": hex.EncodeToString(sum[:8])}
}

// audit 在同一事务里写一条审计（失败整体回滚，不出现「改了没记」）。before / after 为 nil 表示创建 / 删除。
func (s *Service) audit(ctx context.Context, tx bun.IDB, op Operator, action, rtype, rid, batchID string, before, after map[string]any) error {
	row := model.AuditLog{Action: action, ResourceType: rtype, Status: "SUCCESS"}
	if op.ID > 0 {
		row.OperatorID = sql.NullInt64{Int64: op.ID, Valid: true}
	}
	row.OperatorName = nullStr(&op.Name)
	row.ResourceID = nullStr(&rid)
	row.BatchID = nullStr(&batchID)
	row.RequestID = nullStr(&op.RequestID)
	row.ClientIP = nullStr(&op.IP)
	if before != nil {
		row.BeforeJSON, _ = json.Marshal(before)
	}
	if after != nil {
		row.AfterJSON, _ = json.Marshal(after)
	}
	if before != nil && after != nil {
		row.DiffJSON, _ = json.Marshal(diffMaps(before, after))
	}
	_, err := tx.NewInsert().Model(&row).Exec(ctx)
	return err
}

// AuditEntry 审计查询行。
type AuditEntry struct {
	ID           int64           `json:"id"`
	OperatorID   *int64          `json:"operatorId"`
	OperatorName *string         `json:"operatorName"`
	Action       string          `json:"action"`
	ResourceType string          `json:"resourceType"`
	ResourceID   *string         `json:"resourceId"`
	BatchID      *string         `json:"batchId"`
	Before       json.RawMessage `json:"before"`
	After        json.RawMessage `json:"after"`
	Diff         json.RawMessage `json:"diff"`
	Status       string          `json:"status"`
	RequestID    *string         `json:"requestId"`
	ClientIP     *string         `json:"clientIp"`
	CreateTime   string          `json:"createTime"`
}

// AuditFilter 审计检索条件。
type AuditFilter struct {
	ResourceType, ResourceID, Action string
	// OperatorName 按操作人姓名模糊匹配：审计行里记的 operator_name，或关联管理员表（afv_admin_user）的 real_name / username。
	// BatchID 按批量操作的批次 id 精确检索。Action / ResourceType 含 * 时按通配匹配（见 auditMatch）。
	OperatorName, BatchID string
	OperatorID            int64
	From, To              string
	Page, Size            int
}

// AuditPage 审计分页。
type AuditPage struct {
	Total int          `json:"total"`
	Page  int          `json:"page"`
	Size  int          `json:"size"`
	Items []AuditEntry `json:"items"`
}

// AdminAudit 单对象历史 / 全局检索（需 plaza:audit）。
func (s *Service) AdminAudit(ctx context.Context, f AuditFilter) (*AuditPage, error) {
	if err := s.requireDB(); err != nil {
		return nil, err
	}
	page, size := normPage(f.Page, f.Size, 20)
	var rows []model.AuditLog
	q := s.db.NewSelect().Model(&rows)
	if v := strings.TrimSpace(f.ResourceType); v != "" {
		cond, args := auditMatch("al.resource_type", v)
		q = q.Where(cond, args...)
	}
	if f.ResourceID != "" {
		q = q.Where("al.resource_id = ?", f.ResourceID)
	}
	if v := strings.TrimSpace(f.Action); v != "" {
		cond, args := auditMatch("al.action", v)
		q = q.Where(cond, args...)
	}
	if v := strings.TrimSpace(f.BatchID); v != "" {
		q = q.Where("al.batch_id = ?", v)
	}
	if v := strings.TrimSpace(f.OperatorName); v != "" {
		like := "%" + likeEscape(v) + "%"
		q = q.Where("(al.operator_name LIKE ? OR al.operator_id IN (SELECT id FROM afv_admin_user WHERE real_name LIKE ? OR username LIKE ?))", like, like, like)
	}
	if f.OperatorID > 0 {
		q = q.Where("al.operator_id = ?", f.OperatorID)
	}
	if from, err := parseAdminTime(f.From); err != nil {
		return nil, err
	} else if from.Valid {
		q = q.Where("al.create_time >= FROM_UNIXTIME(? / 1000)", from.Time.UnixMilli())
	}
	if to, err := parseAdminTime(f.To); err != nil {
		return nil, err
	} else if to.Valid {
		q = q.Where("al.create_time < FROM_UNIXTIME(? / 1000)", to.Time.UnixMilli())
	}
	total, err := q.Order("al.id DESC").Limit(int64(size)).Offset(int64((page - 1) * size)).ScanAndCount(ctx)
	if err != nil {
		return nil, err
	}
	out := &AuditPage{Total: int(total), Page: page, Size: size, Items: make([]AuditEntry, 0, len(rows))}
	for _, r := range rows {
		e := AuditEntry{ID: r.ID, Action: r.Action, ResourceType: r.ResourceType, Status: r.Status,
			Before: r.BeforeJSON, After: r.AfterJSON, Diff: r.DiffJSON, CreateTime: r.CreateTime.Format(timeLayout),
			OperatorName: strPtr(r.OperatorName), ResourceID: strPtr(r.ResourceID), BatchID: strPtr(r.BatchID),
			RequestID: strPtr(r.RequestID), ClientIP: strPtr(r.ClientIP)}
		if r.OperatorID.Valid {
			v := r.OperatorID.Int64
			e.OperatorID = &v
		}
		out.Items = append(out.Items, e)
	}
	return out, nil
}

// normPage page 缺省 1，size 缺省 def、上限 100。
func normPage(page, size, def int) (int, int) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = def
	}
	if size > maxPageSize {
		size = maxPageSize
	}
	return page, size
}

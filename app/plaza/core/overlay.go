package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/pkg/bundb"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/userquota"
	"aigc-local-life/pkg/xerr"

	"github.com/uptrace/bun"
	"github.com/zeromicro/go-zero/core/logx"
)

// 模板文字叠加（DESIGN 17.3，P1）。
//
// 落点在 Go（plaza 层）而不在 Java：出图成功后由前端带「成品图地址 + 模板 id + 用户文字」来调，这里拿到
// 模板 layout + 文字 + 无字原图，经媒体服务 image/text 渲染，再转存素材库桶、登记素材。Java 的出图 / 计费链路不动，
// 不新增 Java→Go 依赖；叠字不走计费，所以怎么重试都不会扣点。
// 失败不抛：记录落库为 failed，原图照常交付，前端标「文字没加上，可重试」。

const (
	// ReasonOverlayNotReady 叠字能力没配（媒体服务未配置）：7003。
	ReasonOverlayNotReady = "PLAZA_OVERLAY_NOT_READY"
	// ReasonOverlayTooFrequent 叠字太频繁：4002。
	ReasonOverlayTooFrequent = "PLAZA_OVERLAY_TOO_FREQUENT"

	OverlayOK     = "ok"
	OverlayFailed = "failed"

	overlayStatusOK     = 1
	overlayStatusFailed = 2

	maxOverlayURL    = 1024
	maxOverlayLookup = 20
)

// OverlayConfig 叠字配置（Plaza.Overlay）。
type OverlayConfig struct {
	// DefaultFontRid layout 没给 fontRid 时用的字体（媒体服务 font 摄入后的 rid，须是有商用授权的中文字体）。空 = 不兜底，缺字体时按失败处理。
	DefaultFontRid string `json:",optional"`
	// AssetEnabled 叠字版 / 无字版是否登记进素材库（afv_asset，properties.textVariant 标注）。关闭 = 只存叠加记录。
	AssetEnabled bool `json:",default=true"`
	// PerMinute / PerDay 按用户的叠字（新建 / 改字 / 重试）配额，<=0 不限。
	PerMinute int `json:",default=30"`
	PerDay    int `json:",default=500"`
	// Auto 服务端自动叠字（overlay_auto.go）：做同款提交时冻结叠字意图，产物入库后后台排队叠字，不依赖用户页面开着。
	Auto OverlayAutoConfig `json:",optional"`
}

// OverlayAutoConfig 自动叠字配置。后台循环只在 gateway-api（叠字依赖所在进程）里跑，多副本靠行租约互斥。
type OverlayAutoConfig struct {
	// Enabled 关闭 = 不受理叠字意图（POST /overlay-intents 回 7003 PLAZA_OVERLAY_AUTO_OFF，前端退回逐张手动叠字）、不起后台循环。
	Enabled bool `json:",default=true"`
	// MaxAttempts 每张图最多自动尝试几次（含首次）；用完记为最终失败，落 failed 叠加记录供手动重试。
	MaxAttempts int `json:",default=5"`
	// MatchWindowMinutes 意图登记后多久内还去找它的产物（智能体一轮可能很久、用户还可能在会话里重试失败的图）。
	MatchWindowMinutes int `json:",default=1440"`
	// Batch 每轮最多扫多少条意图 / 处理多少张图。
	Batch int `json:",default=10"`
	// EntryCodes 只给这些入口的产物自动叠字（素材行 afv_asset.entry_code，与创作任务同值）；空 = 只认智能体生图 agentImage。
	EntryCodes []string `json:",optional"`
}

// OverlayMedia 叠字用到的媒体能力（*mediasvc.Client 满足；单测给假实现）。
type OverlayMedia interface {
	IngestURL(ctx context.Context, srcURL, purpose, idem string) (mediasvc.Resource, error)
	ImageText(ctx context.Context, rid string, slots []mediasvc.TextSlot, idem string) (mediasvc.Resource, error)
	CopyToLibrary(ctx context.Context, rid, purpose, idem string) (mediasvc.Resource, error)
}

// OverlayAssets 把叠字版登记进素材库（pkg/asset.Writer 的包装；nil = 不登记）。
type OverlayAssets interface {
	SaveTextOverlayVariant(ctx context.Context, userID int64, plainURL, overlayURL, oldOverlayURL string) (bool, error)
}

// OverlayOwnership 判断某张成品图是不是该用户自己的（素材库行 / 创作任务结果）。
type OverlayOwnership func(ctx context.Context, userID int64, resultURL string) (bool, error)

// OverlayRecord 对外返回的叠加记录。
type OverlayRecord struct {
	ID          int64             `json:"id"`
	TemplateID  int64             `json:"templateId"`
	SourceURL   string            `json:"sourceUrl"`
	RenderedURL *string           `json:"renderedUrl"`
	Status      string            `json:"status"` // ok | failed
	FailReason  string            `json:"failReason,omitempty"`
	Values      map[string]string `json:"values"`
	UpdatedAt   string            `json:"updatedAt"`
}

type overlayRow struct {
	ID          int64           `bun:"id"`
	UserID      int64           `bun:"user_id"`
	TemplateID  int64           `bun:"template_id"`
	SourceURL   string          `bun:"source_url"`
	SourceHash  string          `bun:"source_hash"`
	ValuesJSON  json.RawMessage `bun:"values_json"`
	PlainRid    *string         `bun:"plain_rid"`
	RenderedRid *string         `bun:"rendered_rid"`
	RenderedURL *string         `bun:"rendered_url"`
	Status      int             `bun:"status"`
	AssetSynced int             `bun:"asset_synced"`
	FailReason  *string         `bun:"fail_reason"`
	UpdateTime  time.Time       `bun:"update_time"`
}

func (r *overlayRow) record() OverlayRecord {
	vals := map[string]string{}
	_ = json.Unmarshal(r.ValuesJSON, &vals)
	o := OverlayRecord{ID: r.ID, TemplateID: r.TemplateID, SourceURL: r.SourceURL, RenderedURL: r.RenderedURL,
		Status: OverlayFailed, Values: vals, UpdatedAt: r.UpdateTime.Format(timeLayout)}
	if r.Status == overlayStatusOK && r.RenderedURL != nil {
		o.Status = OverlayOK
	} else {
		o.RenderedURL = nil
	}
	if r.FailReason != nil && o.Status == OverlayFailed {
		o.FailReason = *r.FailReason
	}
	return o
}

// SetOverlay 注入叠字依赖（生产：mediasvc 客户端 + pkg/asset 包装；media 为 nil = 叠字不可用）。
func (s *Service) SetOverlay(media OverlayMedia, assets OverlayAssets) {
	s.ovMedia, s.ovAssets = media, assets
	s.ovLim = userquota.New("plaza:overlay", s.cfg.Overlay.PerMinute, s.cfg.Overlay.PerDay, s.rd)
	// 意图一条消息一次，和手动叠字同一量级；单独计数，登记意图不占用户的叠字次数
	s.ovIntentLim = userquota.New("plaza:overlay_intent", s.cfg.Overlay.PerMinute, s.cfg.Overlay.PerDay, s.rd)
}

func errOverlayNotReady() error {
	return xerr.NewReason(xerr.NotReady, ReasonOverlayNotReady, "文字叠加暂未开放")
}

func sourceHash(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:])
}

func checkOverlaySource(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if u == "" || len(u) > maxOverlayURL {
		return "", xerr.Param("成品图地址不合法")
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return "", xerr.Param("成品图地址必须是 http(s) 地址")
	}
	return u, nil
}

// overlaySlot 模板里一个可叠字的文字位。
type overlaySlot struct {
	key    string
	layout *TextLayout
	maxLen int
	raw    json.RawMessage // 原始 layout，自动叠字冻结快照用（overlay_auto.go）
}

// overlaySlots 取模板里 type=text 且带合法 layout 的位（按 order 稳定排序）。模板没开 text_overlay → 参数错误。
func (s *Service) overlaySlots(ctx context.Context, templateID int64) ([]overlaySlot, error) {
	c, err := s.catalog(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := c.Get(templateID, s.now())
	if err != nil {
		return nil, err
	}
	if rec.t.TextOverlay != 1 {
		return nil, xerr.Param("这个模板没有开启文字叠加")
	}
	schema, perr := ParseSlotSchema(rec.t.SlotsJSON)
	if perr != nil || schema == nil {
		return nil, xerr.New(xerr.ServerError, "模板入参定义有误，请联系运营")
	}
	var out []overlaySlot
	for i := range schema.Items {
		sl := &schema.Items[i]
		if sl.Type != SlotText || len(sl.Layout) == 0 {
			continue
		}
		l, errs := ParseLayout(sl.Layout, "layout")
		if len(errs) > 0 {
			logx.WithContext(ctx).Errorf("plaza: 模板 %d 的文字位 %s layout 不合法，跳过叠字: %s", templateID, sl.Key, errs[0])
			continue
		}
		ml := 100
		if sl.MaxLength != nil {
			ml = *sl.MaxLength
		}
		out = append(out, overlaySlot{key: sl.Key, layout: l, maxLen: ml, raw: sl.Layout})
	}
	if len(out) == 0 {
		return nil, xerr.Param("这个模板没有可叠加的文字位")
	}
	return out, nil
}

// cleanOverlayValues 只留模板里有 layout 的文字位、去首尾空白、丢空值、超长拒收。
func cleanOverlayValues(slots []overlaySlot, in map[string]string) (map[string]string, error) {
	byKey := map[string]overlaySlot{}
	for _, sl := range slots {
		byKey[sl.key] = sl
	}
	out := map[string]string{}
	for k, v := range in {
		sl, ok := byKey[k]
		if !ok {
			continue // 没有版式的位（前端可能整份入参都传来）不叠，直接忽略
		}
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !utf8.ValidString(v) || strings.ContainsRune(v, 0) || utf8.RuneCountInString(v) > sl.maxLen {
			return nil, xerr.Param("文字位 " + k + " 的内容不合法或超长")
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil, xerr.Param("请至少填一处要加到图上的文字")
	}
	return out, nil
}

// CreateOverlay 对一张自己的成品图叠字。同一（用户、模板、成品图）已有成功记录且文字没变 → 直接返回，不重复渲染。
func (s *Service) CreateOverlay(ctx context.Context, userID, templateID int64, sourceURL string, values map[string]string) (*OverlayRecord, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if s.ovMedia == nil {
		return nil, errOverlayNotReady()
	}
	if templateID <= 0 {
		return nil, xerr.New(xerr.ParamMissing, "缺少模板")
	}
	src, err := checkOverlaySource(sourceURL)
	if err != nil {
		return nil, err
	}
	slots, err := s.overlaySlots(ctx, templateID)
	if err != nil {
		return nil, err
	}
	vals, err := cleanOverlayValues(slots, values)
	if err != nil {
		return nil, err
	}
	hash := sourceHash(src)
	if old, err := s.overlayBySource(ctx, userID, templateID, hash); err != nil {
		return nil, err
	} else if old != nil && old.Status == overlayStatusOK && sameValues(old.ValuesJSON, vals) {
		o := old.record()
		return &o, nil
	}
	// 先验所有权再扣配额：拿别人的 / 不存在的地址来试探，不该消耗自己的叠字次数
	if err := s.checkOverlayOwner(ctx, userID, src); err != nil {
		return nil, err
	}
	if err := s.takeOverlayQuota(ctx, userID); err != nil {
		return nil, err
	}
	row := &overlayRow{UserID: userID, TemplateID: templateID, SourceURL: src, SourceHash: hash}
	if old, _ := s.overlayBySource(ctx, userID, templateID, hash); old != nil {
		row = old
	}
	return s.renderOverlay(ctx, row, slots, vals)
}

// UpdateOverlay 改字 / 重试：总是从无字原图重新渲染。values 为 nil = 沿用已存文字（重试）。
func (s *Service) UpdateOverlay(ctx context.Context, userID, id int64, values *map[string]string) (*OverlayRecord, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	if s.ovMedia == nil {
		return nil, errOverlayNotReady()
	}
	row, err := s.overlayByID(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	slots, err := s.overlaySlots(ctx, row.TemplateID)
	if err != nil {
		return nil, err
	}
	var vals map[string]string
	if values != nil {
		if vals, err = cleanOverlayValues(slots, *values); err != nil {
			return nil, err
		}
	} else {
		stored := map[string]string{}
		_ = json.Unmarshal(row.ValuesJSON, &stored)
		if vals, err = cleanOverlayValues(slots, stored); err != nil {
			return nil, err
		}
	}
	if err := s.takeOverlayQuota(ctx, userID); err != nil {
		return nil, err
	}
	return s.renderOverlay(ctx, row, slots, vals)
}

// GetOverlay 取一条记录（只能取自己的）。
func (s *Service) GetOverlay(ctx context.Context, userID, id int64) (*OverlayRecord, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	row, err := s.overlayByID(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	s.healOverlayAssets(ctx, []overlayRow{*row})
	o := row.record()
	return &o, nil
}

// ListOverlays 按成品图地址批量取自己的记录（刷新页面后恢复叠字结果用），最多 20 个地址。
func (s *Service) ListOverlays(ctx context.Context, userID int64, sourceURLs []string) ([]OverlayRecord, error) {
	if s.db == nil {
		return nil, ErrNotReady()
	}
	seen := map[string]bool{}
	var hashes []string
	for _, u := range sourceURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		h := sourceHash(u)
		if !seen[h] {
			seen[h] = true
			hashes = append(hashes, h)
		}
	}
	if len(hashes) == 0 {
		return nil, xerr.Param("缺少成品图地址")
	}
	if len(hashes) > maxOverlayLookup {
		return nil, xerr.Param("一次最多查 20 张")
	}
	var rows []overlayRow
	if err := s.db.NewRaw("SELECT id, user_id, template_id, source_url, source_hash, values_json, plain_rid, rendered_rid, rendered_url, status, asset_synced, fail_reason, update_time "+
		"FROM afv_plaza_overlay WHERE user_id = ? AND source_hash IN (?) ORDER BY id", userID, bun.In(hashes)).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	s.healOverlayAssets(ctx, rows)
	out := make([]OverlayRecord, 0, len(rows))
	for i := range rows {
		out = append(out, rows[i].record())
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Service) takeOverlayQuota(ctx context.Context, userID int64) error {
	if s.ovLim != nil && s.ovLim.Take(ctx, userID) != userquota.Allowed {
		return xerr.NewReason(xerr.Conflict, ReasonOverlayTooFrequent, "加字太频繁了，请稍后再试")
	}
	return nil
}

func (s *Service) checkOverlayOwner(ctx context.Context, userID int64, src string) error {
	own := s.ovOwner
	if own == nil {
		own = s.ownsResultURL
	}
	ok, err := own(ctx, userID, src)
	if err != nil {
		return err
	}
	if !ok {
		return xerr.New(xerr.NotFound, "没有找到这张成品图")
	}
	return nil
}

// ownsResultURL 成品图属于该用户：素材库行，或创作任务的结果 / 水印结果。
func (s *Service) ownsResultURL(ctx context.Context, userID int64, resultURL string) (bool, error) {
	var n int
	if err := s.db.NewRaw("SELECT COUNT(1) FROM afv_asset WHERE user_id = ? AND deleted = 0 AND (result_url = ? OR watermark_url = ?)",
		userID, resultURL, resultURL).Scan(ctx, &n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	q := s.db.NewRaw("SELECT COUNT(1) FROM afv_creation_task WHERE user_id = ? AND deleted = 0 AND "+
		"(result_url = ? OR watermark_url = ? OR JSON_CONTAINS(result_urls, JSON_QUOTE(?)) OR JSON_CONTAINS(watermark_urls, JSON_QUOTE(?)))",
		userID, resultURL, resultURL, resultURL, resultURL)
	if err := q.Scan(ctx, &n); err != nil {
		return false, err
	}
	return n > 0, nil
}

func sameValues(raw json.RawMessage, vals map[string]string) bool {
	old := map[string]string{}
	if json.Unmarshal(raw, &old) != nil || len(old) != len(vals) {
		return false
	}
	for k, v := range vals {
		if old[k] != v {
			return false
		}
	}
	return true
}

// renderOverlay 摄入无字图 → 叠字 → 转存素材库桶 → 登记素材 → 保存记录。媒体侧任何失败都只落「failed」记录并返回它（不抛错）。
func (s *Service) renderOverlay(ctx context.Context, row *overlayRow, slots []overlaySlot, vals map[string]string) (*OverlayRecord, error) {
	rec, _, err := s.renderOverlayOpt(ctx, row, slots, vals, true, false)
	return rec, err
}

// renderOverlayOpt 同 renderOverlay；saveFailure=false 时渲染失败不落库（自动叠字的中间重试：还会再试，
// 不让前端先看到一条「failed」），返回 (nil, 失败原因, nil)。成功总是落库并返回记录。
//
// auto=true（服务端自动叠字作业）：写回走 saveAutoOverlayRow——同一事务内复查，已有成功记录（用户手动叠过 / 改过字）就放弃
// 本次渲染结果并返回 errOverlayTaken，绝不覆盖；手动路径（auto=false）照旧 upsert（用户的操作以最后一次为准）。
func (s *Service) renderOverlayOpt(ctx context.Context, row *overlayRow, slots []overlaySlot, vals map[string]string, saveFailure, auto bool) (*OverlayRecord, *renderFail, error) {
	valuesJSON, _ := json.Marshal(vals)
	row.ValuesJSON = valuesJSON
	oldURL := ""
	if row.RenderedURL != nil {
		oldURL = *row.RenderedURL
	}
	url, rid, plainRid, ferr := s.doRender(ctx, row, slots, vals)
	if plainRid != "" {
		row.PlainRid = &plainRid
	}
	if ferr != nil {
		reason := ferr.reason
		logx.WithContext(ctx).Errorf("plaza: 叠字失败 user=%d template=%d: %s: %v", row.UserID, row.TemplateID, reason, ferr.cause)
		if !saveFailure {
			return nil, ferr, nil
		}
		row.Status, row.FailReason = overlayStatusFailed, &reason
		// 失败时保留旧的成功图地址以便素材登记不丢，但对外按 failed 展示
	} else {
		row.Status, row.FailReason = overlayStatusOK, nil
		row.RenderedURL, row.RenderedRid = &url, &rid
		row.AssetSynced = 0
		if s.ovAssets == nil {
			row.AssetSynced = 1 // 不登记素材库，没有待补的事
		}
	}
	save := s.saveOverlayRow
	if auto {
		save = func(ctx context.Context, r *overlayRow) error {
			taken, err := s.saveAutoOverlayRow(ctx, r)
			if err == nil && taken != nil {
				return errOverlayTaken
			}
			return err
		}
	}
	if err := save(ctx, row); err != nil {
		return nil, ferr, err
	}
	if ferr == nil && s.ovAssets != nil {
		s.syncOverlayAsset(ctx, row, oldURL) // 失败 / 素材行未落库时 asset_synced 保持 0，之后的 GET 会补登记
	}
	saved, err := s.overlayByID(ctx, row.UserID, row.ID)
	if err != nil {
		return nil, ferr, err
	}
	o := saved.record()
	return &o, ferr, nil
}

type renderFail struct {
	reason string
	cause  error
	// permanent 重试也不会好（没有可叠的文字 / 模板与系统都没配字体）：自动叠字直接终止，不再退避重试
	permanent bool
}

func (s *Service) doRender(ctx context.Context, row *overlayRow, slots []overlaySlot, vals map[string]string) (url, rid, plainRid string, fail *renderFail) {
	m := s.ovMedia
	// 摄入带固定幂等键：重试 / 改字拿到同一个 rid，不会重复拉图
	plain, err := m.IngestURL(ctx, row.SourceURL, mediasvc.PurposeIntermediate, "plaza-ov-in:"+row.SourceHash)
	if err != nil {
		return "", "", "", &renderFail{reason: "无字原图读取失败", cause: err}
	}
	if plain.Height <= 0 {
		return "", "", plain.RID, &renderFail{reason: "无字原图尺寸未知"}
	}
	var ts []mediasvc.TextSlot
	for _, sl := range slots {
		txt, ok := vals[sl.key]
		if !ok {
			continue
		}
		ts = append(ts, sl.layout.ToSlot(txt, plain.Height, s.cfg.Overlay.DefaultFontRid))
	}
	if len(ts) == 0 {
		return "", "", plain.RID, &renderFail{reason: "没有可叠加的文字", permanent: true}
	}
	for _, t := range ts {
		if t.FontRid == "" {
			return "", "", plain.RID, &renderFail{reason: "没有可用字体（模板与系统都没配置字体）", permanent: true}
		}
	}
	out, err := m.ImageText(ctx, plain.RID, ts, "")
	if err != nil {
		return "", "", plain.RID, &renderFail{reason: "叠字渲染失败", cause: err}
	}
	pub, err := m.CopyToLibrary(ctx, out.RID, mediasvc.PurposeAssetLibrary, "")
	if err != nil {
		return "", "", plain.RID, &renderFail{reason: "叠字图转存失败", cause: err}
	}
	if pub.URL == "" {
		return "", "", plain.RID, &renderFail{reason: "叠字图没有可用地址"}
	}
	return pub.URL, out.RID, plain.RID, nil
}

const overlayCols = "id, user_id, template_id, source_url, source_hash, values_json, plain_rid, rendered_rid, rendered_url, status, asset_synced, fail_reason, update_time"

func (s *Service) overlayBySource(ctx context.Context, userID, templateID int64, hash string) (*overlayRow, error) {
	var rows []overlayRow
	if err := s.db.NewRaw("SELECT "+overlayCols+" FROM afv_plaza_overlay WHERE user_id = ? AND template_id = ? AND source_hash = ? LIMIT 1",
		userID, templateID, hash).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	return &rows[0], nil
}

func (s *Service) overlayByID(ctx context.Context, userID, id int64) (*overlayRow, error) {
	var rows []overlayRow
	if err := s.db.NewRaw("SELECT "+overlayCols+" FROM afv_plaza_overlay WHERE id = ? AND user_id = ? LIMIT 1", id, userID).Scan(ctx, &rows); err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, xerr.New(xerr.NotFound, "没有找到这条叠字记录")
	}
	return &rows[0], nil
}

// saveOverlayRow 新建或更新（唯一键 user + template + 成品图；并发创建同一张图时后到的覆盖先到的，结果等价）。
func (s *Service) saveOverlayRow(ctx context.Context, r *overlayRow) error {
	_, err := s.db.NewRaw("INSERT INTO afv_plaza_overlay (user_id, template_id, source_url, source_hash, values_json, plain_rid, rendered_rid, rendered_url, status, asset_synced, fail_reason) "+
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON DUPLICATE KEY UPDATE values_json = VALUES(values_json), plain_rid = VALUES(plain_rid), "+
		"rendered_rid = VALUES(rendered_rid), rendered_url = VALUES(rendered_url), status = VALUES(status), asset_synced = VALUES(asset_synced), fail_reason = VALUES(fail_reason), update_time = CURRENT_TIMESTAMP",
		r.UserID, r.TemplateID, r.SourceURL, r.SourceHash, string(r.ValuesJSON), r.PlainRid, r.RenderedRid, r.RenderedURL, r.Status, r.AssetSynced, r.FailReason).Exec(ctx)
	if err != nil {
		return err
	}
	if r.ID == 0 {
		var id int64
		if err := s.db.NewRaw("SELECT id FROM afv_plaza_overlay WHERE user_id = ? AND template_id = ? AND source_hash = ?", r.UserID, r.TemplateID, r.SourceHash).Scan(ctx, &id); err != nil {
			return err
		}
		r.ID = id
	}
	return nil
}

// errOverlayTaken 自动叠字写回时发现同一张图已有成功叠加记录（用户在作业渲染期间手动叠了字）：放弃本次结果。
var errOverlayTaken = errors.New("已有成功的叠字记录，放弃自动叠字结果")

// saveAutoOverlayRow 自动叠字写回（成功或失败记录都走这里）：同一事务内 FOR UPDATE 复查同一（用户, 模板, 成品图）的记录，
// 已有成功记录 → 不写、返回它（taken）；否则按 id 更新（且仍限定非成功）或插入新行。不用 ON DUPLICATE KEY，
// 不会覆盖作业渲染期间用户手动生成的叠字。并发插入撞唯一键时返回错误，作业退避后重来会读到那条记录。
func (s *Service) saveAutoOverlayRow(ctx context.Context, r *overlayRow) (taken *overlayRow, err error) {
	err = bundb.RunInTx(ctx, s.db, func(ctx context.Context, tx bun.Tx) error {
		var rows []overlayRow
		if err := tx.NewRaw("SELECT "+overlayCols+" FROM afv_plaza_overlay WHERE user_id = ? AND template_id = ? AND source_hash = ? LIMIT 1 FOR UPDATE",
			r.UserID, r.TemplateID, r.SourceHash).Scan(ctx, &rows); err != nil {
			return err
		}
		if len(rows) > 0 && rows[0].Status == overlayStatusOK {
			taken = &rows[0]
			return nil
		}
		if len(rows) > 0 {
			res, err := tx.NewRaw("UPDATE afv_plaza_overlay SET values_json = ?, plain_rid = ?, rendered_rid = ?, rendered_url = ?, status = ?, asset_synced = ?, "+
				"fail_reason = ?, update_time = CURRENT_TIMESTAMP WHERE id = ? AND status <> ?",
				string(r.ValuesJSON), r.PlainRid, r.RenderedRid, r.RenderedURL, r.Status, r.AssetSynced, r.FailReason, rows[0].ID, overlayStatusOK).Exec(ctx)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 { // 行锁下不该发生；保守按「已被占用」处理
				taken = &rows[0]
				return nil
			}
			r.ID = rows[0].ID
			return nil
		}
		res, err := tx.NewRaw("INSERT INTO afv_plaza_overlay (user_id, template_id, source_url, source_hash, values_json, plain_rid, rendered_rid, rendered_url, status, asset_synced, fail_reason) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			r.UserID, r.TemplateID, r.SourceURL, r.SourceHash, string(r.ValuesJSON), r.PlainRid, r.RenderedRid, r.RenderedURL, r.Status, r.AssetSynced, r.FailReason).Exec(ctx)
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		r.ID = id
		return nil
	})
	if err != nil {
		return nil, err
	}
	if taken != nil {
		logx.WithContext(ctx).Infof("plaza: 自动叠字放弃本次结果 判据=OVERLAY_ALREADY_OK user=%d template=%d overlay=%d（已有成功叠字，不覆盖）",
			r.UserID, r.TemplateID, taken.ID)
	}
	return taken, nil
}

// syncOverlayAsset 登记叠字版进素材库；成功才把 asset_synced 置 1。失败只记日志，由 healOverlayAssets 在后续读取时补登记。
func (s *Service) syncOverlayAsset(ctx context.Context, row *overlayRow, oldURL string) bool {
	if s.ovAssets == nil || row.RenderedURL == nil {
		return false
	}
	saved, err := s.ovAssets.SaveTextOverlayVariant(ctx, row.UserID, row.SourceURL, *row.RenderedURL, oldURL)
	if err != nil {
		logx.WithContext(ctx).Errorf("plaza: 叠字版登记素材库失败（不影响交付，稍后读取时补登记）overlay=%d: %v", row.ID, err)
		return false
	}
	if !saved { // 无字版素材行还没同步入库
		return false
	}
	if _, err := s.db.NewRaw("UPDATE afv_plaza_overlay SET asset_synced = 1 WHERE id = ? AND rendered_url = ?", row.ID, *row.RenderedURL).Exec(ctx); err != nil {
		logx.WithContext(ctx).Errorf("plaza: 标记叠字版已登记失败 overlay=%d: %v", row.ID, err)
	}
	row.AssetSynced = 1
	return true
}

// overlayHealWindow 只补最近这么久的未登记记录（更早的多半是来源根本不在素材库，不再反复试）。
const overlayHealWindow = 7 * 24 * time.Hour

// healOverlayAssets 读取时顺手补登记：成功渲染但 asset_synced=0 的记录再试一次。
func (s *Service) healOverlayAssets(ctx context.Context, rows []overlayRow) {
	if s.ovAssets == nil {
		return
	}
	for i := range rows {
		r := &rows[i]
		if r.Status == overlayStatusOK && r.AssetSynced == 0 && s.now().Sub(r.UpdateTime) < overlayHealWindow {
			s.syncOverlayAsset(ctx, r, "")
		}
	}
}

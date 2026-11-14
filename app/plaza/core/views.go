package core

import (
	"encoding/json"
	"strings"
	"time"

	"aigc-local-life/app/plaza/model"
)

// ChannelSpec 解析后的渠道规格（逐模板覆盖优先于字典）。
type ChannelSpec struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Ratio  string `json:"ratio"`
}

// TemplateItem 用户端模板项（列表 / 详情 / 最近用过同形）。
type TemplateItem struct {
	PlazaTemplateID   int64           `json:"plazaTemplateId"`
	Title             string          `json:"title"`
	AgentSkillID      *string         `json:"agentSkillId"`
	Tags              []string        `json:"tags"`
	Payload           json.RawMessage `json:"payload"`
	AuthorName        string          `json:"authorName"`
	UsageCount        *int64          `json:"usageCount"`
	UsageVisible      bool            `json:"usageVisible"`
	MediaType         string          `json:"mediaType"`
	VideoURL          *string         `json:"videoUrl"`
	PosterURL         *string         `json:"posterUrl"`
	CoverThumbURL     string          `json:"coverThumbUrl"`
	IsNew             bool            `json:"isNew"`
	CreatedAt         string          `json:"createdAt"`
	NeedsProductImage bool            `json:"needsProductImage"`
	// TextOverlay 文字走后期叠加（slots 里 text 位带 layout 时出图后由系统叠字）。
	TextOverlay    bool          `json:"textOverlay"`
	TargetChannels []ChannelSpec `json:"targetChannels"`
	Industries     []IndustryRef `json:"industries"`
	// Slots 入参 schema：只在原生「模板详情」里下发，浏览列表不下发。
	Slots *SlotSchema `json:"slots,omitempty"`
}

// officialAuthor 广场模板都是运营上架，默认作者名（同 Java OFFICIAL_AUTHOR）。
const officialAuthor = "官方"

// timeLayout 与 Java LocalDateTime 序列化一致（无时区、无小数）。
const timeLayout = "2006-01-02T15:04:05"

func (s *Service) item(c *Catalog, rec *tplRec, usage map[int64]int64, now time.Time) TemplateItem {
	t := &rec.t
	it := TemplateItem{
		PlazaTemplateID:   t.ID,
		Title:             t.Title,
		Tags:              readTags(t.TagsJSON),
		Payload:           sanitizePayload(t.DataJSON),
		AuthorName:        officialAuthor,
		MediaType:         t.MediaType,
		CreatedAt:         t.CreateTime.Format(timeLayout),
		NeedsProductImage: t.NeedsProductImage == 1,
		TextOverlay:       t.TextOverlay == 1,
		TargetChannels:    c.resolveChannels(t.ChannelsJSON),
		Industries:        c.industriesOf(rec),
	}
	if a := strings.TrimSpace(topString(t.DataJSON, "authorName")); a != "" {
		it.AuthorName = a
	}
	if id := skillID(t.DataJSON); id != "" {
		it.AgentSkillID = &id
	}
	uses := usage[t.ID]
	it.UsageVisible = uses >= int64(s.cfg.usageMin())
	if it.UsageVisible { // 低于阈值时服务端不下发具体数字
		it.UsageCount = &uses
	}
	cover := firstResult(t.DataJSON)
	if t.MediaType == model.MediaVideo {
		if t.VideoURL.Valid && t.VideoURL.String != "" {
			v := t.VideoURL.String
			it.VideoURL = &v
		}
		if t.PosterURL.Valid && t.PosterURL.String != "" {
			p := t.PosterURL.String
			it.PosterURL = &p
			cover = p
		}
	}
	it.CoverThumbURL = thumbURL(cover, s.cfg.Media.ThumbRule)
	it.IsNew = now.Sub(t.CreateTime) <= time.Duration(s.cfg.freshDays())*24*time.Hour
	return it
}

// thumbURL 给公有桶（阿里云 OSS）地址追加缩略图处理参数；规则缺失、非 OSS 地址、地址已带参数 → 退回原图。
func thumbURL(u, rule string) string {
	if u == "" || rule == "" || !isOSSHost(u) || strings.Contains(u, "?") {
		return u
	}
	if !strings.HasPrefix(rule, "?") {
		rule = "?" + rule
	}
	return u + rule
}

func isOSSHost(u string) bool {
	rest := u
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	rest = strings.ToLower(rest)
	return strings.HasSuffix(rest, ".aliyuncs.com") && strings.Contains(rest, "oss-")
}

// channelOverride channels_json 的元素：渠道 code 字符串，或带宽高覆盖的对象。
type channelOverride struct {
	Code   string
	Width  int
	Height int
}

func parseChannelRefs(raw []byte) ([]channelOverride, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, errParam("channels 必须是数组")
	}
	out := make([]channelOverride, 0, len(arr))
	for _, el := range arr {
		var code string
		if json.Unmarshal(el, &code) == nil {
			out = append(out, channelOverride{Code: strings.TrimSpace(code)})
			continue
		}
		var obj struct {
			Code   string `json:"code"`
			Width  int    `json:"width"`
			Height int    `json:"height"`
		}
		if json.Unmarshal(el, &obj) != nil {
			return nil, errParam("channels 的元素必须是渠道 code 或 {code,width,height}")
		}
		out = append(out, channelOverride{Code: strings.TrimSpace(obj.Code), Width: obj.Width, Height: obj.Height})
	}
	return out, nil
}

// resolveChannels 解析成规格：覆盖优先于字典，只允许覆盖宽高；未知 / 停用的渠道跳过。
func (c *Catalog) resolveChannels(raw []byte) []ChannelSpec {
	out := []ChannelSpec{}
	refs, err := parseChannelRefs(raw)
	if err != nil {
		return out
	}
	for _, r := range refs {
		ch, ok := c.channels[r.Code]
		if !ok || ch.Enabled != 1 {
			continue
		}
		spec := ChannelSpec{Code: ch.Code, Name: ch.Name, Width: ch.Width, Height: ch.Height, Ratio: ch.Ratio}
		if r.Width > 0 && r.Height > 0 {
			spec.Width, spec.Height = r.Width, r.Height
		}
		out = append(out, spec)
	}
	return out
}

// validateChannels 后台保存：每个渠道 code 必须存在且启用；覆盖的宽高须成对为正，且比例须与字典一致。
func validateChannels(raw []byte, dict map[string]model.Channel) error {
	refs, err := parseChannelRefs(raw)
	if err != nil {
		return err
	}
	for _, r := range refs {
		ch, ok := dict[r.Code]
		if !ok || ch.Enabled != 1 {
			return errParam("渠道不存在或已停用：" + r.Code)
		}
		if r.Width != 0 || r.Height != 0 {
			if r.Width <= 0 || r.Height <= 0 {
				return errParam("渠道 " + r.Code + " 的宽高必须同时为正")
			}
			if !sameRatio(r.Width, r.Height, ch.Width, ch.Height) {
				return errParam("渠道 " + r.Code + " 的覆盖尺寸比例须与字典一致（" + ch.Ratio + "）")
			}
		}
	}
	return nil
}

func sameRatio(w1, h1, w2, h2 int) bool { return w1*h2 == w2*h1 }

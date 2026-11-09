package logic

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/app/assetlib/rpc/internal/svc"
	"aigc-local-life/pkg/pagex"
	"aigc-local-life/pkg/storage"
	"aigc-local-life/pkg/xerr"
)

// 详情里三组子分页的默认 / 上限。
const (
	subPageSize    = 6
	subPageSizeMax = 50
	maxJSONBytes   = 16 << 10 // params / provenance 等 JSON 文本上限
)

// errNotFound 资产不存在、不属于当前用户、已删除，一律同一个口径（越权按不存在）。
var errNotFound = xerr.New(xerr.NotFound, "资产不存在")

func needUser(uid int64) error {
	if uid <= 0 {
		return xerr.New(xerr.Unauthorized, "未登录")
	}
	return nil
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// checkListKind List 的 kind：空 / uncategorized / 5 个 kind / 视频二级分类键（只读筛选，不可写入）。
func checkListKind(k string) error {
	if k == "" || k == "uncategorized" || model.Kind(k).Valid() || slices.Contains(model.VideoKinds, k) {
		return nil
	}
	return xerr.Param("kind 只能是 model / scene / makeup / hair / accessory / uncategorized 或视频二级分类")
}

// checkKindGroup 大类：空 / image / video。
func checkKindGroup(g string) error {
	if g == "" || model.KindGroupKinds(g) != nil {
		return nil
	}
	return xerr.Param("kind_group 只能是 image / video")
}

// aspectRatioRe 比例 宽:高，各 1~2 位数字（1:1 / 3:4 / 16:9 …）。
var aspectRatioRe = regexp.MustCompile(`^[1-9][0-9]?:[1-9][0-9]?$`)

// cleanAspectRatio 去首尾空白；空 = 不限，否则必须是 宽:高。
func cleanAspectRatio(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || aspectRatioRe.MatchString(s) {
		return s, nil
	}
	return "", xerr.Param("aspect_ratio 格式应为 宽:高，如 1:1 / 3:4 / 16:9")
}

// entryCodeRe 单个入口码：字母数字与 _ . -，1~64 位。
var entryCodeRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// maxEntryCodes entry_codes 最多几个（一级分类展开后的入口并集也远小于它）。
const maxEntryCodes = 200

// parseEntryCodes entry_codes 逗号分隔；各段去首尾空白、跳过空段、去重保序。
// 空 = 不限（返回 nil）；任一段格式不对或超过 maxEntryCodes 个返回参数错误。
func parseEntryCodes(s string) ([]string, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var out []string
	seen := map[string]bool{}
	for _, c := range strings.Split(s, ",") {
		c = strings.TrimSpace(c)
		if c == "" || seen[c] {
			continue
		}
		if !entryCodeRe.MatchString(c) {
			return nil, xerr.Param("entry_codes 每项只能是字母、数字、_ . -，1~64 位")
		}
		seen[c] = true
		out = append(out, c)
		if len(out) > maxEntryCodes {
			return nil, xerr.Param("entry_codes 最多 200 个")
		}
	}
	return out, nil
}

func checkSource(s string) error {
	switch s {
	case "", model.SourceInput, model.SourceGenerated, model.SourceOfficial, model.SourceUpload:
		return nil
	}
	return xerr.Param("source 只能是 input / generated / official / upload")
}

func checkMedia(m string) error {
	if m == "" || m == "image" || m == "video" {
		return nil
	}
	return xerr.Param("media 只能是 image / video")
}

// cleanTagName 标签名：去首尾空白，非空，≤limit 字，不含控制字符。
func cleanTagName(name string, limit int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", xerr.New(xerr.ParamMissing, "标签名不能为空")
	}
	if runeLen(name) > limit {
		return "", xerr.Newf(xerr.ParamError, "标签名最多 %d 字", limit)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", xerr.Param("标签名含非法字符")
	}
	return name, nil
}

// checkJSON 可空的 JSON 文本：必须合法、≤16KB。
func checkJSON(field, s string) (json.RawMessage, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if len(s) > maxJSONBytes {
		return nil, xerr.Newf(xerr.ParamError, "%s 过长", field)
	}
	if !json.Valid([]byte(s)) {
		return nil, xerr.Newf(xerr.ParamError, "%s 不是合法 JSON", field)
	}
	return json.RawMessage(s), nil
}

// checkURL 图片地址：http(s)、≤1024 字符。
func checkURL(u string) error {
	u = strings.TrimSpace(u)
	if u == "" {
		return xerr.New(xerr.ParamMissing, "缺少图片地址")
	}
	if len(u) > 1024 {
		return xerr.Param("图片地址过长")
	}
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
		return xerr.Param("图片地址必须是 http(s) 地址")
	}
	return nil
}

// checkOwnedURL 写入库的图片地址必须是本服务签发的（storage.Owned）或在 InputURLPrefixes 白名单内，
// 否则任何登录用户都能把任意外链写进素材库再被前端渲染。owns 为 nil 时一律拒绝。
func checkOwnedURL(owns func(string) bool, field, u string) error {
	if err := checkURL(u); err != nil {
		return err
	}
	if owns == nil || !owns(strings.TrimSpace(u)) {
		return xerr.Newf(xerr.ParamError, "%s 不是本服务上传的地址", field)
	}
	return nil
}

// ownsURL 生产用的归属判断：本存储签发，或命中 InputURLPrefixes。
func ownsURL(s *svc.ServiceContext) func(string) bool {
	return func(u string) bool {
		if s.Storage != nil && storage.Owned(s.Storage, u) == nil && cleanHTTPURL(u) {
			return true
		}
		return matchPrefixes(s.Config.InputURLPrefixes, u)
	}
}

// cleanHTTPURL 解析后的地址不带账号信息、无 .. 路径段（防 storage.Owns 纯字符串前缀被花式拼接绕过）。
func cleanHTTPURL(u string) bool {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil || strings.Contains(u, "\\") {
		return false
	}
	for _, seg := range strings.Split(p.Path, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

// normalizePrefix 白名单条目允许只写域名（或域名+路径），缺协议时按 https 处理：
// "cdn.example.com" = "https://cdn.example.com"，"assets.example.com/lib" = "https://assets.example.com/lib"。
// 已带 "://" 的条目原样保留（协议仍须与图片地址一致）。
func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || strings.Contains(p, "://") {
		return p
	}
	return "https://" + p
}

// matchPrefixes u 命中任一前缀：scheme + host 完全相等，路径以前缀路径（补尾 /）开头。
// 不能用字符串 HasPrefix：前缀没带尾部 / 时 https://cdn.example.com 会放过 https://cdn.example.com.evil.io。
func matchPrefixes(prefixes []string, u string) bool {
	if !cleanHTTPURL(u) {
		return false
	}
	pu, _ := url.Parse(u)
	for _, p := range prefixes {
		p = normalizePrefix(p)
		if p == "" {
			continue
		}
		pp, err := url.Parse(p)
		if err != nil || pp.Host == "" || pp.User != nil {
			continue
		}
		if !strings.EqualFold(pp.Scheme, pu.Scheme) || !strings.EqualFold(pp.Host, pu.Host) {
			continue
		}
		base := strings.TrimRight(pp.Path, "/") + "/"
		if strings.HasPrefix(pu.Path, base) {
			return true
		}
	}
	return false
}

func subPage(page, size int64) (limit, offset int64) {
	if page < 1 {
		page = 1
	}
	if size <= 0 {
		size = subPageSize
	}
	if size > subPageSizeMax {
		size = subPageSizeMax
	}
	return size, (page - 1) * size
}

func listPage(page, size int64) (limit, offset int64) {
	return pagex.Offset(page, size)
}

// mustOwn 取本人的资产，否则 4001。
func mustOwn(ctx context.Context, s *svc.ServiceContext, id, uid int64) (*repo.AssetRow, error) {
	if err := needUser(uid); err != nil {
		return nil, err
	}
	if id <= 0 {
		return nil, errNotFound
	}
	row, err := s.Repo.Asset.GetOwned(ctx, id, uid)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, errNotFound
	}
	return row, nil
}

// toAssets 一批行转前端 Asset（一次查出全部标签）。
func toAssets(ctx context.Context, s *svc.ServiceContext, uid int64, rows []*repo.AssetRow) ([]*assetlib.Asset, error) {
	ids := make([]int64, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	tags, err := s.Repo.Tag.TagsOf(ctx, uid, ids)
	if err != nil {
		return nil, err
	}
	// 列表 / 详情 / 相关资产 / 生成结果都经这里：mr://{rid} 现签成地址，失败置空
	resolveRefsWith(ctx, signerOf(s), s.Config.FreeWatermark.Process, rows)
	out := make([]*assetlib.Asset, 0, len(rows))
	for _, r := range rows {
		names := make([]string, 0, len(tags[r.ID]))
		for _, t := range tags[r.ID] {
			names = append(names, t.Name)
		}
		out = append(out, toAsset(r, names))
	}
	return out, nil
}

// toAsset Java 资产 + 旁表 → 前端 Asset。
func toAsset(r *repo.AssetRow, tags []string) *assetlib.Asset {
	a := &assetlib.Asset{
		Id:               r.ID,
		Kind:             r.ExtKind.String,
		Title:            r.Name,
		Prompt:           r.AIPrompt.String,
		Media:            "image",
		Source:           model.SourceOf(r.SourceType),
		Tags:             tags,
		CreatedAt:        r.CreatedMs(),
		Favorite:         r.ExtFavorite.Int64 == 1,
		License:          r.ExtLicense.String,
		Metadata:         &assetlib.Metadata{},
		Provenance:       &assetlib.Provenance{},
		CurrentVersionId: r.CurrentVersionID.Int64,
		EntryCode:        r.EntryCode,
		Width:            r.Width,
		Height:           r.Height,
	}
	if a.Tags == nil {
		a.Tags = []string{}
	}
	if r.IsVideo {
		a.Media = "video"
		a.VideoUrl = r.ResultURL.String
		a.Image = first(r.CoverURL.String, r.ResultURL.String)
	} else {
		a.Image = first(r.ResultURL.String, r.CoverURL.String)
	}
	if r.CurrentImage.Valid && r.CurrentImage.String != "" {
		a.Image = r.CurrentImage.String
	}
	if len(r.ExtMetadata) > 0 {
		var m model.Metadata
		if json.Unmarshal(r.ExtMetadata, &m) == nil {
			a.Metadata = &assetlib.Metadata{Industry: m.Industry, Style: m.Style, Shot: m.Shot, Color: m.Color}
		}
	}
	if len(r.ExtProvenance) > 0 {
		var p model.Provenance
		if json.Unmarshal(r.ExtProvenance, &p) == nil {
			a.Provenance = &assetlib.Provenance{RunId: p.RunID, WorkflowId: p.WorkflowID, NodeId: p.NodeID, Operator: p.Operator,
				Model: p.Model, Params: string(p.Params), CreatedAt: p.CreatedAt}
		}
	} else if a.Source == model.SourceGenerated {
		// 没写过溯源的 AI 生成资产：用 Java 的来源任务号与创建时间兜底
		a.Provenance = &assetlib.Provenance{RunId: r.SourceTaskID.String, CreatedAt: a.CreatedAt}
	}
	return a
}

func toVersion(v *model.Version) *assetlib.Version {
	return &assetlib.Version{Id: v.ID, VersionNo: v.VersionNo, Image: v.ImageURL, Prompt: v.Prompt.String, Model: v.Model,
		Params: string(v.Params), CreatedAt: pagex.Ms(v.CreatedAt)}
}

func first(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// categoryItems all + 5 个 kind + uncategorized，固定顺序，没有的计 0。
func categoryItems(rows []repo.KindCount) []*assetlib.Category {
	counts := map[string]int64{}
	var all int64
	for _, r := range rows {
		k := r.Kind.String
		if !r.Kind.Valid || k == "" {
			k = "uncategorized"
		}
		counts[k] += r.N
		all += r.N
	}
	out := []*assetlib.Category{{Key: "all", Label: model.KindLabels["all"], Count: all}}
	for _, k := range model.Kinds {
		out = append(out, &assetlib.Category{Key: string(k), Label: model.KindLabels[string(k)], Count: counts[string(k)]})
	}
	return append(out, &assetlib.Category{Key: "uncategorized", Label: model.KindLabels["uncategorized"], Count: counts["uncategorized"]})
}

func uniq(ids []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id > 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// checkRelation 关系两端都必须是本人的资产，类型合法，不能自关联。
func checkRelation(ctx context.Context, s *svc.ServiceContext, in *assetlib.RelationReq) error {
	if !model.ValidRelation(in.RelationType) {
		return xerr.Param("relation_type 只能是 reference / generated_from / version_of")
	}
	if in.RelatedAssetId <= 0 || in.RelatedAssetId == in.Id {
		return xerr.Param("related_asset_id 不合法")
	}
	if err := needUser(in.UserId); err != nil {
		return err
	}
	ids, err := s.Repo.Asset.OwnedIDs(ctx, in.UserId, []int64{in.Id, in.RelatedAssetId})
	if err != nil {
		return err
	}
	if len(ids) != 2 {
		return errNotFound
	}
	return nil
}

// ensureTags 按名字取本人的标签，字典里没有的新建（受每用户上限约束），返回与 names 同序的 id。
func ensureTags(ctx context.Context, tx *repo.Repo, uid int64, names []string, perUser int) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	have, err := tx.Tag.ByNames(ctx, uid, names)
	if err != nil {
		return nil, err
	}
	byName := map[string]int64{}
	for _, t := range have {
		byName[strings.ToLower(t.Name)] = t.ID
	}
	var missing []string
	for _, n := range names {
		if _, ok := byName[strings.ToLower(n)]; !ok {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		cnt, err := tx.Tag.Count(ctx, uid)
		if err != nil {
			return nil, err
		}
		if cnt+int64(len(missing)) > int64(perUser) {
			return nil, xerr.Newf(xerr.ParamError, "标签最多 %d 个", perUser)
		}
		for _, n := range missing {
			t := &model.Tag{UserID: uid, Name: n}
			if err := tx.Tag.Insert(ctx, t); err != nil {
				if !repo.IsDuplicate(err) {
					return nil, err
				}
				// 排序规则认为同名（如全角 / 半角）而 ToLower 没对上：取库里那条
				got, err := tx.Tag.ByNames(ctx, uid, []string{n})
				if err != nil || len(got) == 0 {
					return nil, xerr.New(xerr.Conflict, "标签已存在")
				}
				t = got[0]
			}
			byName[strings.ToLower(n)] = t.ID
		}
	}
	out := make([]int64, 0, len(names))
	for _, n := range names {
		out = append(out, byName[strings.ToLower(n)])
	}
	return out, nil
}

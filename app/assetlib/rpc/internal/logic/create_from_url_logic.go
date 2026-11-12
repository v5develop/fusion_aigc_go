package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/assetlib/model"
	"aigc-local-life/app/assetlib/rpc/assetlib"
	"aigc-local-life/app/assetlib/rpc/internal/config"
	"aigc-local-life/app/assetlib/rpc/internal/repo"
	"aigc-local-life/app/assetlib/rpc/internal/svc"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type CreateFromUrlLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewCreateFromUrlLogic(ctx context.Context, svcCtx *svc.ServiceContext) *CreateFromUrlLogic {
	return &CreateFromUrlLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 画布「存为资产」默认标题、来源取值、去重键前缀、默认字节上限。
const (
	defaultFromURLTitle = "画布图片"
	sourceCanvas        = "canvas"
	dedupePrefix        = "cu:"
	defaultFromURLBytes = int64(10 << 20)
)

// 面向用户的拒收原因（msg 直接展示）。
var (
	errNotOwnImage = xerr.Param("只能把自己上传的图片或资产库里的图片存为资产，这张图的来源无法确认")
	errNotImage    = xerr.Param("只能存图片")
)

// CreateFromUrl 把用户自己的一张图存为资产（画布「存为资产」）。
//
//	归属：地址必须是 ① 本人在资产库里已有的图（直接返回该资产）② 本存储签发、且路径里的用户段就是本人的上传图；
//	      其它一律拒收（别人的上传、外链、InputURLPrefixes 白名单里的公共地址、mr:// 私有引用、data: / blob:）。
//	入库：交给媒体服务 ingest 到素材库公有桶（落桶时探测并解码校验图片，得到 sha256 / 类型 / 尺寸），不信任地址后缀与前端声明。
//	去重：key = 本人 + 内容 sha256；已有未删除的同内容资产时返回它（existing=true），重复点击、两张同内容的图都不会建第二条。
func (l *CreateFromUrlLogic) CreateFromUrl(in *assetlib.CreateFromUrlReq) (*assetlib.CreateFromUrlResp, error) {
	s := l.svcCtx
	d := &fromURLDeps{
		cfg:  s.Config,
		repo: s.Repo,
		ownsUpload: func(uid int64, u string) (string, bool) {
			if s.Storage == nil {
				return "", false
			}
			return ownUploadURL(s.Storage.Owns, s.Config.Storage.Prefix, uid, u)
		},
		load: func(ctx context.Context, uid, id int64) (*assetlib.Asset, error) {
			row, err := mustOwn(ctx, s, id, uid)
			if err != nil {
				return nil, err
			}
			out, err := toAssets(ctx, s, uid, []*repo.AssetRow{row})
			if err != nil {
				return nil, err
			}
			return out[0], nil
		},
		now: time.Now,
	}
	if s.Media != nil {
		d.ingest = func(ctx context.Context, src, idem string) (mediasvc.Resource, error) {
			return s.Media.IngestURL(ctx, src, mediasvc.PurposeAssetLibrary, idem)
		}
	}
	if s.Media != nil {
		d.remove = func(ctx context.Context, rid string) error { return s.Media.Delete(ctx, rid, "") }
	}
	return d.run(l.ctx, in)
}

// fromURLStore 数据访问（生产是 *repo.Repo，单测用假实现）。
type fromURLStore interface {
	IDByURL(ctx context.Context, userID int64, url string) (int64, error)
	CreateOnce(ctx context.Context, userID int64, key string, create func(ctx context.Context, tx *repo.Repo) (int64, error)) (int64, bool, error)
}

// repoStore 把 *repo.Repo 适配成 fromURLStore。
type repoStore struct{ r *repo.Repo }

func (s repoStore) IDByURL(ctx context.Context, uid int64, u string) (int64, error) {
	return s.r.Asset.IDByURL(ctx, uid, u)
}

func (s repoStore) CreateOnce(ctx context.Context, uid int64, key string, create func(ctx context.Context, tx *repo.Repo) (int64, error)) (int64, bool, error) {
	return s.r.CreateOnce(ctx, uid, key, create)
}

type fromURLDeps struct {
	cfg        config.Config
	repo       *repo.Repo // 生产用；单测设 store 即可
	store      fromURLStore
	ownsUpload func(uid int64, u string) (clean string, ok bool)
	ingest     func(ctx context.Context, srcURL, idem string) (mediasvc.Resource, error) // nil = 媒体服务未配置
	remove     func(ctx context.Context, rid string) error                               // 删除已落桶对象（清理孤儿）；nil = 不清理
	load       func(ctx context.Context, uid, id int64) (*assetlib.Asset, error)
	now        func() time.Time
}

func (d *fromURLDeps) st() fromURLStore {
	if d.store != nil {
		return d.store
	}
	return repoStore{d.repo}
}

func (d *fromURLDeps) run(ctx context.Context, in *assetlib.CreateFromUrlReq) (*assetlib.CreateFromUrlResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	lim := d.cfg.Limits
	// ---- 参数校验（全部通过才碰库 / 媒体服务）----
	raw := strings.TrimSpace(in.Url)
	if raw == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少图片地址")
	}
	if len(raw) > 1024 {
		return nil, xerr.Param("图片地址过长")
	}
	if strings.HasPrefix(strings.ToLower(raw), "data:") || strings.HasPrefix(strings.ToLower(raw), "blob:") {
		return nil, xerr.Param("不支持本地临时地址，请先上传图片")
	}
	if in.Source != "" && in.Source != sourceCanvas {
		return nil, xerr.Param("source 只能是 canvas")
	}
	title := stripControl(in.Title)
	if title == "" && strings.TrimSpace(in.Title) != "" {
		return nil, xerr.Param("标题不能只含控制字符")
	}
	if title == "" {
		title = defaultFromURLTitle
	}
	if runeLen(title) > lim.TitleLen && lim.TitleLen > 0 {
		return nil, xerr.Newf(xerr.ParamError, "标题最多 %d 字", lim.TitleLen)
	}
	canvasID, nodeID := strings.TrimSpace(in.CanvasId), strings.TrimSpace(in.NodeId)
	if len(canvasID) > 64 || len(nodeID) > 64 || strings.ContainsFunc(canvasID+nodeID, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return nil, xerr.Param("canvas_id / node_id 不合法")
	}
	tagNames, err := cleanTagNames(in.Tags, lim.TagNameLen, lim.TagsPerAsset)
	if err != nil {
		return nil, err
	}

	// ---- 归属 ----
	// ① 本人资产库里已有这张图（含画布自动入库的成品）：直接当作已存在，不复制。
	//    只按「本人 + 未删除」匹配，别人的素材库地址（哪怕同一个公有桶、在 InputURLPrefixes 白名单里）匹配不上。
	if id, err := d.st().IDByURL(ctx, in.UserId, raw); err != nil {
		return nil, err
	} else if id > 0 {
		return d.reply(ctx, in.UserId, id, true)
	}
	// ② 本人上传：本存储签发 + 路径里的用户段 = 本人。
	clean, ok := d.ownsUpload(in.UserId, raw)
	if !ok {
		logx.WithContext(ctx).Infof("[assetlib] 存为资产拒收：地址不是本人上传的图（判据 storage.Owns + 路径用户段 = 当前用户，且不在本人资产库） user=%d", in.UserId)
		return nil, errNotOwnImage
	}
	if d.ingest == nil {
		return nil, xerr.New(xerr.NotReady, "媒体服务暂未开放，请稍后再试")
	}

	// ---- 落素材库桶（媒体服务校验并解码图片；幂等键固定 → 重试得到同一个对象）----
	res, err := d.ingest(ctx, clean, ingestIdem(in.UserId, clean))
	if err != nil {
		return nil, mapIngestErr(ctx, err)
	}
	// 从这里起对象已经落进公有桶：任何不入库的出口都要清理，否则成孤儿对象（见 dropIngested）。
	if !strings.HasPrefix(strings.ToLower(res.Mime), "image/") {
		d.dropIngested(ctx, in.UserId, res, "不是图片")
		return nil, errNotImage
	}
	max := lim.FromURLBytes
	if max <= 0 {
		max = defaultFromURLBytes
	}
	if res.Bytes > max {
		d.dropIngested(ctx, in.UserId, res, "超过大小上限")
		return nil, xerr.Newf(xerr.ParamError, "图片不能超过 %d MB", max>>20)
	}
	libURL := strings.TrimSpace(res.URL)
	if libURL == "" || !cleanHTTPURL(libURL) { // afv_asset 只写公开地址，绝不写 mr://
		logx.WithContext(ctx).Errorf("[assetlib] 存为资产失败：媒体服务没有返回素材库公开地址 user=%d rid=%s", in.UserId, res.RID)
		d.dropIngested(ctx, in.UserId, res, "没有公开地址")
		return nil, xerr.New(xerr.ServerError, "素材库暂时不可用，请稍后再试")
	}
	if len(res.SHA256) < 32 {
		d.dropIngested(ctx, in.UserId, res, "缺少内容哈希")
		return nil, xerr.New(xerr.ServerError, "素材库暂时不可用，请稍后再试")
	}

	// ---- 按内容去重建行 ----
	key := dedupePrefix + strings.ToLower(res.SHA256[:32])
	props, _ := json.Marshal(map[string]any{"origin": sourceCanvas, "rid": res.RID, "sha256": strings.ToLower(res.SHA256), "bytes": res.Bytes})
	var prov json.RawMessage
	if canvasID != "" || nodeID != "" {
		prov, _ = json.Marshal(model.Provenance{WorkflowID: canvasID, NodeID: nodeID, Operator: sourceCanvas, CreatedAt: d.now().UnixMilli()})
	}
	id, created, err := d.st().CreateOnce(ctx, in.UserId, key, func(ctx context.Context, tx *repo.Repo) (int64, error) {
		id, err := tx.Asset.InsertUpload(ctx, repo.NewUpload{
			UserID: in.UserId, SourceTaskID: key, Name: title, URL: libURL, EntryCode: d.cfg.FromURLEntryCode,
			Width: int32(res.Width), Height: int32(res.Height), AspectRatio: aspectRatioOf(res.Width, res.Height), Properties: string(props),
		})
		if err != nil {
			return 0, err
		}
		if len(prov) > 0 {
			if err := tx.Ext.Upsert(ctx, in.UserId, &model.Ext{AssetID: id, UserID: in.UserId, Provenance: prov}); err != nil {
				return 0, err
			}
		}
		if len(tagNames) > 0 {
			ids, err := ensureTags(ctx, tx, in.UserId, tagNames, lim.TagsPerUser)
			if err != nil {
				return 0, err
			}
			if err := tx.Tag.ReplaceAssetTags(ctx, id, in.UserId, ids); err != nil {
				return 0, err
			}
		}
		return id, nil
	})
	if err != nil {
		if errors.Is(err, repo.ErrLockBusy) {
			// 不清理：同一地址的落桶是幂等的（同 rid），持锁的那个请求很可能正拿着这个对象建行，删了会让它的资产变死链；
			// 用户重试时若是同内容不同 rid 的重复对象，会在去重命中分支被清掉。
			return nil, xerr.New(xerr.Conflict, "这张图正在保存，请稍后再试")
		}
		d.dropIngested(ctx, in.UserId, res, "入库失败")
		return nil, err
	}
	if !created {
		// 同内容已有资产：这次落的对象若是另一个 rid 就是重复对象，删掉；与已有资产同一对象（重试同一地址）则必须保留。
		d.dropIngested(ctx, in.UserId, res, "内容重复，复用已有资产")
	}
	return d.reply(ctx, in.UserId, id, !created)
}

// dropIngested 清理已落进素材库公有桶、但最终不会被资产引用的对象。尽力而为：失败只记日志，不影响返回给用户的结果
// （媒体服务删除是软删除 + 保留期后清理，且对已不存在的对象幂等）。
// 安全阀：若本人有未删除资产的 result_url / cover_url 就是这个对象的地址（同一地址重试得到同一个 rid，对象被已有资产使用），不删；
// 查不出来也不删（宁可留孤儿，不删在用的图）。
func (d *fromURLDeps) dropIngested(ctx context.Context, uid int64, res mediasvc.Resource, why string) {
	if d.remove == nil || res.RID == "" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if u := strings.TrimSpace(res.URL); u != "" {
		id, err := d.st().IDByURL(ctx, uid, u)
		if err != nil {
			logx.WithContext(ctx).Errorf("[assetlib] 存为资产清理跳过：查对象是否在用失败 user=%d rid=%s err=%v", uid, res.RID, err)
			return
		}
		if id > 0 {
			return
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := d.remove(cctx, res.RID); err != nil {
		logx.WithContext(ctx).Errorf("[assetlib] 存为资产清理孤儿对象失败（%s） user=%d rid=%s err=%v", why, uid, res.RID, err)
		return
	}
	logx.WithContext(ctx).Infof("[assetlib] 存为资产已清理孤儿对象（%s） user=%d rid=%s", why, uid, res.RID)
}

func (d *fromURLDeps) reply(ctx context.Context, uid, id int64, existing bool) (*assetlib.CreateFromUrlResp, error) {
	a, err := d.load(ctx, uid, id)
	if err != nil {
		return nil, err
	}
	return &assetlib.CreateFromUrlResp{Asset: a, Existing: existing}, nil
}

// cleanTagNames 标签名：逐个清洗、不分大小写去重、数量上限；全部合法才返回。
func cleanTagNames(in []string, nameLen, perAsset int) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, n := range in {
		name, err := cleanTagName(n, nameLen)
		if err != nil {
			return nil, err
		}
		if k := strings.ToLower(name); !seen[k] {
			seen[k] = true
			out = append(out, name)
		}
	}
	if perAsset > 0 && len(out) > perAsset {
		return nil, xerr.Newf(xerr.ParamError, "每个资产最多 %d 个标签", perAsset)
	}
	return out, nil
}

// ownUploadURL 地址是否是 uid 本人通过上传接口传的图：返回去掉 query / fragment 的干净地址。
//
// 上传接口签发的 key 形如 <prefix>/<kind>/<userID>/<yyyymmdd>/<随机>.<ext>（storage.ObjectKey），所以除了「本存储签发」（owns），
// 还要求前缀后第一段是 image、第二段恰好是本人的 userID——别人的上传图同样是本存储签发的，只靠 owns 会放过。
// 路径里出现 % / .. / 反斜杠 / 空段一律拒收（防编码绕过与路径拼接）。
func ownUploadURL(owns func(string) bool, prefix string, uid int64, raw string) (string, bool) {
	p, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
		return "", false
	}
	if strings.ContainsAny(raw, "\\") || strings.Contains(p.EscapedPath(), "%") || prefix == "" || uid <= 0 {
		return "", false
	}
	clean := (&url.URL{Scheme: p.Scheme, Host: p.Host, Path: p.Path}).String()
	if owns == nil || !owns(clean) {
		return "", false
	}
	marker := "/" + strings.Trim(prefix, "/") + "/"
	i := strings.Index(p.Path, marker)
	if i < 0 {
		return "", false
	}
	segs := strings.Split(p.Path[i+len(marker):], "/")
	if len(segs) < 4 { // kind / uid / ... / 文件
		return "", false
	}
	for _, sg := range segs {
		if sg == "" || sg == "." || sg == ".." {
			return "", false
		}
	}
	if segs[0] != "image" || segs[1] != strconv.FormatInt(uid, 10) {
		return "", false
	}
	return clean, true
}

// ingestIdem 落桶幂等键：同一用户同一地址重试得到同一个 rid。
func ingestIdem(uid int64, u string) string {
	sum := sha256.Sum256([]byte(u))
	return fmt.Sprintf("assetlib-fromurl:%d:%s", uid, hex.EncodeToString(sum[:16]))
}

// mapIngestErr 媒体服务错误 → 面向用户的口径。4xx（图片损坏 / 类型不对 / 源不可读）是调用方的问题，5xx / 网络是暂时的。
func mapIngestErr(ctx context.Context, err error) error {
	var me *mediasvc.Error
	if errors.As(err, &me) && me.Status >= 400 && me.Status < 500 && me.Status != 429 && me.Status != 408 {
		logx.WithContext(ctx).Infof("[assetlib] 存为资产拒收：媒体服务 %d %s", me.Status, me.Code)
		return xerr.Param("这张图无法读取或不是有效的图片，请换一张再试")
	}
	logx.WithContext(ctx).Errorf("[assetlib] 存为资产失败：媒体服务暂时不可用: %v", err)
	return xerr.New(xerr.ServerError, "服务繁忙，请稍后再试")
}

// 常见比例，宽高比落在 3% 内就取它（素材库列表按 aspect_ratio 精确匹配，必须是 宽:高 各 1~2 位数字）。
var commonRatios = [][2]int{{1, 1}, {3, 4}, {4, 3}, {2, 3}, {3, 2}, {9, 16}, {16, 9}, {4, 5}, {5, 4}, {21, 9}, {9, 21}}

// aspectRatioOf 宽高 → 比例串；未知或不贴近任何常见比例返回空串。
func aspectRatioOf(w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	r := float64(w) / float64(h)
	for _, c := range commonRatios {
		cr := float64(c[0]) / float64(c[1])
		if d := r/cr - 1; d < 0.03 && d > -0.03 {
			return fmt.Sprintf("%d:%d", c[0], c[1])
		}
	}
	return ""
}

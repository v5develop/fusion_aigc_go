package logic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 区域蒙版的私有签名地址来源（POST /api/v1/studio/masks）：付费成品转存素材库前 / 永久失败时是 mr://，免费档是 mrw://，
// 读口（signViews）下发的是媒体服务私有桶的 10 分钟签名地址，前端拿它来识别区域。这种地址不在白名单里，
// 只有确认是「该用户自己的成品」时才放行。
//
// 精确口径（2026-11-11 只增，优先）：请求带来源标识，直接从归属记录取稳定 rid，不做任何反查——
//   - task_id（studio 子任务号）：按该用户未删除子任务的 result_url 取（resolveTask）；带了 image_url 时须与该成品一致
//     （私有成品比 host + path，公开地址比去查询串的地址），不一致按参数错误拒收。
//   - source_rid（画布候选）：网关先调 canvas-rpc CandidateSource（run 归属 = 本人、未删除、成功）取得 rid 再传进来；
//     studio 不读画布表，按调用方已确认的归属处理（与 user_id 同一信任口径：只有网关会填）。
//
// 兜底口径（老前端只传 image_url）：
//
//   - 地址 → rid：媒体服务对象键 = HMAC(密钥, rid) 单向派生（app/media/identity.go），路径
//     v1/<类别>/<yyyy>/<mm>/<对象键>[.<后缀>] 里反推不出 rid。先查本实例签出的反查表（signedIndex，键 = host + path）；
//     没有（别的实例签的 / 已过期）再在该用户的私有引用成品里按路径里的年月取候选、逐个现签，host + path 全等才算命中。
//     查询串（签名、过期时间、x-oss-process）一概不参与判断。
//   - 归属：反查表命中的 rid 再按用户查子任务（OwnsResultRID，未删除）；候选本身就是该用户未删除的成品。
//     画布（afv_canvas_node_run）的私有成品不在兜底之列（studio 不跨域读画布表）：走精确口径的 source_rid。
//   - 交给网关的不是用户给的地址，而是按 rid 现签的干净短期地址（水印视图也在原件上分割，与成品自动蒙版同口径）；
//     缓存键 / 幂等键按稳定引用 mr://{rid}（签名每次不同，水印视图与原件同一份蒙版），不按地址。

const (
	// maskOwnedScanMax 反查表没命中时最多现签比对多少个候选（按最近的先比，命中即停）
	maskOwnedScanMax = 100
	// maskOwnedSignChunk 每批现签多少个
	maskOwnedSignChunk = 20
	// maskSourceTTL 交给网关的干净签名地址有效期（与 maskSourceURL 一致；只给网关，不落库、不下发）
	maskSourceTTL = 10 * time.Minute
)

// maskPrivatePath 媒体服务私有桶对象路径（app/media/object_path.go datedPath）：路径以它结尾即可（路径式桶地址前面还有桶名）。
// 只认 user / work：asset 在公有素材库桶（走白名单），deliver 在第三方桶（ResourceURLs 签不了）。
var maskPrivatePath = regexp.MustCompile(`/v1/(user|work)/(\d{4})/(\d{2})/[a-z2-7]{26}(?:\.[a-z0-9]{1,8})?$`)

// maskShanghai 媒体服务按上海时区取路径里的年月。
var maskShanghai = time.FixedZone("CST", 8*3600)

// maskSigner 现签干净地址（*mediasvc.Client 实现）。
type maskSigner interface {
	ResourceURLs(ctx context.Context, rids []string, ttl time.Duration) (map[string]string, error)
}

// maskOwner 私有成品归属（*repo.TaskRepo 实现）。
type maskOwner interface {
	OwnsResultRID(ctx context.Context, userID int64, rid string) (bool, error)
	OwnedResultURL(ctx context.Context, userID int64, taskID string) (string, bool, error)
	ListPrivateResultRIDs(ctx context.Context, userID int64, class string,
		createdFrom, createdTo, finishedFrom, finishedTo time.Time, limit int) ([]string, error)
}

// maskPrivateRef 私有成品的稳定引用（缓存键 / 幂等键用）。
func maskPrivateRef(rid string) string { return mrPrefix + rid }

// maskRefKey 稳定引用的缓存键（sha256 hex，与 normalizeMaskURL 同形）。
func maskRefKey(ref string) string {
	sum := sha256.Sum256([]byte(ref))
	return hex.EncodeToString(sum[:])
}

// parseMaskPrivatePath 签名地址 → (反查键 host+path, rid 类别, 年月起点)；不像媒体服务私有桶对象地址返回 ok=false。
// 只做形状判断，归属与 host 是否真是本平台私有桶由 resolveOwned 用现签结果比对。
func parseMaskPrivatePath(raw string) (key, class string, month time.Time, ok bool) {
	if !cleanHTTPURL(raw) {
		return "", "", time.Time{}, false
	}
	p, err := url.Parse(raw)
	if err != nil || p.Fragment != "" {
		return "", "", time.Time{}, false
	}
	m := maskPrivatePath.FindStringSubmatch(p.EscapedPath())
	if m == nil {
		return "", "", time.Time{}, false
	}
	y, _ := strconv.Atoi(m[2])
	mo, _ := strconv.Atoi(m[3])
	if y < 2000 || mo < 1 || mo > 12 {
		return "", "", time.Time{}, false
	}
	return urlIndexKey(raw), m[1], time.Date(y, time.Month(mo), 1, 0, 0, 0, 0, maskShanghai), true
}

// resolveOwned 用户给的私有签名地址 → (rid, 干净短期签名地址)；不是该用户自己的私有成品返回 ok=false（调用方按来源不合法拒收）。
func (m *masker) resolveOwned(ctx context.Context, userID int64, raw string) (string, string, bool) {
	log := logx.WithContext(ctx)
	if m == nil || m.sign == nil || m.owner == nil || userID <= 0 {
		return "", "", false
	}
	key, class, month, ok := parseMaskPrivatePath(raw)
	if !ok || key == "" {
		return "", "", false
	}
	now := m.now()
	// 1) 本实例签过：路径 → rid 一一对应（对象键由 rid 单向派生），反查结果可信，只差归属
	if rid := ridOfSignedURL(raw, now); rid != "" {
		owned, err := m.owner.OwnsResultRID(ctx, userID, rid)
		if err != nil {
			log.Errorw("studio mask: own private url check failed (check=mask_url_owned_private)", logx.Field("err", err.Error()))
			return "", "", false
		}
		if !owned {
			log.Infow("studio mask: private url belongs to another user or deleted task (check=mask_url_owned_private)",
				logx.Field("user_id", userID), logx.Field("rid_hint", ridHint(rid)))
			return "", "", false
		}
		u, err := m.sign.ResourceURLs(ctx, []string{rid}, maskSourceTTL)
		if err != nil || u[rid] == "" {
			log.Errorw("studio mask: sign private source failed (check=mask_source_sign)", logx.Field("rid_hint", ridHint(rid)),
				logx.Field("err", maskSignErr(err)))
			return "", "", false
		}
		indexSigned(rid, u[rid], maskSourceTTL, now)
		return rid, u[rid], true
	}
	// 2) 别的实例签的 / 反查表已过期：在该用户按路径年月取的候选里逐个现签比对 host + path
	end := month.AddDate(0, 1, 0)
	day := 24 * time.Hour
	rids, err := m.owner.ListPrivateResultRIDs(ctx, userID, class,
		month.Add(-7*day), end.Add(day), // 创建早于落桶（生成耗时 / 重试），往前多看一周
		month.Add(-day), end.Add(7*day), maskOwnedScanMax) // 完成晚于落桶，往后多看一周
	if err != nil {
		log.Errorw("studio mask: list private results failed (check=mask_url_owned_private)", logx.Field("err", err.Error()))
		return "", "", false
	}
	for i := 0; i < len(rids); i += maskOwnedSignChunk {
		if ctx.Err() != nil {
			return "", "", false
		}
		chunk := validRIDs(rids[i:min(i+maskOwnedSignChunk, len(rids))])
		for rid, u := range m.signEach(ctx, chunk) {
			indexSigned(rid, u, maskSourceTTL, now) // 顺手进反查表：之后同一实例直接命中
			if urlIndexKey(u) == key {
				return rid, u, true
			}
		}
	}
	log.Infow("studio mask: private url not found in own results (check=mask_url_owned_private)",
		logx.Field("user_id", userID), logx.Field("candidates", len(rids)))
	return "", "", false
}

// signEach 整批现签；整批失败（个别 rid 坏）时逐个重签，只丢坏的那几个。
func (m *masker) signEach(ctx context.Context, rids []string) map[string]string {
	if len(rids) == 0 {
		return nil
	}
	got, err := m.sign.ResourceURLs(ctx, rids, maskSourceTTL)
	if err == nil {
		return got
	}
	got = map[string]string{}
	for _, rid := range rids {
		if ctx.Err() != nil {
			break
		}
		one, e := m.sign.ResourceURLs(ctx, []string{rid}, maskSourceTTL)
		if e != nil {
			logx.WithContext(ctx).Infow("studio mask: warning sign candidate failed, skipped (check=mask_source_sign)",
				logx.Field("rid_hint", ridHint(rid)), logx.Field("err", e.Error()))
			continue
		}
		got[rid] = one[rid]
	}
	return got
}

// validRIDs 只留合法 rid（ResourceURLs 遇到不合法的会整批失败）。
func validRIDs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, r := range in {
		if mediasvc.ValidRID(r) {
			out = append(out, r)
		}
	}
	return out
}

// maskSignErr 现签失败原因（只进日志）：没报错但没给地址时写明。
func maskSignErr(err error) string {
	if err == nil {
		return "empty url"
	}
	return err.Error()
}

// errMaskTaskMismatch 带了 task_id 又带了 image_url，但两者不是同一张图。
var errMaskTaskMismatch = xerr.Param("图片地址与任务的成品不一致")

// resolveTask task_id → (rid, 干净短期签名地址, 公开地址)：私有成品给 rid + 现签地址，公开成品给落库的长期地址（rid 为空）。
// 不是该用户未删除子任务的成品返回 ok=false（调用方按来源不合法拒收）；imageURL 非空时须指同一张图，否则返回 errMaskTaskMismatch。
func (m *masker) resolveTask(ctx context.Context, userID int64, taskID, imageURL string) (rid, src, public string, ok bool, err error) {
	log := logx.WithContext(ctx)
	if m == nil || m.owner == nil || userID <= 0 || taskID == "" {
		return "", "", "", false, nil
	}
	ref, found, qerr := m.owner.OwnedResultURL(ctx, userID, taskID)
	if qerr != nil {
		log.Errorw("studio mask: load own task result failed (check=mask_task_owned)", logx.Field("err", qerr.Error()))
		return "", "", "", false, xerr.New(xerr.ServerError, "暂时无法读取任务，请稍后再试")
	}
	if !found {
		log.Infow("studio mask: task not owned / deleted / no result (check=mask_task_owned)",
			logx.Field("user_id", userID), logx.Field("task_id", taskID))
		return "", "", "", false, nil
	}
	rid = privateRidOf(ref)
	if rid == "" {
		if strings.HasPrefix(ref, mrPrefix) || isWatermarkRef(ref) {
			return "", "", "", false, nil // 引用但 rid 不合法
		}
		if imageURL != "" {
			a, _, e1 := normalizeMaskURL(imageURL)
			b, _, e2 := normalizeMaskURL(ref)
			if e1 != nil || e2 != nil || a != b {
				return "", "", "", false, errMaskTaskMismatch
			}
		}
		return "", "", ref, true, nil
	}
	if m.sign == nil {
		return "", "", "", false, nil
	}
	u, serr := m.sign.ResourceURLs(ctx, []string{rid}, maskSourceTTL)
	if serr != nil || u[rid] == "" {
		log.Errorw("studio mask: sign private source failed (check=mask_source_sign)", logx.Field("rid_hint", ridHint(rid)),
			logx.Field("err", maskSignErr(serr)))
		return "", "", "", false, errMaskUnavailable
	}
	if imageURL != "" && urlIndexKey(imageURL) != urlIndexKey(u[rid]) {
		return "", "", "", false, errMaskTaskMismatch
	}
	indexSigned(rid, u[rid], maskSourceTTL, m.now())
	return rid, u[rid], "", true, nil
}

// signRID 调用方已确认归属的私有 rid → 干净短期签名地址（画布候选，见文件头 source_rid）。
func (m *masker) signRID(ctx context.Context, rid string) (string, error) {
	if m == nil || m.sign == nil || !mediasvc.ValidRID(rid) {
		return "", xerr.Param("图片地址不合法")
	}
	u, err := m.sign.ResourceURLs(ctx, []string{rid}, maskSourceTTL)
	if err != nil || u[rid] == "" {
		logx.WithContext(ctx).Errorw("studio mask: sign private source failed (check=mask_source_sign)", logx.Field("rid_hint", ridHint(rid)),
			logx.Field("err", maskSignErr(err)))
		return "", errMaskUnavailable
	}
	return u[rid], nil
}

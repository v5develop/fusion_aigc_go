package logic

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/xerr"
)

// 上游引用的服务端解析（只在同一张画布内），**受理时解析一次并冻结**，之后每次派发只现签冻结的长期引用。
//
// 先交付后转存之后，上游产物在转存完成前是 mr:// 引用、读时现签 10 分钟；前端请求体里带的地址可能早已过期
// （run 排队超过 10 分钟、或前端拿着旧签名）。所以受理（RunNode）时按服务端记录解析：
//
//   - 图片口带 candidate_index：上游节点最近一次成功 run 的第 i 个候选；
//   - 视频口带 node_id：上游节点最近一次成功 run 的 video_url；
//   - 以上都没有命中、但请求地址是我们自己签出的地址（带 OSS / S3 签名参数，且路径与本画布某个产物现签后一致）：
//     换回该产物的长期引用；工具参数 image_url / garment_image / models[].image 同理；
//   - 其它（用户上传、外部地址、素材库公开地址）原样使用。
//
// 解析结果（长期引用 + 上游 run 主键 + 候选下标）冻结进落库请求的 frozen 字段。排队后重新派发（specFromRun）只用冻结值，
// 不再看「最近一次成功」：受理后上游重跑成功也不会换掉输入——否则同一个网关幂等键下请求内容变了（网关 409 幂等冲突，
// run 被判失败），语义上也是拿用户没提交过的输入出图。重新派发时按冻结的 run 主键读该 run 同一下标的当前值
// （转存完成后它从 mr:// 换成素材库地址，内容是同一份拷贝），读不到再用冻结的引用本身；然后现签。
//
// 为什么现签不改变网关幂等哈希：网关输入图一律先经媒体服务 ingest 成 mr://{rid} + sha256（gatewayRequests → mediaRef），
// 网关 request_hash 对带 sha256 的 MediaRef 只取 sha256（contract/media.py），签名串 / rid 变了哈希也不变；
// 只有内容变了才会变——冻结保证的正是内容不变。Java 工具另有 backend_request 逐字节重发。
//
// 归属：受理时只查 canvasID 下的 run（画布已按用户校验归属），别的画布 / 别的用户的节点与产物一律查不到，
// 签名地址也只会映射到本画布自己的产物上；冻结后派发时读上游 run 也校验它仍属本画布，否则只用冻结的引用（受理时已校验过归属）。
//
// 旧 run（本改动之前受理、落库请求没有 frozen 字段）重新派发时保持原行为：只按 candidate_index 取最近一次成功的候选。

// maxOwnSignedProbe 识别「自己签出的地址」时最多现签比对的产物引用数（兜住大画布；只在受理时、且地址带签名参数时才比对）。
const maxOwnSignedProbe = 32

// frozenRef 受理时冻结的一个上游引用。
type frozenRef struct {
	Ref   string `json:"ref"`              // 长期引用：mr:// / mrw:// 或素材库公开地址（绝不是签名地址）
	RunID int64  `json:"run_id,omitempty"` // 上游 run 主键（派发时读它同一下标的当前值：转存后换成素材库地址）
	Index int    `json:"index"`            // 候选下标；-1 = 视频
}

// frozenRefs afv_canvas_node_run.request.frozen：槽位 → 冻结引用。槽位：references.{落库顺序下标} / params.image_url /
// params.garment_image / params.models.{i}.image / params.models.{i}。没有的槽位 = 受理时用的就是请求地址，派发时照旧。
type frozenRefs struct {
	Refs map[string]frozenRef `json:"refs,omitempty"`
}

type refMode int8

const (
	refAccept refMode = iota // 受理：按最近一次成功解析并记录
	refFrozen                // 派发：只用冻结值
	refLegacy                // 旧 run 派发：原行为（只按 candidate_index 取最近一次成功）
)

// refLookup 同画布上游引用解析器。nil 安全：什么都不解析，调用方按请求地址处理。
type refLookup struct {
	c        *core
	ctx      context.Context
	canvasID string
	mode     refMode

	frozen *frozenRefs // refFrozen：落库的冻结值
	out    *frozenRefs // refAccept：本次解析结果

	loaded bool
	runs   map[string]*model.NodeRun // node_id → 最近一次成功 run
	byID   map[int64]*model.NodeRun  // 冻结的上游 run（派发时按主键读）
	signed map[string]string         // 引用 → 现签地址（本次构造内缓存）
}

// refResolver 受理用：按本画布各节点最近一次成功的 run 解析（懒加载），结果由 frozenOut 取出落库。
func (c *core) refResolver(ctx context.Context, canvasID string) *refLookup {
	return &refLookup{c: c, ctx: ctx, canvasID: canvasID, mode: refAccept, out: &frozenRefs{}}
}

// runRefResolver 重新派发用：有冻结值只用冻结值；旧 run 保持原行为。
func (c *core) runRefResolver(ctx context.Context, run *model.NodeRun, s storedRequest) *refLookup {
	l := &refLookup{c: c, ctx: ctx, canvasID: run.CanvasID, mode: refLegacy}
	if s.Frozen != nil {
		l.mode, l.frozen = refFrozen, s.Frozen
	}
	return l
}

// frozenOut 受理时的解析结果（非受理模式为 nil）。没有任何解析也返回非 nil 的空值：落库后据此区分新旧 run。
func (l *refLookup) frozenOut() *frozenRefs {
	if l == nil || l.mode != refAccept {
		return nil
	}
	return l.out
}

func refSlot(i int) string { return "references." + strconv.Itoa(i) }

func (l *refLookup) load() {
	if l.loaded {
		return
	}
	l.loaded = true
	if l.runs == nil {
		l.runs = map[string]*model.NodeRun{}
	}
	if l.c == nil || l.c.s == nil || l.c.s.Repo == nil || l.canvasID == "" {
		return
	}
	if runs, err := l.c.s.Repo.Run.LatestSucceededByNode(l.ctx, l.canvasID); err == nil {
		for _, r := range runs {
			if r.CanvasID == "" || r.CanvasID == l.canvasID {
				l.runs[r.NodeID] = r
			}
		}
	}
}

func (l *refLookup) latest(nodeID string) *model.NodeRun {
	if strings.TrimSpace(nodeID) == "" {
		return nil
	}
	l.load()
	return l.runs[nodeID]
}

// latestCandidate 上游节点最近一次成功 run 的第 i 个候选；找不到 ok=false。
func (l *refLookup) latestCandidate(nodeID string, i int) (frozenRef, bool) {
	r := l.latest(nodeID)
	if r == nil {
		return frozenRef{}, false
	}
	list := r.CandidateList()
	if i < 0 || i >= len(list) || strings.TrimSpace(list[i].URL) == "" {
		return frozenRef{}, false
	}
	return frozenRef{Ref: list[i].URL, RunID: r.ID, Index: i}, true
}

// candidateDims 上游节点最近一次成功 run 的第 i 个候选的宽高（产出时记录；没有 / 未记录返回 0, 0）。
func (l *refLookup) candidateDims(nodeID string, i int) (int, int) {
	if l == nil {
		return 0, 0
	}
	r := l.latest(nodeID)
	if r == nil {
		return 0, 0
	}
	list := r.CandidateList()
	if i < 0 || i >= len(list) {
		return 0, 0
	}
	return list[i].W, list[i].H
}

// latestVideo 上游节点最近一次成功 run 的视频；上游不是视频产物 / 找不到 ok=false。
func (l *refLookup) latestVideo(nodeID string) (frozenRef, bool) {
	r := l.latest(nodeID)
	if r == nil || !r.VideoURL.Valid || strings.TrimSpace(r.VideoURL.String) == "" {
		return frozenRef{}, false
	}
	return frozenRef{Ref: r.VideoURL.String, RunID: r.ID, Index: -1}, true
}

// image 图片口地址。cand 为请求里的 candidate_index（可空）。
func (l *refLookup) image(slot, nodeID string, cand *int32, raw string) (string, error) {
	if l == nil {
		return raw, nil
	}
	switch l.mode {
	case refFrozen:
		return l.thaw(slot, nodeID, raw)
	case refLegacy:
		if cand != nil {
			if f, ok := l.latestCandidate(nodeID, int(*cand)); ok {
				if u := l.signSoft(f.Ref); u != "" {
					return u, nil
				}
			}
		}
		return raw, nil
	}
	if cand != nil {
		if f, ok := l.latestCandidate(nodeID, int(*cand)); ok {
			return l.freeze(slot, nodeID, f)
		}
	}
	if f, ok := l.ownSigned(nodeID, raw); ok {
		return l.freeze(slot, nodeID, f)
	}
	return raw, nil
}

// video 视频口地址：上游是视频产物时用它（不信请求里的地址），否则按签名地址映射，再否则原样。
func (l *refLookup) video(slot, nodeID, raw string) (string, error) {
	if l == nil {
		return raw, nil
	}
	switch l.mode {
	case refFrozen:
		return l.thaw(slot, nodeID, raw)
	case refLegacy:
		return raw, nil
	}
	if f, ok := l.latestVideo(nodeID); ok {
		return l.freeze(slot, nodeID, f)
	}
	if f, ok := l.ownSigned(nodeID, raw); ok {
		return l.freeze(slot, nodeID, f)
	}
	return raw, nil
}

// param 工具参数里的输入图地址：本画布产物的签名地址换回长期引用现签；其它原样。
func (l *refLookup) param(slot, raw string) (string, error) {
	if l == nil {
		return raw, nil
	}
	switch l.mode {
	case refFrozen:
		return l.thaw(slot, "", raw)
	case refLegacy:
		return raw, nil
	}
	if f, ok := l.ownSigned("", raw); ok {
		return l.freeze(slot, "", f)
	}
	return raw, nil
}

// freeze 受理：记录冻结值并现签。
func (l *refLookup) freeze(slot, nodeID string, f frozenRef) (string, error) {
	if l.out == nil {
		l.out = &frozenRefs{}
	}
	if l.out.Refs == nil {
		l.out.Refs = map[string]frozenRef{}
	}
	l.out.Refs[slot] = f
	return l.signStrict(nodeID, f.Ref)
}

// thaw 派发：槽位有冻结值 → 冻结的上游 run 同一下标的当前值（读不到用冻结引用本身）现签；没有 → 请求地址原样。
func (l *refLookup) thaw(slot, nodeID, raw string) (string, error) {
	if l.frozen == nil {
		return raw, nil
	}
	f, ok := l.frozen.Refs[slot]
	if !ok {
		return raw, nil
	}
	ref, err := l.current(f)
	if err != nil {
		return "", err
	}
	return l.signStrict(nodeID, ref)
}

// current 冻结的上游 run 同一下标的当前长期引用：转存完成后是素材库地址（同一份内容的拷贝）。
// 上游 run 不在 / 不属本画布 / 不再是成功态 / 下标没有值 → 冻结的引用本身。查库失败是暂时性错误（留在排队重试）。
func (l *refLookup) current(f frozenRef) (string, error) {
	if f.RunID <= 0 || l.c == nil || l.c.s == nil || l.c.s.Repo == nil {
		return f.Ref, nil
	}
	r, seen := l.byID[f.RunID]
	if !seen {
		var err error
		if r, err = l.c.s.Repo.Run.Get(l.ctx, f.RunID); err != nil {
			return "", temporary(fmt.Errorf("读取上游 run %d 失败: %w", f.RunID, err))
		}
		if l.byID == nil {
			l.byID = map[int64]*model.NodeRun{}
		}
		l.byID[f.RunID] = r
	}
	if r == nil || r.CanvasID != l.canvasID || r.Status != model.RunSucceeded {
		return f.Ref, nil
	}
	cur := ""
	if f.Index < 0 {
		if r.VideoURL.Valid {
			cur = r.VideoURL.String
		}
	} else if list := r.CandidateList(); f.Index < len(list) {
		cur = list[f.Index].URL
	}
	if cur = strings.TrimSpace(cur); cur == "" {
		return f.Ref, nil
	}
	return cur, nil
}

// signStrict 长期引用 → 可读地址：mr:// / mrw:// 现签，素材库公开地址原样。签不出时报错（不回退到请求地址：
// 冻结的输入与实际下发必须一致）：媒体服务 4xx（对象不存在等）→ 参数错误（永久，退款）；其它 → 暂时性错误（排队重试）。
func (l *refLookup) signStrict(nodeID, ref string) (string, error) {
	if !isMediaRef(ref) {
		return ref, nil
	}
	if v := l.signed[ref]; v != "" {
		return v, nil
	}
	gone := xerr.Param(fmt.Sprintf("引用 %s 的上游产物已不可用，请重新执行上游节点", firstNonEmpty(nodeID, "（工具参数）")))
	rid, wm, _ := refRID(ref)
	if rid == "" {
		return "", gone
	}
	var u string
	if wm || l.c == nil || l.c.s == nil || l.c.s.Media == nil {
		// 水印视图（只能签带水印参数的地址）/ 未接媒体服务（仅开发）：走通用现签，签不出为空
		u = l.signSoft(ref)
	} else {
		urls, err := l.c.s.Media.ResourceURLs(l.ctx, []string{rid}, refURLTTL)
		if err != nil {
			var me *mediasvc.Error
			if errors.As(err, &me) && me.Permanent() {
				return "", gone
			}
			return "", temporary(fmt.Errorf("上游产物现签失败 rid=%s: %w", ridHint(rid), err))
		}
		u = urls[rid]
		l.cache(ref, u)
	}
	if u == "" {
		return "", gone
	}
	return u, nil
}

// signSoft 现签，失败为空串（比对 / 旧行为用）。
func (l *refLookup) signSoft(ref string) string {
	if !isMediaRef(ref) {
		return ref
	}
	if v, ok := l.signed[ref]; ok {
		return v
	}
	u := ref
	if l.c != nil {
		l.c.resolveMediaRefs(l.ctx, []*string{&u})
	} else {
		u = ""
	}
	l.cache(ref, u)
	return u
}

func (l *refLookup) cache(ref, u string) {
	if l.signed == nil {
		l.signed = map[string]string{}
	}
	l.signed[ref] = u
}

// ownSigned 请求地址是本画布某个产物的（可能已过期的）签名地址 → 该产物的冻结值。
// 先比对 nodeID 自己的产物，再比对本画布其它节点的（例如把候选拖成了独立的图片节点）；最多现签 maxOwnSignedProbe 个，并发进行。
// 比对只看路径：路径末段是媒体服务用密钥派生的对象键（不可猜、全局唯一），换了 CDN / 自定义域名也能认回；
// 认回的只可能是本画布自己的产物，不会把别人的资源签出来。
func (l *refLookup) ownSigned(nodeID, raw string) (frozenRef, bool) {
	if l == nil || !looksSigned(raw) {
		return frozenRef{}, false
	}
	key := objectPath(raw)
	if key == "" {
		return frozenRef{}, false
	}
	l.load()
	order := make([]*model.NodeRun, 0, len(l.runs))
	if r := l.runs[nodeID]; r != nil {
		order = append(order, r)
	}
	others := make([]string, 0, len(l.runs))
	for id := range l.runs {
		if id != nodeID {
			others = append(others, id)
		}
	}
	sort.Strings(others)
	for _, id := range others {
		order = append(order, l.runs[id])
	}
	var probes []frozenRef
collect:
	for _, r := range order {
		for i, x := range r.CandidateList() {
			if isMediaRef(x.URL) { // 素材库公开地址是长期地址，前端拿到的就是它本身，不会是签名地址
				if len(probes) >= maxOwnSignedProbe {
					break collect
				}
				probes = append(probes, frozenRef{Ref: x.URL, RunID: r.ID, Index: i})
			}
		}
		if r.VideoURL.Valid && isMediaRef(r.VideoURL.String) {
			if len(probes) >= maxOwnSignedProbe {
				break
			}
			probes = append(probes, frozenRef{Ref: r.VideoURL.String, RunID: r.ID, Index: -1})
		}
	}
	if len(probes) == 0 {
		return frozenRef{}, false
	}
	// 并发现签（各自独立的 HTTP 调用），按顺序取第一个路径一致的
	urls := make([]string, len(probes))
	limit := 4
	if l.c != nil && l.c.s != nil {
		limit = l.c.fanoutLimit()
	}
	cached := make([]bool, len(probes))
	for i, p := range probes {
		if v, ok := l.signed[p.Ref]; ok {
			urls[i], cached[i] = v, true
		}
	}
	fanout(len(probes), limit, func(i int) error {
		if !cached[i] {
			u := probes[i].Ref
			if l.c != nil {
				l.c.resolveMediaRefs(l.ctx, []*string{&u})
			} else {
				u = ""
			}
			urls[i] = u
		}
		return nil
	})
	for i, p := range probes {
		if !cached[i] {
			l.cache(p.Ref, urls[i])
		}
	}
	for i, p := range probes {
		if urls[i] != "" && objectPath(urls[i]) == key {
			return p, true
		}
	}
	return frozenRef{}, false
}

// looksSigned 地址是否带对象存储的查询签名（OSS V1 Signature+Expires/OSSAccessKeyId、OSS V4 x-oss-signature、S3 X-Amz-Signature）。
// 误判为签名（如 CloudFront 的 Signature+Expires）只会多做一次比对、比对不上原样用；漏判（别家签名格式）按原地址拉取，与此前一致。
func looksSigned(raw string) bool {
	if !isHTTP(raw) {
		return false
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.RawQuery == "" {
		return false
	}
	keys := map[string]bool{}
	for k := range u.Query() {
		keys[strings.ToLower(k)] = true
	}
	switch {
	case keys["signature"] && (keys["expires"] || keys["ossaccesskeyid"]):
		return true
	case keys["x-oss-signature"], keys["x-amz-signature"]:
		return true
	}
	return false
}

// objectPath 地址的路径（忽略协议、主机与查询串），用来判断两个签名地址是否指向同一个对象。
// 路径太短（根路径 / 单段无扩展名）不参与比对，免得把无关地址认成同一个对象。
func objectPath(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	p := u.EscapedPath()
	if len(p) < 8 {
		return ""
	}
	return p
}

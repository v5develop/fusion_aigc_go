package logic

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
	"google.golang.org/protobuf/proto"
)

// 智能体工具的读取与生成：canvas_list / canvas_get / canvas_catalog / canvas_run_node / canvas_get_run。

func (c *core) agentList(ctx context.Context, userID int64, args map[string]any) (any, error) {
	res, err := c.ListCanvases(ctx, &canvas.ListCanvasesReq{UserId: userID, Page: int32(intData(args, "page")), PageSize: int32(intData(args, "page_size"))})
	if err != nil {
		return nil, err
	}
	list := make([]map[string]any, 0, len(res.List))
	for _, x := range res.List {
		list = append(list, map[string]any{"canvas_id": x.Id, "title": x.Title, "revision": x.Revision, "node_count": x.NodeCount,
			"cover_url": x.CoverUrl, "updated_at": x.UpdatedAt})
	}
	return map[string]any{"total": res.Total, "canvases": list}, nil
}

func (c *core) agentGet(ctx context.Context, userID int64, args map[string]any) (any, error) {
	cid := strData(args, "canvas_id")
	runtime := true
	if v, ok := args["include_runtime"].(bool); ok {
		runtime = v
	}
	var (
		title, rev, snap string
		count            int32
	)
	if runtime {
		doc, err := c.GetCanvas(ctx, &canvas.CanvasIdReq{UserId: userID, Id: cid})
		if err != nil {
			return nil, err
		}
		title, rev, snap, count = doc.Title, doc.Revision, doc.Snapshot, doc.NodeCount
	} else {
		x, err := c.s.Repo.Canvas.GetOwned(ctx, userID, cid)
		if err != nil {
			return nil, err
		}
		if x == nil {
			return nil, xerr.NotFoundf("画布")
		}
		title, rev, snap, count = x.Title, x.Revision, string(x.Snapshot), x.NodeCount
	}
	d, err := ParseSnapshot([]byte(snap), 0)
	if err != nil {
		return nil, err
	}
	var only map[string]bool
	if ids, ok := args["node_ids"].([]any); ok && len(ids) > 0 {
		only = map[string]bool{}
		for _, v := range ids {
			if s, ok := v.(string); ok {
				only[s] = true
			}
		}
	}
	nodes, edges := agentView(d, only)
	return map[string]any{"canvas_id": cid, "title": title, "revision": rev, "node_count": count, "nodes": nodes, "edges": edges}, nil
}

func (c *core) agentCatalog(ctx context.Context, userID int64, kind string) (any, error) {
	res, err := c.Catalog(ctx, &canvas.CatalogReq{UserId: userID})
	if err != nil {
		return nil, err
	}
	var list []*canvas.CatalogModel
	if kind != "video" {
		list = append(list, res.ImageModels...)
	}
	if kind != "image" {
		list = append(list, res.VideoModels...)
	}
	models := make([]map[string]any, 0, len(list))
	for _, m := range list {
		prices := make([]map[string]any, 0, len(m.Prices))
		for _, p := range m.Prices {
			prices = append(prices, map[string]any{"size": p.Size, "duration": p.Duration, "points": p.Points})
		}
		models = append(models, map[string]any{
			"model": m.Model, "model_id": m.ModelId, "label": m.Label, "kind": m.Kind, "ready": m.Ready,
			"ratios": nonNil(m.Ratios), "sizes": nonNil(m.Sizes), "qualities": nonNil(m.Qualities), "durations": nonNil(m.Durations),
			"sound": m.Sound, "count_max": m.CountMax, "max_image_refs": m.MaxImageRefs, "prices": prices,
		})
	}
	return map[string]any{"models": models}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func parseRequestID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, xerr.Param("request_id 不合法")
	}
	return id, nil
}

// runView RunResult → 工具出参。
func runView(r *canvas.RunResult) map[string]any {
	cands := make([]map[string]any, 0, len(r.Candidates))
	for _, x := range r.Candidates {
		cands = append(cands, map[string]any{"url": x.Url, "width": x.Width, "height": x.Height})
	}
	return map[string]any{
		"request_id": r.RequestId, "run_id": r.RunId, "canvas_id": r.CanvasId, "node_id": r.NodeId, "kind": r.Kind,
		"status": r.Status, "progress": r.Progress, "model": r.Model, "candidates": cands, "selected_index": r.SelectedIndex,
		"video_url": r.VideoUrl, "text": r.Text, "credits": r.Credits, "error": r.Error, "error_code": r.ErrorCode,
		"retryable": r.Retryable, "library_status": r.LibraryStatus, "created_at": r.CreatedAt, "finished_at": r.FinishedAt,
		"cancel_state": r.CancelState,
	}
}

func finalStatus(s string) bool {
	return model.RunFinal(model.ParseStatus(s))
}

// agentGetRun 查询一次生成；wait_seconds>0 时每 agentPollInterval 查一次直到终态或超时。
func (c *core) agentGetRun(ctx context.Context, userID int64, args map[string]any) (any, error) {
	id, err := parseRequestID(strData(args, "request_id"))
	if err != nil {
		return nil, err
	}
	deadline := c.now().Add(time.Duration(intData(args, "wait_seconds")) * time.Second)
	for {
		r, err := c.GetRun(ctx, &canvas.RunIdReq{UserId: userID, Id: id})
		if err != nil {
			return nil, err
		}
		if finalStatus(r.Status) || !c.now().Before(deadline) {
			return runView(r), nil
		}
		select {
		case <-ctx.Done():
			return runView(r), nil
		case <-time.After(agentPollInterval):
		}
	}
}

// agentRunID canvas_run_node 的 run_id：显式给的 > 由 call_id 派生（同一次工具调用重试不重复扣点）> 随机。
// attempt 为同一 call_id 的第几次重跑（0 起；0 与旧派生口径一致，已落库的 run 照旧命中）。
func agentRunID(userID int64, explicit, callID string, attempt int) string {
	if s := strings.TrimSpace(explicit); s != "" {
		return s
	}
	if callID != "" {
		seed := fmt.Sprintf("%d:%s", userID, callID)
		if attempt > 0 {
			seed += fmt.Sprintf("#%d", attempt)
		}
		sum := sha256.Sum256([]byte(seed))
		return "ag_" + hex.EncodeToString(sum[:12])
	}
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "ag_" + hex.EncodeToString(b)
}

// agentRunMaxAttempts 同一 call_id 最多派生多少个 run_id（每个都是「受理时就失败且没产生计费」才会换下一个）。
const agentRunMaxAttempts = 10

// agentSubmitFailCodes 受理阶段（冻结 / 提交）就失败的错误码：run 从没在后端跑起来。
var agentSubmitFailCodes = map[string]bool{"BILLING_REJECTED": true, "SUBMIT_FAILED": true, "PARAM_INVALID": true, "SUBMIT_TIMEOUT": true}

// agentRunRetryable 已落库的 run 是否「受理时失败且没产生计费」：失败终态 + 受理阶段错误码 + 没冻结（none）或已退回（refunded）。
// 冻结中 / 已结算 / 待对账（review）/ Java、studio 自行计费的一律不算——重跑可能重复扣点，照旧回放原 run。
func agentRunRetryable(r *model.NodeRun) (bool, string) {
	switch {
	case r.Status != model.RunFailed:
		return false, "status=" + model.StatusText(r.Status) + " 非失败终态"
	case !agentSubmitFailCodes[r.ErrorCode.String]:
		return false, "error_code=" + r.ErrorCode.String + " 非受理阶段失败"
	case r.BillingStatus != model.BillNone && r.BillingStatus != model.BillRefunded:
		return false, "billing_status=" + r.BillingStatus + " 可能已产生计费"
	}
	return true, ""
}

// agentPickRunID 由 call_id 派生 run_id：派生出的 run 已存在、且受理时失败未计费、参数与本次相同 → 换下一个尝试序号重跑；
// 其余情况（不存在 / 仍在跑 / 已成功 / 可能已计费 / 参数不同）返回该 run_id，交给 RunNode 新建、回放或报 CONFLICT。
// 显式 run_id 或没有 call_id 时不派生（显式 run_id 的重放语义由调用方自己负责）。
func (c *core) agentPickRunID(ctx context.Context, userID int64, explicit, callID, canvasID, nodeID string, req *canvas.RunNodeRequest) (string, error) {
	if strings.TrimSpace(explicit) != "" || callID == "" {
		return agentRunID(userID, explicit, callID, 0), nil
	}
	cmp := proto.Clone(req).(*canvas.RunNodeRequest)
	c.normalizeRunSize(cmp) // 与 RunNode 落库前同口径
	want, _ := json.Marshal(toStored(cmp))
	log := logx.WithContext(ctx)
	for a := 0; a < agentRunMaxAttempts; a++ {
		id := agentRunID(userID, "", callID, a)
		old, err := c.s.Repo.Run.GetByUserRun(ctx, userID, id)
		if err != nil {
			return "", err
		}
		if old == nil {
			return id, nil
		}
		if ok, why := agentRunRetryable(old); !ok {
			if a > 0 || old.Status == model.RunFailed {
				log.Infof("canvas agent: user=%d call_id=%s run_id=%s 不重跑（%s），回放原 run", userID, callID, id, why)
			}
			return id, nil
		}
		if old.CanvasID != canvasID || old.NodeID != nodeID || !sameRequest(old.Request, want) {
			return id, nil // 参数改过：RunNode 回放时报 CONFLICT
		}
		log.Infof("canvas agent: user=%d call_id=%s run_id=%s 第 %d 次受理失败且未计费（status=failed error_code=%s billing_status=%s），换派生 run_id 重跑",
			userID, callID, id, a+1, old.ErrorCode.String, old.BillingStatus)
	}
	log.Infof("canvas agent: user=%d call_id=%s 拒绝重跑：已连续 %d 次受理失败（判据 agentRunMaxAttempts）", userID, callID, agentRunMaxAttempts)
	return "", agentErr(agenttool.ErrIdempotencyConflict, fmt.Sprintf("同一个 call_id 已连续 %d 次提交失败，请检查原因后换一个 call_id", agentRunMaxAttempts), false)
}

// agentRunNode 按节点 data + 已保存的连线组装 RunNode 请求（前端 buildRunRequest / buildReferences 同口径），交给 core.RunNode：
// 准入、冻结点数、提交、结算 / 退款全部沿用现有链路。模型节点与工具节点都走这里；工具节点的计费按原链路
// （i2p gateway 冻结、crop 不计费、upscale / outpaint / mask Java 计费、tryon studio 计费），见 agent_tool.go。
func (c *core) agentRunNode(ctx context.Context, userID int64, args map[string]any, callID string, meta *agentMeta) (any, error) {
	cid, nid := strData(args, "canvas_id"), strData(args, "node_id")
	cv, err := c.s.Repo.Canvas.GetOwned(ctx, userID, cid)
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, xerr.NotFoundf("画布")
	}
	meta.revBefore, meta.revAfter = cv.Revision, cv.Revision
	d, err := ParseSnapshot(cv.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	n := d.NodeByID()[nid]
	if n == nil {
		return nil, xerr.New(xerr.NotFound, "节点 "+nid+" 不存在")
	}
	if n.Type != "model" && n.Type != "tool" {
		return nil, xerr.Param("只能对图片生成（image_model）、视频生成（video_model）或工具（tool）节点发起执行")
	}
	// 来源规则（§6.1）：MCP 令牌不借用网页登录令牌，Java 计费的工具直接拒（不组请求、不落 run）
	if javaToolKind(n) && viaMcpToken(ctx) {
		return nil, javaToolUnsupported()
	}
	succeeded, err := c.s.Repo.Run.LatestSucceededByNode(ctx, cv.ID)
	if err != nil {
		return nil, err
	}
	done := make(map[string]*model.NodeRun, len(succeeded))
	for _, r := range succeeded {
		done[r.NodeID] = r
	}
	req, err := agentNodeRequest(d, n, done, args)
	if err != nil {
		return nil, err
	}
	if !explicitRunID(args) { // 显式 run_id：重放语义由调用方负责，不做短窗口去重
		if out, err := c.agentDedupeRun(ctx, userID, cv.ID, n.ID, req, upstreamRuns(req, done), meta); err != nil || out != nil {
			return out, err
		}
	}
	runID, err := c.agentPickRunID(ctx, userID, strData(args, "run_id"), callID, cv.ID, n.ID, req)
	if err != nil {
		return nil, err
	}
	if b, ok := agentBudgetArgs(args); ok { // 点数上限：RunNode 冻结前比较（agent_budget.go）
		ctx = withBudget(ctx, b)
	}
	res, err := c.RunNode(ctx, &canvas.RunNodeReq{UserId: userID, CanvasId: cv.ID, NodeId: n.ID, RunId: runID, Request: req})
	if err != nil {
		return nil, agentJavaLoginErr(ctx, n, err)
	}
	if id, err := strconv.ParseInt(res.RequestId, 10, 64); err == nil {
		meta.runRequestID = id
	}
	warns := res.Warnings
	if warns == nil {
		warns = []string{}
	}
	return map[string]any{
		"request_id": res.RequestId, "run_id": res.RunId, "status": res.Status, "credits": res.Credits, "max_points": res.MaxPoints,
		"settle_mode": res.SettleMode, "warnings": warns, "replayed": res.Replayed,
	}, nil
}

// buildAgentRunRequest 节点 → RunNodeRequest（args 里的模型参数临时覆盖节点值，不写回节点）。
func buildAgentRunRequest(d *Doc, n *Node, done map[string]*model.NodeRun, args map[string]any) (*canvas.RunNodeRequest, error) {
	pick := func(key, dataKey string) string {
		if v, ok := args[key].(string); ok {
			return v
		}
		return strings.TrimSpace(fmt.Sprint(firstNonNil(n.Data[dataKey], "")))
	}
	kind := "image"
	if n.Kind() == "video" {
		kind = "video"
	}
	req := &canvas.RunNodeRequest{
		Kind: kind, Prompt: strings.TrimSpace(pick("prompt", "prompt")), Ratio: pick("ratio", "ratio"), Size: pick("size", "size"),
		Quality: qualityOf(pick("quality", "quality")),
	}
	if v, ok := args["model"].(string); ok {
		req.Model = v
	} else {
		req.Model, req.ModelId = strData(n.Data, "model"), strData(n.Data, "modelId")
	}
	if req.Model == "" && req.ModelId == "" {
		return nil, xerr.Param("节点还没选模型：先用 canvas_update_nodes 设置 model，或在本次调用里给 model")
	}
	if f, ok := toFloat(n.Data["cfg"]); ok {
		req.Cfg = f
	}
	count := intData(n.Data, "count")
	if v, ok := args["count"].(float64); ok {
		count = int(v)
	}
	if kind == "image" {
		// 生成格式：args.output_format 临时覆盖，否则节点 outputFormat；都没有（旧画布）按 jpeg
		f, ok := outputFormatOf(pick("output_format", "outputFormat"))
		if !ok {
			return nil, xerr.Param("output_format 只支持 jpeg / png / webp")
		}
		req.OutputFormat = f
	}
	if kind == "video" {
		req.Duration = pick("duration", "duration")
		req.Sound = boolData(n.Data, "sound")
		if v, ok := args["sound"].(bool); ok {
			req.Sound = v
		}
		count = 1
	}
	req.Count = int32(max(count, 1))
	refs, err := agentReferences(d, n, done)
	if err != nil {
		return nil, err
	}
	req.References = refs
	return req, nil
}

func firstNonNil(v, def any) any {
	if v == nil {
		return def
	}
	return v
}

func boolData(m map[string]any, k string) bool {
	b, _ := m[k].(bool)
	return b
}

// agentReferences 节点的入边 → references（前端 buildReferences：prompt / image（含 image1、image2）/ video 分组计序）。
func agentReferences(d *Doc, n *Node, done map[string]*model.NodeRun) ([]*canvas.RunReference, error) {
	nodes := d.NodeByID()
	var in []*Edge
	hasImage := false
	for _, e := range d.Edges {
		if e.Target == n.ID {
			in = append(in, e)
			hasImage = hasImage || NormalizeHandle(e.TargetHandle) == HandleImage
		}
	}
	// 工具节点没连图片线时可用 data.sourceNodeId 指定上游图（前端 buildReferences 同口径，validate 也认它）
	if sid := strData(n.Data, "sourceNodeId"); n.Type == "tool" && !hasImage && sid != "" && nodes[sid] != nil {
		in = append(in, &Edge{Source: sid, Target: n.ID, TargetHandle: HandleImage})
	}
	rank := func(e *Edge) int {
		switch e.TargetHandle {
		case "prompt":
			return 0
		case "image1", "first":
			return 1
		case "image2", "last":
			return 2
		case "video":
			return 4
		}
		return 3
	}
	sort.SliceStable(in, func(i, j int) bool {
		a, b := rank(in[i]), rank(in[j])
		if a != b {
			return a < b
		}
		return in[i].Order < in[j].Order
	})
	var refs []*canvas.RunReference
	imageIdx := 0
	for _, e := range in {
		src := nodes[e.Source]
		if src == nil {
			continue
		}
		ref := &canvas.RunReference{Handle: e.TargetHandle, NodeId: src.ID, SourceHandle: e.SourceHandle, Index: 1}
		switch NormalizeHandle(e.TargetHandle) {
		case HandlePrompt:
			if src.Type == "text" {
				ref.Text = strings.TrimSpace(strData(src.Data, "text"))
			} else if r := done[src.ID]; r != nil {
				ref.Text = strings.TrimSpace(r.ResultText.String)
			}
			if ref.Text == "" {
				return nil, xerr.Param(fmt.Sprintf("提示词来源 %s 还没有内容", src.ID))
			}
		case HandleImage:
			imageIdx++
			ref.Index = int32(imageIdx)
			switch src.Type {
			case "image":
				ref.Image = strings.TrimSpace(strData(src.Data, "image"))
				ref.AssetId, ref.MediaId = strData(src.Data, "assetId"), strData(src.Data, "mediaId")
				if ref.Image == "" {
					if r := done[src.ID]; r != nil {
						ref.CandidateIndex = candidateIndexOf(e, r)
					} else {
						return nil, xerr.Param(fmt.Sprintf("参考图节点 %s 还没有图片", src.ID))
					}
				}
			default: // 模型 / 工具输出：按候选下标解析（服务端冻结长期引用）
				r := done[src.ID]
				if r == nil {
					return nil, xerr.Param(fmt.Sprintf("上游节点 %s 还没有生成结果，请先生成它", src.ID))
				}
				ref.CandidateIndex = candidateIndexOf(e, r)
			}
		case HandleVideo:
			if src.Type == "video" {
				// 用户上传的视频素材：节点自带地址（与 web 运行时 sourceMediaOf 同一份字段），服务端按引用现签
				ref.Video = strings.TrimSpace(strData(src.Data, "video"))
				ref.AssetId, ref.MediaId = strData(src.Data, "assetId"), strData(src.Data, "mediaId")
				if ref.Video == "" {
					return nil, xerr.Param(fmt.Sprintf("视频素材节点 %s 还没有上传视频", src.ID))
				}
			} else if done[src.ID] == nil {
				return nil, xerr.Param(fmt.Sprintf("参考视频节点 %s 还没有生成结果", src.ID))
			}
		default:
			continue
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// candidateIndexOf 连线源口 candidate-N（1 起）优先，否则取上游 run 当前选中的候选。
func candidateIndexOf(e *Edge, r *model.NodeRun) *int32 {
	if s, ok := strings.CutPrefix(e.SourceHandle, "candidate-"); ok {
		if v, err := strconv.Atoi(s); err == nil && v >= 1 {
			i := int32(v - 1)
			return &i
		}
	}
	i := r.SelectedIndex
	if i < 0 {
		i = 0
	}
	return &i
}

// agentDedupeWindow 无人值守入口（服务间 / MCP 令牌）canvas_run_node 去重时，**已成功** run 的回放窗口；在途的 run 不受此限。
var agentDedupeWindow = 120 * time.Second

// explicitRunID 调用方显式给了 run_id（跳过短窗口去重，按 run_id 幂等）。
func explicitRunID(args map[string]any) bool { return strings.TrimSpace(strData(args, "run_id")) != "" }

// upstreamRuns 本次请求每个按上游产物解析的引用（图片口带候选下标、视频口）当前会解析到的上游 run 主键：槽位 → run id。
// 槽位与受理时冻结（ref_resolve.go frozen.refs）同口径：references.{落库顺序下标}。
func upstreamRuns(req *canvas.RunNodeRequest, done map[string]*model.NodeRun) map[string]int64 {
	out := map[string]int64{}
	for i, ref := range req.References {
		if ref.CandidateIndex == nil && NormalizeHandle(ref.Handle) != HandleVideo {
			continue
		}
		if r := done[ref.NodeId]; r != nil {
			out["references."+strconv.Itoa(i)] = r.ID
		}
	}
	return out
}

// sameUpstream 候选 run 受理时冻结的上游 run 与本次当前解析到的是否逐槽一致（上游重做后不再算同一请求）。
// 候选 run 没有冻结记录（旧 run）而本次有上游引用 → 不算一致（宁可不去重）。
func sameUpstream(r *model.NodeRun, want map[string]int64) bool {
	if len(want) == 0 {
		return true
	}
	st := parseStored(r.Request)
	if st.Frozen == nil {
		return false
	}
	for slot, id := range want {
		if f, ok := st.Frozen.Refs[slot]; !ok || f.RunID != id {
			return false
		}
	}
	return true
}

// agentDedupeRun 服务间入口（auth_source=svc）与 MCP 令牌（mcp_token）调 canvas_run_node 时，按（user、canvas、node、输入指纹）去重：
// 同指纹的 run 仍在途（提交中 / 排队 / 运行中，不限时间）或 agentDedupeWindow 内已成功 → 直接返回该 run（replayed=true，不再冻结点数）。
// 兜住 Java 判「结果未知」后模型换 call_id 重复提交；登录令牌（网页 / 用户自己的 Function Call）与显式 run_id 不受影响。
// 指纹 = 与 RunNode 落库同口径的归一请求（toStored：模型 / 提示词 / 参数 / 参考图，sameRequest 比较）
// + 每个上游引用当前解析到的实际产物（上游最新成功 run 的主键，对比候选 run 受理时冻结的值）。
// 查询失败不挡生成（只记日志，退回原有幂等链路）。返回 (nil, nil) 表示未命中。
func (c *core) agentDedupeRun(ctx context.Context, userID int64, canvasID, nodeID string, req *canvas.RunNodeRequest, upstream map[string]int64, meta *agentMeta) (map[string]any, error) {
	src, _ := ctx.Value(authSourceKey{}).(string)
	if src != mcptoken.AuthSourceSvc && src != mcptoken.AuthSourceMcpToken {
		return nil, nil
	}
	cmp := proto.Clone(req).(*canvas.RunNodeRequest)
	c.normalizeRunSize(cmp)
	want, _ := json.Marshal(toStored(cmp))
	list, err := c.s.Repo.Run.DedupeCandidatesOnNode(ctx, userID, canvasID, nodeID, c.now().Add(-agentDedupeWindow), 20)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas agent: 短窗口去重查询失败 user=%d canvas=%s node=%s（check=agent_run.dedupe，放行）: %v", userID, canvasID, nodeID, err)
		return nil, nil
	}
	for _, r := range list {
		if !sameRequest(r.Request, want) || !sameUpstream(r, upstream) {
			continue
		}
		why := "在途（不限时间）"
		if r.Status == model.RunSucceeded {
			why = agentDedupeWindow.String() + " 内已成功"
		}
		logx.WithContext(ctx).Infof("canvas agent: 去重命中 user=%d canvas=%s node=%s source=%s → 回放 run=%d（check=agent_run.dedupe 判据=同指纹同上游且%s，状态=%s）",
			userID, canvasID, nodeID, src, r.ID, why, model.StatusText(r.Status))
		meta.runRequestID = r.ID
		return map[string]any{
			"request_id": strconv.FormatInt(r.ID, 10), "run_id": r.RunID, "status": model.StatusText(r.Status), "credits": r.Points,
			"max_points": r.Points, "settle_mode": "", "warnings": []string{"相同输入的生成已提交过（" + why + "），已返回该次结果，没有重复扣点"},
			"replayed": true,
		}, nil
	}
	return nil, nil
}

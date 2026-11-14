package logic

// 智能体对工具节点的执行（canvas_run_node 放开工具节点）与生成前报价（canvas_quote_node）。设计见 docs/design/canvas-agent-tools.md §6。
//
// 计费一律按节点原链路，执行器不碰计费：
//
//	tool          后端     计费归属                 来源规则
//	i2p           gateway  Go：RunNode 冻结 / 结算   user / svc / mcp_token 都可用
//	crop          media    不计费                    同上
//	tryon         studio   studio 按 user_id 冻结     同上（studio 只认 user_id，不要用户令牌）
//	upscale       java     Java（透传用户 Bearer）    user：本次登录令牌；svc：该用户缓存的登录令牌，缓存没有 → LOGIN_REQUIRED；
//	outpaint                                          mcp_token：MCP_TOKEN_JAVA_TOOL_UNSUPPORTED（不借用缓存，见 agent_auth.go）
//	mask(inpaint)

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/billing"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// agentNodeRequest 节点 → RunNodeRequest：模型节点按 buildAgentRunRequest，工具节点按 buildAgentToolRequest。
func agentNodeRequest(d *Doc, n *Node, done map[string]*model.NodeRun, args map[string]any) (*canvas.RunNodeRequest, error) {
	switch n.Type {
	case "model":
		if _, ok := args["params"]; ok {
			return nil, xerr.Param("params 只用于工具节点；模型节点请用 model / prompt / ratio / size 等参数")
		}
		return buildAgentRunRequest(d, n, done, args)
	case "tool":
		return buildAgentToolRequest(d, n, done, args)
	}
	return nil, xerr.Param("只能对图片生成（image_model）、视频生成（video_model）或工具（tool）节点发起执行")
}

// agentNodeType 节点的工具层类型（报价出参用）。
func agentNodeType(n *Node) string {
	switch {
	case n.Type == "tool":
		return "tool"
	case n.Type == "model" && n.Kind() == "video":
		return "video_model"
	case n.Type == "model":
		return "image_model"
	}
	return n.Type
}

// toolParamsFromData 工具节点 data → params（前端 useCanvasEditor.toolParamsOf 同口径；缺项不报错，交给 buildSpec 统一校验，
// 这样 args.params 能补齐节点上没填的项）。
func toolParamsFromData(tool string, data map[string]any) map[string]any {
	p := map[string]any{}
	switch tool {
	case "crop":
		crop, _ := data["crop"].(map[string]any)
		if crop == nil {
			crop = map[string]any{"shape": "rect", "x": 0.1, "y": 0.1, "w": 0.6, "h": 0.6}
		}
		p["shape"] = firstNonEmpty(strData(crop, "shape"), "rect")
		for _, k := range []string{"x", "y", "w", "h"} {
			if v, ok := crop[k]; ok {
				p[k] = v
			}
		}
		if b, ok := crop["brush"]; ok {
			p["brush"] = b
		}
	case "upscale":
		p["scale"] = firstNonNil(data["scale"], 2)
	case "outpaint":
		w, h := toInt(data["outWidth"]), toInt(data["outHeight"])
		if w > 0 || h > 0 {
			if w > 0 {
				p["width"] = w
			}
			if h > 0 {
				p["height"] = h
			}
			break
		}
		if r := strData(data, "ratio"); r != "" {
			p["ratio"] = r
		}
		p["resolution"] = firstNonEmpty(strData(data, "resolution"), "2k")
	case "mask":
		if s := strings.TrimSpace(strData(data, "maskPrompt")); s != "" {
			p["mask_prompt"] = s
		}
		if box, ok := data["maskBox"].(map[string]any); ok {
			x, _ := toFloat(box["x"])
			y, _ := toFloat(box["y"])
			w, _ := toFloat(box["w"])
			h, _ := toFloat(box["h"])
			if w > 0 && h > 0 {
				p["bbox"] = []any{
					map[string]any{"x": x, "y": y}, map[string]any{"x": x + w, "y": y},
					map[string]any{"x": x + w, "y": y + h}, map[string]any{"x": x, "y": y + h},
				}
			}
		}
	case "i2p":
		p["language"] = firstNonEmpty(strData(data, "language"), "zh")
	case "tryon":
		items := []any{"top"}
		if xs, ok := data["tryonItems"].([]any); ok && len(xs) > 0 {
			items = xs
		}
		p["items"] = items
		if s := strings.TrimSpace(strData(data, "garmentName")); s != "" {
			p["garment_name"] = s
		}
		if s := strings.TrimSpace(strData(data, "fabric")); s != "" {
			p["fabric"] = s
		}
		p["model"] = firstNonEmpty(strData(data, "tryonModel"), "通用")
		p["resolution"] = firstNonEmpty(strData(data, "tryonResolution"), "1k")
		p["aspect_ratio"] = firstNonEmpty(strData(data, "tryonAspectRatio"), "3:4")
	}
	return p
}

// snakeKey camelCase → snake_case（args.params 是自由对象，Schema 层不做键归一，这里补上）。
func snakeKey(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r + 32)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// agentToolOverrideKeys 工具参数里只能由服务端写入的键（防引用别人的媒体资源）。
var agentToolForbiddenKeys = map[string]bool{"mask_rid": true}

// buildAgentToolRequest 工具节点 → RunNodeRequest（前端 buildRunRequest 的 tool 分支同口径）：
// 节点 data 推出的参数 ← args.params 覆盖 ← 通用覆盖项（size / ratio / model / prompt 落到该工具对应的参数）。
func buildAgentToolRequest(d *Doc, n *Node, done map[string]*model.NodeRun, args map[string]any) (*canvas.RunNodeRequest, error) {
	tool := n.Tool()
	if tool == "" {
		return nil, xerr.Param("工具节点缺少工具类型（data.tool）")
	}
	if _, ok := ToolKind(tool); !ok {
		return nil, xerr.Param("不支持的工具：" + tool)
	}
	p := toolParamsFromData(tool, n.Data)
	if ov, ok := args["params"].(map[string]any); ok {
		for k, v := range ov {
			k = snakeKey(k)
			if agentToolForbiddenKeys[k] {
				return nil, xerr.Param("params." + k + " 只能由服务端写入")
			}
			if v == nil {
				delete(p, k)
				continue
			}
			p[k] = v
		}
	}
	req := &canvas.RunNodeRequest{Kind: "tool", Tool: tool}
	str := func(k string) (string, bool) {
		v, ok := args[k].(string)
		return strings.TrimSpace(v), ok && strings.TrimSpace(v) != ""
	}
	switch tool {
	case "upscale":
		if v, ok := str("size"); ok {
			delete(p, "scale")
			p["resolution"] = strings.ToLower(v)
		}
	case "outpaint":
		if v, ok := str("ratio"); ok {
			delete(p, "width")
			delete(p, "height")
			p["ratio"] = v
		}
		if v, ok := str("size"); ok {
			p["resolution"] = strings.ToLower(v)
		}
	case "mask":
		if v, ok := str("prompt"); ok {
			p["mask_prompt"] = v
		}
	case "tryon":
		if v, ok := str("model"); ok {
			p["model"] = v
		}
		if v, ok := str("size"); ok {
			p["resolution"] = strings.ToLower(v)
		}
		if v, ok := str("ratio"); ok {
			p["aspect_ratio"] = v
		}
		if v, ok := str("prompt"); ok {
			req.Prompt = v
		}
	}
	if items, ok := p["items"].([]any); ok && len(items) > 3 {
		return nil, xerr.Param("试穿单品最多选 3 件")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, xerr.Param("params 不是合法 JSON 对象")
	}
	req.ToolParams = string(b)
	refs, err := agentReferences(d, n, done)
	if err != nil {
		return nil, err
	}
	req.References = refs
	if tool != "tryon" && paramStr(p, "image_url") == "" {
		hasImage := false
		for _, r := range refs {
			if NormalizeHandle(r.Handle) == HandleImage {
				hasImage = true
				break
			}
		}
		if !hasImage {
			return nil, xerr.Param(fmt.Sprintf("工具节点 %s 还没有输入图：先用 canvas_connect 把上游图片 / 生成节点连到它的 image 口", n.ID))
		}
	}
	return req, nil
}

// javaToolKind 是否 Java 计费的工具（放大 / 扩图 / 局部重绘）。
func javaToolKind(n *Node) bool {
	if n == nil || n.Type != "tool" {
		return false
	}
	k, ok := ToolKind(n.Tool())
	return ok && model.BackendOf(k) == model.BackendJava
}

// agentJavaLoginErr 服务间入口执行 Java 工具时，Java 回 401（缓存的登录令牌已过期）→ LOGIN_REQUIRED（与没有缓存同一句话）。
func agentJavaLoginErr(ctx context.Context, n *Node, err error) error {
	if err == nil || !viaSvc(ctx) || !javaToolKind(n) {
		return err
	}
	if code, _, ok := xerr.Parse(err); ok && code == xerr.Unauthorized {
		logx.WithContext(ctx).Infof("canvas agent: 服务间入口执行 Java 工具，用户登录令牌已失效（check=agent_run.java_login）: %v", err)
		return loginRequired()
	}
	return err
}

// ---------------------------------------------------------------- 报价

// agentQuoteNode canvas_quote_node：与 canvas_run_node 同一份请求组装与归一（buildSpec），按节点原计费链路报价，不冻结、不提交。
//
//   - canvas（gateway：image / video / i2p）：billing.Service.Quote，与 RunNode 冻结同一个 Charge（c.charge）→ 报价 = 冻结；
//     计费开关 off 时冻结为 0，报 0；shadow 报价目但 charged=false。
//   - studio（tryon）：studio-rpc Quote（与提交同一套归一与 charge），张数 = 模特数。
//   - java（upscale / outpaint / mask）：价目在 Java，Go 不复刻规则：给出 java_request{route, body}（执行时发给 Java 的同一份请求，
//     Java 画布助手据此在进程内报价）；user / svc 且拿得到该用户 Bearer 时，另用同一请求体调 Java 只读报价接口
//     POST {path}/quote 得到准确点数（price_known=true）。mcp_token 不借用 Bearer、旧版 Java 没有报价接口（404）→ price_known=false。
//   - free（crop）：0 点。
func (c *core) agentQuoteNode(ctx context.Context, userID int64, args map[string]any) (any, error) {
	cid, nid := strData(args, "canvas_id"), strData(args, "node_id")
	cv, err := c.s.Repo.Canvas.GetOwned(ctx, userID, cid)
	if err != nil {
		return nil, err
	}
	if cv == nil {
		return nil, xerr.NotFoundf("画布")
	}
	d, err := ParseSnapshot(cv.Snapshot, 0)
	if err != nil {
		return nil, err
	}
	n := d.NodeByID()[nid]
	if n == nil {
		return nil, xerr.New(xerr.NotFound, "节点 "+nid+" 不存在")
	}
	q := &quoteOut{CanvasID: cv.ID, NodeID: n.ID, NodeType: agentNodeType(n), Warnings: []string{}}
	switch n.Type {
	case "model":
		q.Kind = string(model.KindImage)
		if n.Kind() == "video" {
			q.Kind = string(model.KindVideo)
		}
	case "tool":
		q.Tool = n.Tool()
		if k, ok := ToolKind(q.Tool); ok {
			q.Kind = string(k)
		}
	default:
		return nil, xerr.Param("只能对图片生成（image_model）、视频生成（video_model）或工具（tool）节点报价")
	}
	q.setBackend(model.BackendOf(model.RunKind(q.Kind)))
	// 来源规则先于参数：MCP 令牌碰 Java 工具，参数再齐也执行不了
	if q.Billing == "java" {
		q.RequiresLogin = true
		switch {
		case viaMcpToken(ctx):
			q.unavailable(agenttool.ErrMcpTokenJavaToolUnsupported, "这个工具的价格需要在网页里登录后查看")
		default:
			q.LoginAvailable = c.rememberBearer(ctx, userID) != ""
		}
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
		return q.fromErr(ctx, err)
	}
	q.Warnings = append(q.Warnings, c.normalizeRunSize(req)...)
	c.refreshGroups(ctx)
	sp, err := c.buildSpec(req, c.refResolver(ctx, cv.ID))
	if err != nil {
		return q.fromErr(ctx, err)
	}
	q.Warnings = mergeWarnings(q.Warnings, sp.Warnings)
	if q.Warnings == nil {
		q.Warnings = []string{}
	}
	if err := c.checkRefs(d, &canvas.RunNodeReq{UserId: userID, CanvasId: cv.ID, NodeId: n.ID, Request: req}, sp); err != nil {
		return q.fromErr(ctx, err)
	}
	q.fillBasis(sp)
	q.EstimatedSeconds, q.EstimateSource = c.estimateSeconds(ctx, sp)
	if q.Available == nil {
		if err := c.quoteReady(ctx, sp); err != nil {
			return q.fromErr(ctx, err)
		}
	}
	switch q.Billing {
	case "canvas":
		c.quoteGateway(ctx, userID, sp, q)
	case "studio":
		c.quoteStudio(ctx, userID, sp, q)
	case "java":
		c.quoteJava(ctx, userID, sp, q)
	case "free":
		q.PriceKnown, q.Charged = true, false
		q.Note = "裁剪不计费"
	}
	if q.Available == nil {
		q.Available = boolPtr(true)
	}
	return q.view(), nil
}

// quoteReady 报价时的后端就绪检查（ready 去掉令牌项：令牌是否可用由 login_available 单独给出）。
func (c *core) quoteReady(ctx context.Context, sp *runSpec) error {
	if sp.Backend == model.BackendJava {
		if !c.s.Java.Enabled() {
			return xerr.New(xerr.NotReady, "该工具暂未开放，请稍后再试")
		}
		return nil
	}
	return c.ready(ctx, sp, "")
}

func (c *core) quoteGateway(ctx context.Context, userID int64, sp *runSpec, q *quoteOut) {
	if c.s.Billing == nil {
		q.PriceKnown, q.BillingMode = true, billing.ModeOff
		q.Note = "当前不计费"
		return
	}
	if err := c.s.Billing.CheckAccess(ctx, userID, sp.EntryCode); err != nil {
		e := billing.UserError(ctx, userID, sp.EntryCode, err)
		_, msg, _ := xerr.Parse(e)
		code := "BILLING_REJECTED"
		if errors.Is(err, billing.ErrNoFeature) {
			code = "BILLING_NO_FEATURE"
		}
		q.unavailable(code, firstNonEmpty(msg, billing.MsgUnavailable))
		return
	}
	mode := c.s.Billing.Mode()
	q.BillingMode, q.Charged = mode, mode == billing.ModeOn
	if mode == billing.ModeOff {
		q.PriceKnown = true // 不计费：RunNode 不冻结（credits=0），如实报 0
		q.Note = "当前不计费"
		return
	}
	r, err := c.s.Billing.Quote(ctx, c.charge(userID, "quote", sp))
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas agent: 报价失败 user=%d entry=%s group=%s（check=agent_quote.billing）: %v", userID, sp.EntryCode, sp.ModelGroup, err)
		if errors.Is(err, billing.ErrNoOffer) {
			q.unavailable("BILLING_NO_OFFER", billing.MsgNoOffer)
			return
		}
		q.Note = "暂时无法报价，执行时以实际冻结为准"
		return
	}
	q.PriceKnown, q.Points = true, r.PricePoints
	q.SettleMode = r.SettleMode
	if q.SettleMode == "" {
		q.SettleMode = billing.SettleFixed
	}
	q.MaxPoints = r.PricePoints
	q.Basis.PriceUnit, q.Basis.Quantity = r.PriceUnit, r.Quantity
	if r.Quantity > 0 && r.PricePoints%int64(r.Quantity) == 0 {
		q.Basis.UnitPoints = r.PricePoints / int64(r.Quantity)
	}
	if q.SettleMode == billing.SettleActualCost {
		q.Note = fmt.Sprintf("按实际用量结算，最多 %d 点", r.PricePoints)
	}
}

func (c *core) quoteStudio(ctx context.Context, userID int64, sp *runSpec, q *quoteOut) {
	_, models, err := tryonInputs(sp)
	if err != nil {
		q.unavailable(agenttool.ErrParam, quoteErrText(err))
		return
	}
	resp, err := c.s.Studio.Quote(ctx, &studio.QuoteReq{
		UserId: userID, Kind: "tryon", Model: firstNonEmpty(paramStr(sp.Params, "model"), "通用"),
		Resolution:  strings.ToLower(firstNonEmpty(paramStr(sp.Params, "resolution"), "1k")),
		AspectRatio: firstNonEmpty(paramStr(sp.Params, "aspect_ratio"), "3:4"), TaskTotal: int32(len(models)),
	})
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas agent: studio 报价失败 user=%d（check=agent_quote.studio）: %v", userID, err)
		q.Note = "暂时无法报价，执行时以实际冻结为准"
		return
	}
	if !resp.Available {
		q.unavailable("STUDIO_QUOTE_UNAVAILABLE", firstNonEmpty(resp.Reason, "暂时无法报价"))
		return
	}
	q.PriceKnown, q.Points, q.MaxPoints = true, resp.Points, resp.MaxPoints
	q.SettleMode, q.BillingMode, q.Charged = resp.SettleMode, resp.BillingMode, resp.Charged
	q.Basis.UnitPoints, q.Basis.PriceUnit, q.Basis.Quantity = resp.UnitPoints, resp.PriceUnit, int(resp.TaskTotal)
	q.Basis.Count = int(resp.TaskTotal)
	if resp.Resolution != "" {
		q.Basis.Size = resp.Resolution
	}
	q.Warnings = mergeWarnings(q.Warnings, resp.Warnings)
}

func (c *core) quoteJava(ctx context.Context, userID int64, sp *runSpec, q *quoteOut) {
	run := &model.NodeRun{Kind: sp.Kind}
	path, body, err := c.javaToolBody(ctx, run, sp, true)
	if err != nil {
		q.unavailable(agenttool.ErrParam, quoteErrText(err))
		return
	}
	q.JavaRequest = map[string]any{"route": strings.TrimPrefix(path, "/api/v1"), "path": path, "body": body}
	q.Note = "由主站按其价目扣点，画布报不出准确点数；执行时以主站实际扣点为准"
	if q.Available == nil && !q.LoginAvailable {
		q.unavailable(agenttool.ErrLoginRequired, msgLoginRequired)
		return
	}
	if q.Available != nil || viaMcpToken(ctx) {
		return
	}
	// 带用户登录令牌（user）或该用户本人缓存的令牌（svc）：问 Java 只读报价接口（与提交同一份请求体、同一受理）。
	jq, err := c.javaQuote(ctx, userID, path, body)
	switch {
	case err == nil:
		q.PriceKnown, q.Charged, q.Points = true, true, jq.Points
		q.Basis.PriceUnit, q.Basis.Quantity = jq.PriceUnit, jq.Quantity
		q.Note = "点数由主站按其价目报出（与提交时冻结的点数一致）；执行时以主站实际扣点为准"
		if jq.FreeQuotaApplied {
			q.Warnings = mergeWarnings(q.Warnings, []string{"本次会用上免费额度，实际扣点可能少于报价"})
		}
	case errors.Is(err, javatool.ErrQuoteUnsupported):
		// 旧版 Java 没有报价接口：保持 price_known=false（部署解耦，不要求同批上线）
	default:
		var je *javatool.Error
		if errors.As(err, &je) && je.Unauthorized() {
			q.unavailable(agenttool.ErrLoginRequired, msgLoginRequired)
			return
		}
		logx.WithContext(ctx).Infof("canvas agent: Java 工具报价失败，按价格未知处理 user=%d path=%s（check=java_quote err=%v）", userID, path, err)
	}
}

// javaQuoteTimeout Java 只读报价的超时（报价不该拖住智能体）。
const javaQuoteTimeout = 8 * time.Second

// javaQuote 用该用户的登录令牌（user 本次令牌 / svc 本人缓存；mcp_token 不借用）向 Java 报价。
func (c *core) javaQuote(ctx context.Context, userID int64, path string, body any) (*javatool.QuoteResult, error) {
	bearer := c.rememberBearer(ctx, userID)
	if bearer == "" {
		return nil, javatool.ErrNoBearer
	}
	qctx, cancel := context.WithTimeout(ctx, javaQuoteTimeout)
	defer cancel()
	return c.s.Java.Quote(qctx, bearer, path, body)
}

// quoteErrText 错误 → 给用户的短句。
func quoteErrText(err error) string {
	var ae *agentError
	if errors.As(err, &ae) {
		return ae.Message
	}
	if _, msg, ok := xerr.Parse(err); ok {
		return msg
	}
	return "参数不对"
}

// quoteOut canvas_quote_node 出参。
type quoteOut struct {
	CanvasID, NodeID, NodeType, Tool, Kind string
	Available                              *bool
	Reason, ReasonCode                     string
	Billing                                string
	PriceKnown, Charged                    bool
	Points, MaxPoints                      int64
	SettleMode, BillingMode                string
	Basis                                  quoteBasis
	RequiresLogin, LoginAvailable          bool
	EstimatedSeconds                       int
	EstimateSource                         string
	Warnings                               []string
	Note                                   string
	JavaRequest                            map[string]any
}

type quoteBasis struct {
	Model, ModelGroup, Size, Quality, Ratio, PriceUnit string
	Count, DurationS, Quantity                         int
	UnitPoints                                         int64
}

func boolPtr(b bool) *bool { return &b }

func (q *quoteOut) setBackend(b model.Backend) {
	switch b {
	case model.BackendGateway:
		q.Billing = "canvas"
	case model.BackendJava:
		q.Billing = "java"
	case model.BackendStudio:
		q.Billing = "studio"
	default:
		q.Billing = "free"
	}
}

// unavailable 记第一个不可执行的原因（后面的不覆盖）。
func (q *quoteOut) unavailable(code, reason string) {
	if q.Available != nil && !*q.Available {
		return
	}
	q.Available, q.ReasonCode, q.Reason = boolPtr(false), code, reason
}

// fromErr 请求组装 / 归一 / 就绪的错误：用户可修正的（参数、就绪、来源）渲染成 available=false；其余照常作工具错误。
func (q *quoteOut) fromErr(ctx context.Context, err error) (any, error) {
	ae := toAgentError(ctx, agenttool.ToolQuoteNode, err)
	switch ae.ErrorCode {
	case agenttool.ErrParam, agenttool.ErrNotReady, agenttool.ErrMcpTokenJavaToolUnsupported, agenttool.ErrLoginRequired, agenttool.ErrForbidden:
		q.unavailable(ae.ErrorCode, ae.Message)
		return q.view(), nil
	}
	return nil, ae
}

func (q *quoteOut) fillBasis(sp *runSpec) {
	b := &q.Basis
	b.ModelGroup, b.Size, b.Ratio, b.Count, b.DurationS = sp.ModelGroup, sp.Size, sp.Ratio, max(sp.Count, 1), sp.DurationS
	if sp.Kind == model.KindImage || sp.Kind == model.KindVideo {
		b.Model = modelDisplay(sp.Model)
		if sp.Kind == model.KindImage {
			b.Quality = imageQuality(sp)
		}
	}
	switch sp.Kind {
	case model.KindToolUpscale:
		if r, err := upscaleResolution(sp.Params, sp.Size); err == nil {
			b.Size = r
		}
	case model.KindToolOutpaint:
		b.Size = strings.ToLower(firstNonEmpty(paramStr(sp.Params, "resolution"), "2k"))
	case model.KindToolTryon:
		b.Model = firstNonEmpty(paramStr(sp.Params, "model"), "通用")
		b.Size = strings.ToLower(firstNonEmpty(paramStr(sp.Params, "resolution"), "1k"))
		b.Ratio = firstNonEmpty(paramStr(sp.Params, "aspect_ratio"), "3:4")
	}
}

func (q *quoteOut) view() map[string]any {
	avail := q.Available == nil || *q.Available
	out := map[string]any{
		"canvas_id": q.CanvasID, "node_id": q.NodeID, "node_type": q.NodeType, "kind": q.Kind,
		"available": avail, "billing": q.Billing, "price_known": q.PriceKnown && avail,
		"points": q.Points, "max_points": q.MaxPoints, "settle_mode": q.SettleMode, "charged": q.Charged,
		"basis": map[string]any{
			"model": q.Basis.Model, "model_group": q.Basis.ModelGroup, "size": q.Basis.Size, "quality": q.Basis.Quality,
			"ratio": q.Basis.Ratio, "count": q.Basis.Count, "duration_s": q.Basis.DurationS, "unit_points": q.Basis.UnitPoints,
			"price_unit": q.Basis.PriceUnit, "quantity": q.Basis.Quantity,
		},
		"requires_login_token": q.RequiresLogin, "login_available": q.LoginAvailable,
		"estimated_seconds": q.EstimatedSeconds, "estimate_source": q.EstimateSource, "warnings": q.Warnings,
	}
	if !avail || !q.PriceKnown {
		out["points"], out["max_points"] = int64(0), int64(0)
	}
	if q.Tool != "" {
		out["tool"] = q.Tool
	}
	if q.BillingMode != "" {
		out["billing_mode"] = q.BillingMode
	}
	if !avail {
		out["reason"], out["reason_code"] = q.Reason, q.ReasonCode
	}
	if q.Note != "" {
		out["note"] = q.Note
	}
	if q.JavaRequest != nil {
		out["java_request"] = q.JavaRequest
	}
	return out
}

// ---------------------------------------------------------------- 预计耗时

// defaultRunSeconds 没有近期样本时的经验耗时（秒，提交到出结果）。
var defaultRunSeconds = map[model.RunKind]int{
	model.KindImage: 60, model.KindVideo: 240, model.KindToolI2P: 15, model.KindToolCrop: 5,
	model.KindToolUpscale: 60, model.KindToolOutpaint: 90, model.KindToolInpaint: 90, model.KindToolTryon: 120,
}

const (
	etaSampleWindow = 7 * 24 * time.Hour
	etaSampleSize   = 50
	etaMinSamples   = 5
	etaCacheTTL     = 10 * time.Minute
)

type etaEntry struct {
	at  time.Time
	sec int
}

// etaCache 按（kind, model_group）缓存近期中位耗时，避免每次报价查库。
var etaCache = struct {
	sync.Mutex
	m map[string]etaEntry
}{m: map[string]etaEntry{}}

// estimateSeconds 预计耗时：近 7 天同类（kind + 模型组）成功 run 的创建 → 完成耗时中位数（≥5 个样本），否则经验值。
// 查询失败不影响报价（退回经验值，只记日志）。
func (c *core) estimateSeconds(ctx context.Context, sp *runSpec) (int, string) {
	key := string(sp.Kind) + "|" + sp.ModelGroup
	etaCache.Lock()
	if e, ok := etaCache.m[key]; ok && time.Since(e.at) < etaCacheTTL {
		etaCache.Unlock()
		if e.sec > 0 {
			return e.sec, "recent_median"
		}
		return defaultRunSeconds[sp.Kind], "default"
	}
	etaCache.Unlock()
	sec := 0
	if c.s.Repo != nil && c.s.Repo.Run != nil {
		list, err := c.s.Repo.Run.RecentDurations(ctx, sp.Kind, sp.ModelGroup, c.now().Add(-etaSampleWindow), etaSampleSize)
		if err != nil {
			logx.WithContext(ctx).Infof("canvas agent: 查近期耗时失败（check=agent_quote.eta，用经验值）: %v", err)
		} else if len(list) >= etaMinSamples {
			sort.Ints(list)
			sec = list[len(list)/2]
		}
	}
	etaCache.Lock()
	etaCache.m[key] = etaEntry{at: time.Now(), sec: sec}
	etaCache.Unlock()
	if sec > 0 {
		return sec, "recent_median"
	}
	return defaultRunSeconds[sp.Kind], "default"
}

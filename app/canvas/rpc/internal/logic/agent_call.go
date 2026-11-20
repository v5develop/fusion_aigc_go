package logic

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/mcptoken"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/repo"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 画布智能体工具执行器（rpc AgentCall）：校验 → 幂等占位 → 执行 → 审计收尾。设计见 docs/design/canvas-agent-tools.md。
//
//   - 身份只来自 in.UserId（网关鉴权写入）；工具 Schema 里没有 user_id，所有读写都按 (user_id, canvas_id) 走 GetOwned / ownedRun。
//   - 写类工具在存储态快照上应用语义补丁（agent_ops.go），ValidateDoc 后按读到的 revision CAS 保存；不带 base_revision 时 CAS 失败重读重放。
//   - 生成只经 core.RunNode（准入 / 冻结 / 结算 / 退款全部沿用），不碰 pkg/billing。
//   - 业务错误放进结果（is_error），不作 gRPC 错误；只有未登录才返回 gRPC 错误。

const (
	agentMaxResultBytes = 64 << 10        // 审计表 args / result 超过不落
	agentStaleClaim     = 2 * time.Minute // 幂等占位超过这么久仍未收尾 → 视为崩溃残留，可接管
	agentSaveAttempts   = 3               // 不带 base_revision 时 CAS 冲突的重放次数
)

// agentPollInterval canvas_get_run wait_seconds 的查询间隔（变量便于单测调小）。
var agentPollInterval = 1500 * time.Millisecond

// agentError 工具业务错误（带 error_code）。
type agentError struct{ agenttool.ErrorData }

func (e *agentError) Error() string { return e.ErrorCode + ": " + e.Message }

func agentErr(code, msg string, retryable bool) *agentError {
	return &agentError{agenttool.ErrorData{ErrorCode: code, Message: msg, Retryable: retryable}}
}

func conflictErr(rev string) *agentError {
	e := agentErr(agenttool.ErrConflict, "画布已被修改（可能是你在别的窗口或前端保存过），请先 canvas_get 读取最新内容再改；或不带 base_revision 让服务端在最新版本上应用", true)
	e.Revision = rev
	return e
}

// toAgentError 任意错误 → 工具错误（xerr 码映射；非业务错误给通用文案，细节只进日志）。
func toAgentError(ctx context.Context, tool string, err error) *agentError {
	var ae *agentError
	if errors.As(err, &ae) {
		return ae
	}
	var ve *agenttool.ValidationError
	if errors.As(err, &ve) {
		return agentErr(agenttool.ErrParam, "参数不对："+ve.Error(), false)
	}
	code, msg, ok := xerr.Parse(err)
	if !ok {
		logx.WithContext(ctx).Errorf("canvas agent: 工具 %s 执行失败: %v", tool, err)
		return agentErr(agenttool.ErrServer, "服务繁忙，请稍后重试", true)
	}
	switch code {
	case xerr.ParamError, xerr.ParamMissing:
		return agentErr(agenttool.ErrParam, msg, false)
	case xerr.NotFound:
		return agentErr(agenttool.ErrNotFound, msg, false)
	case xerr.Conflict:
		return agentErr("CONFLICT", msg, true)
	case xerr.InsufficientPoints:
		return agentErr(agenttool.ErrInsufficientPoints, msg, false)
	case xerr.NotReady:
		return agentErr(agenttool.ErrNotReady, msg, true)
	case xerr.Forbidden, xerr.AgreementReq, xerr.AccountDisabled:
		return agentErr(agenttool.ErrForbidden, msg, false)
	case xerr.Unauthorized:
		return agentErr(agenttool.ErrUnauthorized, msg, false)
	}
	if code >= 5000 {
		logx.WithContext(ctx).Errorf("canvas agent: 工具 %s 执行失败: %v", tool, err)
		return agentErr(agenttool.ErrServer, "服务繁忙，请稍后重试", true)
	}
	return agentErr(fmt.Sprintf("E%d", code), msg, false)
}

// agentMeta 执行过程中收集的审计字段。
type agentMeta struct {
	canvasID, revBefore, revAfter string
	runRequestID                  int64
}

// AgentCall 执行一个画布智能体工具。
func (c *core) AgentCall(ctx context.Context, in *canvas.AgentCallReq) (*canvas.AgentCallResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	start := c.now()
	// 本次调用产生的画布写入在事件 / 快照历史里记为 source=agent（服务间入口 actor=svc:<keyId>）
	ctx = withActor(ctx, canvasevent.SourceAgent, agentActor(in.Client, in.Agent))
	ctx = withAuthSource(ctx, in.AuthSource)
	t, ok := agenttool.Find(strings.TrimSpace(in.Tool))
	if !ok {
		return agentResp(nil, agentErr(agenttool.ErrUnknownTool, "没有这个工具："+in.Tool, false), false), nil
	}
	// MCP 访问令牌的作用域二次校验（网关 Caller 已拦一次；这里防网关漏传 / 其他调用方直连）
	if in.AuthSource == mcptoken.AuthSourceMcpToken && !mcptoken.AllowsTool(in.TokenScope, t.Generates) {
		logx.WithContext(ctx).Infof("canvas agent: 拒绝 user=%d tool=%s（check=agent_call.token_scope scope=%q generates=%v）", in.UserId, t.Name, in.TokenScope, t.Generates)
		return agentResp(nil, agentErr(agenttool.ErrForbiddenScope, "该 MCP 令牌的权限范围不含这个工具；需要生成请在画布页新建带「生成」权限的令牌", false), false), nil
	}
	args, err := agenttool.ParseArguments(t, json.RawMessage(in.Arguments))
	if err != nil {
		return agentResp(nil, toAgentError(ctx, t.Name, err), false), nil
	}
	callID := strings.TrimSpace(in.CallId)
	if utf8.RuneCountInString(callID) > 64 {
		return agentResp(nil, agentErr(agenttool.ErrParam, "call_id 最长 64 字符", false), false), nil
	}
	argsJSON, _ := json.Marshal(args) // map 序列化键有序：同参数同字节
	sum := sha256.Sum256(append([]byte(t.Name+"\n"), argsJSON...))
	hash := hex.EncodeToString(sum[:])
	meta := &agentMeta{canvasID: strData(args, "canvas_id")}

	if !t.Mutates() { // 只读：不落审计，只打日志
		out, err := c.agentExec(ctx, in.UserId, t, args, callID, meta)
		resp := agentResp(out, asAgentErr(ctx, t.Name, err), false)
		logx.WithContext(ctx).Infof("canvas agent: user=%d tool=%s canvas=%s client=%s ok=%v cost=%s", in.UserId, t.Name, meta.canvasID, in.Client, !resp.IsError, c.now().Sub(start))
		return resp, nil
	}

	row := &model.AgentCall{
		UserID: in.UserId, Tool: t.Name, Client: clip(in.Client, 16), Agent: clip(in.Agent, 64), ArgsHash: hash,
		ClientIP: clip(in.ClientIp, 64), Status: model.AgentCallRunning,
	}
	if meta.canvasID != "" {
		row.CanvasID = sql.NullString{String: meta.canvasID, Valid: true}
	}
	if len(argsJSON) <= agentMaxResultBytes {
		row.Args = argsJSON
	}
	if callID != "" {
		row.CallID = sql.NullString{String: callID, Valid: true}
		row.IdemKey = row.CallID
		replay, err := c.agentClaim(ctx, row)
		if err != nil {
			return agentResp(nil, asAgentErr(ctx, t.Name, err), false), nil
		}
		if replay != nil {
			return replay, nil
		}
		// 执行前复核占位仍属于自己：占位到这里之间若被判过期接管（STALE 置失败、key 已释放给别的请求），就不能再执行
		if owned, err := c.s.Repo.AgentCall.StillOwned(ctx, row.ID, row.LeaseToken); err != nil {
			return agentResp(nil, asAgentErr(ctx, t.Name, err), false), nil
		} else if !owned {
			logx.WithContext(ctx).Errorf("WARN canvas agent: 放弃执行 user=%d tool=%s call_id=%s id=%d lease=%s（check=agent_call.still_owned：占位已被过期接管或不存在）",
				row.UserID, row.Tool, callID, row.ID, row.LeaseToken)
			return agentResp(nil, agentErr(agenttool.ErrInProgress, "同一个 call_id 的调用已被接管，请稍后再查", true), false), nil
		}
	}

	out, execErr := c.agentExec(ctx, in.UserId, t, args, callID, meta)
	ae := asAgentErr(ctx, t.Name, execErr)
	resp := agentResp(out, ae, false)
	fin := repo.AgentFinish{
		Status: model.AgentCallOK, CanvasID: meta.canvasID, RevisionBefore: meta.revBefore, RevisionAfter: meta.revAfter,
		RunRequestID: meta.runRequestID, DurationMS: int32(c.now().Sub(start) / time.Millisecond),
	}
	if ae != nil {
		fin.Status, fin.ErrorCode, fin.ErrorMsg, fin.ReleaseIdem = model.AgentCallFailed, clip(ae.ErrorCode, 32), clip(ae.Message, 255), true
	} else if len(resp.Result) <= agentMaxResultBytes {
		fin.Result = json.RawMessage(resp.Result)
	}
	c.agentAudit(ctx, row, fin)
	return resp, nil
}

func asAgentErr(ctx context.Context, tool string, err error) *agentError {
	if err == nil {
		return nil
	}
	return toAgentError(ctx, tool, err)
}

func agentResp(out any, ae *agentError, replayed bool) *canvas.AgentCallResp {
	if ae != nil {
		b, _ := json.Marshal(ae.ErrorData)
		return &canvas.AgentCallResp{Result: string(b), IsError: true}
	}
	if out == nil {
		out = map[string]any{}
	}
	b, err := json.Marshal(out)
	if err != nil {
		b, _ = json.Marshal(agenttool.ErrorData{ErrorCode: agenttool.ErrServer, Message: "结果序列化失败", Retryable: true})
		return &canvas.AgentCallResp{Result: string(b), IsError: true}
	}
	return &canvas.AgentCallResp{Result: string(b), Replayed: replayed}
}

// agentClaim 幂等占位。返回非 nil 的 resp 表示直接回放（或给出进行中 / 冲突结果）。
func (c *core) agentClaim(ctx context.Context, row *model.AgentCall) (*canvas.AgentCallResp, error) {
	for attempt := 0; attempt < 2; attempt++ {
		row.ID, row.LeaseToken = 0, newLeaseToken()
		err := c.s.Repo.AgentCall.Insert(ctx, row)
		if err == nil {
			return nil, nil
		}
		if !repo.IsDuplicate(err) {
			return nil, err
		}
		old, err := c.s.Repo.AgentCall.GetByIdem(ctx, row.UserID, row.IdemKey.String)
		if err != nil {
			return nil, err
		}
		if old == nil {
			continue // 对方刚失败释放了 key：再占一次
		}
		if old.Tool != row.Tool || old.ArgsHash != row.ArgsHash {
			return agentResp(nil, agentErr(agenttool.ErrIdempotencyConflict, "同一个 call_id 已用于不同的工具或参数，请换一个 call_id", false), false), nil
		}
		switch old.Status {
		case model.AgentCallOK:
			if len(old.Result) == 0 {
				return agentResp(map[string]any{"canvas_id": old.CanvasID.String, "revision": old.RevisionAfter.String,
					"message": "该调用已执行过（结果过大未留存），请用 canvas_get 读取最新状态"}, nil, true), nil
			}
			return &canvas.AgentCallResp{Result: string(old.Result), Replayed: true}, nil
		case model.AgentCallRunning:
			if ok, err := c.s.Repo.AgentCall.ReleaseStale(ctx, old.ID, c.now().Add(-agentStaleClaim)); err != nil {
				return nil, err
			} else if ok {
				continue
			}
			return agentResp(nil, agentErr(agenttool.ErrInProgress, "同一个 call_id 的调用还在执行，请稍后再查", true), false), nil
		}
	}
	return agentResp(nil, agentErr(agenttool.ErrInProgress, "同一个 call_id 的调用还在执行，请稍后再查", true), false), nil
}

// agentAuditTimeout 审计收尾写库的时限（与调用方请求的取消解耦）。
const agentAuditTimeout = 5 * time.Second

// agentAudit 审计收尾：有占位行就更新它，没有就落一行终态。写失败只记日志，不影响已返回的结果。
// 用 context.WithoutCancel + 独立时限：调用方断开（MCP 客户端超时、网关取消）时工具多半已执行完，
// 收尾不能跟着被取消——否则占位行停在「执行中」，同 call_id 要等 agentStaleClaim 过期才能重试，审计也丢了终态。
func (c *core) agentAudit(parent context.Context, row *model.AgentCall, f repo.AgentFinish) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), agentAuditTimeout)
	defer cancel()
	var err error
	if row.ID > 0 {
		var ok bool
		if ok, err = c.s.Repo.AgentCall.Finish(ctx, row.ID, row.LeaseToken, f); err == nil && !ok {
			// 占位在执行期间被判过期接管（STALE 置失败、key 已释放），或行不存在：本次结果不进审计 / 幂等回放
			logx.WithContext(ctx).Errorf("WARN canvas agent: 审计收尾影响 0 行 user=%d tool=%s call_id=%s id=%d lease=%s status=%d（check=agent_call.finish：占位已被过期接管或不存在）",
				row.UserID, row.Tool, row.CallID.String, row.ID, row.LeaseToken, f.Status)
		}
	} else {
		row.Status, row.DurationMS = f.Status, f.DurationMS
		row.CanvasID = nullStr(firstNonEmpty(f.CanvasID, row.CanvasID.String))
		row.ErrorCode, row.ErrorMsg = nullStr(f.ErrorCode), nullStr(f.ErrorMsg)
		row.RevisionBefore, row.RevisionAfter = nullStr(f.RevisionBefore), nullStr(f.RevisionAfter)
		if f.RunRequestID > 0 {
			row.RunRequestID = sql.NullInt64{Int64: f.RunRequestID, Valid: true}
		}
		row.Result = f.Result
		err = c.s.Repo.AgentCall.Insert(ctx, row)
	}
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas agent: 审计落库失败 user=%d tool=%s canvas=%s status=%d: %v", row.UserID, row.Tool, f.CanvasID, f.Status, err)
	}
}

// newLeaseToken 占位令牌（32 位十六进制）。
func newLeaseToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func nullStr(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// decodeArgs 归一后的入参 map → 结构体。
func decodeArgs(m any, v any) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return agentErr(agenttool.ErrParam, "参数不对："+err.Error(), false)
	}
	return nil
}

// ---------------------------------------------------------------- 分派

func (c *core) agentExec(ctx context.Context, userID int64, t agenttool.Tool, args map[string]any, callID string, meta *agentMeta) (any, error) {
	ctx = withoutThumbs(ctx) // 工具出参（runView / 画布读取）不带缩略图，读出口不额外签
	cid, baseRev := strData(args, "canvas_id"), strData(args, "base_revision")
	switch t.Name {
	case agenttool.ToolList:
		return c.agentList(ctx, userID, args)
	case agenttool.ToolCreate:
		doc, err := c.CreateCanvas(ctx, &canvas.CreateCanvasReq{UserId: userID, Title: strData(args, "title")})
		if err != nil {
			return nil, err
		}
		meta.canvasID, meta.revAfter = doc.Id, doc.Revision
		return map[string]any{"canvas_id": doc.Id, "title": doc.Title, "revision": doc.Revision, "node_count": doc.NodeCount}, nil
	case agenttool.ToolGet:
		return c.agentGet(ctx, userID, args)
	case agenttool.ToolCatalog:
		return c.agentCatalog(ctx, userID, strData(args, "kind"))
	case agenttool.ToolRunNode:
		return c.agentRunNode(ctx, userID, args, callID, meta)
	case agenttool.ToolQuoteNode:
		return c.agentQuoteNode(ctx, userID, args)
	case agenttool.ToolGetRun:
		return c.agentGetRun(ctx, userID, args)
	case agenttool.ToolListRuns:
		lim := intData(args, "limit")
		if lim <= 0 {
			lim = 20
		}
		res, err := c.ListRuns(ctx, &canvas.ListRunsReq{UserId: userID, CanvasId: cid, NodeId: strData(args, "node_id"), Limit: int32(lim)})
		if err != nil {
			return nil, err
		}
		runs := make([]map[string]any, 0, len(res.List))
		for _, r := range res.List {
			runs = append(runs, runView(r))
		}
		return map[string]any{"runs": runs}, nil
	case agenttool.ToolCancelRun:
		id, err := parseRequestID(strData(args, "request_id"))
		if err != nil {
			return nil, err
		}
		meta.runRequestID = id
		r, err := c.CancelRun(ctx, &canvas.RunIdReq{UserId: userID, Id: id})
		if err != nil {
			return nil, err
		}
		meta.canvasID = r.CanvasId
		return runView(r), nil
	case agenttool.ToolSelectCandidate:
		r, err := c.SelectCandidate(ctx, &canvas.SelectCandidateReq{UserId: userID, CanvasId: cid, NodeId: strData(args, "node_id"),
			RunId: strData(args, "run_id"), Index: int32(intData(args, "index"))})
		if err != nil {
			return nil, err
		}
		if id, err := parseRequestID(r.RequestId); err == nil {
			meta.runRequestID = id
		}
		return runView(r), nil
	}
	// 其余都是画布编辑：一次读 → 应用 → 校验 → CAS 保存
	return c.agentMutate(ctx, userID, cid, baseRev, meta, func(e *docEditor) (map[string]any, error) {
		return applyEditTool(e, t.Name, args)
	})
}

// applyEditTool 编辑类工具在编辑器上执行（canvas_batch_edit 逐步复用单工具的实现）。
func applyEditTool(e *docEditor, tool string, args map[string]any) (map[string]any, error) {
	switch tool {
	case agenttool.ToolAddNodes:
		var specs []agentNodeSpec
		if err := decodeArgs(args["nodes"], &specs); err != nil {
			return nil, err
		}
		outs := make([]*agentNodeOut, 0, len(specs))
		for i, s := range specs {
			o, err := e.addNode(s, fmt.Sprintf("nodes[%d]", i))
			if err != nil {
				return nil, err
			}
			outs = append(outs, o)
		}
		return map[string]any{"nodes": outs}, nil
	case agenttool.ToolUpdateNodes:
		var ps []agentNodePatch
		if err := decodeArgs(args["updates"], &ps); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(ps))
		for i, p := range ps {
			id, err := e.updateNode(p, fmt.Sprintf("updates[%d]", i))
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return map[string]any{"updated": ids}, nil
	case agenttool.ToolDeleteNodes:
		var ids []string
		if err := decodeArgs(args["node_ids"], &ids); err != nil {
			return nil, err
		}
		nodes, edges := []string{}, []string{}
		for i, id := range ids {
			if _, ok := e.nodes[id]; !ok {
				if _, ok := e.refs[id]; !ok {
					continue // 已不存在：幂等
				}
			}
			nid, rm, err := e.deleteNode(id, fmt.Sprintf("node_ids[%d]", i))
			if err != nil {
				return nil, err
			}
			nodes, edges = append(nodes, nid), append(edges, rm...)
		}
		return map[string]any{"deleted_nodes": nodes, "deleted_edges": edges}, nil
	case agenttool.ToolMoveNodes:
		var ms []agentMove
		if err := decodeArgs(args["moves"], &ms); err != nil {
			return nil, err
		}
		outs := make([]*agentPosOut, 0, len(ms))
		for i, m := range ms {
			o, err := e.moveNode(m, fmt.Sprintf("moves[%d]", i))
			if err != nil {
				return nil, err
			}
			outs = append(outs, o)
		}
		return map[string]any{"nodes": outs}, nil
	case agenttool.ToolResizeNodes:
		var ss []agentResize
		if err := decodeArgs(args["sizes"], &ss); err != nil {
			return nil, err
		}
		outs := make([]*agentSizeOut, 0, len(ss))
		for i, s := range ss {
			o, err := e.resizeNode(s, fmt.Sprintf("sizes[%d]", i))
			if err != nil {
				return nil, err
			}
			outs = append(outs, o)
		}
		return map[string]any{"nodes": outs}, nil
	case agenttool.ToolConnect:
		var es []agentEdgeSpec
		if err := decodeArgs(args["edges"], &es); err != nil {
			return nil, err
		}
		outs, replaced := make([]*agentEdgeOut, 0, len(es)), []string{}
		for i, s := range es {
			o, rp, err := e.connect(s, fmt.Sprintf("edges[%d]", i))
			if err != nil {
				return nil, err
			}
			outs, replaced = append(outs, o), append(replaced, rp...)
		}
		return map[string]any{"edges": outs, "replaced": replaced}, nil
	case agenttool.ToolDisconnect:
		var in struct {
			EdgeIDs []string          `json:"edge_ids"`
			Pairs   []agentDisconnect `json:"pairs"`
		}
		if err := decodeArgs(args, &in); err != nil {
			return nil, err
		}
		if len(in.EdgeIDs) == 0 && len(in.Pairs) == 0 {
			return nil, xerr.Param("edge_ids 与 pairs 至少给一个")
		}
		removed := []string{}
		for i, id := range in.EdgeIDs {
			rm, err := e.disconnect(agentDisconnect{EdgeID: id}, fmt.Sprintf("edge_ids[%d]", i))
			if err != nil {
				return nil, err
			}
			removed = append(removed, rm...)
		}
		for i, p := range in.Pairs {
			rm, err := e.disconnect(p, fmt.Sprintf("pairs[%d]", i))
			if err != nil {
				return nil, err
			}
			removed = append(removed, rm...)
		}
		return map[string]any{"removed": removed}, nil
	case agenttool.ToolAutoLayout:
		var l agentLayout
		if err := decodeArgs(args, &l); err != nil {
			return nil, err
		}
		outs, err := e.layout(l, "layout")
		if err != nil {
			return nil, err
		}
		return map[string]any{"nodes": outs}, nil
	case agenttool.ToolBatchEdit:
		return applyBatch(e, args)
	}
	return nil, agentErr(agenttool.ErrUnknownTool, "没有这个工具："+tool, false)
}

// applyBatch canvas_batch_edit：逐步校验 args 并执行；任何一步失败整批不保存（编辑器只在内存里）。
func applyBatch(e *docEditor, args map[string]any) (map[string]any, error) {
	var ops []struct {
		Op   string         `json:"op"`
		Args map[string]any `json:"args"`
	}
	if err := decodeArgs(args["ops"], &ops); err != nil {
		return nil, err
	}
	results := make([]any, 0, len(ops))
	for i, op := range ops {
		path := fmt.Sprintf("ops[%d]", i)
		s, ok := agenttool.BatchArgSchema(op.Op)
		if !ok {
			return nil, xerr.Param(path + "：不支持的操作 " + op.Op)
		}
		if op.Args == nil {
			op.Args = map[string]any{}
		}
		if err := agenttool.Validate(s, op.Args); err != nil {
			return nil, xerr.Param(path + ".args." + err.Error())
		}
		var (
			res any
			err error
		)
		switch op.Op {
		case "add_node":
			var sp agentNodeSpec
			if err = decodeArgs(op.Args, &sp); err == nil {
				res, err = e.addNode(sp, path)
			}
		case "update_node":
			var p agentNodePatch
			if err = decodeArgs(op.Args, &p); err == nil {
				var id string
				id, err = e.updateNode(p, path)
				res = map[string]any{"node_id": id}
			}
		case "delete_node":
			var nid string
			var rm []string
			nid, rm, err = e.deleteNode(strData(op.Args, "node_id"), path)
			res = map[string]any{"node_id": nid, "deleted_edges": rm}
		case "move_node":
			var m agentMove
			if err = decodeArgs(op.Args, &m); err == nil {
				res, err = e.moveNode(m, path)
			}
		case "resize_node":
			var r agentResize
			if err = decodeArgs(op.Args, &r); err == nil {
				res, err = e.resizeNode(r, path)
			}
		case "connect":
			var sp agentEdgeSpec
			if err = decodeArgs(op.Args, &sp); err == nil {
				var o *agentEdgeOut
				var rp []string
				o, rp, err = e.connect(sp, path)
				res = map[string]any{"edge": o, "replaced": rp}
			}
		case "disconnect":
			var d agentDisconnect
			if err = decodeArgs(op.Args, &d); err == nil {
				var rm []string
				rm, err = e.disconnect(d, path)
				res = map[string]any{"removed": rm}
			}
		case "auto_layout":
			var l agentLayout
			if err = decodeArgs(op.Args, &l); err == nil {
				res, err = e.layout(l, path)
			}
		}
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	return map[string]any{"results": results, "refs": e.refs}, nil
}

// agentMutate 读存储态快照 → fn 编辑 → 整段校验 → 按读到的 revision CAS 保存。
func (c *core) agentMutate(ctx context.Context, userID int64, canvasID, baseRev string, meta *agentMeta, fn func(e *docEditor) (map[string]any, error)) (map[string]any, error) {
	for attempt := 0; attempt < agentSaveAttempts; attempt++ {
		cur, err := c.s.Repo.Canvas.GetOwned(ctx, userID, canvasID)
		if err != nil {
			return nil, err
		}
		if cur == nil {
			return nil, xerr.NotFoundf("画布")
		}
		if meta.revBefore == "" {
			meta.revBefore = cur.Revision
		}
		if baseRev != "" && baseRev != cur.Revision {
			return nil, conflictErr(cur.Revision)
		}
		d, err := ParseSnapshot(cur.Snapshot, 0)
		if err != nil {
			return nil, err
		}
		e := newDocEditor(d, c.agentFindModel, c.agentModels, c.now())
		out, err := fn(e)
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = map[string]any{}
		}
		out["canvas_id"] = cur.ID
		if !e.changed {
			out["revision"], out["node_count"] = cur.Revision, cur.NodeCount
			out["changed_node_ids"] = []string{}
			meta.revAfter = cur.Revision
			return out, nil
		}
		if c.s.Config.Limits.MaxNodes > 0 && len(d.Nodes) > c.s.Config.Limits.MaxNodes {
			return nil, xerr.Param(fmt.Sprintf("节点数超过上限 %d", c.s.Config.Limits.MaxNodes))
		}
		saved, err := c.prepareSnapshot(ctx, cur.ID, d.Marshal())
		if err != nil {
			return nil, err
		}
		rev, snap := NewRevision(c.now()), saved.Marshal()
		ok, err := c.s.Repo.Canvas.Save(ctx, repo.SaveArgs{
			ID: cur.ID, UserID: userID, NewRevision: rev, Snapshot: snap, NodeCount: saved.NodeCount(),
			CoverURL: saved.Cover(), IfMatch: cur.Revision,
		})
		if err != nil {
			return nil, err
		}
		if ok {
			changed := diffSnapshots(cur.Snapshot, snap)
			out["revision"], out["node_count"] = rev, saved.NodeCount()
			out["changed_node_ids"] = clipIDs(changed)
			meta.revAfter = rev
			c.afterWrite(ctx, cur.ID, userID, rev, snap, saved.NodeCount(), changed)
			return out, nil
		}
		if baseRev != "" {
			now, err := c.s.Repo.Canvas.GetOwned(ctx, userID, canvasID)
			if err != nil {
				return nil, err
			}
			if now == nil {
				return nil, xerr.NotFoundf("画布")
			}
			return nil, conflictErr(now.Revision)
		}
		// 不带 base_revision：在新版本上重放（语义补丁，不覆盖别人的修改）
	}
	return nil, conflictErr("")
}

// agentFindModel 模型档（目录口径）。
func (c *core) agentFindModel(key, kind string) (*canvas.CatalogModel, bool) {
	m, ok := c.s.Config.FindModel(key, key, kind)
	if !ok {
		return nil, false
	}
	return c.catalogModel(m), true
}

func (c *core) agentModels(kind string) []*canvas.CatalogModel {
	var out []*canvas.CatalogModel
	for _, m := range c.s.Config.ModelTable() {
		if m.Kind == kind {
			out = append(out, c.catalogModel(m))
		}
	}
	return out
}

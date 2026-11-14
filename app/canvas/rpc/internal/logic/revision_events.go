package logic

// 画布实时同步（P0-8）的服务端部分：
//   - 每次 revision 成功变化（用户保存、智能体写入、新建 / 另存 / 从模板新建）→ afterWrite：记快照历史（afv_canvas_revision）+ 发 Redis 事件；
//   - run 状态变化（受理 / 成功 / 失败 / 取消 / 选中候选）→ publishRun：发 source=run 事件（run 结果不写快照，revision 不变）；
//   - GetCanvas ?revision= 读历史快照（三方合并 base），GetCanvasRevision 给网关 SSE / 轮询取当前 revision。
// 历史与事件都是尽力而为：失败只记日志，不影响写入本身。设计见 docs/design/canvas-agent-tools.md §11。

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/agenttool"
	"aigc-local-life/app/canvas/canvasevent"
	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// ReasonRevisionNotRetained ?revision= 超出保留范围（或从未存在 / 历史没记上）。
const ReasonRevisionNotRetained = "CANVAS_REVISION_NOT_RETAINED"

// afterWriteTimeout 历史写入 + 事件发布的总时限（与调用方的取消解耦）。
const afterWriteTimeout = 3 * time.Second

type actorKey struct{}

type writeActor struct{ source, actor string }

// withActor 标记本次写入的来源（智能体入口在 AgentCall 开头设置；没设置的写入按用户保存处理）。
func withActor(ctx context.Context, source, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, writeActor{source: source, actor: clip(actor, 64)})
}

func actorOf(ctx context.Context, userID int64) (source, actor string) {
	if a, ok := ctx.Value(actorKey{}).(writeActor); ok && a.source != "" {
		return a.source, a.actor
	}
	return canvasevent.SourceUser, "user:" + strconv.FormatInt(userID, 10)
}

// agentActor 智能体调用的执行者：只有服务间入口（client=svc，网关验过 HMAC 后写入 agent=svc:<keyId>）才认 svc: 前缀；
// 其余为 <client>:<agent>，agent 是客户端自报名，再清洗一次（转义 svc: 前缀），防止冒充服务调用。
func agentActor(client, agent string) string {
	client, agent = strings.TrimSpace(client), strings.TrimSpace(agent)
	if client == ClientSvc && strings.HasPrefix(agent, "svc:") {
		return agent
	}
	agent = agenttool.SanitizeAgentName(agent)
	if agent == "" {
		return client
	}
	return client + ":" + agent
}

// ClientSvc 服务间入口的 client 标记（网关 /internal/v1/canvas/agent/call 写入；用户入口为 mcp / function）。
const ClientSvc = "svc"

// afterWrite revision 已变（CAS 保存成功 / 新建落库）后的收尾：记历史 + 发事件。
func (c *core) afterWrite(ctx context.Context, canvasID string, userID int64, rev string, snapshot []byte, nodeCount int32, changed []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), afterWriteTimeout)
	defer cancel()
	source, actor := actorOf(ctx, userID)
	c.recordRevision(ctx, canvasID, userID, rev, source, actor, snapshot, nodeCount)
	if err := c.s.CanvasEvents.Publish(ctx, canvasevent.Event{
		CanvasID: canvasID, Revision: rev, Source: source, Actor: actor, ChangedNodeIDs: changed, At: c.now().UnixMilli(),
	}); err != nil {
		logx.WithContext(ctx).Errorf("canvas: 发画布变更事件失败 canvas=%s rev=%s（check=canvas.event_publish）: %v", canvasID, rev, err)
	}
}

// recordRevision 记一个 revision 的快照并裁剪到最近 RevisionKeep 个。
func (c *core) recordRevision(ctx context.Context, canvasID string, userID int64, rev, source, actor string, snapshot []byte, nodeCount int32) {
	keep := c.s.Config.Limits.RevisionKeep
	if keep <= 0 || c.s.Repo == nil || c.s.Repo.Revision == nil {
		return
	}
	gz, err := gzipBytes(snapshot)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 压缩快照失败 canvas=%s rev=%s: %v", canvasID, rev, err)
		countRevisionWrite("gzip_failed")
		observeRevisionWrite(ctx, canvasID, source, len(snapshot), -1)
		return
	}
	row := &model.CanvasRevision{CanvasID: canvasID, UserID: userID, Revision: rev, Source: source, Actor: actor,
		NodeCount: nodeCount, SnapshotBytes: int32(len(snapshot)), Snapshot: gz}
	if err := c.s.Repo.Revision.Insert(ctx, row); err != nil {
		logx.WithContext(ctx).Errorf("canvas: 记快照历史失败 canvas=%s rev=%s（check=canvas.revision_record，Go_V1.0.40 是否已执行）: %v", canvasID, rev, err)
		countRevisionWrite("insert_failed")
		return
	}
	countRevisionWrite("ok")
	observeRevisionWrite(ctx, canvasID, source, len(snapshot), len(gz))
	pruned, err := c.s.Repo.Revision.Prune(ctx, canvasID, keep)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 裁剪快照历史失败 canvas=%s（check=canvas.revision_prune）: %v", canvasID, err)
	}
	countRevisionPruned(pruned, err == nil)
}

// publishRun run 状态变化事件（revision 取画布当前值）。没配事件 Redis 时不查库。
func (c *core) publishRun(ctx context.Context, run *model.NodeRun) {
	if run == nil || !c.s.CanvasEvents.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), afterWriteTimeout)
	defer cancel()
	rev, err := c.s.Repo.Canvas.RevisionOf(ctx, run.CanvasID)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: run %d 事件取 revision 失败: %v", run.ID, err)
		return
	}
	if rev == "" {
		return // 画布已不存在
	}
	reqID := strconv.FormatInt(run.ID, 10)
	if err := c.s.CanvasEvents.Publish(ctx, canvasevent.Event{
		CanvasID: run.CanvasID, Revision: rev, Source: canvasevent.SourceRun, Actor: "run:" + reqID,
		ChangedNodeIDs: []string{run.NodeID}, At: c.now().UnixMilli(), RequestID: reqID, RunStatus: model.StatusText(run.Status),
	}); err != nil {
		logx.WithContext(ctx).Errorf("canvas: 发 run 事件失败 run=%d（check=canvas.event_publish）: %v", run.ID, err)
	}
}

// changedNodeIDs 两份存储态快照之间变化的节点：新增 / 删除 / 内容（含位置、尺寸、data）改动，以及增删改的连线两端。
// 按新快照节点顺序、再按旧快照里被删的节点顺序输出，去重。old 为 nil 时视为空画布。
func changedNodeIDs(old, cur *Doc) []string {
	if old == nil {
		old = &Doc{}
	}
	if cur == nil {
		cur = &Doc{}
	}
	before := make(map[string]string, len(old.Nodes))
	for _, n := range old.Nodes {
		before[n.ID] = canonicalNode(n)
	}
	seen := map[string]bool{}
	var out []string
	add := func(id string) {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	after := make(map[string]bool, len(cur.Nodes))
	for _, n := range cur.Nodes {
		after[n.ID] = true
		if b, ok := before[n.ID]; !ok || b != canonicalNode(n) {
			add(n.ID)
		}
	}
	for _, n := range old.Nodes {
		if !after[n.ID] {
			add(n.ID)
		}
	}
	edgesBefore := make(map[string]string, len(old.Edges))
	ends := make(map[string][2]string, len(old.Edges))
	for _, e := range old.Edges {
		edgesBefore[e.ID] = canonicalEdge(e)
		ends[e.ID] = [2]string{e.Source, e.Target}
	}
	edgesAfter := make(map[string]bool, len(cur.Edges))
	for _, e := range cur.Edges {
		edgesAfter[e.ID] = true
		if b, ok := edgesBefore[e.ID]; !ok || b != canonicalEdge(e) {
			add(e.Source)
			add(e.Target)
			if ok {
				add(ends[e.ID][0])
				add(ends[e.ID][1])
			}
		}
	}
	for _, e := range old.Edges {
		if !edgesAfter[e.ID] {
			add(e.Source)
			add(e.Target)
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// canonicalNode 节点的规范化 JSON（键排序、数字按 float64 重编码），用于判「是否改过」：
// 库里的 JSON 列会重排键序、改数字写法，不能直接比字节。
func canonicalNode(n *Node) string {
	m := make(map[string]json.RawMessage, len(n.raw)+3)
	for k, v := range n.raw {
		m[k] = v
	}
	m["id"] = mustJSON(n.ID)
	if n.Type != "" {
		m["type"] = mustJSON(n.Type)
	}
	m["data"] = mustJSON(n.Data)
	return canonicalJSON(m)
}

func canonicalEdge(e *Edge) string {
	m := make(map[string]json.RawMessage, len(e.raw)+3)
	for k, v := range e.raw {
		m[k] = v
	}
	m["id"] = mustJSON(e.ID)
	m["source"] = mustJSON(e.Source)
	m["target"] = mustJSON(e.Target)
	return canonicalJSON(m)
}

func canonicalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	var x any
	if json.Unmarshal(b, &x) != nil {
		return string(b)
	}
	b, _ = json.Marshal(x)
	return string(b)
}

// diffSnapshots 两段快照 JSON 的变化节点（解析失败时返回 nil，事件里就没有明细，前端整画布刷新）。
func diffSnapshots(oldRaw, newRaw []byte) []string {
	o, err1 := ParseSnapshot(oldRaw, 0)
	n, err2 := ParseSnapshot(newRaw, 0)
	if err1 != nil || err2 != nil {
		return nil
	}
	return changedNodeIDs(o, n)
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if _, err := w.Write(b); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(b []byte, limit int) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if limit <= 0 {
		limit = 16 << 20
	}
	return io.ReadAll(io.LimitReader(r, int64(limit)+1))
}

// GetCanvasRevision 画布当前 revision（只读元数据，不读快照）。
func (c *core) GetCanvasRevision(ctx context.Context, in *canvas.CanvasIdReq) (*canvas.CanvasRevisionResp, error) {
	if err := needUser(in.UserId); err != nil {
		return nil, err
	}
	x, err := c.s.Repo.Canvas.GetOwnedMeta(ctx, in.UserId, in.Id)
	if err != nil {
		return nil, err
	}
	if x == nil {
		return nil, xerr.NotFoundf("画布")
	}
	return &canvas.CanvasRevisionResp{Revision: x.Revision, UpdatedAt: ms(x.UpdatedAt), NodeCount: x.NodeCount}, nil
}

// historicalSnapshot ?revision= 的历史快照（归属已由调用方校验）。没有返回 4001 + CANVAS_REVISION_NOT_RETAINED。
func (c *core) historicalSnapshot(ctx context.Context, x *model.Canvas, rev string) (*model.CanvasRevision, []byte, error) {
	notRetained := xerr.NewReason(xerr.NotFound, ReasonRevisionNotRetained,
		"该版本已超出保留范围（每张画布只保留最近 "+strconv.Itoa(max(c.s.Config.Limits.RevisionKeep, 0))+" 个版本），请载入最新版本")
	if c.s.Config.Limits.RevisionKeep <= 0 || len(rev) > 32 {
		return nil, nil, notRetained
	}
	h, err := c.s.Repo.Revision.Get(ctx, x.ID, rev)
	if err != nil {
		// 表还不存在（迁移未执行）等：按「没留存」答，细节进日志
		logx.WithContext(ctx).Errorf("canvas: 读快照历史失败 canvas=%s rev=%s（check=canvas.revision_get）: %v", x.ID, rev, err)
		return nil, nil, notRetained
	}
	if h == nil {
		return nil, nil, notRetained
	}
	raw, err := gunzipBytes(h.Snapshot, max(c.s.Config.Limits.MaxSnapshotBytes, 2<<20)*2)
	if err != nil {
		logx.WithContext(ctx).Errorf("canvas: 解压快照历史失败 canvas=%s rev=%s: %v", x.ID, rev, err)
		return nil, nil, notRetained
	}
	return h, raw, nil
}

// changedForEvent 只有会发事件时才算节点差异（整段解析两份快照不便宜）。
func (c *core) changedForEvent(oldRaw, newRaw []byte) []string {
	if !c.s.CanvasEvents.Enabled() {
		return nil
	}
	return diffSnapshots(oldRaw, newRaw)
}

// clipIDs 工具出参里的 changed_node_ids（≤200，与事件同一上限）。
func clipIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	if len(ids) > canvasevent.MaxChangedNodeIDs {
		return ids[:canvasevent.MaxChangedNodeIDs]
	}
	return ids
}

package logic

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/pkg/xerr"
)

// 快照是前端 Vue Flow 的 {nodes[{id,type,position,data}], edges[{id,source,target,sourceHandle,targetHandle,data{order}}]}，
// 服务端整段存、只校验，不翻译字段（data 内部保持前端 camelCase）。未知键（节点上的 draggable / zIndex、
// 顶层的 viewport 等）原样保留：解析时留着原始 JSON，写回时只覆盖改动过的键。

// Node 快照里的一个节点。
type Node struct {
	ID   string
	Type string
	Data map[string]any
	raw  map[string]json.RawMessage
}

// Kind data.kind。
func (n *Node) Kind() string { return strData(n.Data, "kind") }

// Tool data.tool。
func (n *Node) Tool() string { return strData(n.Data, "tool") }

// Edge 快照里的一条连线。
type Edge struct {
	ID, Source, Target, SourceHandle, TargetHandle string
	Order                                          int
	raw                                            map[string]json.RawMessage
}

// Doc 解析后的快照。
type Doc struct {
	Nodes []*Node
	Edges []*Edge
	raw   map[string]json.RawMessage
}

// 节点类型（原型 6 种）。
var nodeTypes = map[string]bool{"image": true, "text": true, "model": true, "module": true, "video": true, "tool": true}

// EmptySnapshot 新画布的快照。
const EmptySnapshot = `{"nodes":[],"edges":[]}`

func decodeUseNumber(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	return d.Decode(v)
}

// ParseSnapshot 解析快照；结构不对返回 1003。maxBytes>0 时先查体积。
func ParseSnapshot(b []byte, maxBytes int) (*Doc, error) {
	if maxBytes > 0 && len(b) > maxBytes {
		return nil, xerr.Param(fmt.Sprintf("画布内容超过 %dMB", maxBytes>>20))
	}
	if len(bytes.TrimSpace(b)) == 0 {
		b = []byte(EmptySnapshot)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil || top == nil {
		return nil, xerr.Param("画布快照必须是 JSON 对象 {nodes, edges}")
	}
	d := &Doc{raw: top}
	var rawNodes, rawEdges []map[string]json.RawMessage
	if v, ok := top["nodes"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &rawNodes); err != nil {
			return nil, xerr.Param("快照 nodes 必须是数组")
		}
	}
	if v, ok := top["edges"]; ok && string(v) != "null" {
		if err := json.Unmarshal(v, &rawEdges); err != nil {
			return nil, xerr.Param("快照 edges 必须是数组")
		}
	}
	for i, rn := range rawNodes {
		n := &Node{raw: rn, Data: map[string]any{}}
		_ = json.Unmarshal(rn["id"], &n.ID)
		_ = json.Unmarshal(rn["type"], &n.Type)
		if v, ok := rn["data"]; ok && string(v) != "null" {
			if err := decodeUseNumber(v, &n.Data); err != nil || n.Data == nil {
				return nil, xerr.Param(fmt.Sprintf("第 %d 个节点的 data 必须是对象", i+1))
			}
		}
		d.Nodes = append(d.Nodes, n)
	}
	for _, re := range rawEdges {
		e := &Edge{raw: re}
		_ = json.Unmarshal(re["id"], &e.ID)
		_ = json.Unmarshal(re["source"], &e.Source)
		_ = json.Unmarshal(re["target"], &e.Target)
		_ = json.Unmarshal(re["sourceHandle"], &e.SourceHandle)
		_ = json.Unmarshal(re["targetHandle"], &e.TargetHandle)
		if v, ok := re["data"]; ok {
			var ed map[string]any
			if decodeUseNumber(v, &ed) == nil {
				e.Order = intData(ed, "order")
			}
		}
		d.Edges = append(d.Edges, e)
	}
	return d, nil
}

// Marshal 写回 JSON（节点 data 与 id、连线 id/source/target 以结构体为准）。
func (d *Doc) Marshal() []byte {
	top := make(map[string]json.RawMessage, len(d.raw)+2)
	for k, v := range d.raw {
		top[k] = v
	}
	nodes := make([]map[string]json.RawMessage, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		m := make(map[string]json.RawMessage, len(n.raw)+3)
		for k, v := range n.raw {
			m[k] = v
		}
		m["id"] = mustJSON(n.ID)
		if n.Type != "" {
			m["type"] = mustJSON(n.Type)
		}
		m["data"] = mustJSON(n.Data)
		nodes = append(nodes, m)
	}
	edges := make([]map[string]json.RawMessage, 0, len(d.Edges))
	for _, e := range d.Edges {
		m := make(map[string]json.RawMessage, len(e.raw)+3)
		for k, v := range e.raw {
			m[k] = v
		}
		m["id"] = mustJSON(e.ID)
		m["source"] = mustJSON(e.Source)
		m["target"] = mustJSON(e.Target)
		edges = append(edges, m)
	}
	top["nodes"] = mustJSON(nodes)
	top["edges"] = mustJSON(edges)
	b, _ := json.Marshal(top)
	return b
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// NodeByID 节点索引。
func (d *Doc) NodeByID() map[string]*Node {
	m := make(map[string]*Node, len(d.Nodes))
	for _, n := range d.Nodes {
		m[n.ID] = n
	}
	return m
}

// NodeCount 节点数（含 module 框）。
func (d *Doc) NodeCount() int32 { return int32(len(d.Nodes)) }

// Cover 封面：按节点顺序第一张可用的 http(s) 图（data.image，其次候选首张）。
func (d *Doc) Cover() string {
	for _, n := range d.Nodes {
		if u := strData(n.Data, "image"); isHTTP(u) {
			return u
		}
		if c, ok := n.Data["candidates"].([]any); ok && len(c) > 0 {
			if u, ok := c[0].(string); ok && isHTTP(u) {
				return u
			}
		}
	}
	return ""
}

func isHTTP(u string) bool {
	l := strings.ToLower(strings.TrimSpace(u))
	return strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

// 运行态字段。transientKeys 一律剥离（状态类，回填覆盖）；resultKeys 只剥 model / tool 节点上的成品（image / video 节点的是用户内容）。
// selectedCandidate 是用户的选择，不剥离：有成功 run 的节点以 run.selected_index 为准（POST .../select-candidate 写回），
// 没有 run 的节点（模板示例、导入的旧画布）保留快照里的值。
var (
	transientKeys = []string{"status", "progress", "runId", "requestId", "errorMessage", "requestedCount"}
	resultKeys    = []string{"candidates", "image", "video", "assetId", "resultText", "resultImage", "tryonResults"}
)

// StripRuntime 剥离运行态：所有节点去状态类字段；model / tool 节点且有过 run 的（hasRun）再去成品字段（GET 时由 run 回填）。
// 没有 run 的节点（如模板示例、导入的旧画布）保留成品字段，避免丢内容。
func (d *Doc) StripRuntime(hasRun map[string]bool) {
	for _, n := range d.Nodes {
		for _, k := range transientKeys {
			delete(n.Data, k)
		}
		if (n.Type == "model" || n.Type == "tool") && hasRun[n.ID] {
			for _, k := range resultKeys {
				delete(n.Data, k)
			}
		}
	}
}

// StripAllResults 另存为 / 存为模板：去掉全部运行态与 model / tool 节点的成品（副本节点都是 idle）。
func (d *Doc) StripAllResults() {
	all := make(map[string]bool, len(d.Nodes))
	for _, n := range d.Nodes {
		all[n.ID] = true
	}
	d.StripRuntime(all)
}

// NodeStatus run 状态 → 节点 data.status（原型取值 idle/queued/running/done/error）。
func NodeStatus(s int8) string {
	switch s {
	case model.RunQueued:
		return "queued"
	case model.RunRunning:
		return "running"
	case model.RunSucceeded:
		return "done"
	}
	return "error"
}

// Backfill GET 画布时回填运行态：节点状态（status / progress / runId / errorMessage）取最近一次 run（latest），
// 成品与候选（candidates / image / video / resultText / selectedCandidate）取最近一次**成功**的 run（succeeded）——
// 最近一次失败或还在跑时，节点仍显示上一次成功的结果。两个切片都是每节点至多一条。
func (d *Doc) Backfill(latest, succeeded []*model.NodeRun, progress func(*model.NodeRun) int32) {
	last := make(map[string]*model.NodeRun, len(latest))
	for _, r := range latest {
		last[r.NodeID] = r
	}
	done := make(map[string]*model.NodeRun, len(succeeded))
	for _, r := range succeeded {
		done[r.NodeID] = r
	}
	for _, n := range d.Nodes {
		if r := last[n.ID]; r != nil {
			n.Data["status"] = NodeStatus(r.Status)
			n.Data["progress"] = progress(r)
			n.Data["runId"] = r.RunID
			n.Data["requestId"] = strconv.FormatInt(r.ID, 10)
			n.Data["requestedCount"] = r.RequestedCount
			switch r.Status {
			case model.RunFailed:
				n.Data["errorMessage"] = r.ErrorReason.String
			case model.RunCanceled:
				n.Data["errorMessage"] = firstNonEmpty(r.ErrorReason.String, "已取消执行")
			}
		}
		if r := done[n.ID]; r != nil {
			backfillResult(n, r)
		}
	}
}

// backfillResult 把一次成功 run 的成品写进节点 data。
func backfillResult(n *Node, r *model.NodeRun) {
	cands := r.CandidateList()
	urls := make([]string, 0, len(cands))
	for _, c := range cands {
		urls = append(urls, c.URL)
	}
	sel := int(r.SelectedIndex)
	if sel < 0 || sel >= len(urls) {
		sel = 0
	}
	n.Data["selectedCandidate"] = sel
	if len(urls) > 0 {
		n.Data["candidates"] = urls
	}
	switch r.Kind {
	case model.KindVideo:
		if r.VideoURL.Valid {
			n.Data["video"] = r.VideoURL.String
		}
		if len(urls) > 0 {
			n.Data["image"] = urls[0]
		}
	case model.KindToolI2P:
		n.Data["resultText"] = r.ResultText.String
	case model.KindToolTryon:
		n.Data["tryonResults"] = urls
		if len(urls) > 0 {
			n.Data["image"] = urls[sel]
		}
	default:
		if len(urls) > 0 {
			n.Data["image"] = urls[sel]
			if n.Type == "tool" {
				n.Data["resultImage"] = urls[sel]
			}
		}
	}
}

// RemapIDs 节点 / 连线 id 全部重新生成（从模板建画布、插入子图），内部连线与 data 里对其它节点的引用
// （sourceNodeId、resultNodeId、mentions[].id）同步改写；指向快照外的引用保持原样。
func (d *Doc) RemapIDs(gen func(prefix string) string) map[string]string {
	m := make(map[string]string, len(d.Nodes))
	for _, n := range d.Nodes {
		nid := gen("n")
		m[n.ID] = nid
		n.ID = nid
	}
	re := func(s string) string {
		if v, ok := m[s]; ok {
			return v
		}
		return s
	}
	for _, n := range d.Nodes {
		for _, k := range []string{"sourceNodeId", "resultNodeId", "parentNode", "parentId"} {
			if s, ok := n.Data[k].(string); ok {
				n.Data[k] = re(s)
			}
		}
		if ms, ok := n.Data["mentions"].([]any); ok {
			for _, x := range ms {
				if mm, ok := x.(map[string]any); ok {
					if s, ok := mm["id"].(string); ok {
						mm["id"] = re(s)
					}
				}
			}
		}
		// 节点外层的 parentNode（Vue Flow 子节点）也要跟着改
		for _, k := range []string{"parentNode", "parentId"} {
			if v, ok := n.raw[k]; ok {
				var s string
				if json.Unmarshal(v, &s) == nil {
					n.raw[k] = mustJSON(re(s))
				}
			}
		}
	}
	for _, e := range d.Edges {
		e.ID = gen("e")
		e.Source = re(e.Source)
		e.Target = re(e.Target)
	}
	return m
}

// NewID 随机 id：<prefix>_<unix 毫秒>_<6 位 hex>，与原型 t_1761799000_1 的形状同类。
func NewID(prefix string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixMilli(), hex.EncodeToString(b))
}

// NewCanvasID 画布 id：cv_ + 16 位 hex。
func NewCanvasID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "cv_" + hex.EncodeToString(b)
}

// NewRevision 版本号：<unix 毫秒>-<4 位 hex>。
func NewRevision(now time.Time) string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%d-%s", now.UnixMilli(), hex.EncodeToString(b))
}

func strData(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

func intData(m map[string]any, k string) int { return toInt(m[k]) }

func toInt(v any) int {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i)
		}
		if f, err := x.Float64(); err == nil {
			return int(f)
		}
	case float64:
		return int(x)
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(x))
		return i
	}
	return 0
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := x.Float64()
		return f, err == nil
	case float64:
		return x, true
	case int:
		return float64(x), true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// sortedKeys 稳定遍历。
func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

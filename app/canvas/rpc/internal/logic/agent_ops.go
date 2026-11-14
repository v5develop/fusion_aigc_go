package logic

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/pkg/xerr"
)

// 智能体画布编辑引擎：在**存储态**快照（Doc）上应用语义操作（加 / 改 / 删节点、移动缩放、连线断线、自动排版）。
// 纯内存、不碰库：调用方（agent_call.go mutate）负责读快照、跑 ValidateDoc、乐观锁保存与冲突重放。
// 节点 / 连线的形状与前端工厂一致（fusion-aigc-web src/views/canvas/editor/graph.ts、placement.ts），设计见 docs/design/canvas-agent-tools.md §2。

// agentNodeKind 工具层节点类型 → 快照 type / data.kind / id 前缀 / 默认标题。
type agentNodeKind struct {
	Type, Kind, Prefix, Title string
}

var agentNodeKinds = map[string]agentNodeKind{
	"text":         {"text", "text", "t", "文字"},
	"image":        {"image", "input", "n", "图片"},
	"result_image": {"image", "result", "n", "结果图"},
	"video":        {"video", "video-input", "v", "视频"},
	"image_model":  {"model", "model", "m", "图片生成"},
	"video_model":  {"model", "video", "m", "视频生成"},
	"tool":         {"tool", "tool", "tool", "工具"},
	"group":        {"module", "module", "mod", "分组"},
}

// agentTypeOf 快照节点 → 工具层类型。
func agentTypeOf(n *Node) string {
	switch n.Type {
	case "text":
		return "text"
	case "image":
		if n.Kind() == "result" {
			return "result_image"
		}
		return "image"
	case "video":
		return "video"
	case "model":
		if n.Kind() == "video" {
			return "video_model"
		}
		return "image_model"
	case "tool":
		return "tool"
	case "module":
		return "group"
	}
	return n.Type
}

var toolLabels = map[string]string{"crop": "裁剪", "upscale": "高清放大", "outpaint": "扩图", "mask": "局部重绘", "i2p": "反推提示词", "tryon": "模特试穿"}

// toolDefaults 工具节点的默认参数（前端 makeToolNode）。
func toolDefaults(tool string) map[string]any {
	switch tool {
	case "crop":
		return map[string]any{"crop": map[string]any{"shape": "rect", "x": 0.1, "y": 0.1, "w": 0.6, "h": 0.6}}
	case "upscale":
		return map[string]any{"scale": 2}
	case "i2p":
		return map[string]any{"language": "zh"}
	case "outpaint":
		return map[string]any{"ratio": "16:9"}
	}
	return map[string]any{}
}

// defaultSizeOf 前端 placement.ts defaultSizeOf（data.width / data.height 优先）。
func defaultSizeOf(n *Node) (float64, float64) {
	w, h := 240.0, 260.0
	switch n.Type {
	case "text":
		w, h = 320, 220
	case "video":
		w, h = 338, 380
	case "model":
		w, h = 318, 440
		if n.Kind() == "video" {
			w, h = 338, 480
		}
	case "tool":
		w, h = 340, 320
		if n.Tool() == "tryon" {
			w, h = 380, 460
		}
	case "module":
		w, h = 760, 420
	}
	if v, ok := toFloat(n.Data["width"]); ok && v > 0 {
		w = v
	}
	if v, ok := toFloat(n.Data["height"]); ok && v > 0 {
		h = v
	}
	return w, h
}

// agentKeysIgnored 智能体写 data 时忽略的键：运行态（由 run 回填）与节点身份（由类型决定）。
var agentKeysIgnored = func() map[string]bool {
	m := map[string]bool{"kind": true, "errorCode": true, "retryable": true, "selectedCandidate": true}
	for _, k := range transientKeys {
		m[k] = true
	}
	for _, k := range []string{"candidates", "resultText", "resultImage", "tryonResults"} {
		m[k] = true
	}
	return m
}()

// 边的颜色（前端 INPUT_HANDLE_RULES）。
var handleColors = map[string]string{
	"prompt": "#22c55e", "image": "#38bdf8", "video": "#f97373", "audio": "#a78bfa",
	"garment": "#875f9a", "models": "#2f80ed", "image-in": "#38bdf8",
}

func edgeColor(targetHandle string) string {
	if c, ok := handleColors[targetHandle]; ok {
		return c
	}
	return handleColors["image"]
}

// singleSlot 单槽口：已有连线时新线替换旧线（前端同口径）。
func singleSlot(dst *Node, handle string) bool {
	switch {
	case isInputTool(dst) && handle == "image":
		return true
	case isResultImage(dst):
		return true
	case isTryon(dst) && handle == "garment":
		return true
	}
	switch handle {
	case "prompt", "image1", "image2", "first", "last", "video":
		return true
	}
	return false
}

// modelLookup 按 model / model_id 找模型档（返回目录口径：比例与 gateway 取交集、张数 / 参考图上限取小）。
type modelLookup func(model, kind string) (*canvas.CatalogModel, bool)

// docEditor 一次编辑会话。
type docEditor struct {
	d       *Doc
	findM   modelLookup
	models  func(kind string) []*canvas.CatalogModel
	now     time.Time
	seq     int
	refs    map[string]string // ref → 新节点 id
	nodes   map[string]*Node
	changed bool
}

func newDocEditor(d *Doc, find modelLookup, models func(kind string) []*canvas.CatalogModel, now time.Time) *docEditor {
	return &docEditor{d: d, findM: find, models: models, now: now, refs: map[string]string{}, nodes: d.NodeByID()}
}

func (e *docEditor) newID(prefix string) string {
	for {
		e.seq++
		id := fmt.Sprintf("%s_%d_%d", prefix, e.now.UnixMilli(), e.seq)
		if _, dup := e.nodes[id]; !dup && !e.edgeExists(id) {
			return id
		}
	}
}

func (e *docEditor) edgeExists(id string) bool {
	for _, x := range e.d.Edges {
		if x.ID == id {
			return true
		}
	}
	return false
}

// resolve 节点 id 或本批 ref → 节点。
func (e *docEditor) resolve(idOrRef, path string) (*Node, error) {
	id := strings.TrimSpace(idOrRef)
	if v, ok := e.refs[id]; ok {
		id = v
	}
	n := e.nodes[id]
	if n == nil {
		return nil, xerr.New(xerr.NotFound, fmt.Sprintf("%s：节点 %s 不存在", path, idOrRef))
	}
	return n, nil
}

// ---------------------------------------------------------------- 位置 / 尺寸

type pos struct{ X, Y float64 }

func nodePos(n *Node) pos {
	var p struct {
		X, Y float64
	}
	if v, ok := n.raw["position"]; ok {
		_ = json.Unmarshal(v, &p)
	}
	return pos{p.X, p.Y}
}

func setNodePos(n *Node, p pos) {
	if n.raw == nil {
		n.raw = map[string]json.RawMessage{}
	}
	n.raw["position"] = mustJSON(map[string]float64{"x": round1(p.X), "y": round1(p.Y)})
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }

type rect struct{ X, Y, W, H float64 }

func nodeRect(n *Node) rect {
	p := nodePos(n)
	w, h := defaultSizeOf(n)
	return rect{p.X, p.Y, w, h}
}

func (a rect) overlaps(b rect, pad float64) bool {
	return a.X < b.X+b.W+pad && b.X < a.X+a.W+pad && a.Y < b.Y+b.H+pad && b.Y < a.Y+a.H+pad
}

// freeSpot 前端 spawnPosition + findFreeSpot：按 4 列网格从第 n 格开始找第一个不与现有节点（分组框除外）重叠的位置。
func (e *docEditor) freeSpot(n *Node) pos {
	w, h := defaultSizeOf(n)
	occupied := make([]rect, 0, len(e.d.Nodes))
	for _, x := range e.d.Nodes {
		if x != n && x.Type != "module" {
			occupied = append(occupied, nodeRect(x))
		}
	}
	start := len(e.d.Nodes)
	for i := 0; i < 4000; i++ {
		k := start + i
		p := pos{140 + float64(k%4)*360, 140 + float64(k/4)*520}
		r := rect{p.X, p.Y, w, h}
		free := true
		for _, o := range occupied {
			if r.overlaps(o, 24) {
				free = false
				break
			}
		}
		if free {
			return p
		}
	}
	return pos{140, 140 + float64(start)*40}
}

// ---------------------------------------------------------------- 节点

type agentNodeSpec struct {
	Ref      string         `json:"ref"`
	Type     string         `json:"type"`
	Title    *string        `json:"title"`
	X        *float64       `json:"x"`
	Y        *float64       `json:"y"`
	Width    *float64       `json:"width"`
	Height   *float64       `json:"height"`
	Text     *string        `json:"text"`
	ImageURL *string        `json:"image_url"`
	VideoURL *string        `json:"video_url"`
	Tool     string         `json:"tool"`
	Data     map[string]any `json:"data"`
	agentModelFields
}

type agentModelFields struct {
	Model    *string `json:"model"`
	Prompt   *string `json:"prompt"`
	Ratio    *string `json:"ratio"`
	Size     *string `json:"size"`
	Quality  *string `json:"quality"`
	Count    *int    `json:"count"`
	Duration *string `json:"duration"`
	Sound    *bool   `json:"sound"`
}

func (f agentModelFields) any() bool {
	return f.Model != nil || f.Prompt != nil || f.Ratio != nil || f.Size != nil || f.Quality != nil || f.Count != nil || f.Duration != nil || f.Sound != nil
}

type agentNodeOut struct {
	Ref    string  `json:"ref,omitempty"`
	NodeID string  `json:"node_id"`
	Type   string  `json:"type"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (e *docEditor) addNode(s agentNodeSpec, path string) (*agentNodeOut, error) {
	k, ok := agentNodeKinds[s.Type]
	if !ok {
		return nil, xerr.Param(path + "：不支持的节点类型 " + s.Type)
	}
	if s.Ref != "" {
		if _, dup := e.refs[s.Ref]; dup {
			return nil, xerr.Param(path + "：ref 重复 " + s.Ref)
		}
		if _, clash := e.nodes[s.Ref]; clash {
			return nil, xerr.Param(path + "：ref 不能与已有节点 id 相同 " + s.Ref)
		}
	}
	n := &Node{ID: e.newID(k.Prefix), Type: k.Type, Data: map[string]any{}, raw: map[string]json.RawMessage{}}
	title := k.Title
	switch s.Type {
	case "text":
		n.Data["text"], n.Data["mentions"] = "", []any{}
	case "image", "result_image":
		n.Data["image"] = ""
	case "video":
		n.Data["video"] = ""
	case "image_model":
		for key, v := range map[string]any{"model": "", "modelId": "", "quality": "medium", "count": 1, "cfg": 0.95, "prompt": "",
			"ratio": "1:1", "size": "1K", "maxImageRefs": 5, "capabilities": map[string]any{"count": true}} {
			n.Data[key] = v
		}
	case "video_model":
		for key, v := range map[string]any{"model": "", "modelId": "", "quality": "medium", "count": 1, "cfg": 0.95, "prompt": "",
			"ratio": "16:9", "size": "720p", "duration": "4s", "sound": true, "maxImageRefs": 3,
			"capabilities": map[string]any{"duration": true, "sound": true}} {
			n.Data[key] = v
		}
	case "tool":
		if s.Tool == "" {
			return nil, xerr.Param(path + "：type=tool 时必须给 tool")
		}
		title = toolLabels[s.Tool]
		n.Data["tool"], n.Data["sourceNodeId"] = s.Tool, ""
		for key, v := range toolDefaults(s.Tool) {
			n.Data[key] = v
		}
	case "group":
		n.Data["isGroup"] = true
		n.Data["width"], n.Data["height"] = 760, 420
		n.raw["zIndex"] = mustJSON(-1)
	}
	if s.Tool != "" && s.Type != "tool" {
		return nil, xerr.Param(path + "：只有 type=tool 的节点能给 tool")
	}
	n.Data["kind"] = k.Kind
	n.Data["title"], n.Data["subtitle"] = title, k.Title
	if s.Type == "tool" {
		n.Data["subtitle"] = "工具"
	}
	mergeAgentData(n.Data, s.Data)
	if err := e.applyContent(n, s.Title, s.Text, s.ImageURL, s.VideoURL, path); err != nil {
		return nil, err
	}
	if s.Type == "image_model" || s.Type == "video_model" {
		f := s.agentModelFields
		if f.Model == nil && strData(n.Data, "model") == "" {
			mk := "image"
			if s.Type == "video_model" {
				mk = "video"
			}
			if list := e.models(mk); len(list) > 0 {
				f.Model = &list[0].Model
			}
		}
		if err := e.applyModel(n, f, path); err != nil {
			return nil, err
		}
	} else if s.agentModelFields.any() {
		return nil, xerr.Param(path + "：模型参数（model / prompt / ratio …）只能用于 image_model / video_model 节点")
	}
	if s.Width != nil {
		n.Data["width"] = round1(*s.Width)
	}
	if s.Height != nil {
		n.Data["height"] = round1(*s.Height)
		n.Data["heightFixed"] = true
	}
	e.d.Nodes = append(e.d.Nodes, n)
	e.nodes[n.ID] = n
	if s.Ref != "" {
		e.refs[s.Ref] = n.ID
	}
	var p pos
	if s.X != nil && s.Y != nil {
		p = pos{*s.X, *s.Y}
	} else {
		p = e.freeSpot(n)
		if s.X != nil {
			p.X = *s.X
		}
		if s.Y != nil {
			p.Y = *s.Y
		}
	}
	setNodePos(n, p)
	e.changed = true
	r := nodeRect(n)
	return &agentNodeOut{Ref: s.Ref, NodeID: n.ID, Type: s.Type, X: r.X, Y: r.Y, Width: r.W, Height: r.H}, nil
}

// mergeAgentData JSON Merge Patch：null 删键，对象递归合并；运行态 / 身份键忽略。
func mergeAgentData(dst, patch map[string]any) {
	for k, v := range patch {
		if agentKeysIgnored[k] {
			continue
		}
		if v == nil {
			delete(dst, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeAgentData(dm, pm)
				continue
			}
		}
		dst[k] = v
	}
}

func (e *docEditor) applyContent(n *Node, title, text, image, video *string, path string) error {
	if title != nil {
		n.Data["title"] = strings.TrimSpace(*title)
	}
	if text != nil {
		if n.Type != "text" {
			return xerr.Param(path + "：text 只能用于文字节点")
		}
		n.Data["text"] = *text
	}
	if image != nil {
		if n.Type != "image" {
			return xerr.Param(path + "：image_url 只能用于图片节点")
		}
		u := strings.TrimSpace(*image)
		if u != "" && !isHTTP(u) {
			return xerr.Param(path + "：image_url 必须是 http(s) 地址（dataURL 请先上传）")
		}
		n.Data["image"] = u
	}
	if video != nil {
		if n.Type != "video" {
			return xerr.Param(path + "：video_url 只能用于视频节点")
		}
		u := strings.TrimSpace(*video)
		if u != "" && !isHTTP(u) {
			return xerr.Param(path + "：video_url 必须是 http(s) 地址")
		}
		n.Data["video"] = u
	}
	return nil
}

// applyModel 写模型参数并按模型档校验（前端 applyModelToNode 同口径：model / modelId / badge / maxImageRefs / capabilities / subtitle）。
func (e *docEditor) applyModel(n *Node, f agentModelFields, path string) error {
	kind := "image"
	if n.Kind() == "video" {
		kind = "video"
	}
	cur := strData(n.Data, "modelId")
	if cur == "" {
		cur = strData(n.Data, "model")
	}
	key := cur
	if f.Model != nil {
		key = strings.TrimSpace(*f.Model)
	}
	var m *canvas.CatalogModel
	if key != "" {
		var ok bool
		if m, ok = e.findM(key, kind); !ok {
			return xerr.Param(fmt.Sprintf("%s：没有这个%s模型 %s（用 canvas_catalog 查看可用模型）", path, map[string]string{"image": "图片", "video": "视频"}[kind], key))
		}
	}
	if m != nil && f.Model != nil {
		n.Data["model"], n.Data["modelId"] = m.Model, m.ModelId
		if m.Badge != "" {
			n.Data["badge"] = m.Badge
		}
		if m.MaxImageRefs > 0 {
			n.Data["maxImageRefs"] = int(m.MaxImageRefs)
		}
		caps := map[string]any{"count": m.CapCount}
		if kind == "video" {
			caps = map[string]any{"duration": true, "sound": m.CapSound}
		}
		n.Data["capabilities"] = caps
		// 换模型后旧档位不在新模型里：落回新模型的第一个
		fix := func(key string, list []string) {
			if len(list) > 0 && !containsFold(list, strData(n.Data, key)) {
				n.Data[key] = list[0]
			}
		}
		fix("ratio", m.Ratios)
		fix("size", m.Sizes)
		if kind == "video" {
			fix("duration", m.Durations)
		}
	}
	check := func(field, v string, list []string) error {
		if m != nil && len(list) > 0 && !containsFold(list, v) {
			return xerr.Param(fmt.Sprintf("%s：%s %s 不在该模型可选范围（%s）", path, field, v, strings.Join(list, " / ")))
		}
		return nil
	}
	if f.Prompt != nil {
		n.Data["prompt"] = *f.Prompt
	}
	if f.Ratio != nil {
		if err := check("ratio", *f.Ratio, ratiosOf(m)); err != nil {
			return err
		}
		n.Data["ratio"] = *f.Ratio
	}
	if f.Size != nil {
		if err := check("size", *f.Size, sizesOf(m)); err != nil {
			return err
		}
		n.Data["size"] = *f.Size
	}
	if f.Quality != nil {
		n.Data["quality"] = *f.Quality
	}
	if f.Duration != nil {
		if kind != "video" {
			return xerr.Param(path + "：duration 只能用于视频模型节点")
		}
		if err := check("duration", *f.Duration, durationsOf(m)); err != nil {
			return err
		}
		n.Data["duration"] = *f.Duration
	}
	if f.Sound != nil {
		if kind != "video" {
			return xerr.Param(path + "：sound 只能用于视频模型节点")
		}
		n.Data["sound"] = *f.Sound
	}
	if f.Count != nil {
		c := *f.Count
		if kind == "video" && c != 1 {
			return xerr.Param(path + "：视频一次只能生成 1 个")
		}
		if m != nil && m.CountMax > 0 && c > int(m.CountMax) {
			return xerr.Param(fmt.Sprintf("%s：该模型一次最多 %d 张", path, m.CountMax))
		}
		n.Data["count"] = c
	}
	if m != nil {
		lvl := strData(n.Data, "size")
		if kind == "video" {
			lvl = strData(n.Data, "duration")
		}
		label := m.Label
		if label == "" {
			label = m.Model
		}
		n.Data["subtitle"] = strings.TrimSpace(label + " · " + lvl)
	}
	return nil
}

func ratiosOf(m *canvas.CatalogModel) []string {
	if m == nil {
		return nil
	}
	return m.Ratios
}
func sizesOf(m *canvas.CatalogModel) []string {
	if m == nil {
		return nil
	}
	return m.Sizes
}
func durationsOf(m *canvas.CatalogModel) []string {
	if m == nil {
		return nil
	}
	return m.Durations
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(x, v) {
			return true
		}
	}
	return false
}

type agentNodePatch struct {
	NodeID   string         `json:"node_id"`
	Title    *string        `json:"title"`
	Text     *string        `json:"text"`
	ImageURL *string        `json:"image_url"`
	VideoURL *string        `json:"video_url"`
	Data     map[string]any `json:"data"`
	agentModelFields
}

func (e *docEditor) updateNode(p agentNodePatch, path string) (string, error) {
	n, err := e.resolve(p.NodeID, path)
	if err != nil {
		return "", err
	}
	mergeAgentData(n.Data, p.Data)
	if err := e.applyContent(n, p.Title, p.Text, p.ImageURL, p.VideoURL, path); err != nil {
		return "", err
	}
	if p.agentModelFields.any() {
		if n.Type != "model" {
			return "", xerr.Param(path + "：模型参数只能用于图片 / 视频生成节点")
		}
		if err := e.applyModel(n, p.agentModelFields, path); err != nil {
			return "", err
		}
	}
	e.changed = true
	return n.ID, nil
}

// deleteNode 删节点与相连的连线；其它节点 data 里指向它的 sourceNodeId 清空。
func (e *docEditor) deleteNode(idOrRef, path string) (string, []string, error) {
	n, err := e.resolve(idOrRef, path)
	if err != nil {
		return "", nil, err
	}
	e.d.Nodes = slices.DeleteFunc(e.d.Nodes, func(x *Node) bool { return x == n })
	delete(e.nodes, n.ID)
	var removed []string
	e.d.Edges = slices.DeleteFunc(e.d.Edges, func(x *Edge) bool {
		if x.Source == n.ID || x.Target == n.ID {
			removed = append(removed, x.ID)
			return true
		}
		return false
	})
	for _, x := range e.d.Nodes {
		if strData(x.Data, "sourceNodeId") == n.ID {
			x.Data["sourceNodeId"] = ""
		}
	}
	e.changed = true
	return n.ID, removed, nil
}

type agentMove struct {
	NodeID string   `json:"node_id"`
	X      *float64 `json:"x"`
	Y      *float64 `json:"y"`
	DX     *float64 `json:"dx"`
	DY     *float64 `json:"dy"`
}

type agentPosOut struct {
	NodeID string  `json:"node_id"`
	X      float64 `json:"x"`
	Y      float64 `json:"y"`
}

func (e *docEditor) moveNode(m agentMove, path string) (*agentPosOut, error) {
	n, err := e.resolve(m.NodeID, path)
	if err != nil {
		return nil, err
	}
	if m.X == nil && m.Y == nil && m.DX == nil && m.DY == nil {
		return nil, xerr.Param(path + "：x / y / dx / dy 至少给一个")
	}
	p := nodePos(n)
	if m.X != nil {
		p.X = *m.X
	}
	if m.Y != nil {
		p.Y = *m.Y
	}
	if m.DX != nil {
		p.X += *m.DX
	}
	if m.DY != nil {
		p.Y += *m.DY
	}
	setNodePos(n, p)
	e.changed = true
	p = nodePos(n)
	return &agentPosOut{NodeID: n.ID, X: p.X, Y: p.Y}, nil
}

type agentResize struct {
	NodeID string   `json:"node_id"`
	Width  *float64 `json:"width"`
	Height *float64 `json:"height"`
	Scale  *float64 `json:"scale"`
}

type agentSizeOut struct {
	NodeID string  `json:"node_id"`
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

func (e *docEditor) resizeNode(s agentResize, path string) (*agentSizeOut, error) {
	n, err := e.resolve(s.NodeID, path)
	if err != nil {
		return nil, err
	}
	if s.Width == nil && s.Height == nil && s.Scale == nil {
		return nil, xerr.Param(path + "：width / height / scale 至少给一个")
	}
	w, h := defaultSizeOf(n)
	if s.Scale != nil {
		w, h = w**s.Scale, h**s.Scale
	}
	if s.Width != nil {
		w = *s.Width
	}
	if s.Height != nil {
		h = *s.Height
	}
	w, h = math.Min(math.Max(w, 80), 4000), math.Min(math.Max(h, 80), 4000)
	n.Data["width"], n.Data["height"] = round1(w), round1(h)
	n.Data["heightFixed"] = true
	e.changed = true
	return &agentSizeOut{NodeID: n.ID, Width: round1(w), Height: round1(h)}, nil
}

// ---------------------------------------------------------------- 连线

type agentEdgeSpec struct {
	Source       string `json:"source"`
	Target       string `json:"target"`
	TargetHandle string `json:"target_handle"`
	SourceHandle string `json:"source_handle"`
	Order        int    `json:"order"`
}

type agentEdgeOut struct {
	EdgeID       string `json:"edge_id"`
	Source       string `json:"source"`
	SourceHandle string `json:"source_handle"`
	Target       string `json:"target"`
	TargetHandle string `json:"target_handle"`
	Order        int    `json:"order,omitempty"`
}

func edgeOutOf(x *Edge) agentEdgeOut {
	return agentEdgeOut{EdgeID: x.ID, Source: x.Source, SourceHandle: x.SourceHandle, Target: x.Target, TargetHandle: x.TargetHandle, Order: x.Order}
}

// inferTargetHandle 省略目标口时的推断（设计 §2.2）。
func inferTargetHandle(src, dst *Node) (string, error) {
	switch {
	case isTryon(dst):
		return "", xerr.Param("连到模特试穿节点必须指定 target_handle（garment / models）")
	case src.Type == "text" || (src.Type == "tool" && src.Tool() == "i2p"):
		return "prompt", nil
	case isInputTool(dst):
		return "image", nil
	case isResultImage(dst):
		return "image-in", nil
	case isVideoModel(src):
		return "video", nil
	}
	return "image", nil
}

// sourceHandleOf 源口（前端口名）。
func sourceHandleOf(src *Node) string {
	switch src.Type {
	case "text":
		return "text"
	case "image":
		return "image"
	case "video":
		return "video"
	case "model":
		if src.Kind() == "video" {
			return "video"
		}
		return "image-out"
	case "tool":
		if src.Tool() == "i2p" {
			return "text"
		}
		return "image-out"
	}
	return ""
}

// orderKey 同一输入口计序的键：image / image1 / image2 / first / last 共用 image 序（前端 data.order 口径），其余按原始口。
func orderKey(handle string) string {
	if NormalizeHandle(handle) == HandleImage && handle != "garment" && handle != "models" && handle != "image-in" {
		return HandleImage
	}
	return handle
}

func (e *docEditor) connect(s agentEdgeSpec, path string) (*agentEdgeOut, []string, error) {
	src, err := e.resolve(s.Source, path+".source")
	if err != nil {
		return nil, nil, err
	}
	dst, err := e.resolve(s.Target, path+".target")
	if err != nil {
		return nil, nil, err
	}
	if src.Type == "module" || dst.Type == "module" {
		return nil, nil, xerr.Param(path + "：分组框不参与连线")
	}
	th := s.TargetHandle
	if th == "" {
		if th, err = inferTargetHandle(src, dst); err != nil {
			return nil, nil, xerr.Param(path + "：" + errMsg(err))
		}
	}
	if _, err := checkTarget(src, dst, th); err != nil {
		return nil, nil, xerr.Param(path + "：" + errMsg(err))
	}
	sh := s.SourceHandle
	if sh == "" {
		sh = sourceHandleOf(src)
	}
	for _, x := range e.d.Edges {
		if x.Source == src.ID && x.Target == dst.ID && x.TargetHandle == th && x.SourceHandle == sh {
			o := edgeOutOf(x)
			return &o, nil, nil // 已存在：幂等
		}
	}
	var replaced []string
	if singleSlot(dst, th) {
		e.d.Edges = slices.DeleteFunc(e.d.Edges, func(x *Edge) bool {
			if x.Target == dst.ID && (x.TargetHandle == th || (isInputTool(dst) || isResultImage(dst))) {
				replaced = append(replaced, x.ID)
				return true
			}
			return false
		})
	}
	edge := &Edge{ID: e.newID("e"), Source: src.ID, Target: dst.ID, SourceHandle: sh, TargetHandle: th, raw: map[string]json.RawMessage{}}
	data := map[string]any{"color": edgeColor(th)}
	if th != "prompt" {
		ord := s.Order
		if ord <= 0 {
			for _, x := range e.d.Edges {
				if x.Target == dst.ID && orderKey(x.TargetHandle) == orderKey(th) && x.Order > ord {
					ord = x.Order
				}
			}
			ord++
		}
		edge.Order = ord
		data["order"] = ord
	}
	edge.raw["sourceHandle"] = mustJSON(sh)
	edge.raw["targetHandle"] = mustJSON(th)
	edge.raw["type"] = mustJSON("canvas-bezier")
	edge.raw["data"] = mustJSON(data)
	e.d.Edges = append(e.d.Edges, edge)
	e.changed = true
	o := edgeOutOf(edge)
	return &o, replaced, nil
}

type agentDisconnect struct {
	EdgeID       string `json:"edge_id"`
	Source       string `json:"source"`
	Target       string `json:"target"`
	TargetHandle string `json:"target_handle"`
}

// disconnect 断线：按 id 或按端点；找不到不报错（幂等），返回实际断开的。
func (e *docEditor) disconnect(s agentDisconnect, path string) ([]string, error) {
	if s.EdgeID == "" && (s.Source == "" || s.Target == "") {
		return nil, xerr.Param(path + "：给 edge_id，或同时给 source 与 target")
	}
	src, dst := e.refs[s.Source], e.refs[s.Target]
	if src == "" {
		src = s.Source
	}
	if dst == "" {
		dst = s.Target
	}
	var removed []string
	e.d.Edges = slices.DeleteFunc(e.d.Edges, func(x *Edge) bool {
		hit := false
		if s.EdgeID != "" {
			hit = x.ID == s.EdgeID
		} else {
			hit = x.Source == src && x.Target == dst && (s.TargetHandle == "" || x.TargetHandle == s.TargetHandle)
		}
		if hit {
			removed = append(removed, x.ID)
		}
		return hit
	})
	if len(removed) > 0 {
		e.changed = true
	}
	return removed, nil
}

// ---------------------------------------------------------------- 自动排版

type agentLayout struct {
	NodeIDs   []string `json:"node_ids"`
	Direction string   `json:"direction"`
	GapX      *float64 `json:"gap_x"`
	GapY      *float64 `json:"gap_y"`
	OriginX   *float64 `json:"origin_x"`
	OriginY   *float64 `json:"origin_y"`
}

// layout 分层排版：层号 = 子图内最长路径深度（源在第 0 层）；同层按原坐标（LR 按 y、TB 按 x）稳定排序；
// 每层宽度取层内最宽节点，层内按节点高度累加。分组框不参与（成员关系按几何计算，移动成员不动分组框）。
func (e *docEditor) layout(s agentLayout, path string) ([]agentPosOut, error) {
	var set []*Node
	if len(s.NodeIDs) > 0 {
		seen := map[string]bool{}
		for i, id := range s.NodeIDs {
			n, err := e.resolve(id, fmt.Sprintf("%s.node_ids[%d]", path, i))
			if err != nil {
				return nil, err
			}
			if n.Type != "module" && !seen[n.ID] {
				seen[n.ID] = true
				set = append(set, n)
			}
		}
	} else {
		for _, n := range e.d.Nodes {
			if n.Type != "module" {
				set = append(set, n)
			}
		}
	}
	if len(set) == 0 {
		return []agentPosOut{}, nil
	}
	in := make(map[string]bool, len(set))
	for _, n := range set {
		in[n.ID] = true
	}
	gapX, gapY := 120.0, 80.0
	if s.GapX != nil {
		gapX = *s.GapX
	}
	if s.GapY != nil {
		gapY = *s.GapY
	}
	// 起点：被排节点的最左上角
	ox, oy := math.Inf(1), math.Inf(1)
	for _, n := range set {
		p := nodePos(n)
		ox, oy = math.Min(ox, p.X), math.Min(oy, p.Y)
	}
	if s.OriginX != nil {
		ox = *s.OriginX
	}
	if s.OriginY != nil {
		oy = *s.OriginY
	}
	// 最长路径分层（快照已保证无环；保险起见限制迭代次数）
	depth := map[string]int{}
	for range set {
		moved := false
		for _, ed := range e.d.Edges {
			if in[ed.Source] && in[ed.Target] && depth[ed.Target] < depth[ed.Source]+1 {
				depth[ed.Target] = depth[ed.Source] + 1
				moved = true
			}
		}
		if !moved {
			break
		}
	}
	layers := map[int][]*Node{}
	maxL := 0
	for _, n := range set {
		l := depth[n.ID]
		layers[l] = append(layers[l], n)
		maxL = max(maxL, l)
	}
	tb := s.Direction == "TB"
	out := make([]agentPosOut, 0, len(set))
	cursor := 0.0 // 层方向上的累计偏移
	for l := 0; l <= maxL; l++ {
		ns := layers[l]
		sort.SliceStable(ns, func(i, j int) bool {
			a, b := nodePos(ns[i]), nodePos(ns[j])
			if tb {
				return a.X < b.X || (a.X == b.X && a.Y < b.Y)
			}
			return a.Y < b.Y || (a.Y == b.Y && a.X < b.X)
		})
		thick, along := 0.0, 0.0
		for _, n := range ns {
			w, h := defaultSizeOf(n)
			var p pos
			if tb {
				p = pos{ox + along, oy + cursor}
				along += w + gapX
				thick = math.Max(thick, h)
			} else {
				p = pos{ox + cursor, oy + along}
				along += h + gapY
				thick = math.Max(thick, w)
			}
			setNodePos(n, p)
			q := nodePos(n)
			out = append(out, agentPosOut{NodeID: n.ID, X: q.X, Y: q.Y})
		}
		if tb {
			cursor += thick + gapY
		} else {
			cursor += thick + gapX
		}
	}
	e.changed = true
	return out, nil
}

// ---------------------------------------------------------------- 读出

type agentNodeView struct {
	NodeID string         `json:"node_id"`
	Type   string         `json:"type"`
	Title  string         `json:"title"`
	X      float64        `json:"x"`
	Y      float64        `json:"y"`
	Width  float64        `json:"width"`
	Height float64        `json:"height"`
	Data   map[string]any `json:"data"`
}

// agentView 快照 → 工具出参的节点 / 连线（only 非空时只出这些节点及其之间的连线）。
func agentView(d *Doc, only map[string]bool) ([]agentNodeView, []agentEdgeOut) {
	nodes := make([]agentNodeView, 0, len(d.Nodes))
	for _, n := range d.Nodes {
		if only != nil && !only[n.ID] {
			continue
		}
		r := nodeRect(n)
		nodes = append(nodes, agentNodeView{NodeID: n.ID, Type: agentTypeOf(n), Title: strData(n.Data, "title"),
			X: r.X, Y: r.Y, Width: r.W, Height: r.H, Data: n.Data})
	}
	edges := make([]agentEdgeOut, 0, len(d.Edges))
	for _, x := range d.Edges {
		if only != nil && (!only[x.Source] || !only[x.Target]) {
			continue
		}
		edges = append(edges, edgeOutOf(x))
	}
	return nodes, edges
}

package logic

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"aigc-local-life/pkg/xerr"
)

// 连线规则（07 文件 A.2 / 原型 useCanvasStore.validateConnection，保存与执行两处共用）：
//
//	prompt  ← text 节点或反推（tool=i2p）节点，1 条
//	image   ← image / model / video / tool 节点，≤ 参考图上限（image1 / image2 / first / last 也占这个名额）
//	video   ← 视频模型（model 且 kind=video）的输出，1 条
//	image1 / image2（视频模型的首图 / 次图口）各 1 条
//	garment 1 条、models ≤9 条（试穿工具）；image-in 1 条（结果图片节点）
//	audio   暂不接入
//	tool（裁剪 / 放大 / 扩图 / 重绘 / 反推）← 图片口 image，1 条（工具输入图；也可用 data.sourceNodeId 指向上游）
//	目标只能是 model 节点、工具节点、结果图片节点；不能连自己，不能成环；同一对口不重复连。

// 目标口（归一后）。
const (
	HandlePrompt = "prompt"
	HandleImage  = "image"
	HandleVideo  = "video"
	HandleAudio  = "audio"
)

// 各口上限。
const (
	MaxTryonModels = 9
	defaultRefs    = 3
)

// NormalizeHandle 目标口归一（原型 normalizeHandle）：first/last/image1/image2/garment/models/image-in → image。
func NormalizeHandle(h string) string {
	switch h {
	case HandlePrompt, HandleImage, HandleVideo, HandleAudio:
		return h
	case "first", "last", "image1", "image2", "garment", "models", "image-in":
		return HandleImage
	}
	return ""
}

// ImageLimitFunc 某个目标节点的参考图上限（由 catalog / 配置决定）。
type ImageLimitFunc func(n *Node) int

// DefaultImageLimit 原型 inputLimitForNode：data.maxImageRefs 优先；视频 2；Image 14 / Banana 5 / 其它 3。
func DefaultImageLimit(n *Node) int {
	if n == nil {
		return defaultRefs
	}
	if v := intData(n.Data, "maxImageRefs"); v > 0 {
		return v
	}
	if n.Kind() == "video" {
		return 2
	}
	switch strData(n.Data, "model") {
	case "Image":
		return 14
	case "Banana":
		return 5
	}
	return defaultRefs
}

func isTryon(n *Node) bool       { return n.Type == "tool" && n.Tool() == "tryon" }
func isResultImage(n *Node) bool { return n.Type == "image" && n.Kind() == "result" }

// isInputTool 有图片输入口的工具（试穿除外）：裁剪、放大、扩图、重绘、反推。
func isInputTool(n *Node) bool  { return n.Type == "tool" && n.Tool() != "tryon" }
func isVideoModel(n *Node) bool { return n.Type == "model" && n.Kind() == "video" }

// accepts 归一口接受的来源节点类型。
func accepts(handle string, src *Node) error {
	switch handle {
	case HandlePrompt:
		if src.Type == "text" || (src.Type == "tool" && src.Tool() == "i2p") {
			return nil
		}
		return xerr.Param("提示词端口只收文字节点或反推提示词")
	case HandleImage:
		switch src.Type {
		case "image", "model", "video", "tool":
			return nil
		}
		return xerr.Param("参考图端口只收图片或模型输出")
	case HandleVideo:
		if isVideoModel(src) {
			return nil
		}
		return xerr.Param("参考视频端口只收视频模型的输出")
	case HandleAudio:
		return xerr.Param("音频参考暂未接入")
	}
	return xerr.Param("请连到有编号的输入端口")
}

// checkTarget 目标节点与目标口是否合法（不含数量）。
func checkTarget(src, dst *Node, raw string) (string, error) {
	if src.ID == dst.ID {
		return "", xerr.Param("不能连到自己")
	}
	tryon, result := isTryon(dst), isResultImage(dst)
	toolIn := isInputTool(dst)
	if dst.Type != "model" && !tryon && !result && !toolIn {
		return "", xerr.Param(fmt.Sprintf("节点 %s 没有输入口，只能连到模型节点、工具节点或结果图片", dst.ID))
	}
	if toolIn && raw != "image" {
		return "", xerr.Param("工具节点只有一个图片输入口 image")
	}
	if result && raw != "image-in" {
		return "", xerr.Param("结果图片只有一个输入口")
	}
	if tryon && raw != "garment" && raw != "models" {
		return "", xerr.Param("试穿节点只有服装和模特两个输入口")
	}
	h := NormalizeHandle(raw)
	if h == "" {
		return "", xerr.Param("请连到有编号的输入端口")
	}
	return h, accepts(h, src)
}

// portLimit 某个（目标节点, 计数键）的上限。key 为原始口（试穿 / 结果图 / 视频首次图）或归一口。
func portLimit(dst *Node, key string, imageLimit ImageLimitFunc) (int, string) {
	switch {
	case isInputTool(dst) && key == HandleImage:
		return 1, "工具输入图"
	case isResultImage(dst):
		return 1, "结果图片输入"
	case isTryon(dst) && key == "garment":
		return 1, "服装"
	case isTryon(dst) && key == "models":
		return MaxTryonModels, "模特"
	case key == "image1":
		return 1, "图片 1"
	case key == "image2":
		return 1, "图片 2"
	case key == HandlePrompt:
		return 1, "提示词"
	case key == HandleVideo:
		return 1, "参考视频"
	case key == HandleAudio:
		return 0, "音频参考"
	}
	return imageLimit(dst), "参考图"
}

// ValidateDoc 保存前的整段校验：节点 id、类型、dataURL、连线规则、数量、成环。
func ValidateDoc(d *Doc, maxNodes int, imageLimit ImageLimitFunc) error {
	if imageLimit == nil {
		imageLimit = DefaultImageLimit
	}
	if maxNodes > 0 && len(d.Nodes) > maxNodes {
		return xerr.Param(fmt.Sprintf("节点数超过上限 %d", maxNodes))
	}
	nodes := make(map[string]*Node, len(d.Nodes))
	for i, n := range d.Nodes {
		if strings.TrimSpace(n.ID) == "" {
			return xerr.Param(fmt.Sprintf("第 %d 个节点缺少 id", i+1))
		}
		if _, dup := nodes[n.ID]; dup {
			return xerr.Param("节点 id 重复：" + n.ID)
		}
		if !nodeTypes[n.Type] {
			return xerr.Param(fmt.Sprintf("节点 %s 的类型不支持：%s", n.ID, n.Type))
		}
		nodes[n.ID] = n
	}
	if err := CheckDocInlineMedia(d); err != nil {
		return err
	}
	type pair struct{ src, sh, dst, th string }
	seen := map[pair]bool{}
	counts := map[[2]string]int{} // (target, key) → 条数
	for _, e := range d.Edges {
		src, dst := nodes[e.Source], nodes[e.Target]
		if src == nil || dst == nil {
			return xerr.Param(fmt.Sprintf("连线 %s 引用的节点不存在", e.ID))
		}
		h, err := checkTarget(src, dst, e.TargetHandle)
		if err != nil {
			return wrapEdge(e, err)
		}
		p := pair{e.Source, e.SourceHandle, e.Target, e.TargetHandle}
		if seen[p] {
			return xerr.Param(fmt.Sprintf("连线 %s 重复", e.ID))
		}
		seen[p] = true
		// 试穿 / 结果图 / 视频首次图按原始口计数；其余按归一口。视频首次图同时占参考图名额
		switch {
		case isTryon(dst) || isResultImage(dst):
			counts[[2]string{dst.ID, e.TargetHandle}]++
		default:
			counts[[2]string{dst.ID, h}]++
			if isVideoModel(dst) && (e.TargetHandle == "image1" || e.TargetHandle == "image2") {
				counts[[2]string{dst.ID, e.TargetHandle}]++
			}
		}
	}
	for k, n := range counts {
		dst := nodes[k[0]]
		limit, label := portLimit(dst, k[1], imageLimit)
		if n > limit {
			return xerr.Param(fmt.Sprintf("节点 %s 的%s最多接 %d 条", dst.ID, label, limit))
		}
	}
	if hasCycle(d) {
		return xerr.Param("连线会形成环")
	}
	return nil
}

func wrapEdge(e *Edge, err error) error {
	if code, msg, ok := xerr.Parse(err); ok {
		return xerr.New(code, fmt.Sprintf("连线 %s：%s", e.ID, msg))
	}
	return err
}

// hasCycle 有向图是否有环（三色 DFS）。
func hasCycle(d *Doc) bool {
	adj := map[string][]string{}
	for _, e := range d.Edges {
		adj[e.Source] = append(adj[e.Source], e.Target)
	}
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	var visit func(string) bool
	visit = func(u string) bool {
		color[u] = gray
		for _, v := range adj[u] {
			switch color[v] {
			case gray:
				return true
			case white:
				if visit(v) {
					return true
				}
			}
		}
		color[u] = black
		return false
	}
	for _, n := range d.Nodes {
		if color[n.ID] == white && visit(n.ID) {
			return true
		}
	}
	return false
}

// 这些键是文本内容，不按地址检查（提示词里写了 "data:" 不算内联媒体）。
var textKeys = map[string]bool{"text": true, "prompt": true, "title": true, "subtitle": true, "maskPrompt": true, "resultText": true, "name": true, "label": true}

// IsInlineMedia 是否内联 / 本地地址（data: / blob: / file:），这类地址必须先上传换成 URL。
func IsInlineMedia(s string) bool {
	l := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(l, "data:") || strings.HasPrefix(l, "blob:") || strings.HasPrefix(l, "file:")
}

// inlineIn 递归找内联地址，返回命中的路径（空串 = 没有）。
func inlineIn(path string, v any) string {
	switch x := v.(type) {
	case string:
		if IsInlineMedia(x) {
			return path
		}
	case map[string]any:
		for _, k := range sortedKeys(x) {
			if p := inlineIn(path+"."+k, x[k]); p != "" {
				return p
			}
		}
	case []any:
		for i, y := range x {
			if p := inlineIn(fmt.Sprintf("%s[%d]", path, i), y); p != "" {
				return p
			}
		}
	}
	return ""
}

// CheckNoInlineMedia 检查节点 data 里没有 dataURL：只有 data 第一层的文本键（textKeys）豁免，嵌套层级一律检查。
func CheckNoInlineMedia(nodeID string, data map[string]any) error {
	for _, k := range sortedKeys(data) {
		if _, isText := data[k].(string); isText && textKeys[k] {
			continue
		}
		if p := inlineIn(k, data[k]); p != "" {
			return xerr.Param(fmt.Sprintf("节点 %s 的 %s 不能是 dataURL，请先上传为地址", nodeID, p))
		}
	}
	return nil
}

// checkRawInline 原始 JSON 片段（节点外层、连线、顶层）里不能有内联地址。
func checkRawInline(where string, raw map[string]json.RawMessage, skip ...string) error {
	for k, v := range raw {
		if slices.Contains(skip, k) {
			continue
		}
		var x any
		if json.Unmarshal(v, &x) != nil {
			continue
		}
		if p := inlineIn(k, x); p != "" {
			return xerr.Param(fmt.Sprintf("%s的 %s 不能是 dataURL，请先上传为地址", where, p))
		}
	}
	return nil
}

// CheckDocInlineMedia 整段快照扫描内联地址：节点 data（第一层文本键豁免）、节点外层其它键、连线、顶层其它键（viewport 等）。
func CheckDocInlineMedia(d *Doc) error {
	for _, n := range d.Nodes {
		if err := CheckNoInlineMedia(n.ID, n.Data); err != nil {
			return err
		}
		if err := checkRawInline("节点 "+n.ID+" ", n.raw, "data"); err != nil {
			return err
		}
	}
	for _, e := range d.Edges {
		if err := checkRawInline("连线 "+e.ID+" ", e.raw); err != nil {
			return err
		}
	}
	return checkRawInline("画布顶层", d.raw, "nodes", "edges")
}

// RefInput 执行请求里的一条引用（校验用的最小视图）。
type RefInput struct {
	Handle, NodeID, URL string
}

// RunCheck 执行时的校验参数。
type RunCheck struct {
	NodeID     string
	Kind       string // image / video / tool_*
	Refs       []RefInput
	ImageLimit int // 目标节点的参考图上限
	VideoLimit int // 参考视频上限（0 按 1）
}

// ValidateRun 执行前校验引用：数量上限、不引用自己、不能是 dataURL；doc 里有该节点时再核对来源类型与已保存的连线。
func ValidateRun(doc *Doc, c RunCheck) error {
	counts := map[string]int{}
	for _, r := range c.Refs {
		if r.NodeID != "" && r.NodeID == c.NodeID {
			return xerr.Param("不能引用节点自己")
		}
		if IsInlineMedia(r.URL) {
			return xerr.Param(fmt.Sprintf("引用 %s 的地址不能是 dataURL，请先上传为地址", r.NodeID))
		}
		h := NormalizeHandle(r.Handle)
		if h == "" {
			return xerr.Param("引用的 handle 不支持：" + r.Handle)
		}
		counts[h]++
		if r.Handle != h {
			counts[r.Handle]++
		}
	}
	videoLimit := c.VideoLimit
	if videoLimit <= 0 {
		videoLimit = 1
	}
	imageLimit := c.ImageLimit
	if c.Kind == "tool_tryon" {
		imageLimit = 1 + MaxTryonModels
	}
	switch {
	case counts[HandlePrompt] > 1:
		return xerr.Param("提示词最多接 1 条")
	case counts[HandleImage] > imageLimit:
		return xerr.Param(fmt.Sprintf("参考图最多接 %d 条", imageLimit))
	case counts[HandleVideo] > videoLimit:
		return xerr.Param(fmt.Sprintf("参考视频最多接 %d 条", videoLimit))
	case counts[HandleAudio] > 0:
		return xerr.Param("音频参考暂未接入")
	case counts["image1"] > 1, counts["first"] > 1:
		return xerr.Param("图片 1 最多接 1 条")
	case counts["image2"] > 1, counts["last"] > 1:
		return xerr.Param("图片 2 最多接 1 条")
	case counts["garment"] > 1:
		return xerr.Param("服装最多接 1 条")
	case counts["models"] > MaxTryonModels:
		return xerr.Param(fmt.Sprintf("模特最多接 %d 条", MaxTryonModels))
	}
	if doc == nil {
		return nil
	}
	nodes := doc.NodeByID()
	dst := nodes[c.NodeID]
	if dst == nil {
		return nil // 节点还没保存（自动保存防抖窗口内）：只做数量校验
	}
	linked := map[string]bool{}
	for _, e := range doc.Edges {
		if e.Target == c.NodeID {
			linked[e.Source] = true
		}
	}
	srcRef := strData(dst.Data, "sourceNodeId")
	for _, r := range c.Refs {
		if r.NodeID == "" {
			continue
		}
		src := nodes[r.NodeID]
		if src == nil {
			return xerr.Param(fmt.Sprintf("引用的节点 %s 不存在，请先保存画布", r.NodeID))
		}
		if err := accepts(NormalizeHandle(r.Handle), src); err != nil {
			return err
		}
		if !linked[r.NodeID] && r.NodeID != srcRef {
			return xerr.Param(fmt.Sprintf("节点 %s 与 %s 之间没有连线，请先保存画布", r.NodeID, c.NodeID))
		}
	}
	return nil
}

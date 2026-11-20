package agenttool

// 工具名（canvas-rpc 执行器按它分派）。
const (
	ToolList            = "canvas_list"
	ToolCreate          = "canvas_create"
	ToolGet             = "canvas_get"
	ToolAddNodes        = "canvas_add_nodes"
	ToolUpdateNodes     = "canvas_update_nodes"
	ToolDeleteNodes     = "canvas_delete_nodes"
	ToolMoveNodes       = "canvas_move_nodes"
	ToolResizeNodes     = "canvas_resize_nodes"
	ToolConnect         = "canvas_connect"
	ToolDisconnect      = "canvas_disconnect"
	ToolAutoLayout      = "canvas_auto_layout"
	ToolBatchEdit       = "canvas_batch_edit"
	ToolCatalog         = "canvas_catalog"
	ToolRunNode         = "canvas_run_node"
	ToolGetRun          = "canvas_get_run"
	ToolListRuns        = "canvas_list_runs"
	ToolCancelRun       = "canvas_cancel_run"
	ToolSelectCandidate = "canvas_select_candidate"
	ToolQuoteNode       = "canvas_quote_node"
)

// ToolKinds 工具节点的工具类型（data.tool）。计费路径见 canvas-rpc model.BackendOf：
// i2p 走 gateway（Go 计费）、crop 走媒体服务（不计费）、upscale / outpaint / mask 走 Java（Java 计费，需要用户 Java 登录令牌）、
// tryon 走 studio（studio 计费）。
var ToolKinds = []string{"crop", "upscale", "outpaint", "mask", "i2p", "tryon"}

// 单次批量上限（Schema maxItems 与执行器共用）。
const (
	MaxAddNodes    = 50
	MaxUpdateNodes = 50
	MaxDeleteNodes = 100
	MaxMoveNodes   = 200
	MaxConnect     = 50
	MaxDisconnect  = 100
	MaxBatchOps    = 100
	MaxLayoutNodes = 500
	MaxGetNodeIDs  = 200
	MaxWaitSeconds = 25
)

// NodeTypes 工具层的节点类型（映射到快照 type / data.kind 见 canvas-rpc agent_ops.go）。
var NodeTypes = []string{"text", "image", "result_image", "video", "image_model", "video_model", "tool", "group"}

// TargetHandles 目标口。
var TargetHandles = []string{"prompt", "image", "video", "image1", "image2", "first", "last", "garment", "models", "image-in"}

// BatchOps canvas_batch_edit 的操作类型 → 对应的单元素 Schema（见 batchArgSchema）。
var BatchOps = []string{"add_node", "update_node", "delete_node", "move_node", "resize_node", "connect", "disconnect", "auto_layout"}

// Tool 一个工具。
type Tool struct {
	Name        string
	Title       string
	Description string
	Input       Schema
	Output      Schema
	ReadOnly    bool // 不改任何状态
	Destructive bool // 会删除 / 覆盖内容
	Idempotent  bool // 同参数重复调用结果相同（不含 call_id 时）
	Generates   bool // 会冻结点数（计费）；网关另计生成配额
}

// Mutates 是否写类工具（审计 + 支持 call_id 幂等）。
func (t Tool) Mutates() bool { return !t.ReadOnly }

// ---------------------------------------------------------------- 公共片段

func canvasID() Schema {
	s := str("画布 id（canvas_create / canvas_list 返回的 canvas_id）")
	s["pattern"] = "^cv_[0-9a-f]{16}$"
	return s
}

func baseRevision() Schema {
	return strMax("可选。给了就严格乐观锁：与服务端当前 revision 不符直接返回 CANVAS_CONFLICT；不给则在最新版本上应用（自动重试）", 32)
}

func nodeID() Schema { return strMax("节点 id（canvas_get 返回的 node_id）", 64) }

func nodeRef() Schema {
	return strMax("节点 id；同一次 canvas_batch_edit / canvas_add_nodes 里新建的节点也可以写它的 ref", 64)
}

func mutationOut(extra Props) Schema {
	p := Props{
		"canvas_id":        str("画布 id"),
		"revision":         str("写入后的新 revision"),
		"node_count":       integer("写入后的节点数", 0, 100000),
		"changed_node_ids": arr("本次写入改动到的节点（新增 / 删除 / 内容或位置改动 / 连线两端；≤200）", str("node_id"), 0, 0),
	}
	for k, v := range extra {
		p[k] = v
	}
	return out(p, "canvas_id", "revision")
}

func modelProps(p Props) Props {
	p["model"] = strMax("模型（canvas_catalog 的 model 或 model_id，也接受常见别名）。仅模型节点", 64)
	p["prompt"] = strMax("提示词。仅模型节点", 3000)
	p["ratio"] = strMax("画面比例，如 1:1 / 16:9（须在该模型 ratios 内）", 16)
	p["size"] = strMax("分辨率档位，如 1K / 2K / 720p（须在该模型 sizes 内）", 16)
	p["quality"] = enum("质量档（仅 GPT Image 2.5 / 2.6 等支持画质的模型有效，缺省 high；xhigh 超高、max 极致更贵，用户明确要更高画质时才用；其余模型忽略）",
		"low", "medium", "high", "xhigh", "max")
	p["output_format"] = enum("生成格式（仅图片模型节点；缺省 jpeg，用户要透明底 / 无损时用 png，要更小体积可用 webp；不影响点数；不支持格式参数的模型忽略）",
		"jpeg", "png", "webp")
	p["count"] = integer("一次出几张（视频固定 1）", 1, 8)
	p["duration"] = strMax("视频时长，如 4s / 5s（须在该模型 durations 内）", 8)
	p["sound"] = boolean("视频是否带声音")
	return p
}

// toolParams 工具节点参数的临时覆盖（canvas_run_node / canvas_quote_node）：合并在节点 data 推出的参数之上，不写回节点。
func toolParams() Schema {
	return free("可选，仅工具节点：临时覆盖工具参数（不改节点内容），键为 snake_case。" +
		"crop {shape?:rect, x,y,w,h: 0–1}；upscale {scale: 2|4} 或 {resolution: 1k|2k|4k}；" +
		"outpaint {ratio:\"16:9\", resolution?: 1k|2k|4k} 或 {width,height}；mask（局部重绘）{mask_prompt, bbox:[{x,y}×4，0–1]}；" +
		"i2p {language: zh|en}；tryon {items?:[top|bottom|…], garment_name?, fabric?, model?: 通用|GPT|Banana|Seedream, resolution?: 1k|2k|4k, aspect_ratio?: 3:4}")
}

func nodeSpec() Schema {
	p := Props{
		"ref":       strMax("可选的临时名：同一批里连线可以用它指代这个新节点，返回时给出真实 node_id", 32),
		"type":      enum("节点类型：text 文字 / image 图片素材 / result_image 结果图 / video 视频素材 / image_model 图片生成 / video_model 视频生成 / tool 工具 / group 分组框", NodeTypes...),
		"title":     strMax("节点标题", 100),
		"x":         number("左上角 x（画布坐标，px）；不给自动找空位", -100000, 100000),
		"y":         number("左上角 y", -100000, 100000),
		"width":     number("宽（px）；不给按类型默认", 80, 4000),
		"height":    number("高（px）", 80, 4000),
		"text":      strMax("文字节点内容", 5000),
		"image_url": strMax("图片素材地址（http/https，必须是已上传的地址，不能是 dataURL）", 2048),
		"video_url": strMax("视频素材地址（http/https）", 2048),
		"tool": enum("工具类型（type=tool 时必填，其它类型不要给）。crop 裁剪；upscale 高清放大；outpaint 扩图（改比例补画面）；"+
			"mask 局部重绘；i2p 反推提示词（图片 → 文字）；tryon 模特试穿 / 换装（服装图连到它的 garment 口，模特图连到 models 口，"+
			"一件服装可配多张模特）。前四个与 i2p 的输入图都连到 image 口", ToolKinds...),
		"data": free("可选：额外写入节点 data 的键（前端 camelCase 原样）；运行态键会被忽略"),
	}
	s := obj("要新建的节点", modelProps(p), "type")
	// 条件约束：type=tool 必须给 tool；给了 tool 必须是 type=tool（与执行器校验一致，让模型在调用前就知道）
	s["allOf"] = []any{
		map[string]any{
			"if":   map[string]any{"properties": map[string]any{"type": map[string]any{"const": "tool"}}, "required": []any{"type"}},
			"then": map[string]any{"required": []any{"tool"}},
		},
		map[string]any{
			"if":   map[string]any{"required": []any{"tool"}},
			"then": map[string]any{"properties": map[string]any{"type": map[string]any{"const": "tool"}}},
		},
	}
	return s
}

func nodePatch() Schema {
	p := Props{
		"node_id":   nodeRef(),
		"title":     strMax("新标题", 100),
		"text":      strMax("文字节点内容", 5000),
		"image_url": strMax("图片素材地址（http/https）", 2048),
		"video_url": strMax("视频素材地址（http/https）", 2048),
		"data":      free("JSON Merge Patch 合并进节点 data：值为 null 删除该键；运行态键（status / candidates / runId …）忽略"),
	}
	return obj("对一个节点的修改", modelProps(p), "node_id")
}

func moveSpec() Schema {
	return obj("移动一个节点：给 x/y 为绝对坐标，给 dx/dy 为相对位移（可混用）", Props{
		"node_id": nodeRef(),
		"x":       number("新的 x", -100000, 100000),
		"y":       number("新的 y", -100000, 100000),
		"dx":      number("x 位移", -100000, 100000),
		"dy":      number("y 位移", -100000, 100000),
	}, "node_id")
}

func resizeSpec() Schema {
	return obj("改一个节点的尺寸：给 width/height 为绝对值，或给 scale 按当前尺寸缩放", Props{
		"node_id": nodeRef(),
		"width":   number("新宽（px）", 80, 4000),
		"height":  number("新高（px）", 80, 4000),
		"scale":   number("按当前尺寸缩放的倍数", 0.1, 10),
	}, "node_id")
}

func edgeSpec() Schema {
	return obj("一条连线：source 的输出接到 target 的输入口", Props{
		"source":        nodeRef(),
		"target":        nodeRef(),
		"target_handle": enum("目标口；不给自动推断（文字 / 反推 → prompt，工具 → image，结果图 → image-in，视频模型输出 → video，其余 → image；试穿必须显式 garment / models）", TargetHandles...),
		"source_handle": strMax("源口；不给按源节点类型推断（text / image / video / image-out，多候选可写 candidate-N）", 32),
		"order":         integer("同一输入口内的顺序（1 起）；不给排到最后", 1, 100),
	}, "source", "target")
}

func disconnectPair() Schema {
	return obj("按端点断开：source → target（可限定 target_handle）之间的连线", Props{
		"source":        nodeID(),
		"target":        nodeID(),
		"target_handle": enum("只断这个目标口的线", TargetHandles...),
	}, "source", "target")
}

func layoutProps() Props {
	return Props{
		"node_ids":  arr("只排这些节点（不给 = 全部，分组框不参与）", nodeID(), 1, MaxLayoutNodes),
		"direction": enum("方向：LR 从左到右（默认）/ TB 从上到下", "LR", "TB"),
		"gap_x":     integer("列间距（px，默认 120）", 20, 800),
		"gap_y":     integer("行间距（px，默认 80）", 20, 800),
		"origin_x":  number("排版起点 x（默认取被排节点的最左上角）", -100000, 100000),
		"origin_y":  number("排版起点 y", -100000, 100000),
	}
}

func nodeOut() Schema {
	return out(Props{
		"node_id": str("节点 id"),
		"type":    enum("节点类型", NodeTypes...),
		"title":   str("标题"),
		"x":       Schema{"type": "number"}, "y": Schema{"type": "number"},
		"width": Schema{"type": "number"}, "height": Schema{"type": "number"},
		"data": free("前端 data 原样（含运行态回填：status / candidates / image / video …）"),
	}, "node_id", "type")
}

func edgeOut() Schema {
	return out(Props{
		"edge_id": str("连线 id"), "source": str("源节点"), "source_handle": str("源口"),
		"target": str("目标节点"), "target_handle": str("目标口"), "order": Schema{"type": "integer"},
	}, "edge_id", "source", "target")
}

func runOut() Schema {
	return out(Props{
		"request_id":     str("run 主键（查询 / 取消用）"),
		"run_id":         str("幂等键"),
		"canvas_id":      str("画布"),
		"node_id":        str("节点"),
		"kind":           str("image / video / tool_*"),
		"status":         enum("状态", "submitting", "queued", "running", "succeeded", "failed", "cancelled"),
		"progress":       integer("进度 0–100（运行中为估算）", 0, 100),
		"model":          str("模型"),
		"candidates":     arr("候选产物", out(Props{"url": str("地址（可能是 10 分钟现签地址）"), "width": Schema{"type": "integer"}, "height": Schema{"type": "integer"}}, "url"), 0, 0),
		"selected_index": Schema{"type": "integer"},
		"video_url":      str("视频地址"),
		"text":           str("反推文本"),
		"credits":        Schema{"type": "integer", "description": "冻结 / 扣除的点数；失败退款后为 0"},
		"error":          str("失败原因"),
		"error_code":     str("失败码"),
		"retryable":      boolean("换一个 run_id 重试是否可能成功"),
		"library_status": str("空=已在素材库或不适用；pending=转存中；failed=转存失败"),
		"created_at":     Schema{"type": "integer"},
		"finished_at":    Schema{"type": "integer"},
	}, "request_id", "status")
}

// ---------------------------------------------------------------- 清单

var registry = buildRegistry()

func buildRegistry() []Tool {
	return []Tool{
		{
			Name: ToolList, Title: "列出我的画布", ReadOnly: true, Idempotent: true,
			Description: "列出当前用户自己的画布（按最近修改倒序），返回 canvas_id、标题、revision、节点数。",
			Input: obj("", Props{
				"page":      integer("页码，从 1 起", 1, 10000),
				"page_size": integer("每页条数（默认 20）", 1, 50),
			}),
			Output: out(Props{
				"total": Schema{"type": "integer"},
				"canvases": arr("画布", out(Props{
					"canvas_id": str("画布 id"), "title": str("标题"), "revision": str("版本"),
					"node_count": Schema{"type": "integer"}, "cover_url": str("封面"), "updated_at": Schema{"type": "integer"},
				}, "canvas_id"), 0, 0),
			}, "total", "canvases"),
		},
		{
			Name: ToolCreate, Title: "新建画布",
			Description: "新建一张空画布（属于当前用户），返回 canvas_id 与 revision。",
			Input:       obj("", Props{"title": strMax("标题（不给 = 未命名画布）", 100)}),
			Output:      mutationOut(Props{"title": str("标题")}),
		},
		{
			Name: ToolGet, Title: "读取画布", ReadOnly: true, Idempotent: true,
			Description: "读取画布完整结构：节点（类型、位置、尺寸、标题、data，含最近一次生成的状态与结果回填）与连线，以及当前 revision。" +
				"连线 source_handle/target_handle 决定数据流向：文字 → 模型 prompt 口，图片 / 模型输出 → 模型 image 口。",
			Input: obj("", Props{
				"canvas_id":       canvasID(),
				"node_ids":        arr("只返回这些节点（以及它们之间的连线）；不给 = 全部", nodeID(), 1, MaxGetNodeIDs),
				"include_runtime": boolean("是否回填生成状态与结果（默认 true）"),
			}, "canvas_id"),
			Output: out(Props{
				"canvas_id": str("画布 id"), "title": str("标题"), "revision": str("版本"),
				"node_count": Schema{"type": "integer"},
				"nodes":      arr("节点", nodeOut(), 0, 0),
				"edges":      arr("连线", edgeOut(), 0, 0),
			}, "canvas_id", "revision", "nodes", "edges"),
		},
		{
			Name: ToolAddNodes, Title: "添加节点",
			Description: "在画布上批量添加节点（一次最多 50 个）。图片 / 视频生成节点用 image_model / video_model，可直接带 model、prompt、ratio、size 等参数；" +
				"文字节点带 text；图片素材带 image_url（须是已上传的 http 地址）。不给坐标会自动放到空位。返回每个新节点的 node_id。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"nodes":         arr("要新建的节点", nodeSpec(), 1, MaxAddNodes),
			}, "canvas_id", "nodes"),
			Output: mutationOut(Props{"nodes": arr("新节点", out(Props{
				"ref": str("请求里的 ref"), "node_id": str("真实 id"), "type": str("类型"),
				"x": Schema{"type": "number"}, "y": Schema{"type": "number"}, "width": Schema{"type": "number"}, "height": Schema{"type": "number"},
			}, "node_id"), 0, 0)}),
		},
		{
			Name: ToolUpdateNodes, Title: "修改节点",
			Description: "批量修改节点内容（一次最多 50 个）：标题、文字、素材地址、模型参数（model / prompt / ratio / size / quality / output_format / count / duration / sound），" +
				"或用 data 做合并补丁。位置与尺寸请用 canvas_move_nodes / canvas_resize_nodes。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"updates":       arr("修改项", nodePatch(), 1, MaxUpdateNodes),
			}, "canvas_id", "updates"),
			Output: mutationOut(Props{"updated": arr("已修改的节点", str("node_id"), 0, 0)}),
		},
		{
			Name: ToolDeleteNodes, Title: "删除节点", Destructive: true, Idempotent: true,
			Description: "批量删除节点（一次最多 100 个），与这些节点相连的连线一并删除。已生成的结果仍保留在生成记录与素材库中。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"node_ids":      arr("要删除的节点 id", nodeID(), 1, MaxDeleteNodes),
			}, "canvas_id", "node_ids"),
			Output: mutationOut(Props{
				"deleted_nodes": arr("已删除的节点", str("node_id"), 0, 0),
				"deleted_edges": arr("连带删除的连线", str("edge_id"), 0, 0),
			}),
		},
		{
			Name: ToolMoveNodes, Title: "移动节点",
			Description: "批量移动节点（一次最多 200 个）：x/y 绝对坐标或 dx/dy 相对位移。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"moves":         arr("移动项", moveSpec(), 1, MaxMoveNodes),
			}, "canvas_id", "moves"),
			Output: mutationOut(Props{"nodes": arr("新位置", out(Props{"node_id": str("节点"), "x": Schema{"type": "number"}, "y": Schema{"type": "number"}}, "node_id"), 0, 0)}),
		},
		{
			Name: ToolResizeNodes, Title: "缩放节点",
			Description: "批量调整节点尺寸（一次最多 200 个）：width/height 绝对值或 scale 倍数，范围 80–4000px。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"sizes":         arr("缩放项", resizeSpec(), 1, MaxMoveNodes),
			}, "canvas_id", "sizes"),
			Output: mutationOut(Props{"nodes": arr("新尺寸", out(Props{"node_id": str("节点"), "width": Schema{"type": "number"}, "height": Schema{"type": "number"}}, "node_id"), 0, 0)}),
		},
		{
			Name: ToolConnect, Title: "连线",
			Description: "批量连线（一次最多 50 条）。规则：prompt 口只收文字节点或反推工具；image 口收图片 / 模型 / 工具输出（不超过模型参考图上限）；" +
				"video 口（参考视频）收视频素材节点（type=video）或视频模型输出，视频素材只能连 video 口、不能连 image / image1 / image2；image1/image2 为视频首帧 / 尾帧；不能连自己、不能成环。单槽口（prompt、image1、image2、video、garment、image-in、工具输入）已有连线时自动替换。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"edges":         arr("要新建的连线", edgeSpec(), 1, MaxConnect),
			}, "canvas_id", "edges"),
			Output: mutationOut(Props{
				"edges":    arr("新连线", edgeOut(), 0, 0),
				"replaced": arr("被替换掉的旧连线", str("edge_id"), 0, 0),
			}),
		},
		{
			Name: ToolDisconnect, Title: "断开连线", Destructive: true, Idempotent: true,
			Description: "按连线 id 或按端点（source → target，可限定 target_handle）断开连线，一次最多 100 条。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"edge_ids":      arr("连线 id", strMax("edge_id", 64), 1, MaxDisconnect),
				"pairs":         arr("按端点断开", disconnectPair(), 1, MaxDisconnect),
			}, "canvas_id"),
			Output: mutationOut(Props{"removed": arr("已断开的连线", str("edge_id"), 0, 0)}),
		},
		{
			Name: ToolAutoLayout, Title: "自动排版", Idempotent: true,
			Description: "按数据流向自动整理节点位置：上游在左（或上）、下游在右（或下），同层按原有顺序排列，避免重叠。可只排一部分节点。分组框不参与。",
			Input: func() Schema {
				p := layoutProps()
				p["canvas_id"] = canvasID()
				p["base_revision"] = baseRevision()
				return obj("", p, "canvas_id")
			}(),
			Output: mutationOut(Props{"nodes": arr("新位置", out(Props{"node_id": str("节点"), "x": Schema{"type": "number"}, "y": Schema{"type": "number"}}, "node_id"), 0, 0)}),
		},
		{
			Name: ToolBatchEdit, Title: "批量编辑", Destructive: true,
			Description: "把多种编辑一次原子执行（最多 100 步，全部成功才保存，产生一个 revision）。每步 {op, args}，args 的字段、取值与规则和对应单工具完全相同：" +
				"add_node 的 args = canvas_add_nodes 参数 nodes 里的一个元素（可带 ref，工具节点要同时给 type=tool 与 tool）；" +
				"update_node = canvas_update_nodes 的 updates 里一个元素；delete_node = {node_id}；move_node = canvas_move_nodes 的 moves 里一个元素；" +
				"resize_node = canvas_resize_nodes 的 sizes 里一个元素；connect = canvas_connect 的 edges 里一个元素（source/target 可写本批的 ref）；" +
				"disconnect = {edge_id} 或 {source,target,target_handle}；auto_layout = canvas_auto_layout 去掉 canvas_id。",
			Input: obj("", Props{
				"canvas_id":     canvasID(),
				"base_revision": baseRevision(),
				"ops": arr("按顺序执行的操作", obj("一步操作", Props{
					"op":   enum("操作类型", BatchOps...),
					"args": free("该操作的参数（形状见工具说明）"),
				}, "op", "args"), 1, MaxBatchOps),
			}, "canvas_id", "ops"),
			Output: mutationOut(Props{
				"results": arr("每步结果", free("该步结果"), 0, 0),
				"refs":    free("ref → node_id"),
			}),
		},
		{
			Name: ToolCatalog, Title: "可用模型", ReadOnly: true, Idempotent: true,
			Description: "列出画布可用的图片 / 视频模型及其可选比例、分辨率、质量、时长、最多张数、参考图上限与点数价目。",
			Input:       obj("", Props{"kind": enum("只看某类", "image", "video")}),
			Output: out(Props{"models": arr("模型", out(Props{
				"model": str("节点 data.model 的取值"), "model_id": str("模型 id"), "label": str("展示名"), "kind": str("image / video"),
				"ratios": arr("", str(""), 0, 0), "sizes": arr("", str(""), 0, 0), "qualities": arr("", str(""), 0, 0),
				"durations": arr("", str(""), 0, 0), "sound": Schema{"type": "boolean"}, "count_max": Schema{"type": "integer"},
				"max_image_refs": Schema{"type": "integer"},
				"prices":         arr("价目", out(Props{"size": str(""), "duration": str(""), "points": Schema{"type": "integer"}}), 0, 0),
			}, "model", "kind"), 0, 0)}, "models"),
		},
		{
			Name: ToolRunNode, Title: "执行生成 / 工具节点", Generates: true,
			Description: "对一个图片生成（image_model）、视频生成（video_model）或工具（tool：裁剪 / 高清放大 / 扩图 / 局部重绘 / 反推提示词 / 模特试穿）节点发起执行：" +
				"按节点已保存的参数与连线（文字 → 提示词、上游图片 → 参考图 / 工具输入）组装请求，" +
				"可临时覆盖 model / prompt / ratio / size / quality / output_format / count / duration / sound 与工具参数 params（不改节点内容）。" +
				"计费按节点原链路：图片 / 视频 / 反推由画布冻结点数，放大 / 扩图 / 局部重绘由主站按其价目扣点（需要你在网页登录过），试穿由商拍服务扣点，裁剪不计费；失败或取消自动退回。" +
				"执行前先用 canvas_quote_node 报价并告诉用户点数。返回 request_id，用 canvas_get_run 查询结果；成功后结果会回填到节点上。",
			Input: obj("", modelProps(Props{
				"canvas_id": canvasID(),
				"node_id":   nodeID(),
				"run_id":    strMax("可选幂等键（重试时传同一个，不会重复扣点）；不给时由 call_id 派生", 64),
				"params":    toolParams(),
				"max_points": integer("可选：本次最多愿意花的点数。冻结前按与冻结同源的报价比较，超出则不执行、返回 BUDGET_EXCEEDED（带 quoted_points）；"+
					"报不出准确点数的（canvas_quote_node 的 price_known=false，如放大 / 扩图 / 局部重绘）须同时给 expected_points，否则返回 PRICE_UNKNOWN_NEEDS_CONFIRM", 0, 1000000),
				"expected_points": integer("可选：price_known=false 的工具由调用方给出的估价（点数），与 max_points 比较；price_known=true 时忽略", 0, 1000000),
			}), "canvas_id", "node_id"),
			Output: out(Props{
				"request_id":  str("run 主键"),
				"run_id":      str("幂等键"),
				"status":      str("submitting / queued / running"),
				"credits":     Schema{"type": "integer", "description": "冻结的点数（画布计费的图片 / 视频 / 反推；放大 / 扩图 / 重绘 / 试穿由对应服务计费，这里为 0，实际扣点见对应服务）"},
				"max_points":  Schema{"type": "integer", "description": "本次最多扣多少点"},
				"settle_mode": str("FIXED 按冻结扣 / ACTUAL_COST 按实际用量"),
				"warnings":    arr("参数归一告警", str(""), 0, 0),
				"replayed":    boolean("是否幂等重放"),
			}, "request_id", "status"),
		},
		{
			Name: ToolQuoteNode, Title: "生成前报价", ReadOnly: true, Idempotent: true,
			Description: "只读：报出对某个节点执行 canvas_run_node 预计会扣多少点（不冻结、不提交）。可带与 canvas_run_node 相同的临时覆盖参数" +
				"（model / ratio / size / quality / output_format / count / duration / sound / params）。返回预计点数、计价依据（模型 / 档位 / 张数 / 时长 / 单价）、" +
				"由谁计费（canvas 画布 / java 主站 / studio 商拍 / free 不计费）、是否需要网页登录令牌与预计耗时。" +
				"available=false 时 reason 说明为什么现在不能执行；price_known=false 表示这次报不出准确点数（以执行时实际冻结为准，执行前要先征得用户同意）。",
			Input: obj("", modelProps(Props{
				"canvas_id": canvasID(),
				"node_id":   nodeID(),
				"params":    toolParams(),
			}), "canvas_id", "node_id"),
			Output: out(Props{
				"canvas_id":   str("画布"),
				"node_id":     str("节点"),
				"node_type":   enum("节点类型", "image_model", "video_model", "tool"),
				"tool":        str("工具类型（工具节点）"),
				"kind":        str("执行类型：image / video / tool_crop / tool_upscale / tool_outpaint / tool_inpaint / tool_i2p / tool_tryon"),
				"available":   boolean("此刻能否执行（参数齐、服务就绪、来源允许）"),
				"reason":      str("available=false 的原因（给用户看的短句）"),
				"reason_code": str("available=false 的原因码（如 MCP_TOKEN_JAVA_TOOL_UNSUPPORTED / LOGIN_REQUIRED / NOT_READY / PARAM_INVALID / BILLING_NO_FEATURE）"),
				"billing":     enum("由谁计费", "canvas", "java", "studio", "free"),
				"price_known": boolean("是否报出了准确点数"),
				"points":      Schema{"type": "integer", "description": "预计冻结 / 扣除的点数（price_known=false 时为 0）；与执行时冻结同一口径"},
				"max_points":  Schema{"type": "integer", "description": "本次最多扣多少点（ACTUAL_COST 按实际用量结算时 points 即上限）"},
				"settle_mode": str("FIXED 按冻结扣 / ACTUAL_COST 按实际用量结算（最多 max_points）"),
				"charged":     boolean("执行是否会真的扣点（计费开关 on）"),
				"basis": out(Props{
					"model":       str("模型（展示名）"),
					"model_group": str("模型组"),
					"size":        str("分辨率档"),
					"quality":     str("质量档"),
					"ratio":       str("比例"),
					"count":       Schema{"type": "integer", "description": "张数 / 件数"},
					"duration_s":  Schema{"type": "integer", "description": "视频秒数"},
					"unit_points": Schema{"type": "integer", "description": "单价（每张 / 每秒 / 每件）"},
					"price_unit":  str("计价单位：PER_IMAGE / PER_SECOND / PER_REQUEST …"),
					"quantity":    Schema{"type": "integer", "description": "计价份数（张数或秒数）"},
				}),
				"requires_login_token": boolean("是否需要用户的网页登录令牌（放大 / 扩图 / 局部重绘由主站执行）"),
				"login_available":      boolean("需要登录令牌时，此刻是否拿得到（拿不到执行会失败：请用户在网页登录后再试）"),
				"estimated_seconds":    Schema{"type": "integer", "description": "预计耗时（秒）"},
				"estimate_source":      str("预计耗时来源：recent_median 近期同类成功的中位数 / default 经验值"),
				"warnings":             arr("参数归一告警（如 4K → 2K）", str(""), 0, 0),
				"note":                 str("补充说明"),
				"java_request":         free("主站计费的工具：执行时会发给主站的请求 {route, body}（主站据此报价）"),
			}, "available", "billing", "price_known", "points"),
		},
		{
			Name: ToolGetRun, Title: "查询生成", ReadOnly: true, Idempotent: true,
			Description: "查询一次生成的状态、进度与结果（候选图地址、视频地址、失败原因）。wait_seconds>0 时最多等这么久直到进入终态。",
			Input: obj("", Props{
				"request_id":   Schema{"type": "string", "description": "canvas_run_node 返回的 request_id", "pattern": "^[0-9]{1,20}$"},
				"wait_seconds": integer("最多等待秒数（0 = 立即返回）", 0, MaxWaitSeconds),
			}, "request_id"),
			Output: runOut(),
		},
		{
			Name: ToolListRuns, Title: "生成记录", ReadOnly: true, Idempotent: true,
			Description: "列出画布（或某个节点）最近的生成记录，新的在前。",
			Input: obj("", Props{
				"canvas_id": canvasID(),
				"node_id":   nodeID(),
				"limit":     integer("条数（默认 20）", 1, 50),
			}, "canvas_id"),
			Output: out(Props{"runs": arr("生成记录", runOut(), 0, 0)}, "runs"),
		},
		{
			Name: ToolCancelRun, Title: "取消生成", Destructive: true, Idempotent: true,
			Description: "取消一次排队中或运行中的生成，冻结的点数按规则退回。",
			Input: obj("", Props{
				"request_id": Schema{"type": "string", "description": "request_id", "pattern": "^[0-9]{1,20}$"},
			}, "request_id"),
			Output: runOut(),
		},
		{
			Name: ToolSelectCandidate, Title: "选中候选", Idempotent: true,
			Description: "一次成功生成有多张候选时，选中第 index 张（0 起）：节点展示与下游引用以它为准。",
			Input: obj("", Props{
				"canvas_id": canvasID(),
				"node_id":   nodeID(),
				"run_id":    strMax("该节点某次成功生成的 run_id", 64),
				"index":     integer("候选下标（0 起）", 0, 16),
			}, "canvas_id", "node_id", "run_id", "index"),
			Output: runOut(),
		},
	}
}

// Tools 全部工具（固定顺序，返回副本）。
func Tools() []Tool { return append([]Tool(nil), registry...) }

// Find 按名查工具。
func Find(name string) (Tool, bool) {
	for _, t := range registry {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// batchArgSchemas canvas_batch_edit 每种 op 的 args Schema（执行器逐步校验）。
var batchArgSchemas = map[string]Schema{
	"add_node":    nodeSpec(),
	"update_node": nodePatch(),
	"delete_node": obj("删除一个节点", Props{"node_id": nodeRef()}, "node_id"),
	"move_node":   moveSpec(),
	"resize_node": resizeSpec(),
	"connect":     edgeSpec(),
	"disconnect": obj("断开一条连线：给 edge_id，或给 source + target（可加 target_handle）", Props{
		"edge_id": strMax("连线 id", 64), "source": nodeRef(), "target": nodeRef(), "target_handle": enum("目标口", TargetHandles...),
	}),
	"auto_layout": obj("自动排版", layoutProps()),
}

// BatchArgSchema 某个 batch op 的 args Schema。
func BatchArgSchema(op string) (Schema, bool) {
	s, ok := batchArgSchemas[op]
	return s, ok
}

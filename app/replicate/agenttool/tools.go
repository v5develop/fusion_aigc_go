package agenttool

// 工具名（内部入口按它分派；Java 目录 AgentToolCatalogue 登记同一批名字，启动探测校验两边一致）。
const (
	ToolLimits         = "replicate_limits"
	ToolAnalyze        = "replicate_analyze"
	ToolGetAnalysis    = "replicate_get_analysis"
	ToolListModels     = "replicate_list_models"
	ToolListSamples    = "replicate_list_samples"
	ToolTrySample      = "replicate_try_sample"
	ToolCreatePlan     = "replicate_create_plan"
	ToolUpdatePlan     = "replicate_update_plan"
	ToolGetPlan        = "replicate_get_plan"
	ToolQuote          = "replicate_quote"
	ToolConfirm        = "replicate_confirm"
	ToolGetJob         = "replicate_get_job"
	ToolListJobs       = "replicate_list_jobs"
	ToolContinue       = "replicate_continue"
	ToolRedoSegment    = "replicate_redo_segment"
	ToolStop           = "replicate_stop"
	ToolCancelRest     = "replicate_cancel_rest"
	ToolPreviewFrame   = "replicate_preview_frame"
	ToolQuoteBatch     = "replicate_quote_batch"
	ToolConfirmBatch   = "replicate_confirm_batch"
	ToolGetBatch       = "replicate_get_batch"
	ToolPublishCopy    = "replicate_publish_copy"
	ToolExportRatio    = "replicate_export_ratio"
	ToolListVoices     = "replicate_list_voices"
	ToolRetryAudio     = "replicate_retry_audio"
	ToolTrending       = "replicate_trending"
	ToolShareSample    = "replicate_share_sample"
	ToolWithdrawSample = "replicate_withdraw_sample"
	NoteMaxRunes       = 200
	maxRefImages       = 9
	maxOutfits         = 20 // 一项逐套换装最多列几套
	maxBatchVariants   = 10
	maxTexts           = 4
	maxTextRunes       = 30
	maxVoiceLines      = 80
	maxVoiceLineRune   = 200
	maxDimNoteRunes    = 500
	maxAttachOrdinal   = 32
)

// DimKeys 方案维度（与 core.DimKeys 一致，测试里对账）。
var DimKeys = []string{"model", "garment", "accessory", "product", "motion", "expression", "line", "camera", "light", "scene", "style"}

// Tool 一个工具。
type Tool struct {
	Name        string
	Title       string
	Description string
	Input       Schema
	Output      Schema
	ReadOnly    bool // 不改任何状态
	Destructive bool // 会停止 / 取消在途任务
	Idempotent  bool // 同参数重复调用结果相同
	// Charges 会冻结 / 扣用户点数：expected_points 必填且 >0（CONTRACT §A），调用方须先过同意门
	Charges bool
}

// Mutates 是否写类工具。
func (t Tool) Mutates() bool { return !t.ReadOnly }

// Tools 全部工具（稳定顺序）。
func Tools() []Tool { return append([]Tool(nil), registry...) }

// Find 按名字找工具。
func Find(name string) (Tool, bool) {
	for _, t := range registry {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

var registry = buildRegistry()

// ---------------------------------------------------------------- 公共片段与规则文案

const (
	// 系统注入字段的统一说明：模型绝不能自己写。
	sysFilled = "由系统填写，你不要自己写、不要估算"

	ruleNoPoll = "不要轮询：状态没到终态就结束本轮告诉用户在进行中，系统会在完成 / 失败时自动通知你开新一轮。"

	ruleCharge = "【扣点】会冻结并扣用户的点数，只有在用户明确同意本次点数之后才会真正执行（系统会弹出同意卡让用户确认金额）；" +
		"plan_hash 与 expected_points 由系统按当前报价填写，你绝不能编造、猜测或自己计算；价格变了会返回 QUOTE_CHANGED（没有扣点），" +
		"此时重新取最新方案 / 任务视图，把新价格告诉用户再次征得同意。同一次调用重放（同 call_id）只会返回已有结果，不会重复扣点。"
)

func analysisNo() Schema {
	return strMax("分析编号（replicate_analyze 返回的 analysisNo）", 64)
}

func planNo() Schema {
	return strMax("方案编号（replicate_create_plan / replicate_update_plan 返回的 planNo）。改过方案会得到新编号，旧编号作废，永远用最新的", 64)
}

func jobID() Schema {
	return Schema{"type": "integer", "description": "任务编号（replicate_confirm 返回的 jobId）", "minimum": 1}
}

func modelGroup() Schema {
	return strMax("出片模型组 code（取 replicate_list_models 返回的 code）；不填用推荐模型", 64)
}

func resolution() Schema {
	return enum("分辨率：480p / 720p（默认）/ 1080p；越高越贵", "480p", "720p", "1080p")
}

func aspectRatio() Schema {
	return enum("画幅：16:9 / 9:16 / 1:1 / 4:3 / 3:4 / 21:9；不填跟随原片", "16:9", "9:16", "1:1", "4:3", "3:4", "21:9")
}

func refImagesFrom(action string) Schema {
	return arr("参考图：用户消息里的图片附件序号数组，如 [\"图1\",\"图2\"]，最多 9 张，顺序即图号。"+action+
		"系统会把序号换成真实地址；你只写序号，不要写地址。dims 里的 refs 指的是这个数组里的位置（从 1 起）：[\"图3\",\"图1\"] 时 refs:[1] 是图3、refs:[2] 是图1",
		strMax("附件序号，如 图1", maxAttachOrdinal), 0, maxRefImages)
}

func refImagesInjected() Schema {
	return arr("真实参考图地址（"+sysFilled+"，系统按 ref_images_from 填入；模型只写 ref_images_from）",
		strMax("参考图地址或 mr://rid", 1024), 0, maxRefImages)
}

// dimEdit 一个维度的改法。
func dimEdit() Schema {
	return obj("这一维度的改法", Props{
		"mode": enum("keep 保持原样 / replace 换成参考图里的（须配 refs）/ describe 按文字描述改（须配 note）", "keep", "replace", "describe"),
		"note": strMax("describe 时写要改成什么（用户口径的一句话，如「改成红色连衣裙」）；replace 时可补充说明", maxDimNoteRunes),
		"refs": arr("replace 时用哪几张参考图（ref_images_from 数组里的位置，从 1 起）", integer("位置序号", 1, maxRefImages), 0, maxRefImages),
		"occurrences": arr("逐套换装：这一项在视频里出现多次（如服装换了 3 套，见分析结果 outfits）时，指定第几套换成哪张图；没列出的套保持原样。"+
			"只在 mode=replace 且分析里这一项有 2 套以上时用；用了它就不要再写 refs。只想把整条视频的服装都换成同一张图时不要用它，直接写 refs",
			obj("某一套的换法", Props{
				"index": integer("第几套（从 1 起，与分析结果 outfits 的 index 一致）", 1, maxOutfits),
				"refs":  arr("这一套用哪几张参考图（ref_images_from 数组里的位置，从 1 起）", integer("位置序号", 1, maxRefImages), 1, maxRefImages),
			}, "index", "refs"), 0, maxOutfits),
	}, "mode")
}

// voiceSchema 换声音设置（只在 audio=revoice 时用）。
func voiceSchema() Schema {
	return obj("换声音设置（只在 audio=revoice 时填）：音色、语速，以及要改写的台词。台词原文不会给你看，改台词由用户在表单里逐句填写，你不要自己编台词", Props{
		"voiceId": Schema{"type": "integer", "description": "音色编号（replicate_list_voices 返回的 voiceId，不要自己编）", "minimum": 1},
		"speed":   number("整体语速 0.5~1.5，不填 = 正常语速", 0.5, 1.5),
		"lines": arr("逐句改台词：用户填写的改动。index 从 1 起，与分析结果 lines 的顺序一致；没列出的句沿用原片台词。每句字数不能超过该句的上限（约每秒 5 个字）",
			obj("改一句", Props{
				"index": integer("第几句（从 1 起）", 1, maxVoiceLines),
				"text":  strMax("这一句改成什么（用户写的）", maxVoiceLineRune),
			}, "index", "text"), 0, maxVoiceLines),
	}, "voiceId")
}

func textOverlay() Schema {
	return obj("成片上叠加的文字（店名 / 价格 / 标题）：生成后由系统叠上去，不交给 AI 画，不额外花生成点数", Props{
		"text":  strMax("文字内容，每处最多 30 字", maxTextRunes),
		"kind":  enum("文字类型", "shop_name", "price", "title", "other"),
		"box":   obj("位置（相对画面 0~1）；不给默认在底部居中", Props{"x": number("左", 0, 1), "y": number("上", 0, 1), "w": number("宽", 0, 1), "h": number("高", 0, 1)}, "x", "y", "w", "h"),
		"t0":    number("起始秒（成片时间轴）；t0 / t1 都不给 = 全程", 0, 3600),
		"t1":    number("结束秒", 0, 3600),
		"color": strMax("颜色 #RRGGBB，默认白色", 7),
	}, "text")
}

// annotationsSchema 用户想怎么改。
func annotationsSchema(action string) Schema {
	dims := Props{}
	for _, k := range DimKeys {
		dims[k] = dimEdit()
	}
	return obj(action+"用户想怎么改原视频。dims 的键是十一个维度："+
		"model 人物 / garment 服装 / accessory 配饰 / product 商品 / motion 动作 / expression 表情 / line 台词 / camera 镜头 / light 光线 / scene 场景 / style 风格；"+
		"没写的维度保持原样。要把商品 / 人物 / 服装换成用户自己的，必须给 mode=replace 并配参考图 refs；mode=describe 必须写 note。"+
		"audio：regenerate 重新配乐（默认）/ keep 保留原声 / mute 静音 / revoice 换个声音（用原片台词按所选音色重新配音，须配 voice；口型可能对不上，要告诉用户）。texts：成片叠字，≤4 处",
		Props{
			"dims":  obj("各维度的改法（只写要改的）", dims),
			"audio": enum("声音处理", "regenerate", "keep", "mute", "revoice"),
			"texts": arr("成片叠字", textOverlay(), 0, maxTexts),
			"voice": voiceSchema(),
		})
}

// batchVariants 批量的变体数组：每项 {ref_images_from（模型写）, ref_images（系统注入）}。
func batchVariants() Schema {
	return arr("变体：每一项 = 一条要做的视频，ref_images_from 写这一条用的参考图（用户消息里的图片附件序号，如 [\"图2\"]；张数 = 方案里被替换那一项原来引用的图数，"+
		"一般 1 张）。系统会换成真实地址，你只写序号。最多 10 条，顺序即第 1、2… 条",
		obj("一个变体", Props{
			"ref_images_from": arr("这一条的参考图：图片附件序号数组", strMax("附件序号，如 图1", maxAttachOrdinal), 1, maxRefImages),
			"ref_images":      refImagesInjected(),
		}, "ref_images_from"), 1, maxBatchVariants)
}

func readOnly(t Tool) Tool { t.ReadOnly, t.Idempotent = true, true; return t }

func buildRegistry() []Tool {
	viewOut := func(desc string) Schema {
		return out(desc, Props{})
	}
	return []Tool{
		readOnly(Tool{
			Name:  ToolListVoices,
			Title: "可选配音音色",
			Description: "列出「换声音」可选的配音音色：voiceId、名称、性别、风格、试听样音。用户说「换个声音 / 配音 / 改台词」时先调它，" +
				"先问用户要男声还是女声、什么风格（gender：female / male；style：风格关键词，都可不填）。voice.voiceId 一律取这里返回的 voiceId，不要自己编。" +
				"换声音免费（每人每天有条数限制），口型可能对不上；方案里用 annotations.audio=revoice + annotations.voice。不扣点。",
			Input: obj("", Props{
				"gender": enum("性别筛选：female 女声 / male 男声；不填列全部", "female", "male"),
				"style":  strMax("风格关键词，如 亲切、活泼；不填不筛", 32),
			}),
			Output: out("VoiceList：voices[{voiceId, name, gender, style, sampleUrl?}]", Props{}),
		}),
		readOnly(Tool{
			Name:  ToolTrending,
			Title: "本周热门改法",
			Description: "给没想好改什么的用户灵感：本周同行业复刻最多的改法（近 7 天已确认方案按「改了哪几项 + 声音策略」聚合排序，最多 5 条，如「换模特 + 保留原声」）。" +
				"用户说「不知道改什么 / 有什么推荐 / 别人都怎么改」，或刚上传视频还在拆解时可以调。industry 不填 = 取用户在广场设置的行业，再没有就看全站；" +
				"某行业人数不够时自动回落全站（返回的 industry 为空、fallback=true）。每条有 label（给用户看的中文名）、desc、dims（改了哪几项及 replace / describe）、" +
				"audio（非默认的声音策略）、sampleId（有同组合的示例时给，可直接 replicate_try_sample）。" +
				"数据只有改法组合，不含任何人的参考图、备注或台词，人数不足 20 的组合不会出现，不要向用户编造具体人数。结果每小时更新一次。不扣点。",
			Input: obj("", Props{
				"industry": strMax("行业 code（广场的行业标识）；不填 = 用户的行业偏好，再缺省全站", 64),
			}),
			Output: out("TrendingView：industry、period（7d）、fallback、items[{label, desc, dims[{key,mode}], audio?, sampleId?}]", Props{}),
		}),
		{
			Name:  ToolShareSample,
			Title: "分享成片为示例",
			Description: "把用户已经做好的完整成片分享成公开示例，投给运营审核（通过后作为示例展示，不展示账号信息，可随时撤回）。" +
				"【免费，不扣点，不需要扣点同意卡，但必须先让用户明确授权】用户点「分享为示例」后，先出一张授权表单：「同意作为公开示例展示，可随时撤回；不展示你的账号信息」，用户确认后才调本工具。" +
				"如果这条成片的方案替换了人物（换模特），还要用户明确勾选肖像授权，勾选后传 portrait_consent=true；这一项只有表单里用户确认过才能传 true，不要替用户勾选。" +
				"替换了人物却没传 true 返回 PORTRAIT_CONSENT_REQUIRED（回去补授权表单，不要重试同样的参数）。" +
				"只收完整版成片（预览版 / 部分成片返回 JOB_NOT_DELIVERED）；同一条成片只能投一次，再调用返回已有投稿（replay=true）；被运营驳回的不能再投（SHARE_REJECTED）；撤回过的可以重新投。" +
				"返回 ShareView（submissionId、status、statusLabel、text）；submissionId 撤回时要用。通过与否由运营审核，之后可在对话里问用户进展。",
			Input: obj("", Props{
				"job_id":           Schema{"type": "integer", "description": "任务编号（replicate_confirm / replicate_get_job 返回的 jobId）", "minimum": 1},
				"portrait_consent": Schema{"type": "boolean", "description": "用户已在授权表单里勾选肖像授权（方案替换了人物时必须为 true；只能在用户确认后传 true）"},
			}, "job_id"),
			Output:     out("ShareView：submissionId、jobId、status（pending|approved|rejected|withdrawn）、statusLabel、portraitConsent、replay、text", Props{}),
			Idempotent: true,
		},
		{
			Name:  ToolWithdrawSample,
			Title: "撤回分享的示例",
			Description: "撤回用户之前分享的示例：还在审核中的作废；已经上线的示例立即下架。用户说「撤回我分享的示例 / 别再展示我的视频」时调用，【免费，不扣点，不需要确认】。" +
				"submission_id 是分享时 replicate_share_sample 返回的 submissionId（对话里找得到；找不到就如实告诉用户暂时定位不到这条投稿，不要用 replicate_share_sample 去查，它会对撤回过的成片重新投稿）。" +
				"返回 WithdrawView（status、unpublished=示例是否刚被下架、text）；已经撤回或没通过的重复调用不报错（replay=true）。已发放过的奖励不会收回。",
			Input: obj("", Props{
				"submission_id": Schema{"type": "integer", "description": "投稿号（replicate_share_sample 返回的 submissionId）", "minimum": 1},
			}, "submission_id"),
			Output:     out("WithdrawView：submissionId、status、statusLabel、unpublished、replay、text", Props{}),
			Idempotent: true,
		},
		readOnly(Tool{
			Name:  ToolLimits,
			Title: "复刻限制与开关",
			Description: "查看「视频做同款」当前的限制：原视频最长 / 最短多少秒、最多分几段、每段多长、参考图最多几张、可选模型、重做次数上限，" +
				"以及功能总开关 enabled（false 表示暂停接新提交，只能看历史）。用户问「能传多长的视频 / 最多几张图 / 现在能不能用」时再调，不必每次开场都调。不扣点。",
			Input:  obj("", Props{}),
			Output: out("LimitsView", Props{"enabled": Schema{"type": "boolean"}, "models": arr("开放的模型组", str("code"), 0, 0)}),
		}),
		{
			Name:  ToolAnalyze,
			Title: "分析原视频",
			Description: "分析用户上传的原视频：拆镜头、台词、画面里的人物 / 服装 / 商品 / 场景等维度，得到 analysis_no，之后才能出方案。" +
				"video 填用户消息里视频附件的序号，如 \"视频1\"；source 是视频真实地址，" + sysFilled + "（系统按序号换算，你只写 video）。" +
				"分析在后台跑，需要几十秒到几分钟：返回 status 不是终态（排队 / 分析中）时，" + ruleNoPoll + "同一条视频已经分析过会直接返回已有结果（cached=true），不重复计次。" +
				"不扣点，但有每分钟 / 每日分析次数限制（失败与复用不计次），命中会返回 RATE_LIMITED，如实告诉用户。",
			Input: obj("", Props{
				"video":  strMax("视频附件序号，如 视频1（用户消息里编号的视频）", maxAttachOrdinal),
				"source": strMax("原视频地址（"+sysFilled+"；站内上传地址或 mr://rid）", 1024),
			}),
			Output:     viewOut("AnalysisView：analysisNo、status、summary、shots、dims…"),
			Idempotent: true,
		},
		readOnly(Tool{
			Name:  ToolGetAnalysis,
			Title: "取分析结果",
			Description: "按 analysis_no 取一次分析的结果（看懂了什么、镜头 / 台词 / 各维度）。只在系统通知「分析完成」后、或用户明确问到分析内容时调用；" +
				"绝不要为了等分析完成而反复调用它（" + ruleNoPoll + "）。不扣点。",
			Input:  obj("", Props{"analysis_no": analysisNo()}, "analysis_no"),
			Output: viewOut("AnalysisView"),
		}),
		readOnly(Tool{
			Name:  ToolListModels,
			Title: "可选出片模型",
			Description: "列出可选的出片模型：名称、特点、每秒单价、支持的时长与画幅。用户想换模型、问价格差别、或你要在 create_plan / update_plan 里指定 model_group 前调用，" +
				"model_group 一律取这里返回的 code，不要自己编。不扣点。",
			Input:  obj("", Props{}),
			Output: viewOut("ModelList"),
		}),
		readOnly(Tool{
			Name:        ToolListSamples,
			Title:       "示例列表",
			Description: "列出可直接试看的示例（已分析好的爆款视频 + 示例成片），可按行业筛。用户没有自己的视频、想先看看效果时使用。不扣点、不限频。",
			Input:       obj("", Props{"industry": strMax("行业，如 food；不填列全部", 64)}),
			Output:      viewOut("SampleList"),
		}),
		readOnly(Tool{
			Name:  ToolTrySample,
			Title: "试看示例",
			Description: "试看一个示例：返回它的共享分析和示例成片，零成本（不建任务、不预留、不扣点、不限频）。试看之后想做同款，" +
				"用 replicate_create_plan 的 sample_id 出方案即可，不需要用户再上传视频。",
			Input:  obj("", Props{"sample_id": Schema{"type": "integer", "description": "示例编号（replicate_list_samples 返回的 id）", "minimum": 1}}, "sample_id"),
			Output: viewOut("SampleTry"),
		}),
		{
			Name:  ToolCreatePlan,
			Title: "生成做同款方案",
			Description: "根据分析结果（analysis_no）或示例（sample_id）生成做同款方案，二选一。annotations 写用户想改哪些维度（人物 / 服装 / 商品 / 场景 / 台词…）和怎么改；" +
				"用户没提的维度不要写，保持原样。要换成用户自己的商品 / 人物 / 服装，必须配参考图：ref_images_from 写图片附件序号，dims.<维度>.refs 指向它们。" +
				"返回的 PlanView 已经包含完整报价（cost、stageQuotes：先做第 1 段 / 其余段 / 合计）和预计耗时，" +
				"所以不需要再调 replicate_quote；把「先做第 1 段多少点、全部做完共多少点」讲给用户听，等用户同意后才 replicate_confirm。" +
				"缺项时（如要换商品却没给商品图）以 PARAM_INVALID 错误返回并带 gaps，按 gaps 里的 message 向用户要补充，不要自己编造素材。" +
				"方案有有效期（expiresAt），过期会得到 PLAN_EXPIRED，重新生成即可。生成方案不扣点，但有每分钟 / 每日次数限制。",
			Input: obj("", Props{
				"analysis_no":     analysisNo(),
				"sample_id":       Schema{"type": "integer", "description": "示例编号（与 analysis_no 二选一，两个都给时以示例为准）", "minimum": 1},
				"annotations":     annotationsSchema("生成方案时，"),
				"ref_images_from": refImagesFrom("没有参考图就不填。"),
				"ref_images":      refImagesInjected(),
				"model_group":     modelGroup(),
				"resolution":      resolution(),
				"aspect_ratio":    aspectRatio(),
			}),
			Output: viewOut("PlanView：planNo、planHash、cost、stageQuotes、eta、policy、moderation…"),
		},
		{
			Name:  ToolUpdatePlan,
			Title: "修改方案",
			Description: "修改一张已有方案：改标注、换模型 / 分辨率 / 画幅、换参考图。会生成一张新方案（新 plan_no），旧方案作废（STALE），之后一律用返回的新 plan_no；" +
				"新方案同样自带报价，不用再 replicate_quote。annotations 按维度合并进当前方案：只写要改的维度（要撤销某维度的改动就写 mode=keep），没写的沿用；" +
				"audio / texts 给了就整体替换。ref_images_from 一旦给出就整体替换参考图，这时 dims 里用到 refs 的维度必须一起重写（位置是新数组里的）。" +
				"所有字段都不填则无事可做。不扣点，与「再做一条」共用独立的改方案次数限制，命中返回 RATE_LIMITED。",
			Input: obj("", Props{
				"plan_no":         planNo(),
				"annotations":     annotationsSchema("要改的部分："),
				"ref_images_from": refImagesFrom("不填 = 沿用原参考图；填空数组 = 去掉所有参考图。"),
				"ref_images":      refImagesInjected(),
				"model_group":     modelGroup(),
				"resolution":      resolution(),
				"aspect_ratio":    aspectRatio(),
			}, "plan_no"),
			Output: viewOut("PlanView（新 planNo，旧方案 STALE）"),
		},
		readOnly(Tool{
			Name:        ToolGetPlan,
			Title:       "取方案",
			Description: "按 plan_no 取方案当前状态与报价（status：READY 可确认 / CONFIRMED 已确认 / STALE 已被新方案取代 / 已过期）。用户问「刚才那个方案」或你需要核对方案是否还有效时调用。不扣点。",
			Input:       obj("", Props{"plan_no": planNo()}, "plan_no"),
			Output:      viewOut("PlanView"),
		}),
		{
			Name:  ToolPreviewFrame,
			Title: "定妆图（免费预览第 1 帧）",
			Description: "给一张方案免费出一张「替换后的第 1 帧」定妆图：用原视频第 1 段的画面 + 方案里的参考图和改法合成，让用户确认之前先看到换人 / 换衣服 / 换商品大概长什么样。" +
				"【免费，不扣点，不需要同意卡】每人每天只有 3 次（成功出图才算一次；同一张方案再调用直接返回已有的图，不占次数）。" +
				"这只是一张静态预览，不是最终视频，向用户介绍时一定说清「这是预览图，不是成片」。" +
				"当方案要替换人物 / 服装 / 商品时，在请用户确认（replicate_confirm）之前先调用它；只改台词、声音、叠字的方案没有可预览的画面，不要调用。" +
				"出图要几十秒，等它返回；超时 / 失败会返回可重试的错误（这时没有占用次数）。今天的次数用完会返回 PREVIEW_QUOTA_EXHAUSTED（明天恢复，不可重试），" +
				"如实告诉用户，并说明可以直接确认方案生成视频。方案被改过会得到新 plan_no，旧方案的预览作废，永远用最新的 plan_no。",
			Input:      obj("", Props{"plan_no": planNo()}, "plan_no"),
			Output:     out("PreviewView：planNo、imageUrl、imageRid、frameAtSec、quotaUsed、quotaTotal、urlExpiresAt", Props{}),
			Idempotent: true,
		},
		{
			Name:  ToolQuote,
			Title: "重新报价",
			Description: "对一张方案重新核价并刷新有效期（价格或模型单价可能变了、方案接近过期时用）。replicate_create_plan / replicate_update_plan 刚返回的方案已含最新报价，" +
				"不需要再调它。不扣点。",
			Input:      obj("", Props{"plan_no": planNo()}, "plan_no"),
			Output:     viewOut("PlanView"),
			Idempotent: true,
		},
		{
			Name:  ToolConfirm,
			Title: "确认方案开始生成",
			Description: "确认方案并开始生成视频。" + ruleCharge + "stage：first = 先只做第 1 段（多段视频推荐：满意了再继续剩下的，没做的段不收费）；" +
				"all = 一次做完全部；不填按方案默认。用户明确说「一次全做」才用 all。" +
				"返回 JobView（jobId、各段状态、已冻结 / 已扣 / 已退点数）。生成要几分钟，" + ruleNoPoll + "任务有失败段时，修复一律用 replicate_redo_segment，" +
				"绝不要为了修复失败再确认一个新方案（那会让已做好的段重新生成、重复扣点）。方案已被确认过（CONFIRMED）时返回已有任务，不会重复扣点。",
			Input: obj("", Props{
				"plan_no":         planNo(),
				"stage":           enum("做哪些段：first 先做第 1 段 / all 全部；不填按方案默认", "first", "all"),
				"plan_hash":       strMax("方案校验串（"+sysFilled+"）", 64),
				"expected_points": Schema{"type": "integer", "description": "本次要冻结的点数（" + sysFilled + "；必须与同意卡上用户确认的金额一致）", "minimum": 1},
			}, "plan_no"),
			Output:  viewOut("JobView"),
			Charges: true,
		},
		readOnly(Tool{
			Name:  ToolQuoteBatch,
			Title: "批量报价",
			Description: "批量做多条的报价（只读，不扣点）：「这个方案满意了，换几个商品 / 模特各做一条」。**只有同一方案已经有一条成功成片后才能用**（否则 BATCH_NOT_READY：先做出一条再批量）。" +
				"每一条只换方案里「主替换项」（有换图的那一项，优先 人物 > 商品 > 服装）的参考图，其余改法 / 模型 / 清晰度 / 画幅 / 声音完全沿用，直接做全部段。" +
				"variants 每项给这一条的 ref_images_from，最多 10 条；张数不对返回 PARAM_INVALID。返回 rows（每条点数，与 variants 同序同长）与 total（合计）。",
			Input:  obj("", Props{"plan_no": planNo(), "variants": batchVariants()}, "plan_no", "variants"),
			Output: out("BatchQuoteView：planNo、planHash、rows[{index,points}]、total、mainDim、refsPerVariant", Props{}),
		}),
		{
			Name:  ToolConfirmBatch,
			Title: "确认批量生成",
			Description: "确认批量生成 N 条（会按合计扣点）。" + ruleCharge + "expected_points 是整批合计（replicate_quote_batch 的 total），整批原子：" +
				"点数不够一条都不做、不部分扣；合计与现算不一致返回 QUOTE_CHANGED。必须先 replicate_quote_batch，variants 与报价时完全一致；" +
				"源方案没有成功成片返回 BATCH_NOT_READY；一批 1~10 条，参考图张数不对返回 PARAM_INVALID；点数不足返回 INSUFFICIENT_POINTS。" +
				"返回 BatchView（batchId、jobs、total、status）。生成要几分钟，" + ruleNoPoll,
			Input: obj("", Props{
				"plan_no":         planNo(),
				"variants":        batchVariants(),
				"expected_points": Schema{"type": "integer", "description": "整批合计要冻结的点数（" + sysFilled + "；必须与同意卡一致）", "minimum": 1},
			}, "plan_no", "variants"),
			Output:  viewOut("BatchView"),
			Charges: true,
		},
		readOnly(Tool{
			Name:  ToolGetBatch,
			Title: "查一批任务的进度",
			Description: "查批量任务的整体进度：每条任务的状态与合计点数，整体 status：running 进行中 / succeeded 全部成功 / partial 部分成功 / failed 全部没成功。" +
				"batch_id 取 replicate_confirm_batch 返回的 batchId。只在用户问进度、或系统发来通知后调用；" + ruleNoPoll + "不扣点。",
			Input:  obj("", Props{"batch_id": strMax("批次编号（replicate_confirm_batch 返回的 batchId，数字的字符串形式）", 20)}, "batch_id"),
			Output: viewOut("BatchView"),
		}),
		{
			Name:  ToolPublishCopy,
			Title: "生成发布文案",
			Description: "给已经做好的成片写发布文案：3 个不同风格的标题（≤30 字）、话题标签（≤8 个，# 开头）、描述（≤200 字）和一个建议的封面时刻。" +
				"【免费，不扣点，不需要同意卡】每人每天 20 次，同一条成片同一个平台再次调用直接返回已写好的，不占次数。" +
				"只对已交付的成片可用（完整成片，或用户选择结束后拼好的部分成片）；预览版 / 没做好的任务返回 JOB_NOT_DELIVERED，如实告诉用户成片做好后再来。" +
				"platform：douyin 抖音（缺省）/ kuaishou 快手 / xiaohongshu 小红书 / shipinhao 视频号；用户没说平台就用抖音，并告诉用户可以换别的平台。" +
				"今天的次数用完返回 COPY_QUOTA_EXHAUSTED（明天恢复，不可重试）。文案由模型写出，向用户呈现时让用户自己再看一遍、按需修改后再发布。",
			Input: obj("", Props{
				"job_id":   Schema{"type": "integer", "description": "任务编号（replicate_confirm / replicate_get_job 返回的 jobId）", "minimum": 1},
				"platform": enum("发布平台，缺省 douyin", "douyin", "kuaishou", "xiaohongshu", "shipinhao"),
			}, "job_id"),
			Output:     out("PublishCopyView：jobId、platform、titles[3]、hashtags[≤8]、description、coverAtSec、quotaUsed、quotaTotal", Props{}),
			Idempotent: true,
		},
		{
			Name:  ToolExportRatio,
			Title: "导出其它比例",
			Description: "把已经做好的成片导出成另一个画幅（9:16 竖屏 / 1:1 方形 / 16:9 横屏 / 3:4 / 4:3），用于不同平台发布。" +
				"【免费，不扣点，不需要同意卡】不重新生成，只是重新排版：mode=pad（缺省）= 原画面完整保留、居中，空出来的地方用同一画面的模糊放大铺底；" +
				"mode=crop = 居中裁切铺满（可能裁掉画面边缘）。声音原样保留。每条成片每种比例（每种模式）只做一次，再调用直接返回已做好的；结果会自动保存到素材库。" +
				"与成片比例相同时返回成片本身。只对已交付的成片可用（预览版 / 没做好的任务返回 JOB_NOT_DELIVERED）。导出要几十秒，等它返回。" +
				"返回 ExportView（videoUrl 是短时有效的预览 / 下载地址，library 是保存到素材库的状态）。",
			Input: obj("", Props{
				"job_id": Schema{"type": "integer", "description": "任务编号（replicate_confirm / replicate_get_job 返回的 jobId）", "minimum": 1},
				"ratio":  enum("目标比例", "9:16", "1:1", "16:9", "3:4", "4:3"),
				"mode":   enum("pad = 模糊底铺满（缺省，不裁画面）/ crop = 居中裁切", "pad", "crop"),
			}, "job_id", "ratio"),
			Output:     out("ExportView：jobId、ratio、mode、videoUrl、urlExpiresAt、library{status,state,text}", Props{}),
			Idempotent: true,
		},
		{
			Name:  ToolRetryAudio,
			Title: "免费重做声音",
			Description: "成片的 result.audio=failed（保留原声贴回失败 / 换声音失败，此时成片是静音的，result.audioNote 会这么说）时，免费重做声音这一步：" +
				"【不扣点，不需要同意卡】不重新生成画面，只在现有成片上重新贴回原声（保留原声）或重新合成配音（换声音，不再占今天的换声音条数）。" +
				"看到 audio=failed（或 result.canRetryAudio=true）应主动告诉用户「声音这次没处理成功，可以免费重做」并询问是否重做。" +
				"成功后成片换成带声音的新版本（会自动保存到素材库），返回 AudioRetryView（audio、changed、videoUrl、retriesLeft）。" +
				"每条成片最多重做 3 次，用完返回 AUDIO_RETRY_LIMIT；声音没出问题返回 AUDIO_NOT_FAILED；正在重做或成片正在更新返回 IN_PROGRESS（稍后再试）。" +
				"重做要几十秒，等它返回。只对已交付的成片可用（JOB_NOT_DELIVERED）。",
			Input: obj("", Props{
				"job_id": Schema{"type": "integer", "description": "任务编号（replicate_confirm / replicate_get_job 返回的 jobId）", "minimum": 1},
			}, "job_id"),
			Output:     out("AudioRetryView：jobId、audio（kept|revoiced|none|failed）、audioNote、changed、retriesLeft、canRetryAudio、videoUrl、urlExpiresAt、library", Props{}),
			Idempotent: true,
		},
		readOnly(Tool{
			Name:  ToolGetJob,
			Title: "查任务进度与结果",
			Description: "查任务当前状态：各段进度与成片、已冻结 / 已扣 / 已退点数（money）、下一步可选项（decision：继续 / 重做 / 调整 / 停止及各自点数）、" +
				"cancelRest（不做剩下的段能退多少）。成片 / 封面地址是几十分钟内有效的签名地址。只在用户问进度、或系统发来「完成 / 失败」通知后调用；" + ruleNoPoll + "不扣点。",
			Input:  obj("", Props{"job_id": jobID()}, "job_id"),
			Output: viewOut("JobView（含 decision / money / cancelRest）"),
		}),
		readOnly(Tool{
			Name:        ToolListJobs,
			Title:       "我的任务列表",
			Description: "列出用户自己做过的复刻任务（最近的在前）。用户问「我之前做的」「上次做到哪了」时调用。不扣点。",
			Input:       obj("", Props{"page": integer("页码，从 1 起，默认 1", 1, 1000)}),
			Output:      viewOut("JobList"),
		}),
		{
			Name:  ToolContinue,
			Title: "继续做剩下的段",
			Description: "第 1 段做好、用户看过并满意后，继续做剩下的段。" + ruleCharge + "应付点数在 replicate_get_job 返回的 decision.options 中 action=continue 那项的 points；" +
				"用户不满意第 1 段时不要继续，改走调整方案或停止。继续后" + ruleNoPoll,
			Input: obj("", Props{
				"job_id":          jobID(),
				"plan_hash":       strMax("方案校验串（"+sysFilled+"）", 64),
				"expected_points": Schema{"type": "integer", "description": "其余段合计要冻结的点数（" + sysFilled + "；必须与同意卡一致）", "minimum": 1},
			}, "job_id"),
			Output:  viewOut("JobView"),
			Charges: true,
		},
		{
			Name:  ToolRedoSegment,
			Title: "重做 / 精修一段",
			Description: "对任务里某一个段单独再做一次，其它段原样保留，报价只算这一段。" + ruleCharge + "单段价格在 replicate_get_job 的 segments[].points / refineLabel / redoLabel 里。" +
				"两种用法：1）「失败」的段重做：失败时已退回的点数不变，这一段重新预留并重新生成，note 可选；任务有失败段时修复一律用它，绝不要重新 replicate_confirm 新方案。" +
				"2）「已成功」的段精修（付费）：用户对已做好的这一段不满意想改（segments[].refinable=true）时用，note 必填，写明要改什么；" +
				"会新预留一笔点数（与重做同价），原来那一段已扣的点数不退；精修做好前这一段仍是旧视频，做好后换成新视频并自动重新拼成片，其它段不重做；" +
				"精修失败会退回这笔点数，这一段仍保留旧视频。已成功的段不带 note 会返回 SEGMENT_OK。" +
				"note（≤200 字）：用户对这一段的修改要求，如「动作再大一点」「别换衣服」，只写进这一段这次重做的提示词，会先过内容检查（不通过返回 MODERATION_REJECTED，没扣点）。" +
				"还在生成 / 精修中的段返回 SEGMENT_BUSY，没开始的段 NOT_REDOABLE；每段重做 + 精修合计有次数上限（REDO_LIMIT），到上限就建议用户换模型或调整方案。" +
				"想改叠字（价格 / 店名）不要用它，叠字不需要重新生成。",
			Input: obj("", Props{
				"job_id":          jobID(),
				"seg_index":       Schema{"type": "integer", "description": "要重做的段号，从 1 起（replicate_get_job 的 segments[].index）", "minimum": 1, "maximum": 20},
				"note":            strMax("这一段的精修备注（≤200 字）：失败段重做可选；已成功的段精修必填", NoteMaxRunes),
				"expected_points": Schema{"type": "integer", "description": "这一段要冻结的点数（" + sysFilled + "；必须与同意卡一致）", "minimum": 1},
			}, "job_id", "seg_index"),
			Output:  viewOut("JobView"),
			Charges: true,
		},
		{
			Name:  ToolStop,
			Title: "停止任务",
			Description: "停止整个任务：没开始的段取消并退回已冻结的点数，已经成功的段保留并照常扣点，在途的段会被终止。用户明确说「不做了 / 停掉」才调用；" +
				"只想放弃剩下没开始的段（在途段做完）用 replicate_cancel_rest。会退点，不扣点。",
			Input:       obj("", Props{"job_id": jobID()}, "job_id"),
			Output:      viewOut("JobView"),
			Destructive: true,
		},
		{
			Name:  ToolCancelRest,
			Title: "不做剩下的段",
			Description: "放弃剩下还没开始的段：只取消没开始的段并退回它们的冻结点数，已经在做的段做完、已做好的段保留。幂等（重复调用结果相同）。" +
				"用户说「后面的不做了 / 就第 1 段」时用；可退多少点在 replicate_get_job 的 cancelRest 预览里。会退点，不扣点。",
			Input:       obj("", Props{"job_id": jobID()}, "job_id"),
			Output:      viewOut("JobView"),
			Destructive: true,
			Idempotent:  true,
		},
	}
}

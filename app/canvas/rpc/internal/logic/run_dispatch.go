package logic

// 执行后端分派（node run 粒度）：
//
//	kind           后端     做法                                                    计费归属
//	image          gateway  image.generate / image.edit（有参考图走 edit），count 张 = count 个调用   Go：每 run 一张预留单，部分失败只退失败张
//	video          gateway  video.generate（首尾帧 / 参考图 / 参考视频 / 时长 / 声音）           Go：每 run 一张预留单
//	tool_i2p       gateway  chat.completion 多模态（vlm 模型组），输出纯文本                     Go：每 run 一张预留单
//	tool_crop      media    image/crop → resources/copy 素材库桶，同步完成                    不计费
//	tool_upscale   java     POST /api/v1/tools/ai-image-upscaler（透传用户 Bearer）           Java
//	tool_outpaint  java     POST /api/v1/tools/ai-ratio-convert                               Java
//	tool_inpaint   java     POST /api/v1/tools/ai-inpaint（蒙版 → bbox；dataURL 先 media 拉入）    Java
//	tool_tryon     studio   studio-rpc SubmitTryon（garment 1 + models ≤9，count 1）→ GetBatch    studio
//
// gateway 输入图一律先经媒体服务 ingest 成 mr://{rid}（带 sha256），不直接传 https（gateway 审计 10 号文件）。
// 每次提交带 constraints.deadline_at 与 limits.max_cost；超时由轮询器主动 Cancel。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/javatool"
	studio "aigc-local-life/app/studio/rpc/studio_client"
	"aigc-local-life/pkg/aigateway"
	"aigc-local-life/pkg/mediasvc"
	"aigc-local-life/pkg/modelgroup"
	"aigc-local-life/pkg/taskevent"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

// 操作名。
const (
	opImageEdit     = "image.edit"
	opImageGenerate = "image.generate"
	opVideoGenerate = "video.generate"
	opChat          = "chat.completion"
)

// errTemporary 提交遇到暂时性错误（429 / 5xx / 网络）：run 留在排队，轮询器重提（同一幂等键）。
type errTemporary struct{ err error }

func (e *errTemporary) Error() string { return e.err.Error() }
func (e *errTemporary) Unwrap() error { return e.err }

func temporary(err error) error { return &errTemporary{err: err} }

// isTemporary 是否暂时性错误。
func isTemporary(err error) bool {
	var t *errTemporary
	return errors.As(err, &t)
}

// ---------------- gateway 请求体（contract/operations.py，extra=forbid：空字段一律省略） ----------------

type imageEditInput struct {
	Prompt       string               `json:"prompt"`
	Images       []aigateway.MediaRef `json:"images"`
	N            int                  `json:"n"`
	AspectRatio  string               `json:"aspect_ratio,omitempty"`
	Resolution   string               `json:"resolution,omitempty"`
	Quality      string               `json:"quality,omitempty"`
	OutputFormat string               `json:"output_format,omitempty"` // png / jpeg / webp；不支持的适配器忽略
}

type imageGenerateInput struct {
	Prompt       string `json:"prompt"`
	N            int    `json:"n"`
	AspectRatio  string `json:"aspect_ratio,omitempty"`
	Resolution   string `json:"resolution,omitempty"`
	Quality      string `json:"quality,omitempty"`
	OutputFormat string `json:"output_format,omitempty"`
}

type videoGenerateInput struct {
	Prompt          string               `json:"prompt"`
	DurationS       int                  `json:"duration_s"`
	Resolution      string               `json:"resolution"`
	AspectRatio     string               `json:"aspect_ratio,omitempty"`
	FirstFrame      *aigateway.MediaRef  `json:"first_frame,omitempty"`
	LastFrame       *aigateway.MediaRef  `json:"last_frame,omitempty"`
	ReferenceImages []aigateway.MediaRef `json:"reference_images,omitempty"`
	ReferenceVideos []aigateway.MediaRef `json:"reference_videos,omitempty"`
	GenerateAudio   bool                 `json:"generate_audio"` // 缺省 true，sound=false 必须显式传
}

type chatPart struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URI  string `json:"uri,omitempty"`
	Sha  string `json:"sha256,omitempty"`
}

type chatMessage struct {
	Role    string     `json:"role"`
	Content []chatPart `json:"content"`
}

type chatInput struct {
	Messages    []chatMessage `json:"messages"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Temperature float64       `json:"temperature"`
}

// ReversePromptSystem 反推提示词的系统提示（语义同原型 REVERSE_PROMPT_PRESET，出处 infinite-canvas reversePreset，MIT）。
const ReversePromptSystem = `你是资深的 AI 生图提示词工程师。用户会给你一张参考图片，请根据图片反推一段适合用于 AI 生图模型的提示词。
要求：
1. 只输出提示词正文本身，不要任何解释、标题、前后缀、Markdown 或引号。
2. 覆盖主体、构图、风格、光线、色彩、材质、镜头和氛围。
3. 写成一段可以直接交给生图模型使用的完整提示词，信息具体、可复现，不臆造图中没有的文字或品牌。`

// labels gateway 不透明标签（≤16 对，值 ≤256）。
func runLabels(run *model.NodeRun) map[string]string {
	return map[string]string{
		"app": "canvas", "kind": string(run.Kind), "run": strconv.FormatInt(run.ID, 10),
		"canvas": trunc(run.CanvasID, 256), "user": strconv.FormatInt(run.UserID, 10),
	}
}

// trunc 按字节截到不超过 n，并退到 UTF-8 字符边界：直接 s[:n] 会把中文切成半个字符，
// MySQL utf8mb4 列拒收（Error 1366 Incorrect string value），素材库转存就此反复失败。
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// gatewayIdem 第 i 个调用的幂等键（同一 run 重提用同一个键，拿回同一条调用）。task_id 为空直接报错：
// 退化成 canvas::0 会让所有 run 共用一个键（gateway 409 / 拿到别人的调用）。
func gatewayIdem(run *model.NodeRun, i int) (string, error) {
	if !run.TaskID.Valid || run.TaskID.String == "" {
		return "", errNoTaskID
	}
	return fmt.Sprintf("canvas:%s:%d", run.TaskID.String, i), nil
}

// mediaRef 输入图 → gateway MediaRef：经媒体服务拉入成 mr://{rid}+sha256；媒体服务未配置时（仅开发）原样传 https。
func (c *core) mediaRef(ctx context.Context, url string) (aigateway.MediaRef, error) {
	if strings.HasPrefix(url, aigateway.MRScheme) || c.s.Media == nil {
		return aigateway.MediaRef{URI: url}, nil
	}
	res, err := c.s.Media.IngestURL(ctx, url, mediasvc.PurposeUserInput, "")
	if err != nil {
		return aigateway.MediaRef{}, mediaErr("拉取输入图失败", err)
	}
	return aigateway.MediaRef{URI: aigateway.MRScheme + res.RID, Sha256: res.SHA256, Mime: res.Mime}, nil
}

// mediaRefs 一组输入图并发拉入媒体服务（Poll.FanoutConcurrency），结果与 urls 一一对应。
// 出错时返回按下标顺序的第一个错误（与原先逐张拉入、遇错即停报的是同一张）；拉入按内容幂等，重提时重复拉入无副作用。
func (c *core) mediaRefs(ctx context.Context, urls []string) ([]aigateway.MediaRef, error) {
	out := make([]aigateway.MediaRef, len(urls))
	errs := fanout(len(urls), c.fanoutLimit(), func(i int) error {
		r, err := c.mediaRef(ctx, urls[i])
		out[i] = r
		return err
	})
	if err := firstErr(errs); err != nil {
		return nil, err
	}
	return out, nil
}

// mediaErr 媒体服务错误：4xx 视为输入问题（永久），其余暂时性。
func mediaErr(what string, err error) error {
	var me *mediasvc.Error
	if errors.As(err, &me) && me.Permanent() {
		return xerr.Param(what + "：" + me.Message)
	}
	return temporary(fmt.Errorf("%s: %w", what, err))
}

// gatewayErr gateway 提交错误分类。
func gatewayErr(err error) error {
	var ge *aigateway.Error
	if errors.As(err, &ge) {
		if ge.Temporary() {
			return temporary(err)
		}
		if ge.IsAuth() || ge.IsIdempotencyConflict() {
			return fmt.Errorf("gateway 配置错误: %w", err) // 部署问题：记日志，用户侧服务繁忙
		}
		return xerr.Param("生成参数不被模型接受：" + ge.Message)
	}
	if errors.Is(err, aigateway.ErrNotConfigured) {
		return xerr.New(xerr.NotReady, "生成服务暂未开放，请稍后再试")
	}
	return temporary(err)
}

func (c *core) maxCost(k model.RunKind) string {
	m := c.s.Config.MaxCostCNY
	switch k {
	case model.KindVideo:
		return m.Video
	case model.KindToolI2P:
		return m.I2P
	}
	return m.Image
}

// gatewayRequests 构造本 run 的 gateway 调用（image count 张 = count 个调用；video / i2p 各 1 个）。
func (c *core) gatewayRequests(ctx context.Context, run *model.NodeRun, sp *runSpec) ([]aigateway.CallRequest, error) {
	// deadline_at 按硬上限给：已受理的调用软时限过后我们还会等到硬上限，网关不能先把它判超时（deadline_at 不参与 request_hash，
	// 同幂等键重提时改了也不冲突）
	deadline := c.now().Add(c.hardCap(run.Kind)).UTC()
	base := aigateway.CallRequest{
		ModelGroup: sp.ModelGroup, Labels: runLabels(run), MediaPurpose: aigateway.MediaPurposeIntermediate,
		Constraints: &aigateway.Constraints{DeadlineAt: &deadline},
		Limits:      &aigateway.Limits{MaxCost: c.maxCostFor(run.Kind, sp)},
	}
	refs := func(list []refMedia) ([]aigateway.MediaRef, error) {
		urls := make([]string, len(list))
		for i, m := range list {
			urls[i] = m.URL
		}
		return c.mediaRefs(ctx, urls)
	}
	switch run.Kind {
	case model.KindImage:
		// 受理时已按模型组归一（normalizeImageSize）；这里再过一遍只为兼容改版前落库的请求，新 run 是空操作
		res, _ := normalizeImageSize(sp.ModelGroup, sp.Size)
		res = strings.ToUpper(res)
		quality := imageQuality(sp)
		var in any
		req := base
		if len(sp.Images) > 0 {
			imgs, err := refs(sp.Images)
			if err != nil {
				return nil, err
			}
			req.Operation = opImageEdit
			in = imageEditInput{Prompt: sp.Prompt, Images: imgs, N: 1, AspectRatio: sp.Ratio, Resolution: res, Quality: quality, OutputFormat: imageOutputFormat(sp)}
		} else {
			ratio := sp.Ratio
			if ratio == "" {
				ratio = "1:1"
			}
			req.Operation = opImageGenerate
			in = imageGenerateInput{Prompt: sp.Prompt, N: 1, AspectRatio: ratio, Resolution: res, Quality: quality, OutputFormat: imageOutputFormat(sp)}
		}
		req.Input = in
		out := make([]aigateway.CallRequest, sp.Count)
		for i := range out {
			out[i] = req
		}
		return out, nil
	case model.KindVideo:
		in := videoGenerateInput{Prompt: sp.Prompt, DurationS: sp.DurationS, Resolution: videoResolution(sp.Size),
			AspectRatio: sp.Ratio, GenerateAudio: sp.Sound}
		// 首帧、尾帧、参考图、参考视频一次并发拉入（顺序同原先逐个拉入，出错报的是同一个）
		urls := make([]string, 0, 2+len(sp.Images)+len(sp.Videos))
		if sp.First != "" {
			urls = append(urls, sp.First)
		}
		if sp.Last != "" {
			urls = append(urls, sp.Last)
		}
		for _, m := range sp.Images {
			urls = append(urls, m.URL)
		}
		urls = append(urls, sp.Videos...)
		all, err := c.mediaRefs(ctx, urls)
		if err != nil {
			return nil, err
		}
		if sp.First != "" {
			r := all[0]
			in.FirstFrame, all = &r, all[1:]
		}
		if sp.Last != "" {
			r := all[0]
			in.LastFrame, all = &r, all[1:]
		}
		in.ReferenceImages = make([]aigateway.MediaRef, len(sp.Images))
		copy(in.ReferenceImages, all[:len(sp.Images)])
		in.ReferenceVideos = make([]aigateway.MediaRef, len(sp.Videos))
		copy(in.ReferenceVideos, all[len(sp.Images):])
		req := base
		req.Operation, req.Input = opVideoGenerate, in
		return []aigateway.CallRequest{req}, nil
	case model.KindToolI2P:
		img, err := c.mediaRef(ctx, sp.Images[0].URL)
		if err != nil {
			return nil, err
		}
		ask := "请反推这张图片的生图提示词，用中文输出。"
		if strings.HasPrefix(strings.ToLower(paramStr(sp.Params, "language")), "en") {
			ask = "Reverse-engineer an image-generation prompt for this picture. Output in English only."
		}
		in := chatInput{
			Messages: []chatMessage{
				{Role: "system", Content: []chatPart{{Type: "text", Text: ReversePromptSystem}}},
				{Role: "user", Content: []chatPart{{Type: "image_url", URI: img.URI, Sha: img.Sha256}, {Type: "text", Text: ask}}},
			},
			MaxTokens: c.s.Config.I2P.MaxTokens, Temperature: 0.3,
		}
		req := base
		req.Operation, req.Input = opChat, in
		return []aigateway.CallRequest{req}, nil
	}
	return nil, fmt.Errorf("canvas: %s 不走 gateway", run.Kind)
}

// videoResolution 视频分辨率小写 ^[0-9]{3,4}p$；4K → 2160p。
func videoResolution(s string) string {
	l := strings.ToLower(strings.TrimSpace(s))
	switch l {
	case "4k":
		return "2160p"
	case "2k":
		return "1440p"
	case "":
		return "720p"
	}
	return l
}

// submitGateway 并发提交全部调用（Poll.FanoutConcurrency，每张幂等键 canvas:{task_id}:{i}），成功时返回按下标排列的 call id
// （逗号分隔写 backend_ref）。出错时返回按下标顺序的第一个错误，以及所有已拿到的 id（含排在出错那张之后、并发提交成功的）：
// 暂时性错误重提用同一幂等键会拿回它们；最终失败由 dispatch 取消（count>1 部分提交失败不留孤儿调用）并整单退款。
func (c *core) submitGateway(ctx context.Context, run *model.NodeRun, sp *runSpec) ([]string, error) {
	if !c.s.Gateway.Enabled() {
		return nil, xerr.New(xerr.NotReady, "生成服务暂未开放，请稍后再试")
	}
	if _, err := gatewayIdem(run, 0); err != nil {
		return nil, err
	}
	reqs, err := c.gatewayRequests(ctx, run, sp)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(reqs))
	trace := c.traceIDOf(run.ID)
	errs := fanout(len(reqs), c.fanoutLimit(), func(i int) error {
		key, _ := gatewayIdem(run, i)
		step := stCall + strconv.Itoa(i)
		attrs := taskevent.Attrs{"model_group": reqs[i].ModelGroup, "operation": reqs[i].Operation, "idempotency_key": key}
		c.evStart(ctx, run, step, int(run.Attempt)+1, attrs)
		call, err := c.s.Gateway.Submit(ctx, reqs[i], aigateway.SubmitOptions{IdempotencyKey: key, TraceID: trace})
		if err != nil {
			f := taskevent.Fail{Class: "GATEWAY_SUBMIT", Code: gatewayErrCode(err), Check: "canvas.gateway_submit", Message: traceErrMsg(err)}
			if isTemporary(gatewayErr(err)) {
				f.Class = "TEMPORARY"
			}
			c.evEnd(ctx, run, step, int(run.Attempt)+1, taskevent.StatusFailed, f, attrs)
			return err
		}
		ids[i] = call.ID
		// 调用一创建就直接写库关联（不依赖 reportGatewayCalls 的 HTTP 回报）
		ext := taskevent.WithExternal(taskevent.ExternalGatewayCall, call.ID)
		if c.s.Events != nil {
			c.s.Events.LinkGatewayCall(ctx, trace, step, call.ID, int(run.Attempt)+1, attrs)
		}
		c.evEnd(ctx, run, step, int(run.Attempt)+1, taskevent.StatusOK, taskevent.Fail{}, attrs, ext)
		return nil
	})
	if err := firstErr(errs); err != nil {
		got := make([]string, 0, len(ids))
		for _, id := range ids {
			if id != "" {
				got = append(got, id)
			}
		}
		first := gatewayErr(err)
		if !isTemporary(first) {
			// 首个错误是永久的，但并发提交时别的张可能遇到暂时性错误（超时 / 5xx：网关上可能已建调用、只是响应没拿到）。
			// 原先逐张提交遇错即停，后面的张根本不会发出；现在要标出来，让 dispatch 按幂等键回收，不留孤儿调用
			for _, e := range errs {
				if e != nil && isTemporary(gatewayErr(e)) {
					return got, &maybeCreatedErr{first}
				}
			}
		}
		return got, first
	}
	return ids, nil
}

// maybeCreatedErr 提交以永久错误失败，但其它张有结局未知（暂时性错误）的提交：网关上可能已建了调用。
// Unwrap 透出首个错误，isTemporary / xerr.Parse 的判定与报错文案不变。
type maybeCreatedErr struct{ error }

func (e *maybeCreatedErr) Unwrap() error { return e.error }

// submitMaybeCreated 提交失败时网关上是否可能留有没拿到调用号的调用（需按幂等键取回并取消）。
func submitMaybeCreated(err error) bool {
	var m *maybeCreatedErr
	return isTemporary(err) || errors.As(err, &m)
}

// ---------------- Java 兼容工具 ----------------

// javaErr Java 错误 → 用户错误。
func javaErr(err error) error {
	if errors.Is(err, javatool.ErrNotConfigured) {
		return xerr.New(xerr.NotReady, "该工具暂未开放，请稍后再试")
	}
	if errors.Is(err, javatool.ErrNoBearer) {
		return xerr.New(xerr.NotReady, "该工具需要登录令牌（当前请求没有 Authorization），请重新登录后再试")
	}
	var je *javatool.Error
	if errors.As(err, &je) {
		switch {
		case je.Unauthorized():
			return xerr.New(xerr.Unauthorized, "登录已过期，请重新登录")
		case je.Code == xerr.InsufficientPoints:
			return xerr.New(xerr.InsufficientPoints, firstNonEmpty(je.Msg, "点数不足"))
		case javaRetryable(je): // 与 recoverJavaRef 同口径：网络 / 429 / ≥500 / 1004 / 3010 / 5005 / 46011
			return temporary(err)
		case je.Code >= 1000 && je.Code < 10000 && je.Code != xerr.ServerError:
			return xerr.New(je.Code, firstNonEmpty(je.Msg, "工具调用失败"))
		}
		return xerr.New(xerr.ThirdPartyError, firstNonEmpty(je.Msg, "工具调用失败"))
	}
	return temporary(err)
}

// javaMaybeSubmitted Java run 提交失败时，Java 侧是否可能已有任务：已落 backend_request，且此前有过结局未知的尝试
// （attempt>0：暂时失败 / 提交成功但写库失败）或这次本身是暂时性失败（超时 / 5xx，次数用尽）。
// 首次就被 Java 明确拒绝（参数 / 点数不足等同步校验）说明没建任务，照常直接置失败。
func (c *core) javaMaybeSubmitted(run *model.NodeRun, err error) bool {
	if run.Backend != model.BackendJava {
		return false
	}
	if _, ok := parseJavaRequest(run); !ok {
		return false
	}
	return run.Attempt > 0 || isTemporary(err)
}

// submitJava 提交 Java 兼容工具，返回 taskUid。
// 首次发送前把实际要发的请求（路径 + 原始 body 字节 + 幂等键）落到 run.backend_request；之后的重提一律逐字节重发它
// （body 里的地址 / 蒙版框重算可能变，Java 按内容摘要判同键不同内容为 1003）。见 java_request.go。
func (c *core) submitJava(ctx context.Context, run *model.NodeRun, sp *runSpec, bearer string) (string, error) {
	if jr, ok := parseJavaRequest(run); ok {
		sub, err := c.s.Java.SubmitRaw(ctx, bearer, jr.Path, []byte(jr.Body), jr.Idem)
		if err != nil {
			return "", javaErr(err)
		}
		return sub.TaskID, nil
	}
	idem, err := runKey(run)
	if err != nil {
		return "", err
	}
	path, body, err := c.javaToolBody(ctx, run, sp, false)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	jr := javaRequest{Path: path, Idem: idem, Body: string(raw)}
	if err := c.persistJavaRequest(ctx, run, jr); err != nil {
		return "", err // 没落库就不发：否则取消时无法逐字节找回
	}
	sub, err := c.s.Java.SubmitRaw(ctx, bearer, jr.Path, raw, jr.Idem)
	if err != nil {
		return "", javaErr(err)
	}
	return sub.TaskID, nil
}

// javaToolBody Java 兼容工具的请求路径与请求体（提交与报价同一份组装）。
// forQuote=true（canvas_quote_node 报价）时不碰媒体服务：蒙版只认 bbox / mask_strokes / mask_data_url（内存解码），
// 其余（mask_rid / mask_url）按整图框报价——重绘区域不影响价格，提交时照常按蒙版算框。
func (c *core) javaToolBody(ctx context.Context, run *model.NodeRun, sp *runSpec, forQuote bool) (string, any, error) {
	if len(sp.Images) == 0 {
		return "", nil, xerr.Param("工具需要连接一张图片")
	}
	img := sp.Images[0].URL
	switch run.Kind {
	case model.KindToolUpscale:
		res, err := upscaleResolution(sp.Params, sp.Size)
		if err != nil {
			return "", nil, err
		}
		return javatool.PathUpscale, javatool.UpscaleReq{Title: "画布 · 高清放大", ImageURL: img, Resolution: res}, nil
	case model.KindToolOutpaint:
		r := javatool.RatioConvertReq{Title: "画布 · 扩图", ImageURLs: []string{img}}
		if w, h := toInt(sp.Params["width"]), toInt(sp.Params["height"]); w > 0 && h > 0 {
			r.Width, r.Height = w, h
		} else {
			r.AspectRatio = sp.Ratio
			r.Resolution = strings.ToLower(firstNonEmpty(paramStr(sp.Params, "resolution"), "2k"))
		}
		return javatool.PathOutpaint, r, nil
	case model.KindToolInpaint:
		var box NormBox
		_, hasBox := sp.Params["bbox"]
		_, hasStrokes := sp.Params["mask_strokes"]
		if forQuote && !hasBox && !hasStrokes && paramStr(sp.Params, "mask_data_url") == "" {
			box = NormBox{0, 0, 1, 1}
		} else {
			b, err := c.maskBox(ctx, run, sp)
			if err != nil {
				return "", nil, err
			}
			box = b
		}
		return javatool.PathInpaint, javatool.InpaintReq{
			Title: "画布 · 局部重绘", Content: sp.Prompt, Num: 1,
			Images: []javatool.InpaintImage{{URL: img, BBox: BoxPoints(box)}},
		}, nil
	}
	return "", nil, fmt.Errorf("canvas: %s 不走 Java", run.Kind)
}

// BoxPoints 归一化框 → Java bbox 四点（左上、右上、右下、左下）。
func BoxPoints(b NormBox) []javatool.RatioPoint {
	r := func(v float64) float64 { return float64(int(v*10000+0.5)) / 10000 }
	x0, y0, x1, y1 := r(b.X), r(b.Y), r(b.X+b.W), r(b.Y+b.H)
	return []javatool.RatioPoint{{X: x0, Y: y0}, {X: x1, Y: y0}, {X: x1, Y: y1}, {X: x0, Y: y1}}
}

// maskBox 重绘区域：params.bbox（4 点）> mask_strokes 外接矩形 > 蒙版图标记像素的外接矩形。
// 蒙版图来源：mask_rid（RunNode 已把 mask_data_url 拉入媒体服务）> mask_data_url（旧数据）> mask_url（交给媒体服务安全拉取，
// 本进程不直接访问用户给的地址，防 SSRF / 跟随跳转进内网）。解码前先 DecodeConfig 限制尺寸（防解压炸弹）。
func (c *core) maskBox(ctx context.Context, run *model.NodeRun, sp *runSpec) (NormBox, error) {
	if pts, ok := sp.Params["bbox"].([]any); ok && len(pts) == 4 {
		minX, minY, maxX, maxY := 1.0, 1.0, 0.0, 0.0
		for _, p := range pts {
			m, _ := p.(map[string]any)
			x, ok1 := toFloat(m["x"])
			y, ok2 := toFloat(m["y"])
			if !ok1 || !ok2 {
				return NormBox{}, xerr.Param("bbox 每个点需要 x / y（0–1）")
			}
			minX, minY, maxX, maxY = min(minX, x), min(minY, y), max(maxX, x), max(maxY, y)
		}
		b := clampBox(NormBox{minX, minY, maxX - minX, maxY - minY})
		if b.W <= 0 || b.H <= 0 {
			return NormBox{}, xerr.Param("bbox 为空")
		}
		return b, nil
	}
	if s, ok := sp.Params["mask_strokes"].([]any); ok {
		if b, ok := strokeBounds(s); ok {
			return b, nil
		}
	}
	data, err := c.maskBytes(ctx, sp.Params)
	if err != nil {
		return NormBox{}, err
	}
	img, err := decodeMask(data)
	if err != nil {
		return NormBox{}, err
	}
	b, ok := MaskBounds(img)
	if !ok {
		return NormBox{}, xerr.Param("蒙版上没有涂抹区域")
	}
	return b, nil
}

// maskBytes 取蒙版原始字节（见 maskBox 的来源顺序）。
func (c *core) maskBytes(ctx context.Context, p map[string]any) ([]byte, error) {
	if rid := paramStr(p, "mask_rid"); rid != "" {
		if c.s.Media == nil {
			return nil, xerr.New(xerr.NotReady, "局部重绘暂未开放（媒体服务未配置）")
		}
		b, _, err := c.s.Media.FetchBytes(ctx, rid)
		if err != nil {
			return nil, mediaErr("读取蒙版失败", err)
		}
		return b, nil
	}
	if du := paramStr(p, "mask_data_url"); du != "" {
		return decodeDataURL(du)
	}
	if u := paramStr(p, "mask_url"); isHTTP(u) {
		if c.s.Media == nil {
			return nil, xerr.New(xerr.NotReady, "局部重绘暂未开放（媒体服务未配置）")
		}
		res, err := c.s.Media.IngestURL(ctx, u, mediasvc.PurposeUserInput, "")
		if err != nil {
			return nil, mediaErr("拉取蒙版失败", err)
		}
		b, _, err := c.s.Media.FetchBytes(ctx, res.RID)
		if err != nil {
			return nil, mediaErr("读取蒙版失败", err)
		}
		return b, nil
	}
	return nil, xerr.Param("局部重绘缺少蒙版")
}

// normalizeMask RunNode 落库前：mask_data_url 校验尺寸后拉入媒体服务，参数里换成 mask_rid（request 列不存 dataURL）。
// 媒体服务按内容 sha256 做幂等，同一蒙版重放得到同一个 rid，幂等比对不受影响。
func (c *core) normalizeMask(ctx context.Context, req *canvas.RunNodeRequest) error {
	if k, err := ResolveKind(req.Kind, req.Tool); err != nil || k != model.KindToolInpaint {
		return nil
	}
	p, err := parseParams(req.ToolParams)
	if err != nil {
		return err
	}
	du := paramStr(p, "mask_data_url")
	if du == "" {
		if _, ok := p["mask_rid"]; ok { // mask_rid 只能由服务端写入（防引用别人的媒体资源）
			delete(p, "mask_rid")
			b, _ := json.Marshal(p)
			req.ToolParams = string(b)
		}
		return nil
	}
	data, err := decodeDataURL(du)
	if err != nil {
		return err
	}
	if _, err := maskConfig(data); err != nil {
		return err
	}
	if c.s.Media == nil {
		return xerr.New(xerr.NotReady, "局部重绘暂未开放（媒体服务未配置）")
	}
	sum := sha256.Sum256(data)
	res, err := c.s.Media.IngestBytes(ctx, "mask.png", data, mediasvc.PurposeUserInput, "canvas-mask:"+hex.EncodeToString(sum[:]))
	if err != nil {
		if e := mediaErr("蒙版上传失败", err); isTemporary(e) {
			logx.WithContext(ctx).Errorf("canvas: 蒙版拉入媒体服务失败: %v", err)
			return xerr.New(xerr.ServerError, "蒙版上传失败，请稍后重试")
		} else {
			return e
		}
	}
	delete(p, "mask_data_url")
	p["mask_rid"] = res.RID
	b, err := json.Marshal(p)
	if err != nil {
		return xerr.Param("params 不是合法 JSON")
	}
	req.ToolParams = string(b)
	return nil
}

func decodeDataURL(s string) ([]byte, error) {
	head, payload, ok := strings.Cut(s, ",")
	if !ok || !strings.HasPrefix(strings.ToLower(head), "data:image/") || !strings.Contains(head, ";base64") {
		return nil, xerr.Param("mask_data_url 必须是 data:image/*;base64,…")
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxMaskBytes+3 {
		return nil, xerr.Param("蒙版超过 20MB")
	}
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, xerr.Param("mask_data_url 不是合法的 base64")
	}
	if len(b) > maxMaskBytes {
		return nil, xerr.Param("蒙版超过 20MB")
	}
	return b, nil
}

// 蒙版上限：字节 20MB；宽高各 ≤4096 且总像素 ≤16M（解码后最多约 64MB）。
const (
	maxMaskBytes  = 20 << 20
	maxMaskSide   = 4096
	maxMaskPixels = 16 << 20
)

// maskConfig 只读文件头取尺寸并校验上限（不解码像素）。
func maskConfig(data []byte) (image.Config, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return cfg, xerr.Param("蒙版不是可识别的 PNG / JPEG 图片")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > maxMaskSide || cfg.Height > maxMaskSide ||
		int64(cfg.Width)*int64(cfg.Height) > maxMaskPixels {
		return cfg, xerr.Param(fmt.Sprintf("蒙版尺寸 %d×%d 超出上限（宽高各 ≤%d）", cfg.Width, cfg.Height, maxMaskSide))
	}
	return cfg, nil
}

// decodeMask 先校验声明尺寸再解码。
func decodeMask(data []byte) (image.Image, error) {
	if len(data) > maxMaskBytes {
		return nil, xerr.Param("蒙版超过 20MB")
	}
	if _, err := maskConfig(data); err != nil {
		return nil, err
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, xerr.Param("蒙版不是可识别的 PNG / JPEG 图片")
	}
	return img, nil
}

// pixelReader 按图像具体类型直接读 Pix（避免逐像素 At + 颜色模型转换的接口开销），其余类型退回 At。
func pixelReader(img image.Image) func(x, y int) color.NRGBA {
	switch m := img.(type) {
	case *image.NRGBA:
		return func(x, y int) color.NRGBA {
			i := m.PixOffset(x, y)
			p := m.Pix[i : i+4 : i+4]
			return color.NRGBA{p[0], p[1], p[2], p[3]}
		}
	case *image.RGBA:
		return func(x, y int) color.NRGBA {
			i := m.PixOffset(x, y)
			p := m.Pix[i : i+4 : i+4]
			a := p[3]
			switch a {
			case 0:
				return color.NRGBA{}
			case 255:
				return color.NRGBA{p[0], p[1], p[2], 255}
			}
			un := func(v uint8) uint8 { return uint8(min(255, int(v)*255/int(a))) }
			return color.NRGBA{un(p[0]), un(p[1]), un(p[2]), a}
		}
	case *image.Gray:
		return func(x, y int) color.NRGBA {
			v := m.Pix[m.PixOffset(x, y)]
			return color.NRGBA{v, v, v, 255}
		}
	}
	return func(x, y int) color.NRGBA { return color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA) }
}

// MaskBounds 蒙版标记区域的外接矩形（0–1）。三种蒙版都认：
// 透明底（有透明像素）→ alpha>16 为标记；黑底白笔（几乎全暗）→ 亮像素为标记；原图上的蓝色半透明标注 → 偏蓝像素为标记。
func MaskBounds(img image.Image) (NormBox, bool) {
	r := img.Bounds()
	w, h := r.Dx(), r.Dy()
	if w == 0 || h == 0 {
		return NormBox{}, false
	}
	at := pixelReader(img)
	transparent, bright, total := 0, 0, 0
	for y := r.Min.Y; y < r.Max.Y; y += max(1, h/200) {
		for x := r.Min.X; x < r.Max.X; x += max(1, w/200) {
			c := at(x, y)
			total++
			if c.A < 250 {
				transparent++
			}
			if int(c.R)+int(c.G)+int(c.B) > 96 {
				bright++
			}
		}
	}
	var marked func(c color.NRGBA) bool
	switch {
	case transparent > 0:
		marked = func(c color.NRGBA) bool { return c.A > 16 }
	case bright*2 < total:
		marked = func(c color.NRGBA) bool { return int(c.R)+int(c.G)+int(c.B) > 96 }
	default:
		marked = func(c color.NRGBA) bool { return int(c.B) > int(c.R)+40 && int(c.B) > int(c.G)+40 }
	}
	minX, minY, maxX, maxY := w, h, -1, -1
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if marked(at(x, y)) {
				px, py := x-r.Min.X, y-r.Min.Y
				minX, minY, maxX, maxY = min(minX, px), min(minY, py), max(maxX, px), max(maxY, py)
			}
		}
	}
	if maxX < 0 {
		return NormBox{}, false
	}
	return NormBox{
		X: float64(minX) / float64(w), Y: float64(minY) / float64(h),
		W: float64(maxX-minX+1) / float64(w), H: float64(maxY-minY+1) / float64(h),
	}, true
}

// ---------------- studio 试穿 ----------------

// submitStudio 试穿整批交给 studio：每位模特 1 张，返回 batch_id。
func (c *core) submitStudio(ctx context.Context, run *model.NodeRun, sp *runSpec) (string, error) {
	if c.s.Studio == nil {
		return "", xerr.New(xerr.NotReady, "模特试穿暂未开放，请稍后再试")
	}
	garment, models, err := tryonInputs(sp)
	if err != nil {
		return "", err
	}
	items := []string{"top"}
	if xs, ok := sp.Params["items"].([]any); ok && len(xs) > 0 {
		items = items[:0]
		for _, x := range xs {
			if s, ok := x.(string); ok {
				items = append(items, s)
			}
		}
	}
	role := firstNonEmpty(paramStr(sp.Params, "garment_name"), "整套")
	reqID, err := runKey(run)
	if err != nil {
		return "", err
	}
	resp, err := c.s.Studio.SubmitTryon(ctx, &studio.SubmitTryonReq{
		UserId: run.UserID, RequestId: reqID,
		ModelImageUrls: models, GarmentImages: []*studio.GarmentImage{{Url: garment, Role: role, Angle: "front"}},
		Items: items, SetMode: firstNonEmpty(paramStr(sp.Params, "set_mode"), "one"),
		Fabric: paramStr(sp.Params, "fabric"), Prompt: sp.Prompt,
		Model:       firstNonEmpty(paramStr(sp.Params, "model"), "通用"),
		Resolution:  strings.ToLower(firstNonEmpty(paramStr(sp.Params, "resolution"), "1k")),
		AspectRatio: firstNonEmpty(paramStr(sp.Params, "aspect_ratio"), "3:4"), CountPerModel: 1,
	})
	if err != nil {
		if code, msg, ok := xerr.Parse(err); ok {
			return "", xerr.New(code, msg)
		}
		return "", temporary(err)
	}
	return strconv.FormatInt(resp.BatchId, 10), nil
}

// ---------------- 媒体服务裁剪（同步） ----------------

// runCrop 裁剪：输入图拉入 → 归一化框换像素框 → image/crop → 复制到素材库桶。
// Library.AsyncCopy 时不在这里复制：候选是裁剪结果的私有引用 mr://（读时现签），由 succeed 落转存 outbox 后台复制。
func (c *core) runCrop(ctx context.Context, run *model.NodeRun, sp *runSpec) ([]model.Candidate, error) {
	if c.s.Media == nil {
		return nil, xerr.New(xerr.NotReady, "裁剪暂未开放（媒体服务未配置）")
	}
	box, err := CropBox(sp.Params)
	if err != nil {
		return nil, err
	}
	src, err := c.s.Media.IngestURL(ctx, sp.Images[0].URL, mediasvc.PurposeIntermediate, "")
	if err != nil {
		return nil, mediaErr("拉取原图失败", err)
	}
	w, h := src.Width, src.Height
	if w == 0 || h == 0 {
		info, err := c.s.Media.Probe(ctx, src.RID)
		if err != nil {
			return nil, mediaErr("读取原图尺寸失败", err)
		}
		w, h = info.Width, info.Height
	}
	px := mediasvc.BoxFromNorm(box.X, box.Y, box.W, box.H, w, h)
	crops, err := c.s.Media.Crop(ctx, src.RID, []mediasvc.PixelBox{px}, mediasvc.CropParams{}, "")
	if err != nil || len(crops) == 0 {
		return nil, mediaErr("裁剪失败", err)
	}
	ext := mimeExt(crops[0].Mime, false)
	if c.asyncCopyOn(run) {
		return []model.Candidate{{RID: crops[0].RID, URL: mrRef(crops[0].RID, ext), W: crops[0].Width, H: crops[0].Height}}, nil
	}
	cand, err := c.copyToLibrary(ctx, crops[0].RID, "canvas-copy:"+run.TaskID.String+":0", ext)
	if err != nil {
		return nil, err
	}
	if cand.W == 0 {
		cand.W, cand.H = crops[0].Width, crops[0].Height
	}
	return []model.Candidate{cand}, nil
}

// copyToLibrary rid → 素材库桶（公有可读）的候选。素材库桶没给公开地址时落素材库 rid 的长期引用 mr://{rid}.{ext}
// （读时现签），绝不落签名地址。
func (c *core) copyToLibrary(ctx context.Context, rid, idem, ext string) (model.Candidate, error) {
	res, err := c.s.Media.CopyToLibrary(ctx, rid, mediasvc.PurposeAssetLibrary, idem)
	if err != nil {
		return model.Candidate{}, mediaErr("转存素材库失败", err)
	}
	u := res.URL
	if u == "" {
		if !mediasvc.ValidRID(res.RID) {
			return model.Candidate{}, mediaErr("转存素材库失败", &mediasvc.Error{Status: 422, Code: "no_rid", Message: "素材库没有返回资源号"})
		}
		u = mrRef(res.RID, ext)
	}
	return model.Candidate{RID: res.RID, URL: u, W: res.Width, H: res.Height}, nil
}

// specFromRun 由落库的请求重建 runSpec（轮询器重提用）。
func (c *core) specFromRun(ctx context.Context, run *model.NodeRun) (*runSpec, error) {
	s := parseStored(run.Request)
	req := &canvas.RunNodeRequest{
		Model: s.Model, ModelId: s.ModelID, Kind: s.Kind, Tool: s.Tool, Prompt: s.Prompt, Ratio: s.Ratio, Size: s.Size,
		Quality: s.Quality, OutputFormat: s.OutputFormat, Duration: s.Duration, Sound: s.Sound, Count: s.Count, Cfg: s.Cfg, ToolParams: string(s.Params),
	}
	for _, r := range s.References {
		req.References = append(req.References, &canvas.RunReference{
			Handle: r.Handle, Index: r.Index, NodeId: r.NodeID, Text: r.Text, Image: r.Image, Video: r.Video, Audio: r.Audio,
			AssetId: r.AssetID, MediaId: r.MediaID, SourceHandle: r.SourceHandle, CandidateIndex: r.CandidateIndex, Mime: r.Mime,
		})
	}
	lookup := c.runRefResolver(ctx, run, s) // 只现签受理时冻结的上游引用，不重新取「最近一次成功」
	sp, err := c.buildSpec(req, lookup)
	if err != nil {
		return nil, err
	}
	// 档位后来换了模型组（如 2026-11-10 SD5P 从 Seedream 5.5 回到 Seedream 5.0 Pro）：冻结组码与现配置不等价、
	// 但现配置里还有出这个组的档位（SD55）→ 按冻结组码的档位重建，在途 run 仍按受理时的模型 / 分辨率出图（与冻结的价一致，
	// 同一幂等键的请求体不变）。现配置里已没有这个组 → 按现配置（与此前一致）。
	if run.ModelGroup != "" && sp.ModelGroup != "" && !modelgroup.Equal(run.ModelGroup, sp.ModelGroup) &&
		(sp.Kind == model.KindImage || sp.Kind == model.KindVideo) {
		if m, ok := c.s.Config.FindModel(run.ModelGroup, "", sp.Model.Kind); ok && modelgroup.Equal(m.ModelGroup, run.ModelGroup) {
			req.Model, req.ModelId = m.Model, m.ModelID // req 是本函数现拼的，改它不影响落库请求
			if fsp, ferr := c.buildSpec(req, lookup); ferr == nil {
				sp = fsp
			} else {
				logx.WithContext(ctx).Errorf("canvas: run %d 按冻结组 %s 重建失败，按现配置 %s 重提: %v", run.ID, run.ModelGroup, sp.ModelGroup, ferr)
			}
		}
	}
	// 受理时冻结的组码与现配置等价（如换代前落库的旧 Dev 组码 vs 现在的规范组，见 pkg/modelgroup）：
	// 重提沿用冻结值，同一幂等键的请求体不变，网关不会判幂等冲突；计费按等价组选档，同价。
	if run.ModelGroup != "" && run.ModelGroup != sp.ModelGroup && modelgroup.Equal(run.ModelGroup, sp.ModelGroup) {
		sp.ModelGroup = run.ModelGroup
	}
	return sp, nil
}

// dispatch 把排队的 run 交给后端：成功提交 → 运行中（media 直接到终态）；暂时性错误 → 留在排队，attempt+1；
// 其它错误 → 失败（gateway 后端退款），并把错误返回给调用方（RunNode 内联提交时直接回给前端）。
func (c *core) dispatch(ctx context.Context, run *model.NodeRun, sp *runSpec, bearer string) error {
	log := logx.WithContext(ctx)
	var (
		ref   string
		cands []model.Candidate
		err   error
	)
	var acquired []string // 已拿到的 gateway 调用（失败收尾时取消）
	dn := int(run.Attempt) + 1
	c.evStart(ctx, run, stDispatch, dn, runAttrs(run, nil))
	switch run.Backend {
	case model.BackendGateway:
		if acquired, err = c.submitGateway(ctx, run, sp); err == nil {
			ref = strings.Join(acquired, ",")
		}
	case model.BackendJava:
		if !c.s.Java.Enabled() {
			err = xerr.New(xerr.NotReady, "该工具暂未开放，请稍后再试")
		} else {
			ref, err = c.submitJava(ctx, run, sp, bearer)
		}
	case model.BackendStudio:
		ref, err = c.submitStudio(ctx, run, sp)
	case model.BackendMedia:
		cands, err = c.runCrop(ctx, run, sp)
	default:
		err = fmt.Errorf("canvas: 未知后端 %s", run.Backend)
	}
	if err != nil {
		c.evEnd(ctx, run, stDispatch, dn, taskevent.StatusFailed, dispatchFail(err), runAttrs(run, taskevent.Attrs{"temporary": isTemporary(err)}))
		if isTemporary(err) && int(run.Attempt)+1 < c.s.Config.Poll.MaxSubmitAttempts {
			log.Infof("canvas: run %d 提交暂时失败（第 %d 次），稍后重试: %v", run.ID, run.Attempt+1, err)
			_ = c.s.Repo.Run.Update(ctx, run.ID, map[string]any{"attempt": run.Attempt + 1})
			run.Attempt++
			return nil
		}
		code, msg, ok := xerr.Parse(err)
		if !ok {
			log.Errorf("canvas: run %d 提交失败: %v", run.ID, err)
			code, msg = xerr.ServerError, "提交失败，请稍后重试"
		}
		ecode := "SUBMIT_FAILED"
		if code == xerr.ParamError {
			ecode = "PARAM_INVALID"
		}
		if c.javaMaybeSubmitted(run, err) {
			// 已落 backend_request、此前（或这次）的请求结局未知：Java 可能已建任务并冻结点数，不能直接置失败留下孤儿。
			// 走 stopJava：同键重放找回任务号再取消；找不回 / 被拒按退化告警计数；暂时失败登记待取消由 Poller 退避重试。
			log.Errorf("canvas: run %d Java 提交失败（第 %d 次，%v），按幂等键找回并取消可能已建的任务", run.ID, run.Attempt+1, err)
			c.stopJava(ctx, run, stopSubmit, msg, bearer, 0)
			return xerr.New(code, msg)
		}
		failed := c.failRun(ctx, run, model.RunQueued, ecode, msg, true)
		if failed {
			// 抢到失败（已退款）：已拿到的调用都是孤儿，一律取消
			c.cancelCalls(ctx, run.ID, acquired, "提交失败")
		} else {
			// 没抢到：run 已被别处推进——取消 / 强制失败 / 超时（钱已由那条路径处理），或提交太久、行租约过期后被别的实例
			// 接手并以同一幂等键重提拿回了同一批调用、正在跟踪。只取消 run 行没记录的（见 cancelUnrecordedCalls）
			c.cancelUnrecordedCalls(ctx, run.ID, acquired, "提交失败")
		}
		if failed {
			if run.Backend == model.BackendGateway && (run.Attempt > 0 || submitMaybeCreated(err)) {
				// 此前（或这次超时的）提交可能已在 gateway 建了调用但 backend_ref 没写上：按幂等键取回并取消
				r := *run
				r.Attempt = max(r.Attempt, 1)
				c.reclaimQueuedCalls(ctx, &r, "提交失败")
			}
		}
		return xerr.New(code, msg)
	}
	if run.Backend == model.BackendMedia {
		if ok, err := c.s.Repo.Run.Transit(ctx, run.ID, model.RunQueued, model.RunRunning, map[string]any{"attempt": run.Attempt + 1}); err != nil || !ok {
			c.evEnd(ctx, run, stDispatch, dn, taskevent.StatusFailed, taskevent.Fail{Class: "LOCAL", Code: "STATE_CHANGED", Check: "canvas.dispatch_cas", Message: traceErrMsg(err)}, runAttrs(run, nil))
			return err
		}
		run.Status = model.RunRunning
		c.traceRunning(ctx, run, dn, 1)
		c.succeed(ctx, run, outcome{done: true, ok: true}, cands)
		return nil
	}
	ok, err := c.s.Repo.Run.Transit(ctx, run.ID, model.RunQueued, model.RunRunning, map[string]any{
		"backend_ref": ref, "attempt": run.Attempt + 1, "progress": 5,
	})
	if err != nil {
		// 写库失败：run 仍在排队，下一轮用同一幂等键重提会拿回同一批调用，不取消
		log.Errorf("canvas: run %d 提交成功但转运行中写库失败（下轮按幂等键重提）: %v", run.ID, err)
		_ = c.s.Repo.Run.Update(ctx, run.ID, map[string]any{"attempt": run.Attempt + 1})
		run.Attempt++
		c.evEnd(ctx, run, stDispatch, dn, taskevent.StatusFailed, taskevent.Fail{Class: "LOCAL", Code: "DISPATCH_WRITE", Check: "canvas.dispatch_write", Message: traceErrMsg(err)}, runAttrs(run, nil))
		return nil
	}
	if !ok {
		c.evEnd(ctx, run, stDispatch, dn, taskevent.StatusSkipped, taskevent.Fail{Class: "LOCAL", Code: "STATE_CHANGED", Check: "canvas.dispatch_cas", Message: "提交期间已被取消 / 强制失败 / 超时"}, runAttrs(run, nil))
		// 提交期间 run 已被取消 / 强制失败 / 超时：刚拿到的上游调用成了孤儿，取消掉
		switch run.Backend {
		case model.BackendGateway:
			// 同一批调用可能已被接手本 run 的别的实例（同幂等键）记进 backend_ref 在跟踪：只取消没记录的
			c.cancelUnrecordedCalls(ctx, run.ID, acquired, "提交期间已结束")
		case model.BackendStudio:
			c.cancelStudio(ctx, run, ref, "提交期间已结束")
		case model.BackendJava:
			c.cancelJavaOrphan(ctx, run, ref, bearer)
		}
		return nil
	}
	run.Status, run.BackendRef.String, run.BackendRef.Valid = model.RunRunning, ref, true
	c.traceRunning(ctx, run, dn, len(acquired))
	if run.Backend == model.BackendGateway {
		c.reportGatewayCalls(ctx, run, acquired, bearer)
	}
	return nil
}

// reportGatewayCalls 把画布直接调网关得到的调用号回报给 Java 创作任务（非工作流任务的链路据此补网关阶段）。
//
// 异步、不阻塞调度、不影响运行本身。暂时性失败（网络 / 5xx / 429）有限次重试，4xx 不重试（凭证失效、参数错误重试无意义）。
// 仍然失败只记日志：链路缺一段；历史缺口由 scripts/canvas/backfill-gateway-links.py 补录（Java 侧按调用号幂等）。
func (c *core) reportGatewayCalls(ctx context.Context, run *model.NodeRun, callIDs []string, bearer string) {
	jid := javatool.JavaTaskIDOf(run.TaskID.String)
	if jid == "" || len(callIDs) == 0 || bearer == "" || !c.s.Java.Enabled() {
		return
	}
	ids := append([]string(nil), callIDs...)
	base := context.WithoutCancel(ctx)
	go func() {
		var err error
		for attempt := 0; attempt < linkMaxAttempts; attempt++ {
			if attempt > 0 {
				time.Sleep(linkBackoff[attempt-1])
			}
			cctx, cancel := context.WithTimeout(base, linkAttemptTimeout)
			err = c.s.Java.LinkGatewayCalls(cctx, bearer, jid, ids)
			cancel()
			if err == nil {
				return
			}
			if !linkRetryable(err) {
				break // 4xx：永久失败，不重试
			}
		}
		logx.Errorf("canvas: run %d 回报网关调用到 Java 任务 %s 失败（链路缺网关段，不影响运行）: %v", run.ID, jid, err)
	}()
}

// linkRetryable 暂时性错误才值得重试：网络失败、5xx、429；4xx（凭证失效、参数错误）重试无意义。
func linkRetryable(err error) bool {
	var je *javatool.Error
	if errors.As(err, &je) {
		return je.Temporary()
	}
	return true
}

const linkMaxAttempts = 3

// linkBackoff 第 n 次重试前的等待（下标 0 = 第二次尝试前）。
var linkBackoff = []time.Duration{time.Second, 3 * time.Second}

const linkAttemptTimeout = 5 * time.Second

// imageOutputFormat 生图实际下发给网关的 output_format：空（改版前落库的 run 重新派发）按 jpeg。
// 格式不影响价格，不进冻结 / 估价。
func imageOutputFormat(sp *runSpec) string {
	if sp.OutputFormat == "" {
		return defaultOutputFormat
	}
	return sp.OutputFormat
}

// imageQuality 生图实际下发给网关的 quality（派发与计费共用，按请求参数冻结的质量与出图一致）。
func imageQuality(sp *runSpec) string {
	quality := sp.Quality
	if len(sp.Model.Qualities) > 0 && quality == "" {
		quality = sp.Model.DefaultQuality() // 2026-11-19 起缺省 high（Atlas 部署缺省 low，必须显式给）；与冻结 Usage.Quality 同一取值
	}
	// Nano Banana 2.1 的部署没有 quality 参数：一律不发，兜住改版前落库、带了 quality 的 run 重新派发。
	// GPT Image 2.5 自 2026-11-19 起切标准版（支持画质），不再清空。
	if modelgroup.Is(sp.ModelGroup, modelgroup.NanoBanana21) {
		quality = ""
	}
	return quality
}

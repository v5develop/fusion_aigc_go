package logic

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"aigc-local-life/app/canvas/model"
	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"
	"aigc-local-life/pkg/xerr"

	"google.golang.org/grpc/metadata"
)

// BearerMetadataKey 网关把用户 Authorization 原文放进 gRPC metadata 的键（小写，gRPC 要求）。
// 网关侧见 app/gateway/api/internal/logic/canvas/bearer.go。
const BearerMetadataKey = "x-user-authorization"

// BillingPrefix node run 计费请求号前缀：billing.Charge.RequestID = "canvas-<task_id>"。
const BillingPrefix = "canvas-"

// taskIDStage 任务号步骤段：aigc.NewLifeTaskID(cv, taskcenter.Ref(cv, id), "run")。
const taskIDStage = "run"

// core 各 RPC 共用的业务实现（生成的 *Logic 只做一行转发）。
type core struct {
	s   *svc.ServiceContext
	now func() time.Time

	renewAt time.Time // 上次续期预留单的时间（只有 Poller 的 core 用；零值 = 下一轮立刻做）

	// 以下只有 Poller 的 core 用（单 goroutine）。
	reconcileAfter   int64          // 计费镜像对账的 id 游标（一页取满就从这里接着取，取不满回到 0）
	unsettledAlerted map[int64]bool // 已 ERROR 告警过「交付了没扣成费」的 run（本进程只报一次，之后降为 Info）
}

func newCore(s *svc.ServiceContext) *core { return &core{s: s, now: time.Now} }

// BearerFrom 取请求里透传的用户 Bearer（没有返回空串）。
func BearerFrom(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if v := md.Get(BearerMetadataKey); len(v) > 0 {
		return strings.TrimSpace(v[0])
	}
	return ""
}

// rememberBearer 记住用户的 Bearer，返回它（后台轮询 Java 任务时代用户查询）。
func (c *core) rememberBearer(ctx context.Context, userID int64) string {
	b := BearerFrom(ctx)
	if b != "" {
		c.s.Bearers.Put(userID, b)
		return b
	}
	return c.s.Bearers.Get(userID)
}

func needUser(userID int64) error {
	if userID <= 0 {
		return xerr.New(xerr.Unauthorized, "未登录")
	}
	return nil
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// storedRequest afv_canvas_node_run.request 的结构（接口字段下划线化，params 原样）。
type storedRequest struct {
	Model      string          `json:"model"`
	ModelID    string          `json:"model_id"`
	Kind       string          `json:"kind"`
	Tool       string          `json:"tool,omitempty"`
	Prompt     string          `json:"prompt"`
	References []storedRef     `json:"references"`
	Ratio      string          `json:"ratio"`
	Size       string          `json:"size"`
	Quality    string          `json:"quality"`
	Duration   string          `json:"duration,omitempty"`
	Sound      bool            `json:"sound,omitempty"`
	Count      int32           `json:"count"`
	Cfg        float64         `json:"cfg"`
	Params     json.RawMessage `json:"params,omitempty"`
	// Frozen 受理时服务端解析出的上游引用（见 ref_resolve.go）；不是客户端请求的一部分，幂等比对时忽略。
	// 本字段之前受理的 run 没有它（nil），重新派发按旧行为。
	Frozen *frozenRefs `json:"frozen,omitempty"`
}

type storedRef struct {
	Handle         string `json:"handle"`
	Index          int32  `json:"index"`
	NodeID         string `json:"node_id"`
	Text           string `json:"text,omitempty"`
	Image          string `json:"image,omitempty"`
	Video          string `json:"video,omitempty"`
	Audio          string `json:"audio,omitempty"`
	AssetID        string `json:"asset_id,omitempty"`
	MediaID        string `json:"media_id,omitempty"`
	SourceHandle   string `json:"source_handle,omitempty"`
	CandidateIndex *int32 `json:"candidate_index,omitempty"`
	Mime           string `json:"mime,omitempty"`
}

func toStored(r *canvas.RunNodeRequest) storedRequest {
	s := storedRequest{
		Model: r.Model, ModelID: r.ModelId, Kind: r.Kind, Tool: r.Tool, Prompt: r.Prompt, Ratio: r.Ratio, Size: r.Size,
		Quality: r.Quality, Duration: r.Duration, Sound: r.Sound, Count: r.Count, Cfg: r.Cfg,
		References: make([]storedRef, 0, len(r.References)),
	}
	if p := strings.TrimSpace(r.ToolParams); p != "" && json.Valid([]byte(p)) {
		s.Params = json.RawMessage(p)
	}
	for _, x := range r.References {
		s.References = append(s.References, storedRef{
			Handle: x.Handle, Index: x.Index, NodeID: x.NodeId, Text: x.Text, Image: x.Image, Video: x.Video, Audio: x.Audio,
			AssetID: x.AssetId, MediaID: x.MediaId, SourceHandle: x.SourceHandle, CandidateIndex: x.CandidateIndex, Mime: x.Mime,
		})
	}
	return s
}

func parseStored(raw json.RawMessage) storedRequest {
	var s storedRequest
	_ = json.Unmarshal(raw, &s)
	return s
}

// sameRequest 幂等比对：两份请求 JSON 语义相等（库里的 JSON 列会重排键序，不能比字节）。
// 服务端冻结的 frozen 不参与比对（同一请求在不同时刻受理会解析出不同的上游产物）。
func sameRequest(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	for _, v := range []any{x, y} {
		if m, ok := v.(map[string]any); ok {
			delete(m, "frozen")
		}
	}
	ax, _ := json.Marshal(x)
	by, _ := json.Marshal(y)
	return string(ax) == string(by)
}

// expected 一类 run 的预计耗时（进度估算用）与总时限。
func (c *core) budget(k model.RunKind) (expected, timeout time.Duration) {
	t := c.s.Config.Timeouts
	switch k {
	case model.KindImage:
		return 60 * time.Second, time.Duration(t.Image) * time.Second
	case model.KindVideo:
		return 180 * time.Second, time.Duration(t.Video) * time.Second
	case model.KindToolI2P:
		return 20 * time.Second, time.Duration(t.I2P) * time.Second
	case model.KindToolTryon:
		return 150 * time.Second, time.Duration(t.Tool) * time.Second
	}
	return 60 * time.Second, time.Duration(t.Tool) * time.Second
}

// hardCap gateway 后端 run 的硬上限（从创建算起）：调用已被供应商受理时软时限过后继续等到这里。不小于软时限。
func (c *core) hardCap(k model.RunKind) time.Duration {
	_, soft := c.budget(k)
	t := c.s.Config.Timeouts
	var sec int
	switch k {
	case model.KindImage:
		sec = t.ImageHardCap
	case model.KindVideo:
		sec = t.VideoHardCap
	case model.KindToolI2P:
		sec = t.I2PHardCap
	}
	return max(soft, time.Duration(sec)*time.Second)
}

// progressOf 进度估算：排队 0，运行中按已耗时 / 预计耗时线性到 95，成功 100，其余取库里的值。
func (c *core) progressOf(r *model.NodeRun) int32 {
	switch r.Status {
	case model.RunQueued:
		return 0
	case model.RunSucceeded:
		return 100
	case model.RunRunning:
		start := r.CreatedAt
		if r.StartedAt.Valid {
			start = r.StartedAt.Time
		}
		exp, _ := c.budget(r.Kind)
		p := 5 + int32(float64(c.now().Sub(start))/float64(exp)*90)
		return min(max(p, max(5, r.Progress)), 95)
	}
	return r.Progress
}

// nonRetryable 这些失败码换一个 run_id 重试也没用。
var nonRetryable = map[string]bool{"CONTENT_BLOCKED": true, "INPUT_REJECTED": true, "PARAM_INVALID": true}

// toResult NodeRun → RunNodeResult。
func (c *core) toResult(ctx context.Context, r *model.NodeRun) *canvas.RunResult {
	req := parseStored(r.Request)
	out := &canvas.RunResult{
		RequestId: strconv.FormatInt(r.ID, 10), RunId: r.RunID, CanvasId: r.CanvasID, NodeId: r.NodeID,
		Kind: string(r.Kind), Status: model.StatusText(r.Status), Progress: c.progressOf(r), Model: req.Model,
		SelectedIndex: r.SelectedIndex, Text: r.ResultText.String, VideoUrl: r.VideoURL.String,
		Credits: r.Points, RequestedCount: r.RequestedCount, CreatedAt: ms(r.CreatedAt), TaskId: r.TaskID.String,
	}
	if out.Model == "" {
		out.Model = req.Tool
	}
	if r.FinishedAt.Valid {
		out.FinishedAt = ms(r.FinishedAt.Time)
	}
	if r.Status == model.RunSucceeded {
		rr := c.resolvedRun(ctx, r)
		for _, x := range rr.CandidateList() {
			out.Candidates = append(out.Candidates, &canvas.Candidate{Url: x.URL, Width: int32(x.W), Height: int32(x.H)})
		}
		out.VideoUrl = rr.VideoURL.String
		out.LibraryStatus = libraryStatus(r)
	}
	if r.Status == model.RunFailed || r.Status == model.RunCanceled {
		out.Error = r.ErrorReason.String
		out.ErrorCode = r.ErrorCode.String
		if r.Status == model.RunCanceled && out.Error == "" {
			out.Error = "已取消执行"
		}
		out.Retryable = r.Status == model.RunFailed && !nonRetryable[out.ErrorCode]
		if r.BillingStatus == model.BillRefunded {
			out.Credits = 0
		}
	}
	return out
}

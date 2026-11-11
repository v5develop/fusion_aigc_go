package canvas

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/app/gateway/api/internal/middleware"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/xerr"

	"google.golang.org/grpc/metadata"
)

// bearerMetadataKey 与 canvas-rpc logic.BearerMetadataKey 一致。
const bearerMetadataKey = "x-user-authorization"

// rpcCtx 把 CanvasBearer 中间件取到的 Authorization 原文放进 gRPC metadata，canvas-rpc 调 Java 时原样带上。
func rpcCtx(ctx context.Context) context.Context {
	if b := middleware.Bearer(ctx); b != "" {
		return metadata.AppendToOutgoingContext(ctx, bearerMetadataKey, b)
	}
	return ctx
}

// ConflictError 保存版本冲突：handler 据此回 code=4002，data={conflict:"revision", revision, updated_at}。
type ConflictError struct {
	Revision  string
	UpdatedAt int64
}

func (e *ConflictError) Error() string { return "画布已被其他窗口修改" }

// rawJSON JSON 文本 → 原样输出的 json.RawMessage（空 / 非法返回 nil）。
func rawJSON(s string) interface{} {
	if strings.TrimSpace(s) == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// MaxSnapshotBytes 快照上限 2MB（与 canvas-rpc Limits.MaxSnapshotBytes 一致；网关先拦，避免大包打到 gRPC 4MB 上限）。
const MaxSnapshotBytes = 2 << 20

// snapshotText 请求里的 snapshot → 文本：json.RawMessage 原样透传（JSON 字符串形式的快照解一层引号）；超 2MB 回 1003。
func snapshotText(v interface{}) (string, error) {
	var s string
	switch x := v.(type) {
	case nil:
		return "", xerr.New(xerr.ParamMissing, "缺少 snapshot")
	case json.RawMessage:
		if len(x) > 0 && x[0] == '"' {
			if err := json.Unmarshal(x, &s); err != nil {
				return "", xerr.Param("snapshot 不是合法 JSON")
			}
		} else {
			s = string(x)
		}
	case string:
		s = x
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", xerr.Param(fmt.Sprintf("snapshot 不是合法 JSON：%v", err))
		}
		s = string(b)
	}
	if strings.TrimSpace(s) == "" {
		return "", xerr.New(xerr.ParamMissing, "缺少 snapshot")
	}
	if len(s) > MaxSnapshotBytes {
		return "", xerr.Param(fmt.Sprintf("画布内容超过 %dMB", MaxSnapshotBytes>>20))
	}
	return s, nil
}

func toDetail(d *canvas_client.CanvasDoc) *types.CanvasDetail {
	return &types.CanvasDetail{
		Id: d.Id, Title: d.Title, Revision: d.Revision, NodeCount: d.NodeCount, CoverUrl: d.CoverUrl,
		CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt, Snapshot: rawJSON(d.Snapshot),
	}
}

func toRunResult(r *canvas_client.RunResult) types.CanvasRunResult {
	out := types.CanvasRunResult{
		RequestId: r.RequestId, RunId: r.RunId, CanvasId: r.CanvasId, NodeId: r.NodeId, Kind: r.Kind, Status: r.Status,
		Progress: r.Progress, Model: r.Model, SelectedIndex: r.SelectedIndex, VideoUrl: r.VideoUrl, Text: r.Text,
		Credits: r.Credits, RequestedCount: r.RequestedCount, Error: r.Error, ErrorCode: r.ErrorCode, Retryable: r.Retryable,
		CreatedAt: r.CreatedAt, FinishedAt: r.FinishedAt, Candidates: make([]types.CanvasCandidate, 0, len(r.Candidates)),
		LibraryStatus: r.LibraryStatus,
	}
	for _, c := range r.Candidates {
		out.Candidates = append(out.Candidates, types.CanvasCandidate{Url: c.Url, Width: c.Width, Height: c.Height})
	}
	return out
}

func toCatalogModel(m *canvas_client.CatalogModel) types.CanvasCatalogModel {
	out := types.CanvasCatalogModel{
		Model: m.Model, Label: m.Label, ModelId: m.ModelId, Badge: m.Badge, MaxImageRefs: m.MaxImageRefs, MaxReferenceVideos: m.MaxReferenceVideos,
		Ratios: nz(m.Ratios), Sizes: nz(m.Sizes), Qualities: nz(m.Qualities), Durations: nz(m.Durations), Sound: m.Sound,
		CountMax: m.CountMax, Ready: m.Ready, Prices: []types.CanvasPrice{},
		Capabilities: types.CanvasCapabilities{Quality: m.CapQuality, Count: m.CapCount, Duration: m.CapDuration, Sound: m.CapSound},
	}
	for _, p := range m.Prices {
		out.Prices = append(out.Prices, types.CanvasPrice{Size: p.Size, Duration: p.Duration, Points: p.Points})
	}
	return out
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func toTemplate(t *canvas_client.TemplateInfo) types.CanvasTemplate {
	return types.CanvasTemplate{
		Id: t.Id, Scope: t.Scope, Title: t.Title, Description: t.Description, CoverUrl: t.CoverUrl,
		NodeCount: t.NodeCount, EdgeCount: t.EdgeCount, Snapshot: rawJSON(t.Snapshot), CreatedAt: t.CreatedAt,
	}
}

// ParamsText 工具参数（任意 JSON 对象）→ 文本。
func ParamsText(v interface{}) (string, error) {
	if v == nil {
		return "", nil
	}
	if _, ok := v.(map[string]interface{}); !ok {
		return "", xerr.Param("params 必须是 JSON 对象")
	}
	b, err := json.Marshal(v)
	return string(b), err
}

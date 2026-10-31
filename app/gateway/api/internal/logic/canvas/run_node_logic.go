// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package canvas

import (
	"aigc-local-life/app/canvas/rpc/canvas_client"
	"aigc-local-life/pkg/ctxdata"
	"aigc-local-life/pkg/xerr"
	"context"
	"strings"

	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"

	"github.com/zeromicro/go-zero/core/logx"
)

type RunNodeLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 执行单个节点；必须带 Idempotency-Key（= run_id）
func NewRunNodeLogic(ctx context.Context, svcCtx *svc.ServiceContext) *RunNodeLogic {
	return &RunNodeLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *RunNodeLogic) RunNode(req *types.CanvasRunReq) (resp *types.CanvasRunResp, err error) {
	runID := strings.TrimSpace(req.Key)
	if runID == "" {
		return nil, xerr.New(xerr.ParamMissing, "缺少 Idempotency-Key（值为 run_id）")
	}
	if req.RunId != "" && req.RunId != runID {
		return nil, xerr.Param("Idempotency-Key 必须等于 run_id")
	}
	params, err := ParamsText(req.Params)
	if err != nil {
		return nil, err
	}
	in := &canvas_client.RunNodeRequest{
		Model: req.Model, ModelId: req.ModelId, Kind: req.Kind, Prompt: req.Prompt, Ratio: req.Ratio, Size: req.Size,
		Quality: req.Quality, Duration: req.Duration, Sound: req.Sound, Count: req.Count, Cfg: req.Cfg, Tool: req.Tool, ToolParams: params,
	}
	for _, r := range req.References {
		in.References = append(in.References, &canvas_client.RunReference{
			Handle: r.Handle, Index: r.Index, NodeId: r.NodeId, Text: r.Text, Image: r.Image, Video: r.Video, Audio: r.Audio,
			AssetId: r.AssetId, MediaId: r.MediaId, SourceHandle: r.SourceHandle, CandidateIndex: r.CandidateIndex, Mime: r.Mime,
		})
	}
	out, err := l.svcCtx.CanvasRpc.RunNode(rpcCtx(l.ctx), &canvas_client.RunNodeReq{
		UserId: ctxdata.UserID(l.ctx), CanvasId: req.Id, NodeId: req.NodeId, RunId: runID, Request: in})
	if err != nil {
		return nil, err
	}
	warns := out.Warnings
	if warns == nil {
		warns = []string{}
	}
	return &types.CanvasRunResp{RequestId: out.RequestId, RunId: out.RunId, Status: out.Status, Replayed: out.Replayed, Credits: out.Credits,
		Warnings: warns}, nil
}

package logic

import (
	"context"

	"aigc-local-life/app/canvas/rpc/canvas"
	"aigc-local-life/app/canvas/rpc/internal/svc"

	"github.com/zeromicro/go-zero/core/logx"
)

type AgentCallLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewAgentCallLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AgentCallLogic {
	return &AgentCallLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// 执行一个画布智能体工具（Function Call 与 MCP 两种形式共用）
func (l *AgentCallLogic) AgentCall(in *canvas.AgentCallReq) (*canvas.AgentCallResp, error) {
	return newCore(l.svcCtx).AgentCall(l.ctx, in)
}

// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"context"

	"aigc-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-life/app/gateway/api/internal/svc"
	"aigc-life/app/gateway/api/internal/types"
	"aigc-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type AcceptAgreementLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// 同意 AIGC 功能使用约定
func NewAcceptAgreementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *AcceptAgreementLogic {
	return &AcceptAgreementLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *AcceptAgreementLogic) AcceptAgreement() (resp *types.AgreementResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.AcceptAgreement(l.ctx, &digitalhuman.AgreementReq{UserId: ctxdata.UserID(l.ctx), Code: "aigc"})
	if err != nil {
		return nil, err
	}
	return &types.AgreementResp{Code: out.Code, Version: out.Version, Accepted: out.Accepted}, nil
}

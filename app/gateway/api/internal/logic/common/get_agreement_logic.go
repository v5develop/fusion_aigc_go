// Code scaffolded by goctl. Safe to edit.
// goctl 1.10.2

package common

import (
	"context"

	"aigc-local-life/app/digitalhuman/rpc/digitalhuman"
	"aigc-local-life/app/gateway/api/internal/svc"
	"aigc-local-life/app/gateway/api/internal/types"
	"aigc-local-life/pkg/ctxdata"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAgreementLogic struct {
	logx.Logger
	ctx    context.Context
	svcCtx *svc.ServiceContext
}

// AIGC 功能使用约定：是否已同意当前版本
func NewGetAgreementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAgreementLogic {
	return &GetAgreementLogic{
		Logger: logx.WithContext(ctx),
		ctx:    ctx,
		svcCtx: svcCtx,
	}
}

func (l *GetAgreementLogic) GetAgreement() (resp *types.AgreementResp, err error) {
	out, err := l.svcCtx.DigitalHumanRpc.GetAgreement(l.ctx, &digitalhuman.AgreementReq{UserId: ctxdata.UserID(l.ctx), Code: "aigc"})
	if err != nil {
		return nil, err
	}
	return &types.AgreementResp{Code: out.Code, Version: out.Version, Accepted: out.Accepted}, nil
}

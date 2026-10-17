package logic

import (
	"context"
	"errors"

	"aigc-local-life/app/digitalhuman/model"
	"aigc-local-life/app/digitalhuman/rpc/internal/svc"
	"aigc-local-life/app/digitalhuman/rpc/pb"
	"aigc-local-life/pkg/xerr"

	"github.com/zeromicro/go-zero/core/logx"
)

type GetAgreementLogic struct {
	ctx    context.Context
	svcCtx *svc.ServiceContext
	logx.Logger
}

func NewGetAgreementLogic(ctx context.Context, svcCtx *svc.ServiceContext) *GetAgreementLogic {
	return &GetAgreementLogic{
		ctx:    ctx,
		svcCtx: svcCtx,
		Logger: logx.WithContext(ctx),
	}
}

// ---- App ----
func (l *GetAgreementLogic) GetAgreement(in *pb.AgreementReq) (*pb.AgreementInfo, error) {
	ver := int64(l.svcCtx.Config.Agreement.AigcVersion)
	if in.Code != "" && in.Code != agreementAigc {
		return nil, xerr.Newf(xerr.ParamError, "未知的协议 %s", in.Code)
	}
	_, err := l.svcCtx.Agreement.FindOneByUserIdAgreementCodeVersion(l.ctx, in.UserId, agreementAigc, ver)
	if err != nil && !errors.Is(err, model.ErrNotFound) {
		return nil, err
	}
	return &pb.AgreementInfo{Code: agreementAigc, Version: int32(ver), Accepted: err == nil}, nil
}
